package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gokaykucuk/zymple/internal/appdef"
	"github.com/gokaykucuk/zymple/internal/execx"
	"github.com/gokaykucuk/zymple/internal/releases"
)

// WAL-G keeps a cluster's base backups and WAL in one prefix. A base backup
// is a handful of compressed tar archives, whatever the number of tables.
const (
	walgRepoMount = "/var/lib/wal-g"
	walgConfMount = "/etc/wal-g/walg.json"
	walgConfFile  = "walg.json"
	envWalgConfig = "WALG_CONFIG_PATH=" + walgConfMount
	// envFullBackup overrides the settings file for one backup: with no
	// delta steps allowed, WAL-G takes a full backup.
	envFullBackup = "WALG_DELTA_MAX_STEPS=0"
	// deltaSteps is how many delta backups may follow a full one. A week of
	// daily deltas, so a restore reads at most seven backups.
	deltaSteps = 6
	// BackupFull and BackupDelta are the backup types.
	BackupFull  = "full"
	BackupDelta = "delta"
	// beforeRestore is where the data directory waits while a restore in
	// place runs, so a failed restore can put it back.
	beforeRestore = pgData + ".before-restore"
	// archiveWait bounds the wait for the newest WAL segment to be archived
	// before a restore.
	archiveWait = 60
)

// walgConf renders the WAL-G settings file of an app. The file holds the
// repository's credentials; only the database container reads it.
func (p *Postgres) walgConf(app *appdef.App) string {
	b := p.Config.Backup
	conf := map[string]string{
		"PGHOST":                  "/var/run/postgresql",
		"PGUSER":                  pgAdmin,
		"WALG_COMPRESSION_METHOD": "zstd",
		"WALG_DELTA_MAX_STEPS":    strconv.Itoa(deltaSteps),
	}
	if b.Type == "s3" {
		conf["WALG_S3_PREFIX"] = "s3://" + b.S3.Bucket + path.Join("/", b.Path, app.Name, "postgres")
		conf["AWS_ENDPOINT"] = "https://" + b.S3.Endpoint
		conf["AWS_REGION"] = b.S3.Region
		conf["AWS_ACCESS_KEY_ID"] = b.S3.AccessKey
		conf["AWS_SECRET_ACCESS_KEY"] = b.S3.SecretKey
		if b.S3.PathStyle {
			conf["AWS_S3_FORCE_PATH_STYLE"] = "true"
		}
	} else {
		conf["WALG_FILE_PREFIX"] = walgRepoMount
	}
	if b.CipherPass != "" {
		conf["WALG_LIBSODIUM_KEY"] = CipherKey(b.CipherPass)
		conf["WALG_LIBSODIUM_KEY_TRANSFORM"] = "hex"
	}
	// Maps marshal with sorted keys, so the same settings always give the
	// same file and the same container hash.
	data, _ := json.MarshalIndent(conf, "", "  ")
	return string(data) + "\n"
}

// CipherKey turns the configured passphrase into the 32-byte key WAL-G
// encrypts with, in hex: its SHA-256. Anyone restoring without Zymple needs
// the same derivation.
func CipherKey(pass string) string {
	sum := sha256.Sum256([]byte(pass))
	return hex.EncodeToString(sum[:])
}

func (p *Postgres) writeConf(ctx context.Context, app *appdef.App, image, conf string) error {
	// Readable by the postgres user of the container; the directories above
	// it are root's alone.
	if err := releases.WriteFileAtomic(filepath.Join(p.dir(app.Name), walgConfFile), []byte(conf), 0o644); err != nil {
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
	_, err := execx.Output(ctx, p.Runner, "docker", "run", "--rm", flagUser, "root", flagEntrypoint, "chown",
		"-v", repo+":"+walgRepoMount, image, "postgres:postgres", walgRepoMount)
	return err
}

// walg is a WAL-G command inside the app's running database container.
func (p *Postgres) walg(app string, env []string, args ...string) []string {
	base := []string{"exec", flagUser, "postgres"}
	for _, e := range env {
		base = append(base, "-e", e)
	}
	base = append(base, PostgresContainer(app), "wal-g")
	return append(base, args...)
}

// Backup takes a backup of the given type: full or delta. A delta holds the
// pages changed since the backup before it.
func (p *Postgres) Backup(ctx context.Context, app, kind string, out io.Writer) error {
	if !p.backupsEnabled() {
		return errNoRepository
	}
	var env []string
	switch kind {
	case BackupFull:
		env = []string{envFullBackup}
	case BackupDelta:
	default:
		return fmt.Errorf("backup type %q must be %s or %s", kind, BackupFull, BackupDelta)
	}
	return execx.Stream(ctx, p.Runner, out, "docker", p.walg(app, env, "backup-push", pgData)...)
}

var errNoRepository = errors.New("no backup repository is configured")

// Info writes WAL-G's list of the app's backups.
func (p *Postgres) Info(ctx context.Context, app string, out io.Writer) error {
	if !p.backupsEnabled() {
		return errNoRepository
	}
	return execx.Stream(ctx, p.Runner, out, "docker", p.walg(app, nil, "backup-list", "--pretty", "--detail")...)
}

// walgBackup is one entry of `wal-g backup-list --detail --json`.
type walgBackup struct {
	Name   string    `json:"backup_name"`
	Finish time.Time `json:"finish_time"`
}

// full reports whether the backup stands on its own. WAL-G names a delta
// after the backup it builds on: base_<wal>_D_<wal>.
func (b walgBackup) full() bool { return !strings.Contains(b.Name, "_D_") }

// parseBackups reads WAL-G's listing, oldest first.
func parseBackups(raw string) ([]walgBackup, error) {
	var list []walgBackup
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, fmt.Errorf("wal-g backup-list: %w", err)
	}
	for _, b := range list {
		if !backupNameRe.MatchString(b.Name) {
			return nil, fmt.Errorf("wal-g backup-list: unexpected backup name %q", b.Name)
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Finish.Before(list[j].Finish) })
	return list, nil
}

var listArgs = []string{"backup-list", "--detail", "--json"}

// backups lists the app's backups through its running database container.
func (p *Postgres) backups(ctx context.Context, app string) ([]walgBackup, error) {
	raw, err := execx.Output(ctx, p.Runner, "docker", p.walg(app, nil, listArgs...)...)
	if err != nil {
		return nil, err
	}
	return parseBackups(raw)
}

// DueBackup decides which scheduled backup, if any, the app needs now: a
// full backup weekly and a delta daily. It returns "" for none.
func (p *Postgres) DueBackup(ctx context.Context, app string) (string, error) {
	list, err := p.backups(ctx, app)
	if err != nil {
		return "", err
	}
	var lastFull, lastAny time.Time
	for _, b := range list {
		if b.Finish.After(lastAny) {
			lastAny = b.Finish
		}
		if b.full() && b.Finish.After(lastFull) {
			lastFull = b.Finish
		}
	}
	now := p.Now()
	switch {
	case now.Sub(lastFull) >= fullInterval:
		return BackupFull, nil
	case now.Sub(lastAny) >= diffInterval:
		return BackupDelta, nil
	}
	return "", nil
}

// pruneTarget picks the backup everything older than which can go: the
// newest full backup that is at least the retention period old. Recovery to
// any time inside the period starts from it or from a later backup. It
// returns "" when nothing is old enough, or when nothing is older than it.
func pruneTarget(list []walgBackup, cutoff time.Time) string {
	target := -1
	for i, b := range list {
		if b.full() && !b.Finish.After(cutoff) {
			target = i
		}
	}
	if target <= 0 {
		return ""
	}
	return list[target].Name
}

// Prune removes the backups and WAL that recovery to any time in the last
// pitr_days no longer needs.
func (p *Postgres) Prune(ctx context.Context, app *appdef.App, out io.Writer) error {
	if !p.backupsEnabled() {
		return errNoRepository
	}
	list, err := p.backups(ctx, app.Name)
	if err != nil {
		return err
	}
	cutoff := p.Now().Add(-time.Duration(app.Postgres.PITRDays) * 24 * time.Hour)
	target := pruneTarget(list, cutoff)
	if target == "" {
		return nil
	}
	return execx.Stream(ctx, p.Runner, out, "docker", p.walg(app.Name, nil, "delete", "before", target, "--confirm")...)
}

// targetLayouts are the spellings of a recovery target that targetTimeRe
// lets through.
var targetLayouts = []string{
	"2006-01-02 15:04:05.999999Z07:00", "2006-01-02 15:04:05.999999Z0700", "2006-01-02 15:04:05.999999Z07",
	"2006-01-02 15:04:05.999999",
}

// parseTarget reads a recovery target time. One without a zone is read as
// UTC, which is the time zone the database image runs in.
func parseTarget(s string) (time.Time, error) {
	s = strings.Replace(s, "T", " ", 1)
	for _, layout := range targetLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("time %q must look like 2026-01-02 15:04:05+00", s)
}

// backupFor picks the backup a restore starts from: the newest one, or the
// newest one that finished before the target time.
func backupFor(list []walgBackup, target string) (string, error) {
	if len(list) == 0 {
		return "", errors.New("the repository holds no backup to restore from")
	}
	if target == "" {
		return list[len(list)-1].Name, nil
	}
	at, err := parseTarget(target)
	if err != nil {
		return "", err
	}
	for i := len(list) - 1; i >= 0; i-- {
		if !list[i].Finish.After(at) {
			return list[i].Name, nil
		}
	}
	return "", fmt.Errorf("no backup finished before %s; the oldest is from %s", target, list[0].Finish.UTC().Format(time.RFC3339))
}

// archiveLatest switches the WAL segment and waits for it to be archived,
// so that a restore can reach everything up to now. It fails when the live
// cluster is down or broken, which is exactly when a restore is wanted, so
// the caller treats that as a warning.
func (p *Postgres) archiveLatest(ctx context.Context, app string) error {
	name := PostgresContainer(app)
	segment, err := p.psql(ctx, name, pgAdmin, adminDatabase, "SELECT pg_walfile_name(pg_switch_wal())")
	if err != nil {
		return err
	}
	if !walSegmentRe.MatchString(segment) {
		return fmt.Errorf("unexpected WAL segment name %q", segment)
	}
	sql := fmt.Sprintf("SELECT coalesce(last_archived_wal >= '%s', false) FROM pg_stat_archiver", segment)
	for i := 0; i < archiveWait; i++ {
		done, err := p.psql(ctx, name, pgAdmin, adminDatabase, sql)
		if err != nil {
			return err
		}
		if done == "t" {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.Sleep(ctx, readyInterval)
	}
	return fmt.Errorf("WAL segment %s was not archived within %d seconds", segment, archiveWait)
}

// restoreScript fetches a base backup into an empty data directory and
// leaves the recovery settings for the server's next start. Its arguments
// are the backup name, the target time ("" for the end of the archive) and
// "inplace" or "drill".
//
// A restore in place moves the old data directory aside first and puts it
// back when the fetch fails, so a broken repository cannot cost the data
// that was there.
const restoreScript = `set -eu
old=` + beforeRestore + `
if [ "$3" = inplace ]; then
  rm -rf "$old"
  if [ -d "$PGDATA" ]; then mv "$PGDATA" "$old"; fi
fi
mkdir -p "$PGDATA"
chown postgres:postgres "$PGDATA"
chmod 700 "$PGDATA"
if ! gosu postgres wal-g backup-fetch "$PGDATA" "$1"; then
  if [ "$3" = inplace ] && [ -d "$old" ]; then rm -rf "$PGDATA"; mv "$old" "$PGDATA"; fi
  exit 1
fi
gosu postgres touch "$PGDATA/recovery.signal"
{
  echo "restore_command = 'wal-g wal-fetch %f %p'"
  if [ -n "$2" ]; then
    echo "recovery_target_time = '$2'"
    echo "recovery_target_action = 'promote'"
  fi
} >> "$PGDATA/postgresql.auto.conf"
`

// DrillContainer is the name of a drill cluster's container.
func DrillContainer(app, drill string) string { return PostgresContainer(app) + "-drill-" + drill }

// Restore restores the app's cluster from the backup repository. The caller
// stops the app's own containers first when restoring in place.
func (p *Postgres) Restore(ctx context.Context, app *appdef.App, opts RestoreOptions, out io.Writer) error {
	if !p.backupsEnabled() {
		return errNoRepository
	}
	if err := opts.Validate(); err != nil {
		return err
	}
	image := p.Config.PostgresImage(app.Postgres.Version)
	name := PostgresContainer(app.Name)
	liveDir := filepath.Join(p.dir(app.Name), "data")
	dataDir, mode := liveDir, "inplace"
	if err := p.archiveLatest(ctx, app.Name); err != nil {
		fmt.Fprintf(out, "warning: could not archive the latest WAL before restoring: %v\n", err)
	}
	// The repository is read from a container of its own: the live cluster
	// may be the thing that is broken.
	list := []string{"run", "--rm", flagUser, "postgres", flagEntrypoint, "wal-g"}
	list = append(list, p.mounts(app.Name, liveDir)...)
	raw, err := execx.Output(ctx, p.Runner, "docker", append(append(list, image), listArgs...)...)
	if err != nil {
		return err
	}
	backups, err := parseBackups(raw)
	if err != nil {
		return err
	}
	backup, err := backupFor(backups, opts.Time)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "restoring from backup %s\n", backup)

	if opts.Into != "" {
		name, mode = DrillContainer(app.Name, opts.Into), "drill"
		dataDir = filepath.Join(p.dir(app.Name), "drill-"+opts.Into)
		if _, err := os.Stat(dataDir); err == nil {
			return fmt.Errorf("drill %q already exists; remove it first", opts.Into)
		}
		if err := mountPoint(dataDir); err != nil {
			return err
		}
	} else if _, err := execx.Output(ctx, p.Runner, "docker", "stop", "--time", postgresStopSeconds, name); err != nil {
		return err
	}

	run := []string{"run", "--rm", flagUser, "root", flagEntrypoint, "sh"}
	run = append(run, p.mounts(app.Name, dataDir)...)
	run = append(run, "-e", envPGData+pgData, image, "-c", restoreScript, "restore", backup, opts.Time, mode)
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
		if err := p.afterStart(ctx, app, name, pw); err != nil {
			return fmt.Errorf("%w (the data directory from before the restore is kept in %s)", err,
				filepath.Join(liveDir, filepath.Base(beforeRestore)))
		}
		// The restored cluster is up and writable; the old one can go.
		return os.RemoveAll(filepath.Join(liveDir, filepath.Base(beforeRestore)))
	}
	args := []string{"run", "-d", "--name", name,
		"--network", appdef.Network(app.Name), "--network-alias", "postgres-drill-" + opts.Into,
		flagLabel, appdef.LabelApp + "=" + app.Name, flagLabel, appdef.LabelRole + "=" + roleDrill,
		"--security-opt", "no-new-privileges:true",
		"--shm-size", "256m", "-e", envPGData + pgData}
	args = append(args, p.mounts(app.Name, dataDir)...)
	args = append(args, image)
	// A drill cluster reads the live repository and must never write to it.
	args = append(args, p.serverArgs(false)...)
	if _, err := execx.Output(ctx, p.Runner, "docker", args...); err != nil {
		return err
	}
	return p.waitReady(ctx, name)
}
