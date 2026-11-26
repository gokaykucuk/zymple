package services

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gokaykucuk/zymple/internal/appdef"
	"github.com/gokaykucuk/zymple/internal/execx"
	"github.com/gokaykucuk/zymple/internal/execx/execxtest"
)

const sampleSchema = `--
-- Name: users; Type: TABLE; Schema: public; Owner: legacy_owner
--
CREATE TABLE public.users (
    id integer NOT NULL
);
ALTER TABLE public.users OWNER TO legacy_owner;
ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);
ALTER FUNCTION public.audit() OWNER TO "audit_keeper";
ALTER SCHEMA audit OWNER TO audit_keeper;
ALTER TABLE public.jobs OWNER TO postgres;
CREATE FUNCTION public.f() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    EXECUTE format('GRANT audit_keeper TO %I', current_user);
    ALTER TABLE x OWNER TO inside_a_function_body;
END $$;
GRANT SELECT ON TABLE public.users TO legacy_reader;
GRANT ALL ON SCHEMA public TO PUBLIC;
GRANT SELECT ON TABLE public.users TO "weird role";
GRANT UPDATE ON TABLE public.users TO legacy_writer WITH GRANT OPTION;
REVOKE ALL ON TABLE public.users FROM legacy_revoked;
GRANT SELECT ON TABLE public.t TO CURRENT_USER;
ALTER DEFAULT PRIVILEGES FOR ROLE defaults_owner IN SCHEMA public GRANT SELECT ON TABLES TO defaults_reader;
`

func TestDumpRoles(t *testing.T) {
	roles, err := dumpRoles(strings.NewReader(sampleSchema))
	if err != nil {
		t.Fatal(err)
	}
	want := "audit_keeper defaults_owner defaults_reader legacy_owner legacy_reader legacy_revoked legacy_writer postgres"
	if got := strings.Join(roles, " "); got != want {
		t.Fatalf("roles:\n got %s\nwant %s", got, want)
	}
	// Indented statements inside function bodies, placeholders, PUBLIC and
	// names that are not plain identifiers are not roles to create.
	for _, not := range []string{"inside_a_function_body", "%I", "PUBLIC", "CURRENT_USER", "weird"} {
		for _, role := range roles {
			if strings.Contains(role, not) {
				t.Errorf("%q was taken for a role", role)
			}
		}
	}
	if _, err := dumpRoles(errReader{}); err == nil {
		t.Error("a read error was swallowed")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestImportOptionsValidate(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "dump.pgdump")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, o := range []ImportOptions{{File: file}, {File: file, Jobs: 1}, {File: file, Jobs: 32}} {
		if err := o.Validate(); err != nil {
			t.Errorf("%+v rejected: %v", o, err)
		}
	}
	for name, o := range map[string]ImportOptions{
		"relative path":   {File: "dump.pgdump"},
		"unclean path":    {File: dir + "/../" + filepath.Base(dir) + "/dump.pgdump"},
		"missing file":    {File: filepath.Join(dir, "absent")},
		"a directory":     {File: dir},
		"negative jobs":   {File: file, Jobs: -1},
		"too many jobs":   {File: file, Jobs: 33},
		"empty file name": {},
	} {
		if err := o.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if (ImportOptions{}).jobs() != "4" || (ImportOptions{Jobs: 7}).jobs() != "7" {
		t.Error("jobs default is wrong")
	}
}

// importHost is a fake database host for Import: it answers the commands an
// import runs and records them, with the SQL each psql call received.
type importHost struct {
	mu      sync.Mutex
	log     []string
	state   string
	schema  string
	roles   string
	failOn  string
	staged  string
	sawFile bool
}

func (h *importHost) handle(c execx.Cmd) (string, error) {
	line := c.String()
	sql := ""
	if c.Stdin != nil {
		data, _ := io.ReadAll(c.Stdin)
		sql = string(data)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log = append(h.log, strings.TrimSpace(line+"\n"+sql))
	if h.failOn != "" && (strings.Contains(line, h.failOn) || strings.Contains(sql, h.failOn)) {
		return "", errors.New("injected failure")
	}
	switch {
	case strings.HasPrefix(line, "docker inspect"):
		if h.state == "" {
			return "", errors.New("no such container")
		}
		return h.state + " hash\n", nil
	case strings.Contains(line, "--schema-only"):
		return h.schema, nil
	case strings.Contains(line, "--section="):
		if _, err := os.Stat(h.staged); err == nil {
			h.sawFile = true
		}
	case strings.Contains(sql, "SELECT rolname FROM pg_roles"):
		return h.roles, nil
	case strings.Contains(sql, "import_marker"):
		return "marker created\n", nil
	}
	return "", nil
}

func (h *importHost) all() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.log, "\n")
}

func (h *importHost) index(t *testing.T, sub string) int {
	t.Helper()
	i := strings.Index(h.all(), sub)
	if i < 0 {
		t.Fatalf("no command containing %q in:\n%s", sub, h.all())
	}
	return i
}

func newImport(t *testing.T) (*Postgres, *importHost, *appdef.App, ImportOptions) {
	t.Helper()
	fake := &execxtest.Fake{}
	p := newPostgres(t, "", fake)
	app := testApp(t, "postgres: { version: 18 }\n")
	app.Dir = t.TempDir()
	dump := filepath.Join(t.TempDir(), "prod.pgdump")
	if err := os.WriteFile(dump, []byte("archive bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(p.dir(app.Name), "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	host := &importHost{
		state:  "running",
		schema: sampleSchema,
		roles:  "postgres\napp\naudit_keeper\npg_read_all_data\n",
		staged: filepath.Join(p.dir(app.Name), "data", importFile),
	}
	fake.Handler = host.handle
	return p, host, app, ImportOptions{File: dump, Jobs: 6}
}

func TestImport(t *testing.T) {
	p, host, app, opts := newImport(t)
	if err := os.WriteFile(filepath.Join(app.Dir, AfterImportFile), []byte("CREATE TABLE import_marker();\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := p.Import(ctx, app, opts, &out); err != nil {
		t.Fatalf("Import: %v\n%s", err, host.all())
	}
	log := host.all()

	// The steps happen in this order.
	steps := []string{
		"--schema-only", "SELECT rolname FROM pg_roles", "DROP DATABASE IF EXISTS app WITH (FORCE);",
		"--section=pre-data", "--section=data", "--section=post-data",
		"REASSIGN OWNED BY legacy_owner TO app;", "ALTER SCHEMA %I OWNER TO app", "CREATE TABLE import_marker", "vacuumdb",
	}
	last := -1
	for _, step := range steps {
		i := host.index(t, step)
		if i < last {
			t.Errorf("%q ran out of order", step)
		}
		last = i
	}

	// Roles that exist here are kept as owners; the others are borrowed for
	// the restore, emptied into the app's role and dropped.
	for _, role := range []string{"legacy_owner", "legacy_reader", "legacy_writer", "legacy_revoked", "defaults_owner", "defaults_reader"} {
		for _, want := range []string{"CREATE ROLE " + role + " NOLOGIN;", "REASSIGN OWNED BY " + role + " TO app;", "DROP OWNED BY " + role + ";", "DROP ROLE " + role + ";"} {
			if !strings.Contains(log, want) {
				t.Errorf("missing %q", want)
			}
		}
	}
	for _, kept := range []string{"audit_keeper", "postgres", "app"} {
		for _, never := range []string{"CREATE ROLE " + kept + " ", "DROP ROLE " + kept + ";", "REASSIGN OWNED BY " + kept + " "} {
			if strings.Contains(log, never) {
				t.Errorf("an existing role was touched: %q", never)
			}
		}
	}
	if !strings.Contains(log, "CREATE DATABASE app OWNER app;") {
		t.Error("the database was not recreated for the app's role")
	}

	// The restore is strict, sectioned, parallel where that is safe, and runs
	// as root in the container so the archive needs no wider permissions.
	for _, section := range []string{"pre-data", "data", "post-data"} {
		cmd := ""
		for _, l := range strings.Split(log, "\n") {
			if strings.Contains(l, "--section="+section+" ") || strings.HasSuffix(l, "--section="+section) {
				cmd = l
			}
		}
		if !strings.Contains(cmd, "exec --user root zymple-shop-postgres pg_restore") || !strings.Contains(cmd, "--exit-on-error") ||
			!strings.Contains(cmd, "--username=postgres --dbname=app") || !strings.HasSuffix(cmd, "/var/lib/postgresql/data/"+importFile) {
			t.Errorf("%s restore command: %s", section, cmd)
		}
		if (section == "pre-data") == strings.Contains(cmd, "--jobs=6") {
			t.Errorf("%s: wrong use of --jobs: %s", section, cmd)
		}
	}
	if !host.sawFile {
		t.Error("the archive was not in the data mount while the restore ran")
	}
	if _, err := os.Stat(host.staged); !errors.Is(err, os.ErrNotExist) {
		t.Error("the staged archive was left in the data directory")
	}
	if data, _ := os.ReadFile(opts.File); string(data) != "archive bytes" {
		t.Error("the original archive was changed")
	}
	if !strings.Contains(out.String(), "running after-import.sql") || !strings.Contains(out.String(), "marker created") {
		t.Errorf("after-import output missing:\n%s", out.String())
	}
}

func TestImportRefusesBeforeTouchingTheDatabase(t *testing.T) {
	cases := map[string]func(*importHost, *ImportOptions){
		"database not running": func(h *importHost, _ *ImportOptions) { h.state = "exited" },
		"no database":          func(h *importHost, _ *ImportOptions) { h.state = "" },
		"not an archive":       func(h *importHost, _ *ImportOptions) { h.failOn = "--schema-only" },
		"cannot list roles":    func(h *importHost, _ *ImportOptions) { h.failOn = "SELECT rolname" },
		"missing file":         func(_ *importHost, o *ImportOptions) { o.File = "/nonexistent/dump" },
	}
	for name, breakIt := range cases {
		p, host, app, opts := newImport(t)
		breakIt(host, &opts)
		var out bytes.Buffer
		err := p.Import(ctx, app, opts, &out)
		if !errors.Is(err, ErrDatabaseUntouched) {
			t.Errorf("%s: error = %v, want ErrDatabaseUntouched", name, err)
		}
		if strings.Contains(host.all(), "DROP DATABASE") {
			t.Errorf("%s: the database was dropped", name)
		}
		if _, statErr := os.Stat(host.staged); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("%s: the staged archive was left behind", name)
		}
	}
}

func TestImportFailuresAfterTheDrop(t *testing.T) {
	for _, failOn := range []string{"DROP DATABASE", "--section=pre-data", "--section=data", "--section=post-data", "REASSIGN OWNED BY legacy_owner", "DROP ROLE legacy_reader", "ALTER SCHEMA %I", "import_marker", "vacuumdb"} {
		p, host, app, opts := newImport(t)
		if err := os.WriteFile(filepath.Join(app.Dir, AfterImportFile), []byte("CREATE TABLE import_marker();\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		host.failOn = failOn
		var out bytes.Buffer
		err := p.Import(ctx, app, opts, &out)
		if err == nil {
			t.Errorf("a failing %q was not reported", failOn)
			continue
		}
		if errors.Is(err, ErrDatabaseUntouched) {
			t.Errorf("%q: the error claims the database was not changed", failOn)
		}
		if _, statErr := os.Stat(host.staged); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("%q: the staged archive was left behind", failOn)
		}
		// A failed restore must not leave the borrowed roles in the cluster.
		if strings.HasPrefix(failOn, "--section=") && !strings.Contains(host.all(), "DROP ROLE legacy_owner;") {
			t.Errorf("%q: borrowed roles were not dropped after the failure", failOn)
		}
	}
}

func TestImportWithoutAfterImportFile(t *testing.T) {
	p, host, app, opts := newImport(t)
	var out bytes.Buffer
	if err := p.Import(ctx, app, opts, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), AfterImportFile) || strings.Contains(host.all(), "import_marker") {
		t.Error("something ran for an after-import file that does not exist")
	}
	// An unreadable after-import file is an error, not a skip.
	if err := os.Mkdir(filepath.Join(app.Dir, AfterImportFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := p.Import(ctx, app, opts, &out); err == nil {
		t.Error("an unreadable after-import file was ignored")
	}
}

func TestStage(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(dst, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stage(src, dst); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(dst); string(data) != "data" {
		t.Fatalf("staged content = %q", data)
	}
	if err := stage(filepath.Join(dir, "absent"), filepath.Join(dir, "dst2")); err == nil {
		t.Error("staging a missing file should fail")
	}
	if err := os.MkdirAll(filepath.Join(dir, "busy", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := stage(src, filepath.Join(dir, "busy")); err == nil {
		t.Error("staging over a non-empty directory should fail")
	}
}
