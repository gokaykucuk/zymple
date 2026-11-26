package services

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gokaykucuk/zymple/internal/appdef"
	"github.com/gokaykucuk/zymple/internal/config"
	"github.com/gokaykucuk/zymple/internal/execx"
	"github.com/gokaykucuk/zymple/internal/execx/execxtest"
)

var ctx = context.Background()

func testApp(t *testing.T, extra string) *appdef.App {
	t.Helper()
	app, err := appdef.Parse(
		[]byte("name: shop\ndomains: [shop.example.com]\nweb: { service: web, port: 3000 }\n"+extra),
		[]byte("services:\n  web:\n    image: ${IMAGE}\n"))
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func testConfig(t *testing.T, backup string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	doc := "state_dir: " + filepath.Join(dir, "state") + "\n"
	switch backup {
	case "posix":
		doc += "backup: { type: posix, path: " + filepath.Join(dir, "backups") + ", cipher_pass: Secret }\n"
	case "s3":
		doc += "backup: { type: s3, path: prod, cipher_pass: Secret, s3: { endpoint: s3.example.com, bucket: bkt, region: eu, access_key: AK, secret_key: SK, path_style: true } }\n"
	}
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newPostgres(t *testing.T, backup string, fake *execxtest.Fake) *Postgres {
	t.Helper()
	now := time.Unix(1_700_000_000, 0)
	return &Postgres{
		Runner: fake,
		Config: testConfig(t, backup),
		Sleep:  func(context.Context, time.Duration) { now = now.Add(time.Minute) },
		Now:    func() time.Time { return now },
	}
}

// capture records every command in full, including what was sent on stdin
// and the environment given to the docker CLI.
type capture struct {
	mu    sync.Mutex
	cmds  []execx.Cmd
	stdin []string
}

func (c *capture) wrap(next func(execx.Cmd) (string, error)) func(execx.Cmd) (string, error) {
	return func(cmd execx.Cmd) (string, error) {
		in := ""
		if cmd.Stdin != nil {
			data, _ := io.ReadAll(cmd.Stdin)
			in = string(data)
		}
		c.mu.Lock()
		c.cmds = append(c.cmds, cmd)
		c.stdin = append(c.stdin, in)
		c.mu.Unlock()
		if next == nil {
			return "", nil
		}
		return next(cmd)
	}
}

// sql returns everything sent to psql on stdin.
func (c *capture) sql() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.stdin, "\n")
}

// dockerState answers `docker inspect` with a container state and hash.
type dockerState struct {
	state, hash string
}

func (s *dockerState) handler(extra func(execx.Cmd) (string, error, bool)) func(execx.Cmd) (string, error) {
	return func(c execx.Cmd) (string, error) {
		if extra != nil {
			if out, err, ok := extra(c); ok {
				return out, err
			}
		}
		switch c.Args[0] {
		case "inspect":
			if s.state == "" {
				return "", errors.New("no such container")
			}
			return s.state + " " + s.hash + "\n", nil
		case "run":
			if c.Args[1] == "-d" {
				s.state = "running"
				for i, a := range c.Args {
					if a == "--label" && strings.HasPrefix(c.Args[i+1], labelConfig+"=") {
						s.hash = strings.TrimPrefix(c.Args[i+1], labelConfig+"=")
					}
				}
			}
		case "rm":
			s.state = ""
		}
		return "", nil
	}
}

func TestPostgresEnsureLifecycle(t *testing.T) {
	st := &dockerState{}
	fake := &execxtest.Fake{}
	rec := &capture{}
	fake.Handler = rec.wrap(st.handler(nil))
	p := newPostgres(t, "posix", fake)
	app := testApp(t, "postgres: { version: 17, pitr_days: 9 }\n")

	url, err := p.Ensure(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "postgres://app:") || !strings.HasSuffix(url, "@postgres:5432/app?sslmode=disable") {
		t.Fatalf("url = %q", url)
	}
	run := fake.Find("docker run -d")
	for _, want := range []string{
		"--name zymple-shop-postgres", "--restart unless-stopped", "--network zymple-shop", "--network-alias postgres",
		"ghcr.io/gokaykucuk/zymple-postgres:17 postgres", "archive_mode=on",
		"archive_command=pgbackrest --stanza=shop archive-push %p", ":/var/lib/postgresql/data",
		"pgbackrest.conf:/etc/pgbackrest/pgbackrest.conf:ro", ":/var/lib/pgbackrest",
		"-e POSTGRES_USER=postgres", "-e POSTGRES_DB=app", "-e POSTGRES_PASSWORD -e",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("run command lacks %q:\n%s", want, run)
		}
	}
	if strings.Contains(run, "-p ") || strings.Contains(run, "--publish") {
		t.Error("postgres must not publish a port")
	}
	if fake.Find("pg_isready -h 127.0.0.1") == "" || !strings.Contains(rec.sql(), "ALTER ROLE app LOGIN PASSWORD") || fake.Find("stanza-create") == "" {
		t.Errorf("post-start steps missing: %v", fake.Calls())
	}
	if fake.Find("--entrypoint chown") == "" {
		t.Error("the posix repository was not handed to the postgres user")
	}
	conf, err := os.ReadFile(filepath.Join(p.dir("shop"), "pgbackrest.conf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"repo1-type=posix", "repo1-retention-full=9", "repo1-cipher-pass=Secret", "[shop]", "pg1-path=/var/lib/postgresql/data/pgdata", "pg1-user=postgres"} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("pgbackrest.conf lacks %q:\n%s", want, conf)
		}
	}
	for _, file := range []string{"password", "admin-password"} {
		info, err := os.Stat(filepath.Join(p.dir("shop"), file))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s file: %v, %v", file, info, err)
		}
	}
	// The app's role and the superuser must not share a password.
	appPW, _ := p.password("shop")
	adminPW, _ := p.adminPassword("shop")
	if appPW == adminPW || strings.Contains(url, adminPW) {
		t.Error("the app was given the superuser's password")
	}

	// A second Ensure with nothing changed must not touch the container:
	// a deploy never restarts the database.
	fake.Reset()
	url2, err := p.Ensure(ctx, app)
	if err != nil || url2 != url {
		t.Fatalf("second Ensure: %q, %v", url2, err)
	}
	if fake.Count("docker run") != 0 || fake.Count("docker rm") != 0 || len(fake.Calls()) != 1 {
		t.Errorf("unchanged Ensure ran: %v", fake.Calls())
	}

	// A changed setting recreates the container and keeps the password.
	fake.Reset()
	changed := testApp(t, "postgres: { version: 17, pitr_days: 30 }\n")
	url3, err := p.Ensure(ctx, changed)
	if err != nil || url3 != url {
		t.Fatalf("Ensure after a change: %q, %v", url3, err)
	}
	if fake.Find("docker rm -f zymple-shop-postgres") == "" || fake.Find("docker run -d") == "" {
		t.Errorf("a changed setting did not recreate the container: %v", fake.Calls())
	}
	// Regression: the running database used to be removed with `rm -f`
	// alone, which is SIGKILL. It must be stopped gracefully first.
	calls := strings.Join(fake.Calls(), "\n")
	stop, rm := strings.Index(calls, "docker stop --time 120 zymple-shop-postgres"), strings.Index(calls, "docker rm -f zymple-shop-postgres")
	if stop < 0 || stop > rm {
		t.Errorf("the database was not stopped gracefully before removal:\n%s", calls)
	}

	// A stopped container is replaced as well.
	st.state = "exited"
	fake.Reset()
	if _, err := p.Ensure(ctx, changed); err != nil || fake.Find("docker run -d") == "" {
		t.Errorf("a stopped container was not replaced: %v, %v", err, fake.Calls())
	}
	if fake.Count("docker stop") != 0 {
		t.Errorf("a container that is not running needs no stop: %v", fake.Calls())
	}
}

func TestPostgresWithoutBackups(t *testing.T) {
	st := &dockerState{}
	fake := &execxtest.Fake{}
	fake.Handler = st.handler(nil)
	p := newPostgres(t, "", fake)
	app := testApp(t, "postgres: { version: 16 }\n")
	if _, err := p.Ensure(ctx, app); err != nil {
		t.Fatal(err)
	}
	run := fake.Find("docker run -d")
	if !strings.Contains(run, "archive_mode=off") || strings.Contains(run, "pgbackrest") {
		t.Errorf("without a repository there must be no archiving:\n%s", run)
	}
	if fake.Count("stanza-create") != 0 {
		t.Error("stanza-create ran without a repository")
	}
	var out bytes.Buffer
	if err := p.Backup(ctx, "shop", "full", &out); err == nil {
		t.Error("Backup without a repository should fail")
	}
	if err := p.Info(ctx, "shop", &out); err == nil {
		t.Error("Info without a repository should fail")
	}
	if err := p.Restore(ctx, app, RestoreOptions{}, &out); err == nil {
		t.Error("Restore without a repository should fail")
	}
}

func TestPgbackrestConfS3(t *testing.T) {
	p := newPostgres(t, "s3", &execxtest.Fake{})
	conf := p.pgbackrestConf(testApp(t, "postgres: { version: 17 }\n"))
	for _, want := range []string{
		"repo1-type=s3", "repo1-path=/prod/shop/postgres", "repo1-s3-endpoint=s3.example.com", "repo1-s3-bucket=bkt",
		"repo1-s3-region=eu", "repo1-s3-key=AK", "repo1-s3-key-secret=SK", "repo1-s3-uri-style=path",
		"repo1-cipher-type=aes-256-cbc", "repo1-retention-full=14",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("conf lacks %q:\n%s", want, conf)
		}
	}
	if got := p.mounts("shop", "/data"); len(got) != 4 {
		t.Errorf("s3 needs no repository mount: %v", got)
	}
}

func TestPostgresEnsureFailures(t *testing.T) {
	app := testApp(t, "postgres: { version: 17 }\n")
	for _, failOn := range []string{"--entrypoint chown", "run -d", "psql", "stanza-create", "rm -f", "docker stop"} {
		st := &dockerState{state: "running", hash: "stale"}
		fake := &execxtest.Fake{}
		fake.Handler = st.handler(func(c execx.Cmd) (string, error, bool) {
			if strings.Contains(c.String(), failOn) {
				return "", errors.New("injected"), true
			}
			return "", nil, false
		})
		p := newPostgres(t, "posix", fake)
		if _, err := p.Ensure(ctx, app); err == nil {
			t.Errorf("a failing %q was not reported", failOn)
		}
	}
}

func TestWaitReadyFailsFastOnACrashLoop(t *testing.T) {
	st := &dockerState{}
	fake := &execxtest.Fake{}
	fake.Handler = st.handler(func(c execx.Cmd) (string, error, bool) {
		switch {
		case c.Args[0] == "inspect":
			return "restarting x\n", nil, true
		case strings.Contains(c.String(), "pg_isready"):
			return "", errors.New("no response"), true
		case c.Args[0] == "logs":
			return "mkdir: Permission denied\n", nil, true
		}
		return "", nil, false
	})
	p := newPostgres(t, "", fake)
	err := p.waitReady(ctx, "zymple-shop-postgres")
	if err == nil || !strings.Contains(err.Error(), "Permission denied") || !strings.Contains(err.Error(), "restarting") {
		t.Fatalf("error should carry the state and the logs: %v", err)
	}
	if n := fake.Count("pg_isready"); n > 6 {
		t.Errorf("probed %d times before giving up", n)
	}
}

func TestWaitReadyTimesOutAndHonoursCancellation(t *testing.T) {
	st := &dockerState{state: "running"}
	fake := &execxtest.Fake{}
	fake.Handler = st.handler(func(c execx.Cmd) (string, error, bool) {
		if strings.Contains(c.String(), "pg_isready") {
			return "", errors.New("starting up"), true
		}
		return "", nil, false
	})
	p := newPostgres(t, "", fake)
	if err := p.waitReady(ctx, "x"); err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("timeout error = %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := p.waitReady(cancelled, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}
}

func TestBackupAndInfo(t *testing.T) {
	fake := &execxtest.Fake{}
	p := newPostgres(t, "posix", fake)
	var out bytes.Buffer
	for _, kind := range []string{"full", "diff", "incr"} {
		if err := p.Backup(ctx, "shop", kind, &out); err != nil {
			t.Fatal(err)
		}
	}
	want := "docker exec --user postgres zymple-shop-postgres pgbackrest --stanza=shop --type=diff backup"
	if fake.Find("--type=diff") != want {
		t.Errorf("backup command:\n got %s\nwant %s", fake.Find("--type=diff"), want)
	}
	if err := p.Backup(ctx, "shop", "full; rm -rf /", &out); err == nil {
		t.Error("an unknown backup type was accepted")
	}
	if err := p.Info(ctx, "shop", &out); err != nil || fake.Find("--stanza=shop info") == "" {
		t.Errorf("Info: %v, %v", err, fake.Calls())
	}
}

func TestDueBackup(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	at := func(age time.Duration) string { return itoa(now.Add(-age).Unix()) }
	backup := func(kind string, age time.Duration) string {
		return `{"type":"` + kind + `","timestamp":{"stop":` + at(age) + `}}`
	}
	cases := []struct {
		name, json, want string
	}{
		{"no stanza yet", `[]`, "full"},
		{"no backups", `[{"backup":[]}]`, "full"},
		{"fresh full", `[{"backup":[` + backup("full", time.Hour) + `]}]`, ""},
		{"full a day old", `[{"backup":[` + backup("full", 25*time.Hour) + `]}]`, "diff"},
		{"recent diff", `[{"backup":[` + backup("full", 3*24*time.Hour) + `,` + backup("diff", time.Hour) + `]}]`, ""},
		{"full a week old", `[{"backup":[` + backup("full", 8*24*time.Hour) + `,` + backup("diff", time.Hour) + `]}]`, "full"},
	}
	for _, c := range cases {
		fake := &execxtest.Fake{Handler: func(execx.Cmd) (string, error) { return c.json, nil }}
		p := newPostgres(t, "posix", fake)
		p.Now = func() time.Time { return now }
		got, err := p.DueBackup(ctx, "shop")
		if err != nil || got != c.want {
			t.Errorf("%s: DueBackup = %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	fake := &execxtest.Fake{Handler: func(execx.Cmd) (string, error) { return "not json", nil }}
	if _, err := newPostgres(t, "posix", fake).DueBackup(ctx, "shop"); err == nil {
		t.Error("unparseable info should be an error")
	}
	fake.Handler = func(execx.Cmd) (string, error) { return "", errors.New("down") }
	if _, err := newPostgres(t, "posix", fake).DueBackup(ctx, "shop"); err == nil {
		t.Error("a failing info command should be an error")
	}
}

func itoa(n int64) string {
	var b []byte
	if n == 0 {
		return "0"
	}
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func TestRestoreOptionsValidate(t *testing.T) {
	good := []RestoreOptions{
		{}, {Time: "2026-01-02 15:04:05"}, {Time: "2026-01-02T15:04:05Z"}, {Time: "2026-01-02 15:04:05.123456+00"},
		{Time: "2026-01-02 15:04:05+02:00", Into: "drill-1"}, {Into: "a"},
	}
	for _, o := range good {
		if err := o.Validate(); err != nil {
			t.Errorf("%+v rejected: %v", o, err)
		}
	}
	bad := []RestoreOptions{
		{Time: "yesterday"}, {Time: "2026-01-02"}, {Time: "2026-01-02 15:04:05' --pg1-path=/"},
		{Time: "2026-01-02 15:04:05\n--delta"}, {Into: "Drill"}, {Into: "../x"}, {Into: "-rm"}, {Into: strings.Repeat("a", 40)},
	}
	for _, o := range bad {
		if err := o.Validate(); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
}

func TestRestoreInPlace(t *testing.T) {
	st := &dockerState{state: "running"}
	fake := &execxtest.Fake{}
	rec := &capture{}
	fake.Handler = rec.wrap(st.handler(nil))
	p := newPostgres(t, "posix", fake)
	app := testApp(t, "postgres: { version: 17 }\n")
	var out bytes.Buffer
	if err := p.Restore(ctx, app, RestoreOptions{Time: "2026-01-02 15:04:05+00"}, &out); err != nil {
		t.Fatal(err)
	}
	calls := fake.Calls()
	index := func(sub string) int {
		for i, c := range calls {
			if strings.Contains(c, sub) {
				return i
			}
		}
		t.Fatalf("no call containing %q in %v", sub, calls)
		return -1
	}
	check, stop, restore, start := index("--stanza=shop check"), index("docker stop --time 120 zymple-shop-postgres"), index("--entrypoint sh"), index("docker start zymple-shop-postgres")
	if !(check < stop && stop < restore && restore < start) {
		t.Errorf("wrong order: check=%d stop=%d restore=%d start=%d", check, stop, restore, start)
	}
	cmd := calls[restore]
	for _, want := range []string{"--stanza=shop", "--type=time", "--target=2026-01-02 15:04:05+00", "--target-action=promote", "--delta restore"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("restore command lacks %q:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "--archive-mode=off") {
		t.Error("an in-place restore must keep archiving")
	}
	if !strings.Contains(rec.sql(), "ALTER ROLE app LOGIN PASSWORD") {
		t.Error("the role password was not re-applied after the restore")
	}
}

func TestRestoreWithoutTimeUsesLatest(t *testing.T) {
	st := &dockerState{state: "running"}
	fake := &execxtest.Fake{}
	fake.Handler = st.handler(nil)
	p := newPostgres(t, "posix", fake)
	var out bytes.Buffer
	if err := p.Restore(ctx, testApp(t, "postgres: { version: 17 }\n"), RestoreOptions{}, &out); err != nil {
		t.Fatal(err)
	}
	if cmd := fake.Find("--entrypoint sh"); strings.Contains(cmd, "--type=time") || !strings.Contains(cmd, "--delta restore") {
		t.Errorf("restore command: %s", cmd)
	}
}

func TestRestoreIntoDrill(t *testing.T) {
	st := &dockerState{state: "running"}
	fake := &execxtest.Fake{}
	// "check" fails here: the live cluster being broken must not stop a drill.
	fake.Handler = st.handler(func(c execx.Cmd) (string, error, bool) {
		if strings.HasSuffix(c.String(), " check") {
			return "", errors.New("cluster is down"), true
		}
		if c.Args[0] == "inspect" && strings.HasSuffix(c.String(), "-drill-drill1") {
			return "shop postgres-drill", nil, true
		}
		return "", nil, false
	})
	p := newPostgres(t, "posix", fake)
	app := testApp(t, "postgres: { version: 17 }\n")
	var out bytes.Buffer
	if err := p.Restore(ctx, app, RestoreOptions{Into: "drill1"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "warning") {
		t.Errorf("a failed WAL check should be reported as a warning: %q", out.String())
	}
	if fake.Count("docker stop") != 0 || fake.Count("docker start") != 0 {
		t.Errorf("a drill must not touch the live container: %v", fake.Calls())
	}
	if cmd := fake.Find("--entrypoint sh"); !strings.Contains(cmd, "--archive-mode=off restore") || strings.Contains(cmd, "--delta") {
		t.Errorf("drill restore command: %s", cmd)
	}
	run := fake.Find("docker run -d")
	for _, want := range []string{"--name zymple-shop-postgres-drill-drill1", "--network-alias postgres-drill-drill1", "archive_mode=off", "postgres/drill-drill1:/var/lib/postgresql/data"} {
		if !strings.Contains(run, want) {
			t.Errorf("drill run command lacks %q:\n%s", want, run)
		}
	}
	if strings.Contains(run, "archive_command") || strings.Contains(run, "--restart") {
		t.Errorf("a drill cluster must not archive or restart itself:\n%s", run)
	}
	if err := p.Restore(ctx, app, RestoreOptions{Into: "drill1"}, &out); err == nil {
		t.Error("restoring over an existing drill should fail")
	}
	if err := p.RemoveDrill(ctx, "shop", "drill1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.dir("shop"), "drill-drill1")); !errors.Is(err, os.ErrNotExist) {
		t.Error("drill data was not removed")
	}
	if err := p.RemoveDrill(ctx, "shop", "../data"); err == nil {
		t.Error("a path-like drill name was accepted")
	}
}

func TestRestoreFailures(t *testing.T) {
	app := testApp(t, "postgres: { version: 17 }\n")
	var out bytes.Buffer
	for _, failOn := range []string{"docker stop", "--entrypoint sh", "docker start", "psql"} {
		st := &dockerState{state: "running"}
		fake := &execxtest.Fake{}
		fake.Handler = st.handler(func(c execx.Cmd) (string, error, bool) {
			if strings.Contains(c.String(), failOn) {
				return "", errors.New("injected"), true
			}
			return "", nil, false
		})
		p := newPostgres(t, "posix", fake)
		if err := p.Restore(ctx, app, RestoreOptions{}, &out); err == nil {
			t.Errorf("a failing %q was not reported", failOn)
		}
	}
	p := newPostgres(t, "posix", &execxtest.Fake{})
	if err := p.Restore(ctx, app, RestoreOptions{Time: "nonsense"}, &out); err == nil {
		t.Error("invalid options were accepted")
	}
	fake := &execxtest.Fake{Handler: func(c execx.Cmd) (string, error) {
		if c.Args[0] == "run" && c.Args[1] == "-d" {
			return "", errors.New("injected")
		}
		return "", nil
	}}
	if err := newPostgres(t, "posix", fake).Restore(ctx, app, RestoreOptions{Into: "d"}, &out); err == nil {
		t.Error("a failing drill start was not reported")
	}
	fake.Handler = func(execx.Cmd) (string, error) { return "", errors.New("injected") }
	if err := newPostgres(t, "posix", fake).RemoveDrill(ctx, "shop", "d"); err == nil {
		t.Error("a failing drill removal was not reported")
	}
}

// Regression: a drill was removed by container name alone, and the drill
// "postgres" of app "shop" has the same name as the live database of an app
// called "shop-postgres-drill".
func TestRemoveDrillOnlyRemovesItsOwnDrill(t *testing.T) {
	if DrillContainer("shop", "postgres") != PostgresContainer("shop-postgres-drill") {
		t.Fatal("the names no longer collide; this test needs another example")
	}
	for name, c := range map[string]struct {
		labels  string
		err     error
		removed bool
		fails   bool
	}{
		"its own drill":              {labels: "shop postgres-drill", removed: true},
		"another app's database":     {labels: "shop-postgres-drill postgres", fails: true},
		"another app's drill":        {labels: "other postgres-drill", fails: true},
		"a container without labels": {labels: "", fails: true},
		"no container":               {err: errors.New("Error: No such object: zymple-shop-postgres-drill-postgres")},
		"docker is down":             {err: errors.New("cannot connect to the Docker daemon"), fails: true},
	} {
		fake := &execxtest.Fake{Handler: func(cmd execx.Cmd) (string, error) {
			if cmd.Args[0] == "inspect" {
				return c.labels, c.err
			}
			return "", nil
		}}
		p := newPostgres(t, "posix", fake)
		data := filepath.Join(p.dir("shop"), "drill-postgres")
		if err := os.MkdirAll(data, 0o750); err != nil {
			t.Fatal(err)
		}
		err := p.RemoveDrill(ctx, "shop", "postgres")
		if (err != nil) != c.fails {
			t.Errorf("%s: error = %v, want failure %v", name, err, c.fails)
		}
		if removed := fake.Count("rm -f") > 0; removed != c.removed {
			t.Errorf("%s: container removed = %v, want %v", name, removed, c.removed)
		}
		if _, statErr := os.Stat(data); c.fails == errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("%s: drill data present = %v after error %v", name, statErr == nil, err)
		}
	}
}

func TestSecretFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "password")
	a, err := secretFile(path)
	if err != nil || len(a) != 48 {
		t.Fatalf("secretFile = %q, %v", a, err)
	}
	b, _ := secretFile(path)
	if a != b {
		t.Error("the secret changed between calls")
	}
	if err := os.WriteFile(path, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, _ := secretFile(path); c == "short" || len(c) != 48 {
		t.Errorf("a truncated secret was kept: %q", c)
	}
	if _, err := secretFile(filepath.Dir(path)); err == nil {
		t.Error("a directory in place of the file should be an error")
	}
}

func TestRedisEnsure(t *testing.T) {
	st := &dockerState{}
	fake := &execxtest.Fake{}
	fake.Handler = st.handler(nil)
	r := &Redis{Runner: fake, Config: testConfig(t, "")}
	image := r.Config.RedisImage
	cache := testApp(t, "redis: { persistent: false }\n")
	url, err := r.Ensure(ctx, cache)
	if err != nil || url != RedisURL {
		t.Fatalf("Ensure = %q, %v", url, err)
	}
	run := fake.Find("docker run -d")
	for _, want := range []string{"--name zymple-shop-redis", "--network-alias redis", "no-new-privileges:true", image + " redis-server --save  --appendonly no"} {
		if !strings.Contains(run, want) {
			t.Errorf("run command lacks %q:\n%s", want, run)
		}
	}
	if strings.Contains(run, "-v ") {
		t.Error("a cache-only Redis must not get a volume")
	}
	fake.Reset()
	if _, err := r.Ensure(ctx, cache); err != nil || fake.Count("docker run") != 0 {
		t.Errorf("unchanged Ensure ran: %v", fake.Calls())
	}
	fake.Reset()
	persistent := testApp(t, "redis: { persistent: true }\n")
	if _, err := r.Ensure(ctx, persistent); err != nil {
		t.Fatal(err)
	}
	if run := fake.Find("docker run -d"); !strings.Contains(run, "/redis:/data") || !strings.Contains(run, "--appendonly yes") {
		t.Errorf("persistent run command: %s", run)
	}
	if fake.Find("docker rm -f zymple-shop-redis") == "" {
		t.Error("changing the mode did not recreate the container")
	}
	calls := strings.Join(fake.Calls(), "\n")
	if stop, rm := strings.Index(calls, "docker stop --time 60 zymple-shop-redis"), strings.Index(calls, "docker rm -f"); stop < 0 || stop > rm {
		t.Errorf("Redis was not stopped gracefully before removal:\n%s", calls)
	}
	fake.Handler = func(c execx.Cmd) (string, error) {
		if c.Args[0] == "inspect" {
			return "running stale\n", nil
		}
		return "", errors.New("injected")
	}
	if _, err := r.Ensure(ctx, cache); err == nil {
		t.Error("a failing rm was not reported")
	}
}

func TestRedisBackupAndRestore(t *testing.T) {
	persistent := testApp(t, "redis: { persistent: true }\n")
	cache := testApp(t, "redis: { persistent: false }\n")
	var out bytes.Buffer

	fake := &execxtest.Fake{}
	r := &Redis{Runner: fake, Config: testConfig(t, "posix")}
	if err := r.Backup(ctx, cache, &out); err == nil {
		t.Error("backing up a cache-only Redis should fail")
	}
	if err := r.Restore(ctx, cache, &out); err == nil {
		t.Error("restoring a cache-only Redis should fail")
	}
	if err := r.Backup(ctx, persistent, &out); err != nil {
		t.Fatal(err)
	}
	if fake.Count(" init") != 0 {
		t.Error("an existing repository was initialised again")
	}
	backup := fake.Find("backup /data")
	for _, want := range []string{"/redis:/data:ro", "RESTIC_REPOSITORY=/repo", r.Config.ResticImage + " backup /data"} {
		if !strings.Contains(backup, want) {
			t.Errorf("backup command lacks %q:\n%s", want, backup)
		}
	}
	// A new repository is initialised first.
	fake.Reset()
	fake.Handler = func(c execx.Cmd) (string, error) {
		if strings.HasSuffix(c.String(), "cat config") {
			return "", errors.New("repository does not exist")
		}
		return "", nil
	}
	if err := r.Backup(ctx, persistent, &out); err != nil || fake.Count(" init") != 1 {
		t.Errorf("init was not run for a new repository: %v, %v", err, fake.Calls())
	}
	fake.Handler = func(execx.Cmd) (string, error) { return "", errors.New("injected") }
	if err := r.Backup(ctx, persistent, &out); err == nil {
		t.Error("a failing init was not reported")
	}

	fake.Reset()
	fake.Handler = nil
	if err := r.Restore(ctx, persistent, &out); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(fake.Calls(), "\n")
	stop, restore, start := strings.Index(calls, "docker stop --time 60 zymple-shop-redis"), strings.Index(calls, "restore latest"), strings.Index(calls, "docker start zymple-shop-redis")
	if stop < 0 || !(stop < restore && restore < start) {
		t.Errorf("restore order wrong:\n%s", calls)
	}
	for _, failOn := range []string{"docker stop", "restore latest", "docker start"} {
		fake.Handler = func(c execx.Cmd) (string, error) {
			if strings.Contains(c.String(), failOn) {
				return "", errors.New("injected")
			}
			return "", nil
		}
		if err := r.Restore(ctx, persistent, &out); err == nil {
			t.Errorf("a failing %q was not reported", failOn)
		}
	}

	// S3 repositories get credentials through the environment.
	fake.Reset()
	fake.Handler = nil
	s3 := &Redis{Runner: fake, Config: testConfig(t, "s3")}
	if err := s3.Backup(ctx, persistent, &out); err != nil {
		t.Fatal(err)
	}
	if cmd := fake.Find("backup /data"); !strings.Contains(cmd, "RESTIC_REPOSITORY=s3:https://s3.example.com/bkt/prod/shop/redis") || !strings.Contains(cmd, "-e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY ") {
		t.Errorf("s3 backup command: %s", cmd)
	}

	// No repository, or no passphrase: refuse.
	none := &Redis{Runner: fake, Config: testConfig(t, "")}
	if err := none.Backup(ctx, persistent, &out); err == nil {
		t.Error("a backup without a repository should fail")
	}
	if err := none.Restore(ctx, persistent, &out); err == nil {
		t.Error("a restore without a repository should fail")
	}
	plain := testConfig(t, "posix")
	plain.Backup.CipherPass = ""
	if err := (&Redis{Runner: fake, Config: plain}).Backup(ctx, persistent, &out); err == nil {
		t.Error("a backup without a passphrase should fail")
	}
}

// Regression: restic refuses "restore --target / --delete" without a filter,
// so the restore could never succeed, and the Redis it had stopped stayed
// down.
func TestRedisRestoreCommandAndRestart(t *testing.T) {
	persistent := testApp(t, "redis: { persistent: true }\n")
	fake := &execxtest.Fake{}
	r := &Redis{Runner: fake, Config: testConfig(t, "posix")}
	var out bytes.Buffer
	if err := r.Restore(ctx, persistent, &out); err != nil {
		t.Fatal(err)
	}
	if cmd := fake.Find("restore latest"); !strings.HasSuffix(cmd, " restore latest --target / --delete --include /data") {
		t.Errorf("restore command: %s", cmd)
	}

	fake.Reset()
	fake.Handler = func(c execx.Cmd) (string, error) {
		if strings.Contains(c.String(), "restore latest") {
			return "", errors.New("restic says no")
		}
		return "", nil
	}
	err := r.Restore(ctx, persistent, &out)
	if err == nil || !strings.Contains(err.Error(), "restic says no") {
		t.Fatalf("a failed restore was not reported: %v", err)
	}
	if fake.Find("docker start zymple-shop-redis") == "" {
		t.Errorf("Redis was left stopped after a failed restore: %v", fake.Calls())
	}

	// Both failures are reported when the restart fails too.
	fake.Handler = func(c execx.Cmd) (string, error) {
		if strings.Contains(c.String(), "restore latest") || strings.Contains(c.String(), "docker start") {
			return "", errors.New("failed: " + c.Args[0])
		}
		return "", nil
	}
	if err := r.Restore(ctx, persistent, &out); err == nil || !strings.Contains(err.Error(), "failed: run") || !strings.Contains(err.Error(), "failed: start") {
		t.Errorf("want both errors, got: %v", err)
	}
}

// Regression: the Postgres password, the backup passphrase and the S3 secret
// key were passed as `-e NAME=value` and inside a psql -c argument. Every
// local user can read a process's command line.
func TestNoSecretOnACommandLine(t *testing.T) {
	for _, backup := range []string{"posix", "s3"} {
		st := &dockerState{}
		fake := &execxtest.Fake{}
		rec := &capture{}
		fake.Handler = rec.wrap(st.handler(nil))
		p := newPostgres(t, backup, fake)
		r := &Redis{Runner: fake, Config: p.Config}
		app := testApp(t, "postgres: { version: 17 }\nredis: { persistent: true }\n")
		var out bytes.Buffer
		steps := map[string]func() error{
			"postgres ensure":  func() error { _, err := p.Ensure(ctx, app); return err },
			"postgres backup":  func() error { return p.Backup(ctx, "shop", "full", &out) },
			"postgres restore": func() error { return p.Restore(ctx, app, RestoreOptions{}, &out) },
			"postgres drill":   func() error { return p.Restore(ctx, app, RestoreOptions{Into: "d"}, &out) },
			"redis ensure":     func() error { _, err := r.Ensure(ctx, app); return err },
			"redis backup":     func() error { return r.Backup(ctx, app, &out) },
			"redis restore":    func() error { return r.Restore(ctx, app, &out) },
		}
		for name, step := range steps {
			if err := step(); err != nil {
				t.Fatalf("%s/%s: %v", backup, name, err)
			}
		}
		appPW, _ := p.password("shop")
		adminPW, _ := p.adminPassword("shop")
		secrets := map[string]string{"app password": appPW, "superuser password": adminPW, "cipher pass": "Secret"}
		if backup == "s3" {
			secrets["s3 secret key"] = "SK"
			secrets["s3 access key"] = "AK"
		}
		word := func(secret string) *regexp.Regexp {
			return regexp.MustCompile(`(^|[^A-Za-z0-9])` + regexp.QuoteMeta(secret) + `($|[^A-Za-z0-9])`)
		}
		for what, secret := range secrets {
			re := word(secret)
			for _, cmd := range rec.cmds {
				for _, arg := range cmd.Args {
					if re.MatchString(arg) {
						t.Errorf("%s: the %s is on a command line: %s", backup, what, cmd.String())
					}
				}
			}
		}
		// They still have to arrive: through the docker CLI's environment or
		// psql's stdin.
		env := ""
		for _, cmd := range rec.cmds {
			env += strings.Join(cmd.Env, "\n") + "\n"
		}
		for _, want := range []string{"POSTGRES_PASSWORD=" + adminPW, "RESTIC_PASSWORD=Secret"} {
			if !strings.Contains(env, want+"\n") {
				t.Errorf("%s: %s was not passed in the environment", backup, strings.SplitN(want, "=", 2)[0])
			}
		}
		if backup == "s3" && (!strings.Contains(env, "AWS_SECRET_ACCESS_KEY=SK\n") || !strings.Contains(env, "AWS_ACCESS_KEY_ID=AK\n")) {
			t.Error("the S3 credentials were not passed in the environment")
		}
		if !strings.Contains(rec.sql(), "PASSWORD '"+appPW+"'") {
			t.Errorf("%s: the role password did not reach psql on stdin", backup)
		}
	}
}

// Regression: the app's role was the cluster's superuser, so the app could
// run programs in the database container and read the backup credentials.
func TestAppRoleIsNotTheSuperuser(t *testing.T) {
	st := &dockerState{}
	fake := &execxtest.Fake{}
	rec := &capture{}
	fake.Handler = rec.wrap(st.handler(func(c execx.Cmd) (string, error, bool) {
		if strings.Contains(c.String(), "psql -U postgres") {
			return "f\n", nil, true
		}
		return "", nil, false
	}))
	p := newPostgres(t, "", fake)
	var warnings []string
	p.Warn = func(msg string) { warnings = append(warnings, msg) }
	if _, err := p.Ensure(ctx, testApp(t, "postgres: { version: 17 }\n")); err != nil {
		t.Fatal(err)
	}
	if fake.Count("psql -U app") != 0 {
		t.Errorf("a new cluster was managed through the app's role: %v", fake.Calls())
	}
	sql := rec.sql()
	for _, want := range []string{"CREATE ROLE app LOGIN;", "ALTER DATABASE app OWNER TO app;", "SELECT rolsuper FROM pg_roles WHERE rolname = 'app';"} {
		if !strings.Contains(sql, want) {
			t.Errorf("role setup lacks %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "CREATE ROLE postgres") {
		t.Error("a new cluster already has its superuser")
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
}

// A cluster created before the roles were split has "app" as its only
// superuser. It must keep working, gain the postgres role, and be reported.
func TestLegacyClusterGainsASuperuserAndWarns(t *testing.T) {
	st := &dockerState{}
	fake := &execxtest.Fake{}
	rec := &capture{}
	hasPostgres := false
	fake.Handler = rec.wrap(st.handler(func(c execx.Cmd) (string, error, bool) {
		line := c.String()
		switch {
		case strings.Contains(line, "psql -U postgres") && !hasPostgres:
			return "", errors.New(`role "postgres" does not exist`), true
		case strings.Contains(line, "psql -U app"):
			hasPostgres = true
			return "", nil, true
		case strings.Contains(line, "psql -U postgres"):
			return "t\n", nil, true
		}
		return "", nil, false
	}))
	p := newPostgres(t, "", fake)
	var warnings []string
	p.Warn = func(msg string) { warnings = append(warnings, msg) }
	app := testApp(t, "postgres: { version: 17 }\n")
	if _, err := p.Ensure(ctx, app); err != nil {
		t.Fatal(err)
	}
	sql := rec.sql()
	if !strings.Contains(sql, "CREATE ROLE postgres SUPERUSER LOGIN") || !strings.Contains(sql, "ALTER ROLE app LOGIN PASSWORD") {
		t.Errorf("legacy migration SQL:\n%s", sql)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "superuser") || !strings.Contains(warnings[0], "shop") {
		t.Errorf("warnings = %v", warnings)
	}
	// Without a Warn function the condition is simply not reported.
	p.Warn = nil
	st.state = "exited"
	if _, err := p.Ensure(ctx, app); err != nil {
		t.Fatal(err)
	}

	// A cluster with neither role usable is an error that names both tries.
	broken := &execxtest.Fake{}
	bst := &dockerState{}
	broken.Handler = bst.handler(func(c execx.Cmd) (string, error, bool) {
		if strings.Contains(c.String(), "psql") {
			return "", errors.New("refused for " + c.Args[5]), true
		}
		return "", nil, false
	})
	_, err := newPostgres(t, "", broken).Ensure(ctx, app)
	if err == nil || !strings.Contains(err.Error(), "refused for postgres") || !strings.Contains(err.Error(), "refused for app") {
		t.Errorf("want both attempts in the error, got: %v", err)
	}
}

// Regression: a running container with a matching label was taken as proof
// that the cluster was set up. A first start interrupted before the roles
// were created left a cluster without the app's role for good.
func TestEnsureFinishesAnInterruptedSetup(t *testing.T) {
	app := testApp(t, "postgres: { version: 17 }\n")
	for _, failOn := range []string{"psql", "stanza-create"} {
		st := &dockerState{}
		fake := &execxtest.Fake{}
		rec := &capture{}
		failing := true
		fake.Handler = rec.wrap(st.handler(func(c execx.Cmd) (string, error, bool) {
			if failing && strings.Contains(c.String(), failOn) {
				return "", errors.New("injected"), true
			}
			return "", nil, false
		}))
		p := newPostgres(t, "posix", fake)
		if _, err := p.Ensure(ctx, app); err == nil {
			t.Fatalf("%s: the first Ensure should fail", failOn)
		}
		if st.state != "running" || st.hash == "" {
			t.Fatalf("%s: the container should be left running with its label: %+v", failOn, st)
		}

		// Still failing: the error is reported and the database is not touched.
		fake.Reset()
		if _, err := p.Ensure(ctx, app); err == nil {
			t.Errorf("%s: a setup that fails again was swallowed", failOn)
		}
		assertContainerUntouched(t, fake)

		failing = false
		fake.Reset()
		rec.cmds, rec.stdin = nil, nil
		if _, err := p.Ensure(ctx, app); err != nil {
			t.Fatalf("%s: Ensure after the failure cleared: %v", failOn, err)
		}
		assertContainerUntouched(t, fake)
		if !strings.Contains(rec.sql(), "ALTER ROLE app LOGIN PASSWORD") || fake.Count("stanza-create") != 1 {
			t.Errorf("%s: the setup was not run again: %v", failOn, fake.Calls())
		}

		// Once it has succeeded it is not repeated.
		fake.Reset()
		if _, err := p.Ensure(ctx, app); err != nil || len(fake.Calls()) != 1 {
			t.Errorf("%s: a settled Ensure ran: %v, %v", failOn, err, fake.Calls())
		}
	}
}

func assertContainerUntouched(t *testing.T, fake *execxtest.Fake) {
	t.Helper()
	for _, verb := range []string{"docker run", "docker rm", "docker stop", "docker start"} {
		if fake.Count(verb) != 0 {
			t.Errorf("a running database was touched by %q: %v", verb, fake.Calls())
		}
	}
}

// Regression: after an upgrade of Zymple the database container is already
// running with a matching label, so the new process never looked at its
// roles. A new process sets a running cluster up once, and only once.
func TestEnsureSetsUpARunningClusterOncePerProcess(t *testing.T) {
	app := testApp(t, "postgres: { version: 17 }\n")
	st := &dockerState{}
	fake := &execxtest.Fake{}
	fake.Handler = st.handler(nil)
	old := newPostgres(t, "posix", fake)
	url, err := old.Ensure(ctx, app)
	if err != nil {
		t.Fatal(err)
	}

	// A new process: the same host state, nothing remembered.
	p := newPostgres(t, "posix", fake)
	p.Config = old.Config
	fake.Reset()
	for i := 0; i < 2; i++ {
		got, err := p.Ensure(ctx, app)
		if err != nil || got != url {
			t.Fatalf("Ensure %d = %q, %v; want %q", i, got, err, url)
		}
	}
	assertContainerUntouched(t, fake)
	for sub, want := range map[string]int{
		"docker inspect":                2 + 1, // one per Ensure, one in waitReady
		"pg_isready":                    1,
		"psql -U postgres -d postgres ": 2,
		"psql -U postgres -d app ":      1,
		"stanza-create":                 1,
	} {
		if got := fake.Count(sub); got != want {
			t.Errorf("%q ran %d times, want %d: %v", sub, got, want, fake.Calls())
		}
	}

	// Another app on the same value is tracked separately.
	if p.isSettled("other") || !p.isSettled("shop") {
		t.Error("settled state is not per app")
	}
}

// Regression: on Postgres 14 the public schema belongs to the superuser, so
// the app's ordinary role did not own the schema of its own database.
func TestPublicSchemaGoesToTheAppRoleBeforePostgres15(t *testing.T) {
	app := testApp(t, "postgres: { version: 17 }\n")
	st := &dockerState{}
	fake := &execxtest.Fake{}
	rec := &capture{}
	fake.Handler = rec.wrap(st.handler(nil))
	if _, err := newPostgres(t, "", fake).Ensure(ctx, app); err != nil {
		t.Fatal(err)
	}
	roles, schema := -1, -1
	for i, in := range rec.stdin {
		switch {
		case strings.Contains(in, "ALTER DATABASE app OWNER TO app;"):
			roles = i
		case strings.Contains(in, "ALTER SCHEMA public OWNER TO app;"):
			schema = i
		}
	}
	if roles < 0 || schema < roles {
		t.Fatalf("the schema must change owner after the role script: roles=%d schema=%d", roles, schema)
	}
	if line := rec.cmds[schema].String(); !strings.Contains(line, "psql -U postgres -d app ") {
		t.Errorf("the schema is altered as the superuser inside the app's database: %s", line)
	}
	if in := rec.stdin[schema]; !strings.Contains(in, "IF current_setting('server_version_num')::int < 150000 THEN ALTER SCHEMA") {
		t.Errorf("the change must be limited to servers older than 15:\n%s", in)
	}

	// A failure there is reported.
	failing := &execxtest.Fake{}
	fst := &dockerState{}
	failing.Handler = fst.handler(func(c execx.Cmd) (string, error, bool) {
		if strings.Contains(c.String(), "psql -U postgres -d app ") {
			return "", errors.New("injected"), true
		}
		return "", nil, false
	})
	if _, err := newPostgres(t, "", failing).Ensure(ctx, app); err == nil {
		t.Error("a failing schema change was not reported")
	}
}

func FuzzRestoreOptions(f *testing.F) {
	f.Add("2026-01-02 15:04:05+00", "drill")
	f.Add("2026-01-02T15:04:05Z", "")
	f.Add("", "a")
	f.Add("2026-01-02 15:04:05' --pg1-path=/", "../x")
	f.Add("2026-01-02 15:04:05\n--delta", "-rm")
	nameRe := regexp.MustCompile(`^[a-z0-9-]+$`)
	fixed := map[string]bool{
		"--stanza=shop": true, "--type=time": true, "--target-action=promote": true,
		"--archive-mode=off": true, "--delta": true, "restore": true,
	}
	f.Fuzz(func(t *testing.T, at, into string) {
		opts := RestoreOptions{Time: at, Into: into}
		if opts.Validate() != nil {
			return
		}
		for _, arg := range opts.backrestArgs("shop") {
			if fixed[arg] {
				continue
			}
			// The only argument built from input is the recovery target.
			value, ok := strings.CutPrefix(arg, "--target=")
			if !ok || value != at || !targetTimeRe.MatchString(value) {
				t.Fatalf("unexpected pgbackrest argument %q from %+v", arg, opts)
			}
			if strings.ContainsAny(value, "\n\r\t'\"\\$`;") || strings.HasPrefix(value, "-") || strings.HasPrefix(value, " ") {
				t.Fatalf("recovery target %q is not a plain timestamp", value)
			}
		}
		if into == "" {
			return
		}
		if strings.HasPrefix(into, "-") || strings.ContainsAny(into, "/.") {
			t.Fatalf("drill name %q could act as a flag or a path", into)
		}
		if name := DrillContainer("shop", into); !nameRe.MatchString(name) {
			t.Fatalf("drill container name %q", name)
		}
	})
}

func TestPostgresCreateRolesIsAppliedBothWays(t *testing.T) {
	st := &dockerState{}
	fake := &execxtest.Fake{}
	rec := &capture{}
	fake.Handler = rec.wrap(st.handler(nil))
	p := newPostgres(t, "", fake)

	plain := testApp(t, "postgres: { version: 18 }\n")
	if _, err := p.Ensure(ctx, plain); err != nil {
		t.Fatal(err)
	}
	if sql := rec.sql(); !strings.Contains(sql, "ALTER ROLE app NOCREATEROLE;") || strings.Contains(sql, "ALTER ROLE app CREATEROLE;") {
		t.Fatalf("an app that did not ask for it must not get CREATEROLE:\n%s", sql)
	}
	if strings.Contains(rec.sql(), "ALTER ROLE app SUPERUSER") {
		t.Fatal("the app's role was made a superuser")
	}

	// Turning the setting on recreates the container, so the grant is applied.
	fake.Reset()
	rec = &capture{}
	fake.Handler = rec.wrap(st.handler(nil))
	granted := testApp(t, "postgres: { version: 18, create_roles: true }\n")
	if _, err := p.Ensure(ctx, granted); err != nil {
		t.Fatal(err)
	}
	if fake.Find("docker run -d") == "" {
		t.Fatalf("changing create_roles did not recreate the container: %v", fake.Calls())
	}
	if sql := rec.sql(); !strings.Contains(sql, "ALTER ROLE app CREATEROLE;") || strings.Contains(sql, "SUPERUSER LOGIN PASSWORD") {
		t.Fatalf("create_roles did not grant CREATEROLE:\n%s", sql)
	}

	// Turning it off again takes the privilege away.
	fake.Reset()
	rec = &capture{}
	fake.Handler = rec.wrap(st.handler(nil))
	if _, err := p.Ensure(ctx, plain); err != nil {
		t.Fatal(err)
	}
	if sql := rec.sql(); !strings.Contains(sql, "ALTER ROLE app NOCREATEROLE;") {
		t.Fatalf("removing create_roles did not revoke CREATEROLE:\n%s", sql)
	}
}
