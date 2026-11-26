package services

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gokaykucuk/zymple/internal/appdef"
)

const (
	// importFile is the dump's name inside the cluster's data mount while an
	// import runs.
	importFile = "zymple-import.pgdump"
	// AfterImportFile is an optional SQL file in an app's definition
	// directory. An import runs it as the cluster's superuser in the app's
	// database once the data is loaded.
	AfterImportFile = "after-import.sql"

	defaultImportJobs = 4
	maxImportJobs     = 32
)

var (
	// Statements pg_dump writes at the start of a line.
	ownerToRe   = regexp.MustCompile(`^ALTER [^;]* OWNER TO ("[^"]+"|[^\s";]+);$`)
	grantToRe   = regexp.MustCompile(`^(?:GRANT|REVOKE) [^;]* (?:TO|FROM) ("[^"]+"|[^\s";]+)(?: WITH GRANT OPTION)?;$`)
	defaultACLs = regexp.MustCompile(`^ALTER DEFAULT PRIVILEGES FOR ROLE ("[^"]+"|[^\s";]+) [^;]* (?:TO|FROM) ("[^"]+"|[^\s";]+)(?: WITH GRANT OPTION)?;$`)
	// Role names Zymple is willing to create. Anything else is left for
	// pg_restore to report.
	plainRoleRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)
)

// ErrDatabaseUntouched wraps an import failure that happened before the
// app's database was dropped, so the app can simply be started again.
var ErrDatabaseUntouched = errors.New("the database was not changed")

func untouched(err error) error { return fmt.Errorf("%w: %w", ErrDatabaseUntouched, err) }

// notRoles are words that appear where a role name does but name no role.
var notRoles = map[string]bool{"PUBLIC": true, "CURRENT_USER": true, "SESSION_USER": true, "CURRENT_ROLE": true}

// ImportOptions describes one import of a pg_dump archive.
type ImportOptions struct {
	// File is the absolute path, on the host, of a custom-format archive.
	File string
	// Jobs is the number of parallel restore jobs; 0 means the default.
	Jobs int
}

// Validate checks the options before anything is stopped or dropped.
func (o ImportOptions) Validate() error {
	if !filepath.IsAbs(o.File) || filepath.Clean(o.File) != o.File {
		return fmt.Errorf("dump file %q must be a clean absolute path", o.File)
	}
	info, err := os.Stat(o.File)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("dump file %q is not a regular file", o.File)
	}
	if o.Jobs < 0 || o.Jobs > maxImportJobs {
		return fmt.Errorf("jobs must be between 1 and %d", maxImportJobs)
	}
	return nil
}

func (o ImportOptions) jobs() string {
	if o.Jobs == 0 {
		return strconv.Itoa(defaultImportJobs)
	}
	return strconv.Itoa(o.Jobs)
}

// dumpRoles returns the roles a dump's schema refers to as owners or
// grantees, from the statements pg_dump writes for them.
func dumpRoles(schema io.Reader) ([]string, error) {
	seen := map[string]bool{}
	add := func(raw string) {
		name := strings.Trim(raw, `"`)
		if !notRoles[name] && plainRoleRe.MatchString(name) {
			seen[name] = true
		}
	}
	sc := bufio.NewScanner(schema)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		if m := defaultACLs.FindStringSubmatch(line); m != nil {
			add(m[1])
			add(m[2])
		} else if m := ownerToRe.FindStringSubmatch(line); m != nil {
			add(m[1])
		} else if m := grantToRe.FindStringSubmatch(line); m != nil {
			add(m[1])
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// handOverSQL gives the app's role everything the cluster's superuser owns
// in the app's database, outside extensions. A source that ran as a superuser
// leaves such objects, and the app's role here is an ordinary one.
var handOverSQL = fmt.Sprintf(`DO $do$
DECLARE r record;
BEGIN
  FOR r IN SELECT c.oid::regclass AS obj, c.relkind FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
           WHERE c.relowner = '%[1]s'::regrole AND c.relkind IN ('r','p','S','v','m','f')
             AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
             AND NOT EXISTS (SELECT FROM pg_depend d WHERE d.objid = c.oid AND d.deptype = 'e')
             AND NOT (c.relkind = 'S' AND EXISTS (SELECT FROM pg_depend d WHERE d.objid = c.oid AND d.deptype IN ('a','i')))
  LOOP
    EXECUTE format('ALTER %%s %%s OWNER TO %[2]s',
      CASE r.relkind WHEN 'S' THEN 'SEQUENCE' WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW'
                     WHEN 'f' THEN 'FOREIGN TABLE' ELSE 'TABLE' END, r.obj);
  END LOOP;
  FOR r IN SELECT p.oid::regprocedure AS obj, p.prokind FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
           WHERE p.proowner = '%[1]s'::regrole AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
             AND NOT EXISTS (SELECT FROM pg_depend d WHERE d.objid = p.oid AND d.deptype = 'e')
  LOOP
    EXECUTE format('ALTER %%s %%s OWNER TO %[2]s',
      CASE r.prokind WHEN 'p' THEN 'PROCEDURE' WHEN 'a' THEN 'AGGREGATE' ELSE 'FUNCTION' END, r.obj);
  END LOOP;
  FOR r IN SELECT t.oid::regtype AS obj FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
           WHERE t.typowner = '%[1]s'::regrole AND t.typtype IN ('e','d','r')
             AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
             AND NOT EXISTS (SELECT FROM pg_depend d WHERE d.objid = t.oid AND d.deptype = 'e')
  LOOP
    EXECUTE format('ALTER TYPE %%s OWNER TO %[2]s', r.obj);
  END LOOP;
  FOR r IN SELECT n.nspname FROM pg_namespace n
           WHERE n.nspowner = '%[1]s'::regrole AND n.nspname !~ '^pg_' AND n.nspname NOT IN ('information_schema','public')
             AND NOT EXISTS (SELECT FROM pg_depend d WHERE d.objid = n.oid AND d.deptype = 'e')
  LOOP
    EXECUTE format('ALTER SCHEMA %%I OWNER TO %[2]s', r.nspname);
  END LOOP;
END $do$;
`, pgAdmin, pgUser)

// Import replaces the app's database with the contents of a pg_dump
// archive. The caller stops the app's containers first and holds the deploy
// lock.
//
// A dump names the roles of the cluster it came from. Roles that already
// exist here keep what they own, so an app that separates privileges between
// roles keeps that separation: deploy the app once before importing, so its
// own roles exist. Roles that do not exist here are created for the duration
// of the restore; what they own is handed to the app's role and they are
// dropped again. What the source's superuser owned goes to the app's role
// too.
func (p *Postgres) Import(ctx context.Context, app *appdef.App, opts ImportOptions, out io.Writer) error {
	if err := opts.Validate(); err != nil {
		return untouched(err)
	}
	name := PostgresContainer(app.Name)
	if state, _ := inspect(ctx, p.Runner, name); state != "running" {
		return untouched(errors.New("the app's database is not running; deploy the app once before importing into it"))
	}

	// The archive has to be a file inside the container: a parallel restore
	// seeks in it. A hard link costs nothing; across filesystems it is copied.
	staged := filepath.Join(p.dir(app.Name), "data", importFile)
	if err := stage(opts.File, staged); err != nil {
		return untouched(err)
	}
	defer os.Remove(staged)
	inContainer := pgDataMount + "/" + importFile
	// Root inside the container, so the archive needs no wider permissions.
	restore := func(args ...string) []string {
		return append([]string{"exec", flagUser, "root", name, "pg_restore"}, args...)
	}

	fmt.Fprintln(out, "reading the archive")
	schema, err := dockerOutput(ctx, p.Runner, nil, nil, restore("--schema-only", "--file=-", inContainer)...)
	if err != nil {
		return untouched(fmt.Errorf("the file is not a readable custom-format pg_dump archive: %w", err))
	}
	wanted, err := dumpRoles(strings.NewReader(schema))
	if err != nil {
		return untouched(err)
	}
	existing, err := p.psql(ctx, name, pgAdmin, adminDatabase, "SELECT rolname FROM pg_roles")
	if err != nil {
		return untouched(err)
	}
	have := map[string]bool{}
	for _, role := range strings.Fields(existing) {
		have[role] = true
	}
	var borrowed []string
	for _, role := range wanted {
		if !have[role] {
			borrowed = append(borrowed, role)
		}
	}

	fmt.Fprintln(out, "recreating the database")
	var prepare strings.Builder
	fmt.Fprintf(&prepare, "DROP DATABASE IF EXISTS %[1]s WITH (FORCE);\nCREATE DATABASE %[1]s OWNER %[2]s;\n", pgDatabase, pgUser)
	for _, role := range borrowed {
		fmt.Fprintf(&prepare, "CREATE ROLE %s NOLOGIN;\n", role)
	}
	if _, err := p.psql(ctx, name, pgAdmin, adminDatabase, prepare.String()); err != nil {
		return err
	}
	// From here the borrowed roles exist; they must not outlive the import.
	dropBorrowed := func() error {
		var errs []error
		for _, role := range borrowed {
			sql := fmt.Sprintf("REASSIGN OWNED BY %[1]s TO %[2]s;\nDROP OWNED BY %[1]s;\n", role, pgUser)
			if _, err := p.psql(ctx, name, pgAdmin, pgDatabase, sql); err != nil {
				errs = append(errs, err)
				continue
			}
			if _, err := p.psql(ctx, name, pgAdmin, adminDatabase, fmt.Sprintf("DROP ROLE %s;\n", role)); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}

	// One section at a time: a single parallel run over the whole archive
	// creates triggers while the data they guard is still loading.
	for _, section := range []string{"pre-data", "data", "post-data"} {
		fmt.Fprintf(out, "restoring %s\n", section)
		args := []string{"--username=" + pgAdmin, "--dbname=" + pgDatabase, "--exit-on-error", "--section=" + section}
		if section != "pre-data" {
			args = append(args, "--jobs="+opts.jobs())
		}
		if err := dockerStream(ctx, p.Runner, out, nil, restore(append(args, inContainer)...)...); err != nil {
			return errors.Join(fmt.Errorf("restoring %s: %w", section, err), dropBorrowed())
		}
	}

	fmt.Fprintln(out, "handing ownership to the app's role")
	if err := dropBorrowed(); err != nil {
		return err
	}
	if _, err := p.psql(ctx, name, pgAdmin, pgDatabase, handOverSQL+schemaOwnerSQL); err != nil {
		return err
	}
	if err := p.afterImport(ctx, app, name, out); err != nil {
		return err
	}
	fmt.Fprintln(out, "analyzing")
	return dockerStream(ctx, p.Runner, out, nil, "exec", flagUser, "postgres", name,
		"vacuumdb", "--username="+pgAdmin, "--dbname="+pgDatabase, "--analyze-only", "--jobs="+opts.jobs())
}

// afterImport runs the app's after-import.sql, if it has one. The file is
// part of the app's definition, which only the host's operator writes.
func (p *Postgres) afterImport(ctx context.Context, app *appdef.App, name string, out io.Writer) error {
	sql, err := os.ReadFile(filepath.Join(app.Dir, AfterImportFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "running %s\n", AfterImportFile)
	result, err := p.psql(ctx, name, pgAdmin, pgDatabase, string(sql))
	if result != "" {
		fmt.Fprintln(out, result)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", AfterImportFile, err)
	}
	return nil
}

// stage makes src available at dst without changing src.
func stage(src, dst string) error {
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	file, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, in); err != nil {
		file.Close()
		os.Remove(dst)
		return err
	}
	return file.Close()
}
