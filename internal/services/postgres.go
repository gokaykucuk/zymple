// Package services manages the Postgres and Redis containers Zymple runs
// beside an app.
package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gokaykucuk/zymple/internal/appdef"
	"github.com/gokaykucuk/zymple/internal/config"
	"github.com/gokaykucuk/zymple/internal/execx"
	"github.com/gokaykucuk/zymple/internal/releases"
)

const (
	labelConfig    = "zymple.config"
	flagLabel      = "--label"
	flagUser       = "--user"
	flagFormat     = "--format"
	flagEntrypoint = "--entrypoint"
	envPGData      = "PGDATA="
	rolePostgres   = "postgres"
	roleRedis      = "redis"
	roleDrill      = "postgres-drill"
	pgUser         = "app"
	// pgAdmin is the cluster's superuser. The app's own role is an ordinary
	// one: a superuser can run programs and read files inside the database
	// container, which holds the backup repository's credentials.
	pgAdmin    = "postgres"
	pgDatabase = "app"
	// adminDatabase is the maintenance database role setup connects to.
	adminDatabase = "postgres"
	pgDataMount   = "/var/lib/postgresql/data"
	pgData        = pgDataMount + "/pgdata"
	fullInterval  = 7 * 24 * time.Hour
	diffInterval  = 24 * time.Hour
	readyTimeout  = 10 * time.Minute
	readyInterval = time.Second
)

var (
	targetTimeRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(\.\d{1,6})?(Z|[+-]\d{2}(:?\d{2})?)?$`)
	drillRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
	// What WAL-G and Postgres name their backups and WAL segments. Both
	// names end up on command lines and in SQL, so nothing else is accepted.
	backupNameRe = regexp.MustCompile(`^base_[0-9A-F]{24}(_D_[0-9A-F]{24})?$`)
	walSegmentRe = regexp.MustCompile(`^[0-9A-F]{24}$`)
)

// Postgres manages one Postgres cluster per app.
type Postgres struct {
	Runner execx.Runner
	Config *config.Config
	// Sleep waits between readiness probes; replaced in tests.
	Sleep func(ctx context.Context, d time.Duration)
	Now   func() time.Time
	// Warn receives conditions an operator should know about that do not
	// fail the operation. Nil discards them.
	Warn func(msg string)

	mu sync.Mutex
	// settled holds the apps whose cluster has been through afterStart in
	// this process. The container's label only says it was started with the
	// wanted settings, not that its roles were set up.
	settled map[string]bool
}

func (p *Postgres) isSettled(app string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.settled[app]
}

func (p *Postgres) setSettled(app string, done bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.settled == nil {
		p.settled = map[string]bool{}
	}
	p.settled[app] = done
}

func (p *Postgres) warn(msg string) {
	if p.Warn != nil {
		p.Warn(msg)
	}
}

// PostgresContainer is the name of an app's Postgres container.
func PostgresContainer(app string) string { return "zymple-" + app + "-postgres" }

func (p *Postgres) dir(app string) string {
	return filepath.Join(p.Config.StateDir, "apps", app, "postgres")
}

func (p *Postgres) backupsEnabled() bool { return p.Config.Backup != nil }

// password returns the app role's password, creating it on first use. It is
// re-applied to the cluster at every start, so losing this file only means a
// new password on the next deploy.
func (p *Postgres) password(app string) (string, error) {
	return secretFile(filepath.Join(p.dir(app), "password"))
}

// adminPassword returns the superuser's password. It is only ever given to
// the image's first-run initialisation; Zymple itself connects over the
// container's local socket, and the app never learns it.
func (p *Postgres) adminPassword(app string) (string, error) {
	return secretFile(filepath.Join(p.dir(app), "admin-password"))
}

func secretFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil && len(strings.TrimSpace(string(data))) >= 32 {
		return strings.TrimSpace(string(data)), nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(buf)
	if err := releases.WriteFileAtomic(path, []byte(secret+"\n"), 0o600); err != nil {
		return "", err
	}
	return secret, nil
}

func (p *Postgres) repoDir(app string) string {
	return filepath.Join(p.Config.Backup.Path, app, "postgres")
}

// mounts returns the volume flags shared by the server and one-off
// containers, and with a backup repository the setting that points WAL-G at
// its settings file.
func (p *Postgres) mounts(app, dataDir string) []string {
	args := []string{"-v", dataDir + ":" + pgDataMount}
	if p.backupsEnabled() {
		args = append(args, "-v", filepath.Join(p.dir(app), walgConfFile)+":"+walgConfMount+":ro", "-e", envWalgConfig)
		if p.Config.Backup.Type == "posix" {
			args = append(args, "-v", p.repoDir(app)+":"+walgRepoMount)
		}
	}
	return args
}

func (p *Postgres) serverArgs(archive bool) []string {
	args := []string{"postgres", "-c", "wal_level=replica"}
	if archive {
		return append(args,
			"-c", "archive_mode=on",
			"-c", "archive_command=wal-g wal-push %p",
			"-c", "archive_timeout=60")
	}
	return append(args, "-c", "archive_mode=off")
}

// URL returns the connection string injected into the app.
func (p *Postgres) URL(app string) (string, error) {
	pw, err := p.password(app)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("postgres://%s:%s@postgres:5432/%s?sslmode=disable", pgUser, pw, pgDatabase), nil
}

// Ensure makes the app's Postgres container run with the wanted image and
// settings, and returns the connection URL. A running container that already
// matches is left alone, so a deploy never restarts the database; it only
// gets the role setup when this process has not yet seen that
// succeed, because the start that created it may have been interrupted.
func (p *Postgres) Ensure(ctx context.Context, app *appdef.App) (string, error) {
	name := PostgresContainer(app.Name)
	pw, err := p.password(app.Name)
	if err != nil {
		return "", err
	}
	adminPW, err := p.adminPassword(app.Name)
	if err != nil {
		return "", err
	}
	image := p.Config.PostgresImage(app.Postgres.Version)
	archive := p.backupsEnabled()
	conf := ""
	if archive {
		conf = p.walgConf(app)
	}
	server := p.serverArgs(archive)
	parts := []string{image, strings.Join(server, "\x00"), conf}
	if app.Postgres.CreateRoles {
		// Only added when set, so clusters without it keep their hash and
		// are not restarted by an upgrade.
		parts = append(parts, "create_roles")
	}
	want := configHash(parts...)

	state, have := inspect(ctx, p.Runner, name)
	if state == "running" && have == want {
		if !p.isSettled(app.Name) {
			// A failure is reported, and the running database is left alone.
			if err := p.afterStart(ctx, app, name, pw); err != nil {
				return "", err
			}
		}
		return p.URL(app.Name)
	}
	if state != "" {
		// The name alone does not say whose container this is; see
		// RemoveDrill. Replacing it would destroy another app's cluster. A
		// container that matched above carries this app's settings label,
		// so only the path that removes one needs the check.
		if labels := foreignLabels(ctx, p.Runner, name, app.Name, rolePostgres); labels != "" {
			return "", fmt.Errorf("container %s is not the Postgres of app %s (it is labelled %q); not touching it", name, app.Name, labels)
		}
	}
	if archive {
		if err := p.writeConf(ctx, app, image, conf); err != nil {
			return "", err
		}
	}
	if err := replace(ctx, p.Runner, name, state, postgresStopSeconds); err != nil {
		return "", err
	}
	dataDir := filepath.Join(p.dir(app.Name), "data")
	if err := mountPoint(dataDir); err != nil {
		return "", err
	}
	args := []string{"run", "-d", "--name", name, "--restart", "unless-stopped",
		"--network", appdef.Network(app.Name), "--network-alias", "postgres",
		flagLabel, appdef.LabelApp + "=" + app.Name, flagLabel, appdef.LabelRole + "=" + rolePostgres,
		flagLabel, labelConfig + "=" + want,
		"--security-opt", "no-new-privileges:true",
		"--shm-size", "256m",
		// Read only when the image initialises an empty data directory. The
		// password has no value here: Docker takes it from its environment.
		"-e", "POSTGRES_USER=" + pgAdmin, "-e", "POSTGRES_DB=" + pgDatabase,
		"-e", "POSTGRES_PASSWORD", "-e", envPGData + pgData}
	args = append(args, p.mounts(app.Name, dataDir)...)
	args = append(args, image)
	args = append(args, server...)
	if _, err := dockerOutput(ctx, p.Runner, secretEnv("POSTGRES_PASSWORD="+adminPW), nil, args...); err != nil {
		return "", err
	}
	if err := p.afterStart(ctx, app, name, pw); err != nil {
		return "", err
	}
	return p.URL(app.Name)
}

// afterStart waits for the server and makes sure its roles are as Zymple
// wants them. Every step is safe to repeat. The app counts as settled only once all of them have succeeded.
func (p *Postgres) afterStart(ctx context.Context, app *appdef.App, name, pw string) error {
	p.setSettled(app.Name, false)
	if err := p.waitReady(ctx, name); err != nil {
		return err
	}
	if err := p.waitWritable(ctx, name); err != nil {
		return err
	}
	if err := p.ensureRoles(ctx, app.Name, name, pw, app.Postgres.CreateRoles); err != nil {
		return err
	}
	p.setSettled(app.Name, true)
	return nil
}

// psql runs SQL in one database of a cluster as the given role, over the
// container's local socket, which the image trusts. The SQL travels on stdin:
// it can hold a password, and a command line is readable by every local user.
func (p *Postgres) psql(ctx context.Context, name, role, database, sql string) (string, error) {
	return dockerOutput(ctx, p.Runner, nil, strings.NewReader(sql), "exec", "-i", name,
		"psql", "-U", role, "-d", database, "-v", "ON_ERROR_STOP=1", "-qAt", "-f", "-")
}

// schemaOwnerSQL gives the app's role the public schema on Postgres 14 and
// older. From 15 on the schema belongs to whoever owns the database; before
// that it belongs to the superuser, so the app's role could not grant on it,
// drop it or recreate it.
var schemaOwnerSQL = fmt.Sprintf(`DO $do$ BEGIN
  IF current_setting('server_version_num')::int < 150000 THEN ALTER SCHEMA public OWNER TO %s; END IF;
END $do$;
`, pgUser)

// ensureRoles brings a cluster's roles to the wanted shape, whatever created
// it: a superuser named postgres for Zymple and WAL-G, and the app's own
// role, which owns the app's database and has the generated password. The
// password is re-applied at every start, so it survives a restore from
// another host. Every statement is safe to repeat.
//
// createRoles decides whether the app's role may create other roles. It is
// applied in both directions, so removing the setting takes the privilege
// away again.
func (p *Postgres) ensureRoles(ctx context.Context, app, name, pw string, createRoles bool) error {
	if _, err := p.psql(ctx, name, pgAdmin, adminDatabase, "SELECT 1"); err != nil {
		// Clusters created before Zymple separated the two roles were
		// initialised with the app's role as their only superuser.
		create := fmt.Sprintf("CREATE ROLE %s SUPERUSER LOGIN", pgAdmin)
		if _, legacyErr := p.psql(ctx, name, pgUser, adminDatabase, create); legacyErr != nil {
			return fmt.Errorf("no superuser to manage the cluster with: %w", errors.Join(err, legacyErr))
		}
	}
	// The password is hex, so it cannot break out of the SQL literal.
	sql := fmt.Sprintf(`DO $do$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%[1]s') THEN CREATE ROLE %[1]s LOGIN; END IF;
END $do$;
ALTER ROLE %[1]s LOGIN PASSWORD '%[2]s';
ALTER ROLE %[1]s %[4]s;
ALTER DATABASE %[3]s OWNER TO %[1]s;
SELECT rolsuper FROM pg_roles WHERE rolname = '%[1]s';
`, pgUser, pw, pgDatabase, map[bool]string{true: "CREATEROLE", false: "NOCREATEROLE"}[createRoles])
	super, err := p.psql(ctx, name, pgAdmin, adminDatabase, sql)
	if err != nil {
		return err
	}
	// A schema is altered from inside its own database.
	if _, err := p.psql(ctx, name, pgAdmin, pgDatabase, schemaOwnerSQL); err != nil {
		return err
	}
	if super == "t" {
		// The role that initialised a cluster cannot lose superuser.
		p.warn(fmt.Sprintf("app %s: the database role %q is a superuser because this cluster predates the split between "+
			"the app's role and the superuser; the app can read the backup credentials inside the database container. "+
			"Dump and restore into a new cluster to fix this.", app, pgUser))
	}
	return nil
}

// mountPoint creates the host directory mounted at the image's data path.
// The cluster lives in a subdirectory owned by postgres with mode 0700; the
// mount point itself must be traversable by that user.
func mountPoint(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.Chmod(dir, 0o755) // NOSONAR holds only the 0700 cluster directory
}

func (p *Postgres) waitReady(ctx context.Context, name string) error {
	deadline := p.Now().Add(readyTimeout)
	notRunning := 0
	for {
		// A container that keeps exiting will never become ready; say why.
		if state, _ := inspect(ctx, p.Runner, name); state != "running" {
			if notRunning++; notRunning >= 5 {
				logs, _ := execx.Output(ctx, p.Runner, "docker", "logs", "--tail", "20", name)
				return fmt.Errorf("postgres container is %q, not running: %s", state, logs)
			}
		} else {
			notRunning = 0
		}
		// TCP, not the unix socket: the image's first-run init server only
		// listens on the socket, and it is not the server we are waiting for.
		_, err := execx.Output(ctx, p.Runner, "docker", "exec", name,
			"pg_isready", "-h", "127.0.0.1", "-U", pgUser, "-d", pgDatabase)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p.Now().After(deadline) {
			return fmt.Errorf("postgres did not become ready: %w", err)
		}
		p.Sleep(ctx, readyInterval)
	}
}

// recoverySQL asks whether the server is still replaying its archive.
const recoverySQL = "SELECT pg_is_in_recovery()"

// waitWritable waits until the server has left recovery. After a restore
// Postgres accepts connections while it is still replaying WAL up to the
// target, and is read-only until it promotes; setting up the roles in that
// window fails. When the question cannot be asked at all the answer is left
// to the role setup that follows, which reports why.
func (p *Postgres) waitWritable(ctx context.Context, name string) error {
	deadline := p.Now().Add(readyTimeout)
	for {
		out, err := p.psql(ctx, name, pgAdmin, adminDatabase, recoverySQL)
		if err != nil {
			// A cluster from before the role split has no postgres role.
			out, err = p.psql(ctx, name, pgUser, adminDatabase, recoverySQL)
		}
		if err != nil || out != "t" {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p.Now().After(deadline) {
			return errors.New("postgres is still in recovery and read-only; it did not finish replaying its archive")
		}
		p.Sleep(ctx, readyInterval)
	}
}

// RestoreOptions selects what to restore and where.
type RestoreOptions struct {
	// Time is the recovery target; empty restores to the end of the archive.
	Time string
	// Into names a drill cluster to restore beside the live one. Empty
	// restores the live cluster in place.
	Into string
}

// Validate checks the options before anything is stopped.
func (o RestoreOptions) Validate() error {
	if o.Time != "" && !targetTimeRe.MatchString(o.Time) {
		return fmt.Errorf("time %q must look like 2026-01-02 15:04:05+00", o.Time)
	}
	if o.Into != "" && !drillRe.MatchString(o.Into) {
		return fmt.Errorf("drill name %q must match %s", o.Into, drillRe)
	}
	return nil
}

// RemoveDrill deletes a drill cluster and its data.
func (p *Postgres) RemoveDrill(ctx context.Context, app, drill string) error {
	if !drillRe.MatchString(drill) {
		return fmt.Errorf("drill name %q must match %s", drill, drillRe)
	}
	name := DrillContainer(app, drill)
	// The name alone does not say whose container this is: the drill
	// "postgres" of app "a" and the live database of an app called
	// "a-postgres-drill" are both "zymple-a-postgres-drill-postgres". Only a
	// container Zymple labelled as this app's drill is removed.
	owner, err := execx.Output(ctx, p.Runner, "docker", "inspect", flagFormat, ownerFormat, name)
	switch {
	case err != nil && strings.Contains(strings.ToLower(err.Error()), "no such"):
		// The container is already gone; its data may still be there.
	case err != nil:
		return err
	case owner != app+" "+roleDrill:
		return fmt.Errorf("container %s is not a drill cluster of app %s; not removing it", name, app)
	default:
		if _, err := execx.Output(ctx, p.Runner, "docker", "rm", "-f", name); err != nil {
			return err
		}
	}
	return os.RemoveAll(filepath.Join(p.dir(app), "drill-"+drill))
}

// inspect returns a container's state ("" if it does not exist) and its
// configuration hash label.
func inspect(ctx context.Context, r execx.Runner, name string) (state, hash string) {
	out, err := execx.Output(ctx, r, "docker", "inspect", flagFormat,
		fmt.Sprintf(`{{.State.Status}} {{index .Config.Labels %q}}`, labelConfig), name)
	if err != nil {
		return "", ""
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", ""
	}
	if len(fields) > 1 {
		hash = fields[1]
	}
	return fields[0], hash
}

// ownerFormat prints a container's app and role labels.
var ownerFormat = fmt.Sprintf(`{{index .Config.Labels %q}} {{index .Config.Labels %q}}`, appdef.LabelApp, appdef.LabelRole)

// foreignLabels returns the app and role labels of a container when they
// say it is something other than the given app's container of that role,
// and "" when it is that container, has no such labels, or does not exist.
// A container Zymple did not label is left to the caller as before.
func foreignLabels(ctx context.Context, r execx.Runner, name, app, role string) string {
	out, err := execx.Output(ctx, r, "docker", "inspect", flagFormat, ownerFormat, name)
	if err != nil {
		return ""
	}
	labels := strings.Join(strings.Fields(out), " ")
	if labels == "" || labels == app+" "+role {
		return ""
	}
	return labels
}

func configHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x01")))
	return hex.EncodeToString(sum[:8])
}
