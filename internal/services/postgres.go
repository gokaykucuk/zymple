// Package services manages the Postgres and Redis containers Zymple runs
// beside an app.
package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gokaykucuk/zymple/internal/appdef"
	"github.com/gokaykucuk/zymple/internal/config"
	"github.com/gokaykucuk/zymple/internal/execx"
	"github.com/gokaykucuk/zymple/internal/releases"
)

const (
	labelConfig   = "zymple.config"
	flagLabel     = "--label"
	flagUser      = "--user"
	envPGData     = "PGDATA="
	rolePostgres  = "postgres"
	roleRedis     = "redis"
	pgUser        = "app"
	pgDatabase    = "app"
	pgDataMount   = "/var/lib/postgresql/data"
	pgData        = pgDataMount + "/pgdata"
	pgRepoMount   = "/var/lib/pgbackrest"
	pgConfMount   = "/etc/pgbackrest/pgbackrest.conf"
	fullInterval  = 7 * 24 * time.Hour
	diffInterval  = 24 * time.Hour
	readyTimeout  = 10 * time.Minute
	readyInterval = time.Second
)

var (
	targetTimeRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(\.\d{1,6})?(Z|[+-]\d{2}(:?\d{2})?)?$`)
	drillRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
)

// Postgres manages one Postgres cluster per app.
type Postgres struct {
	Runner execx.Runner
	Config *config.Config
	// Sleep waits between readiness probes; replaced in tests.
	Sleep func(ctx context.Context, d time.Duration)
	Now   func() time.Time
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

// pgbackrestConf renders the pgbackrest configuration for an app.
func (p *Postgres) pgbackrestConf(app *appdef.App) string {
	b := p.Config.Backup
	var s strings.Builder
	s.WriteString("[global]\n")
	if b.Type == "s3" {
		fmt.Fprintf(&s, "repo1-type=s3\nrepo1-path=%s\n", filepath.Join("/", b.Path, app.Name, "postgres"))
		fmt.Fprintf(&s, "repo1-s3-endpoint=%s\nrepo1-s3-bucket=%s\nrepo1-s3-region=%s\n", b.S3.Endpoint, b.S3.Bucket, b.S3.Region)
		fmt.Fprintf(&s, "repo1-s3-key=%s\nrepo1-s3-key-secret=%s\n", b.S3.AccessKey, b.S3.SecretKey)
		if b.S3.PathStyle {
			s.WriteString("repo1-s3-uri-style=path\n")
		}
	} else {
		fmt.Fprintf(&s, "repo1-type=posix\nrepo1-path=%s\n", pgRepoMount)
	}
	if b.CipherPass != "" {
		fmt.Fprintf(&s, "repo1-cipher-type=aes-256-cbc\nrepo1-cipher-pass=%s\n", b.CipherPass)
	}
	fmt.Fprintf(&s, "repo1-retention-full-type=time\nrepo1-retention-full=%d\n", app.Postgres.PITRDays)
	s.WriteString("start-fast=y\nlog-level-console=info\nlog-level-file=off\n\n")
	fmt.Fprintf(&s, "[%s]\npg1-path=%s\npg1-user=%s\n", app.Name, pgData, pgUser)
	return s.String()
}

func (p *Postgres) repoDir(app string) string {
	return filepath.Join(p.Config.Backup.Path, app, "postgres")
}

// mounts returns the volume flags shared by the server and one-off containers.
func (p *Postgres) mounts(app, dataDir string) []string {
	args := []string{"-v", dataDir + ":" + pgDataMount}
	if p.backupsEnabled() {
		args = append(args, "-v", filepath.Join(p.dir(app), "pgbackrest.conf")+":"+pgConfMount+":ro")
		if p.Config.Backup.Type == "posix" {
			args = append(args, "-v", p.repoDir(app)+":"+pgRepoMount)
		}
	}
	return args
}

func (p *Postgres) serverArgs(app *appdef.App, archive bool) []string {
	args := []string{"postgres", "-c", "wal_level=replica"}
	if archive {
		return append(args,
			"-c", "archive_mode=on",
			"-c", "archive_command=pgbackrest --stanza="+app.Name+" archive-push %p",
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
// matches is left alone, so a deploy never restarts the database.
func (p *Postgres) Ensure(ctx context.Context, app *appdef.App) (string, error) {
	name := PostgresContainer(app.Name)
	pw, err := p.password(app.Name)
	if err != nil {
		return "", err
	}
	image := p.Config.PostgresImage(app.Postgres.Version)
	archive := p.backupsEnabled()
	conf := ""
	if archive {
		conf = p.pgbackrestConf(app)
	}
	server := p.serverArgs(app, archive)
	want := configHash(image, strings.Join(server, "\x00"), conf)

	state, have := inspect(ctx, p.Runner, name)
	if state == "running" && have == want {
		url, _ := p.URL(app.Name)
		return url, nil
	}
	if archive {
		if err := p.writeConf(ctx, app, image, conf); err != nil {
			return "", err
		}
	}
	if state != "" {
		if _, err := execx.Output(ctx, p.Runner, "docker", "rm", "-f", name); err != nil {
			return "", err
		}
	}
	dataDir := filepath.Join(p.dir(app.Name), "data")
	if err := mountPoint(dataDir); err != nil {
		return "", err
	}
	args := []string{"run", "-d", "--name", name, "--restart", "unless-stopped",
		"--network", appdef.Network(app.Name), "--network-alias", "postgres",
		flagLabel, appdef.LabelApp + "=" + app.Name, flagLabel, appdef.LabelRole + "=" + rolePostgres,
		flagLabel, labelConfig + "=" + want,
		"--shm-size", "256m",
		"-e", "POSTGRES_USER=" + pgUser, "-e", "POSTGRES_DB=" + pgDatabase,
		"-e", "POSTGRES_PASSWORD=" + pw, "-e", envPGData + pgData}
	args = append(args, p.mounts(app.Name, dataDir)...)
	args = append(args, image)
	args = append(args, server...)
	if _, err := execx.Output(ctx, p.Runner, "docker", args...); err != nil {
		return "", err
	}
	if err := p.afterStart(ctx, app, name, pw); err != nil {
		return "", err
	}
	return p.URL(app.Name)
}

func (p *Postgres) writeConf(ctx context.Context, app *appdef.App, image, conf string) error {
	if err := releases.WriteFileAtomic(filepath.Join(p.dir(app.Name), "pgbackrest.conf"), []byte(conf), 0o644); err != nil {
		return err
	}
	if p.Config.Backup.Type != "posix" {
		return nil
	}
	repo := p.repoDir(app.Name)
	if err := os.MkdirAll(repo, 0o750); err != nil {
		return err
	}
	// The repository must belong to the postgres user inside the image.
	_, err := execx.Output(ctx, p.Runner, "docker", "run", "--rm", flagUser, "root", "--entrypoint", "chown",
		"-v", repo+":"+pgRepoMount, image, "postgres:postgres", pgRepoMount)
	return err
}

// afterStart waits for the server, re-applies the role password and makes
// sure the backup stanza exists.
func (p *Postgres) afterStart(ctx context.Context, app *appdef.App, name, pw string) error {
	if err := p.waitReady(ctx, name); err != nil {
		return err
	}
	// The password is hex, so it cannot break out of the SQL literal.
	sql := fmt.Sprintf("ALTER ROLE %s PASSWORD '%s'", pgUser, pw)
	if _, err := execx.Output(ctx, p.Runner, "docker", "exec", name,
		"psql", "-U", pgUser, "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-qc", sql); err != nil {
		return err
	}
	if !p.backupsEnabled() {
		return nil
	}
	_, err := execx.Output(ctx, p.Runner, "docker", p.backrest(app.Name, "stanza-create")...)
	return err
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

func (p *Postgres) backrest(app string, args ...string) []string {
	base := []string{"exec", flagUser, "postgres", PostgresContainer(app), "pgbackrest", "--stanza=" + app}
	return append(base, args...)
}

// Backup takes a backup of the given type: full, diff or incr.
func (p *Postgres) Backup(ctx context.Context, app, kind string, out io.Writer) error {
	if !p.backupsEnabled() {
		return errors.New("no backup repository is configured")
	}
	switch kind {
	case "full", "diff", "incr":
	default:
		return fmt.Errorf("backup type %q must be full, diff or incr", kind)
	}
	return execx.Stream(ctx, p.Runner, out, "docker", p.backrest(app, "--type="+kind, "backup")...)
}

// Info writes pgbackrest's description of the app's backups.
func (p *Postgres) Info(ctx context.Context, app string, out io.Writer) error {
	if !p.backupsEnabled() {
		return errors.New("no backup repository is configured")
	}
	return execx.Stream(ctx, p.Runner, out, "docker", p.backrest(app, "info")...)
}

type backrestInfo []struct {
	Backup []struct {
		Type      string `json:"type"`
		Timestamp struct {
			Stop int64 `json:"stop"`
		} `json:"timestamp"`
	} `json:"backup"`
}

// DueBackup decides which scheduled backup, if any, the app needs now: a
// full backup weekly and a differential daily. It returns "" for none.
func (p *Postgres) DueBackup(ctx context.Context, app string) (string, error) {
	raw, err := execx.Output(ctx, p.Runner, "docker", p.backrest(app, "--output=json", "info")...)
	if err != nil {
		return "", err
	}
	var info backrestInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		return "", fmt.Errorf("pgbackrest info: %w", err)
	}
	var lastFull, lastAny time.Time
	for _, stanza := range info {
		for _, b := range stanza.Backup {
			stop := time.Unix(b.Timestamp.Stop, 0)
			if stop.After(lastAny) {
				lastAny = stop
			}
			if b.Type == "full" && stop.After(lastFull) {
				lastFull = stop
			}
		}
	}
	now := p.Now()
	switch {
	case now.Sub(lastFull) >= fullInterval:
		return "full", nil
	case now.Sub(lastAny) >= diffInterval:
		return "diff", nil
	}
	return "", nil
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

func (o RestoreOptions) backrestArgs(app string) []string {
	args := []string{"--stanza=" + app}
	if o.Time != "" {
		args = append(args, "--type=time", "--target="+o.Time, "--target-action=promote")
	}
	if o.Into != "" {
		// A drill cluster must never archive into the live repository.
		return append(args, "--archive-mode=off", "restore")
	}
	return append(args, "--delta", "restore")
}

// DrillContainer is the name of a drill cluster's container.
func DrillContainer(app, drill string) string { return PostgresContainer(app) + "-drill-" + drill }

// Restore restores the app's cluster from the backup repository. The caller
// stops the app's own containers first when restoring in place.
func (p *Postgres) Restore(ctx context.Context, app *appdef.App, opts RestoreOptions, out io.Writer) error {
	if !p.backupsEnabled() {
		return errors.New("no backup repository is configured")
	}
	if err := opts.Validate(); err != nil {
		return err
	}
	image := p.Config.PostgresImage(app.Postgres.Version)
	name := PostgresContainer(app.Name)
	dataDir := filepath.Join(p.dir(app.Name), "data")
	// Recovery can only reach a time that archived WAL covers. "check"
	// switches the WAL segment and waits for it to be archived, so everything
	// up to now is restorable. It fails when the live cluster is down or
	// broken, which is exactly when a restore is wanted, so that is not fatal.
	if err := execx.Stream(ctx, p.Runner, out, "docker", p.backrest(app.Name, "check")...); err != nil {
		fmt.Fprintf(out, "warning: could not archive the latest WAL before restoring: %v\n", err)
	}
	if opts.Into != "" {
		name = DrillContainer(app.Name, opts.Into)
		dataDir = filepath.Join(p.dir(app.Name), "drill-"+opts.Into)
		if _, err := os.Stat(dataDir); err == nil {
			return fmt.Errorf("drill %q already exists; remove it first", opts.Into)
		}
		if err := mountPoint(dataDir); err != nil {
			return err
		}
	} else if _, err := execx.Output(ctx, p.Runner, "docker", "stop", name); err != nil {
		return err
	}

	run := []string{"run", "--rm", flagUser, "root", "--entrypoint", "sh"}
	run = append(run, p.mounts(app.Name, dataDir)...)
	// The data directory is created by root on the host; hand it to postgres,
	// then restore as postgres.
	script := `set -e; mkdir -p "$PGDATA"; chown -R postgres:postgres ` + pgDataMount + `; chmod 700 "$PGDATA"; exec gosu postgres pgbackrest "$@"`
	run = append(run, "-e", envPGData+pgData, image, "-c", script, "pgbackrest")
	run = append(run, opts.backrestArgs(app.Name)...)
	if err := execx.Stream(ctx, p.Runner, out, "docker", run...); err != nil {
		return err
	}

	pw, err := p.password(app.Name)
	if err != nil {
		return err
	}
	if opts.Into == "" {
		if _, err := execx.Output(ctx, p.Runner, "docker", "start", name); err != nil {
			return err
		}
		return p.afterStart(ctx, app, name, pw)
	}
	args := []string{"run", "-d", "--name", name,
		"--network", appdef.Network(app.Name), "--network-alias", "postgres-drill-" + opts.Into,
		flagLabel, appdef.LabelApp + "=" + app.Name, flagLabel, appdef.LabelRole + "=postgres-drill",
		"--shm-size", "256m", "-e", envPGData + pgData}
	args = append(args, p.mounts(app.Name, dataDir)...)
	args = append(args, image)
	args = append(args, p.serverArgs(app, false)...)
	if _, err := execx.Output(ctx, p.Runner, "docker", args...); err != nil {
		return err
	}
	return p.waitReady(ctx, name)
}

// RemoveDrill deletes a drill cluster and its data.
func (p *Postgres) RemoveDrill(ctx context.Context, app, drill string) error {
	if !drillRe.MatchString(drill) {
		return fmt.Errorf("drill name %q must match %s", drill, drillRe)
	}
	if _, err := execx.Output(ctx, p.Runner, "docker", "rm", "-f", DrillContainer(app, drill)); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(p.dir(app), "drill-"+drill))
}

// inspect returns a container's state ("" if it does not exist) and its
// configuration hash label.
func inspect(ctx context.Context, r execx.Runner, name string) (state, hash string) {
	out, err := execx.Output(ctx, r, "docker", "inspect", "--format",
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

func configHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x01")))
	return hex.EncodeToString(sum[:8])
}
