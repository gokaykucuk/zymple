package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gokaykucuk/zymple/internal/auth"
	"github.com/gokaykucuk/zymple/internal/config"
	"github.com/gokaykucuk/zymple/internal/deploy"
	"github.com/gokaykucuk/zymple/internal/execx"
	"github.com/gokaykucuk/zymple/internal/execx/execxtest"
	"github.com/gokaykucuk/zymple/internal/proxy"
	"github.com/gokaykucuk/zymple/internal/releases"
	"github.com/gokaykucuk/zymple/internal/runtime"
	"github.com/gokaykucuk/zymple/internal/services"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	token   = "zym_shop_token"
)

// dockerSim is a tiny stateful stand-in for the docker CLI.
type dockerSim struct {
	mu       sync.Mutex
	webs     map[string]map[int]bool // app -> release -> running
	missing  map[string]bool         // digests that do not exist
	failOn   string
	inspect  string // answer to `docker inspect` for service containers
	infoJSON string
}

func newSim() *dockerSim {
	return &dockerSim{webs: map[string]map[int]bool{}, missing: map[string]bool{}, infoJSON: "[]"}
}

func (d *dockerSim) handle(c execx.Cmd) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	line := c.String()
	if d.failOn != "" && strings.Contains(line, d.failOn) {
		return "", errors.New("injected failure")
	}
	switch {
	case strings.Contains(line, "image inspect"):
		for digest := range d.missing {
			if strings.Contains(line, digest) {
				return "", errors.New("Error response from daemon: No such image")
			}
		}
	case strings.HasPrefix(line, "docker inspect"):
		if d.inspect == "" {
			return "", errors.New("no such container")
		}
		return d.inspect + "\n", nil
	case strings.Contains(line, "up -d --no-deps") && strings.Contains(line, "web-r"):
		app, release := d.parseWeb(c.Args)
		if d.webs[app] == nil {
			d.webs[app] = map[int]bool{}
		}
		d.webs[app][release] = true
	case strings.HasPrefix(line, "docker ps") && strings.Contains(line, "role=web"):
		app := between(line, "label=zymple.app=", " ")
		var rows []string
		for release, running := range d.webs[app] {
			state := "exited"
			if running {
				state = "running"
			}
			rows = append(rows, fmt.Sprintf("%s-%d %s %d web-r%d", app, release, state, release, release))
		}
		sort.Strings(rows)
		return strings.Join(rows, "\n") + "\n", nil
	case strings.HasPrefix(line, "docker rm -f") || strings.HasPrefix(line, "docker stop"):
		for _, id := range c.Args {
			app, rel, ok := strings.Cut(id, "-")
			var n int
			if _, err := fmt.Sscanf(rel, "%d", &n); ok && err == nil {
				if strings.HasPrefix(line, "docker rm") {
					delete(d.webs[app], n)
				} else {
					d.webs[app][n] = false
				}
			}
		}
	case strings.HasPrefix(line, "docker logs"):
		return "log line 1\nlog line 2\n", nil
	case strings.Contains(line, " run --rm --no-deps -T"):
		return "command output\n", nil
	case strings.Contains(line, "--output=json info"):
		return d.infoJSON, nil
	}
	return "", nil
}

func (d *dockerSim) parseWeb(args []string) (string, int) {
	var app string
	var release int
	for i, a := range args {
		if a == "-p" {
			app = strings.TrimPrefix(args[i+1], "zymple-")
		}
		if i := strings.LastIndex(a, "-r"); i >= 0 && strings.HasPrefix(a, "web-r") {
			fmt.Sscanf(a[i+2:], "%d", &release)
		}
	}
	return app, release
}

func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	rest := s[i+len(start):]
	if j := strings.Index(rest, end); j >= 0 {
		return rest[:j]
	}
	return rest
}

type okHealth struct{ err error }

func (h okHealth) Wait(context.Context, int, string, time.Duration) error { return h.err }

type fixture struct {
	*Server
	sim     *dockerSim
	fake    *execxtest.Fake
	network http.Handler
	admin   http.Handler
	appsDir string
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeApp(t *testing.T, appsDir, name, extra string) {
	t.Helper()
	dir := filepath.Join(appsDir, name)
	writeFile(t, filepath.Join(dir, "app.yml"),
		fmt.Sprintf("name: %s\ndomains: [%s.example.com]\nweb: { service: web, port: 3000, drain: 0s }\n%s", name, name, extra))
	writeFile(t, filepath.Join(dir, "compose.yml"), "services:\n  web:\n    image: ${IMAGE}\n  worker:\n    image: ${IMAGE}\n")
	line, err := auth.HashLine("ci", "zym_"+name+"_token")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "tokens"), line+"\n")
}

func newFixture(t *testing.T, backup bool) *fixture {
	t.Helper()
	root := t.TempDir()
	doc := fmt.Sprintf("apps_dir: %s\nstate_dir: %s\ncaddy: { snippets_dir: %s }\n",
		filepath.Join(root, "apps"), filepath.Join(root, "state"), filepath.Join(root, "caddy"))
	if backup {
		doc += "backup: { type: posix, path: " + filepath.Join(root, "backups") + ", cipher_pass: Secret }\n"
	}
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	sim := newSim()
	fake := &execxtest.Fake{Handler: sim.handle}
	s := &Server{
		Config:   cfg,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:    &releases.Store{Root: cfg.StateDir},
		Docker:   &runtime.Docker{Runner: fake, StateDir: cfg.StateDir},
		Proxy:    &proxy.Caddy{Dir: cfg.Caddy.SnippetsDir, Reload: cfg.Caddy.Reload, TLS: cfg.Caddy.TLS, Runner: fake},
		Postgres: &services.Postgres{Runner: fake, Config: cfg, Sleep: func(context.Context, time.Duration) {}, Now: time.Now},
		Redis:    &services.Redis{Runner: fake, Config: cfg},
		Registry: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "registry") }),
	}
	s.Engine = &deploy.Engine{
		Store: s.Store, Runtime: s.Docker, Proxy: s.Proxy, Health: okHealth{},
		Ports: &deploy.PortRange{Low: 41000, High: 41999}, Env: s.ServiceEnv,
		Sleep: func(context.Context, time.Duration) {}, Now: time.Now,
	}
	writeApp(t, cfg.AppsDir, "shop", "")
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &fixture{Server: s, sim: sim, fake: fake, network: s.NetworkHandler(), admin: s.AdminHandler(), appsDir: cfg.AppsDir}
}

// call performs a request and returns the status, body and decoded events.
func call(h http.Handler, method, path, bearer, body string) (int, string, []deploy.Event) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var events []deploy.Event
	if strings.Contains(rec.Header().Get("Content-Type"), "ndjson") {
		sc := bufio.NewScanner(bytes.NewReader(rec.Body.Bytes()))
		for sc.Scan() {
			var e deploy.Event
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				events = append(events, e)
			}
		}
	}
	return rec.Code, rec.Body.String(), events
}

func outcome(events []deploy.Event) deploy.Event {
	if len(events) == 0 {
		return deploy.Event{}
	}
	return events[len(events)-1]
}

func (f *fixture) deploy(t *testing.T, digest string) {
	t.Helper()
	code, body, events := call(f.network, "POST", "/api/v1/apps/shop/deploys", token, `{"digest":"`+digest+`"}`)
	if code != 200 || outcome(events).Status != "ok" {
		t.Fatalf("deploy: %d %s", code, body)
	}
	f.ops.Wait()
}

func TestNetworkAuthentication(t *testing.T) {
	f := newFixture(t, false)
	writeApp(t, f.appsDir, "other", "")
	if err := f.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, method, path, bearer string
		want                       int
	}{
		{"no token", "GET", "/api/v1/apps/shop/status", "", 401},
		{"wrong token", "GET", "/api/v1/apps/shop/status", "zym_nope", 401},
		{"another app's token", "GET", "/api/v1/apps/shop/status", "zym_other_token", 401},
		{"unknown app", "GET", "/api/v1/apps/ghost/status", token, 401},
		{"invalid app name", "GET", "/api/v1/apps/Sh*p/status", token, 401},
		{"own token", "GET", "/api/v1/apps/shop/status", token, 200},
		{"own token on releases", "GET", "/api/v1/apps/shop/releases", token, 200},
		{"deploy with another app's token", "POST", "/api/v1/apps/shop/deploys", "zym_other_token", 401},
		{"operator route", "GET", "/admin/apps", token, 404},
		{"operator reload", "POST", "/admin/reload", token, 404},
		{"operator run", "POST", "/admin/apps/shop/run", token, 404},
		{"operator restore", "POST", "/admin/apps/shop/pg/restore", token, 404},
		{"unknown route", "GET", "/api/v1/apps", token, 404},
		{"wrong method", "DELETE", "/api/v1/apps/shop/status", token, 405},
	}
	for _, c := range cases {
		if code, body, _ := call(f.network, c.method, c.path, c.bearer, ""); code != c.want {
			t.Errorf("%s: %s %s = %d, want %d (%s)", c.name, c.method, c.path, code, c.want, body)
		}
	}
	// A basic-auth header is not a bearer token.
	req := httptest.NewRequest("GET", "/api/v1/apps/shop/status", nil)
	req.SetBasicAuth("shop", token)
	rec := httptest.NewRecorder()
	f.network.ServeHTTP(rec, req)
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("basic auth on the API: %d", rec.Code)
	}
}

func TestRegistryIsGuarded(t *testing.T) {
	f := newFixture(t, false)
	for _, path := range []string{"/v2/", "/v2"} {
		if code, _, _ := call(f.network, "GET", path, "", ""); code != 401 {
			t.Errorf("GET %s without credentials = %d", path, code)
		}
	}
	req := httptest.NewRequest("GET", "/v2/", nil)
	req.SetBasicAuth("shop", token)
	rec := httptest.NewRecorder()
	f.network.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "registry" {
		t.Errorf("authenticated registry request: %d %q", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest("POST", "/v2/zymple/other/blobs/uploads/", nil)
	req.SetBasicAuth("shop", token)
	rec = httptest.NewRecorder()
	f.network.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("push into another repository: %d", rec.Code)
	}
}

func TestDeployStatusReleasesRollback(t *testing.T) {
	f := newFixture(t, false)
	code, body, _ := call(f.network, "GET", "/api/v1/apps/shop/releases", token, "")
	if code != 200 || !strings.Contains(body, `"releases":[]`) {
		t.Fatalf("empty releases: %d %s", code, body)
	}
	if _, body, _ := call(f.network, "GET", "/api/v1/apps/shop/status", token, ""); !strings.Contains(body, `"current":null`) {
		t.Fatalf("status before any deploy: %s", body)
	}

	code, _, events := call(f.network, "POST", "/api/v1/apps/shop/deploys", token, `{"digest":"`+digestA+`"}`)
	if code != 200 || outcome(events).Step != "done" || outcome(events).Status != "ok" || outcome(events).Release != 1 {
		t.Fatalf("deploy: %d %+v", code, events)
	}
	f.ops.Wait()
	f.deploy(t, digestB)

	var status Status
	_, body, _ = call(f.network, "GET", "/api/v1/apps/shop/status", token, "")
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatal(err)
	}
	if status.Current == nil || status.Current.Number != 2 || status.Current.By != "ci" || !status.WebRunning || status.Deploying {
		t.Fatalf("status = %+v", status)
	}

	code, _, events = call(f.network, "POST", "/api/v1/apps/shop/rollbacks", token, `{}`)
	if code != 200 || outcome(events).Status != "ok" || outcome(events).Release != 3 {
		t.Fatalf("rollback: %d %+v", code, events)
	}
	f.ops.Wait()
	_, body, _ = call(f.network, "GET", "/api/v1/apps/shop/releases", token, "")
	if !strings.Contains(body, `"current":3`) || !strings.Contains(body, `"rolled_back_from":2`) {
		t.Errorf("releases after rollback: %s", body)
	}
	_, _, events = call(f.network, "POST", "/api/v1/apps/shop/rollbacks", token, `{"release":99}`)
	if outcome(events).Status != "fail" || !strings.Contains(outcome(events).Message, "no such release") {
		t.Errorf("rollback to a missing release: %+v", events)
	}
	f.ops.Wait()

	// The operator socket can do the same without a token, recorded as admin.
	code, _, events = call(f.admin, "POST", "/admin/apps/shop/redeploy", "", "")
	if code != 200 || outcome(events).Status != "ok" {
		t.Fatalf("redeploy: %d %+v", code, events)
	}
	f.ops.Wait()
	if _, body, _ := call(f.admin, "GET", "/api/v1/apps/shop/status", "", ""); !strings.Contains(body, `"by":"admin"`) {
		t.Errorf("admin redeploy not recorded as admin: %s", body)
	}
	if code, body, _ := call(f.admin, "GET", "/admin/apps", "", ""); code != 200 || !strings.Contains(body, `"name":"shop"`) || !strings.Contains(body, `"current":4`) {
		t.Errorf("apps: %d %s", code, body)
	}
}

func TestDeployRejectsBadRequests(t *testing.T) {
	f := newFixture(t, false)
	for name, body := range map[string]string{
		"no digest":     `{}`,
		"short digest":  `{"digest":"sha256:abc"}`,
		"tag":           `{"digest":"latest"}`,
		"not json":      `{"digest":`,
		"unknown field": `{"digest":"` + digestA + `","force":true}`,
		"huge body":     `{"digest":"` + strings.Repeat("a", maxBody) + `"}`,
	} {
		if code, _, _ := call(f.network, "POST", "/api/v1/apps/shop/deploys", token, body); code != 400 {
			t.Errorf("%s: got %d, want 400", name, code)
		}
	}
	if code, _, _ := call(f.network, "POST", "/api/v1/apps/shop/rollbacks", token, `{"release":-1}`); code != 400 {
		t.Errorf("negative release: %d", code)
	}
	if code, _, _ := call(f.network, "POST", "/api/v1/apps/shop/rollbacks", token, `nope`); code != 400 {
		t.Errorf("bad rollback body: %d", code)
	}
	if f.fake.Count("up -d") != 0 {
		t.Error("a rejected request reached docker")
	}
}

func TestDeployOfUnknownDigestFailsInTheStream(t *testing.T) {
	f := newFixture(t, false)
	f.sim.missing[digestA] = true
	code, _, events := call(f.network, "POST", "/api/v1/apps/shop/deploys", token, `{"digest":"`+digestA+`"}`)
	last := outcome(events)
	if code != 200 || last.Step != "done" || last.Status != "fail" || !strings.Contains(last.Message, "no image with that digest") {
		t.Fatalf("unknown digest: %d %+v", code, events)
	}
	f.ops.Wait()
	if _, body, _ := call(f.network, "GET", "/api/v1/apps/shop/status", token, ""); !strings.Contains(body, "last_error") {
		t.Errorf("status does not carry the failure: %s", body)
	}
}

func TestBusyAppAnswersConflict(t *testing.T) {
	f := newFixture(t, false)
	unlock, err := f.Engine.Lock("shop")
	if err != nil {
		t.Fatal(err)
	}
	if code, body, _ := call(f.network, "POST", "/api/v1/apps/shop/deploys", token, `{"digest":"`+digestA+`"}`); code != 409 || !strings.Contains(body, "already in progress") {
		t.Errorf("deploy while busy: %d %s", code, body)
	}
	if _, body, _ := call(f.network, "GET", "/api/v1/apps/shop/status", token, ""); !strings.Contains(body, `"deploying":true`) {
		t.Errorf("status while busy: %s", body)
	}
	unlock()
}

func TestLogs(t *testing.T) {
	f := newFixture(t, false)
	f.deploy(t, digestA)
	code, _, events := call(f.network, "GET", "/api/v1/apps/shop/logs?tail=5", token, "")
	if code != 200 || len(events) != 3 || events[0].Message != "log line 1" || outcome(events).Status != "ok" {
		t.Fatalf("logs: %d %+v", code, events)
	}
	if f.fake.Find("docker logs --tail 5 shop-1") == "" {
		t.Errorf("logs command: %v", f.fake.Calls())
	}
	for name, c := range map[string]struct {
		query string
		want  int
	}{
		"negative tail":   {"?tail=-1", 400},
		"huge tail":       {"?tail=999999", 400},
		"non-numeric":     {"?tail=all", 400},
		"unknown service": {"?service=mailer", 404},
	} {
		if code, _, _ := call(f.network, "GET", "/api/v1/apps/shop/logs"+c.query, token, ""); code != c.want {
			t.Errorf("%s: got %d, want %d", name, code, c.want)
		}
	}
	// A declared service with no running container fails in the stream.
	_, _, events = call(f.network, "GET", "/api/v1/apps/shop/logs?service=worker&follow=true", token, "")
	if outcome(events).Status != "fail" {
		t.Errorf("logs of a stopped service: %+v", events)
	}
	// Naming the web service is the same as naming none.
	if _, _, events := call(f.network, "GET", "/api/v1/apps/shop/logs?service=web", token, ""); outcome(events).Status != "ok" {
		t.Errorf("logs of the web service: %+v", events)
	}
}

func TestOperatorRun(t *testing.T) {
	f := newFixture(t, false)
	if code, _, _ := call(f.admin, "POST", "/admin/apps/shop/run", "", `{"command":["env"]}`); code != 409 {
		t.Errorf("run before any release: %d", code)
	}
	f.deploy(t, digestA)
	if code, _, _ := call(f.admin, "POST", "/admin/apps/shop/run", "", `{"command":[]}`); code != 400 {
		t.Errorf("empty command: %d", code)
	}
	if code, _, _ := call(f.admin, "POST", "/admin/apps/shop/run", "", `{`); code != 400 {
		t.Errorf("bad body: %d", code)
	}
	if code, _, _ := call(f.admin, "POST", "/admin/apps/ghost/run", "", `{"command":["env"]}`); code != 404 {
		t.Errorf("unknown app: %d", code)
	}
	_, _, events := call(f.admin, "POST", "/admin/apps/shop/run", "", `{"command":["bin/console","--help"]}`)
	if len(events) != 2 || events[0].Message != "command output" || outcome(events).Status != "ok" {
		t.Fatalf("run: %+v", events)
	}
	if got := f.fake.Find("run --rm --no-deps -T web-r1"); !strings.HasSuffix(got, "web-r1 bin/console --help") {
		t.Errorf("run command: %s", got)
	}
}

func TestPostgresRoutes(t *testing.T) {
	f := newFixture(t, true)
	// The plain app has no Postgres.
	for _, r := range [][2]string{
		{"GET", "/admin/apps/shop/pg/info"}, {"POST", "/admin/apps/shop/pg/backup"},
		{"POST", "/admin/apps/shop/pg/restore"}, {"DELETE", "/admin/apps/shop/pg/drills/x"},
	} {
		if code, _, _ := call(f.admin, r[0], r[1], "", `{}`); code != 409 {
			t.Errorf("%s %s without Postgres = %d, want 409", r[0], r[1], code)
		}
	}

	writeApp(t, f.appsDir, "ledger", "postgres: { version: 17 }\n")
	f.sim.inspect = "running stale"
	if err := f.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	post := func(path, body string) deploy.Event {
		t.Helper()
		code, raw, events := call(f.admin, "POST", path, "", body)
		if code != 200 {
			t.Fatalf("POST %s: %d %s", path, code, raw)
		}
		f.ops.Wait()
		return outcome(events)
	}
	// Deploy it so there are containers to stop and start around a restore.
	if code, _, events := call(f.admin, "POST", "/api/v1/apps/ledger/deploys", "", `{"digest":"`+digestA+`"}`); code != 200 || outcome(events).Status != "ok" {
		t.Fatalf("deploy ledger: %d %+v", code, events)
	}
	f.ops.Wait()
	if f.fake.Find("--name zymple-ledger-postgres") == "" {
		t.Fatal("the deploy did not start Postgres")
	}

	if got := post("/admin/apps/ledger/pg/backup", `{}`); got.Status != "ok" || f.fake.Find("--type=diff backup") == "" {
		t.Errorf("default backup: %+v", got)
	}
	if got := post("/admin/apps/ledger/pg/backup", `{"type":"full"}`); got.Status != "ok" || f.fake.Find("--type=full backup") == "" {
		t.Errorf("full backup: %+v", got)
	}
	if got := post("/admin/apps/ledger/pg/backup", `{"type":"everything"}`); got.Status != "fail" {
		t.Errorf("bad backup type: %+v", got)
	}
	if code, _, events := call(f.admin, "GET", "/admin/apps/ledger/pg/info", "", ""); code != 200 || outcome(events).Status != "ok" {
		t.Errorf("info: %d %+v", code, events)
	}
	for _, body := range []string{`{"time":"soon"}`, `{"into":"../x"}`, `{`} {
		if code, _, _ := call(f.admin, "POST", "/admin/apps/ledger/pg/restore", "", body); code != 400 {
			t.Errorf("restore %s = %d, want 400", body, code)
		}
	}

	f.fake.Reset()
	if got := post("/admin/apps/ledger/pg/restore", `{"time":"2026-01-02 15:04:05+00"}`); got.Status != "ok" {
		t.Fatalf("in-place restore: %+v", got)
	}
	calls := strings.Join(f.fake.Calls(), "\n")
	stopApp, restore, startApp := strings.Index(calls, "docker stop ledger-1"), strings.Index(calls, "--delta restore"), strings.Index(calls, "docker start ledger-1")
	if stopApp < 0 || !(stopApp < restore && restore < startApp) {
		t.Errorf("the app must be stopped before and started after the restore:\n%s", calls)
	}
	if f.Engine.Busy("ledger") {
		t.Error("the restore did not release the deploy lock")
	}

	// A restore refuses to run beside a deploy.
	unlock, _ := f.Engine.Lock("ledger")
	if got := post("/admin/apps/ledger/pg/restore", `{}`); got.Status != "fail" || !strings.Contains(got.Message, "in progress") {
		t.Errorf("restore during a deploy: %+v", got)
	}
	unlock()

	// A failed restore still starts the app again.
	f.fake.Reset()
	f.sim.failOn = "--delta restore"
	if got := post("/admin/apps/ledger/pg/restore", `{}`); got.Status != "fail" {
		t.Errorf("failed restore: %+v", got)
	}
	if f.fake.Find("docker start ledger-1") == "" {
		t.Error("the app was not started again after a failed restore")
	}
	f.sim.failOn = "docker stop ledger-1"
	if got := post("/admin/apps/ledger/pg/restore", `{}`); got.Status != "fail" {
		t.Errorf("restore when the app cannot be stopped: %+v", got)
	}
	f.sim.failOn = "docker start ledger-1"
	if got := post("/admin/apps/ledger/pg/restore", `{}`); got.Status != "fail" {
		t.Errorf("restore when the app cannot be started: %+v", got)
	}
	f.sim.failOn = ""

	f.fake.Reset()
	if got := post("/admin/apps/ledger/pg/restore", `{"into":"drill"}`); got.Status != "ok" {
		t.Fatalf("drill restore: %+v", got)
	}
	if f.fake.Count("docker stop ledger-1") != 0 {
		t.Error("a drill stopped the live app")
	}
	if code, _, _ := call(f.admin, "DELETE", "/admin/apps/ledger/pg/drills/drill", "", ""); code != 200 {
		t.Errorf("drill removal: %d", code)
	}
	if code, _, _ := call(f.admin, "DELETE", "/admin/apps/ledger/pg/drills/BAD", "", ""); code != 400 {
		t.Errorf("bad drill name: %d", code)
	}
}

func TestRedisRoutes(t *testing.T) {
	f := newFixture(t, true)
	_, _, events := call(f.admin, "POST", "/admin/apps/shop/redis/backup", "", "")
	if outcome(events).Status != "fail" {
		t.Errorf("backup of an app without Redis: %+v", events)
	}
	f.ops.Wait()
	writeApp(t, f.appsDir, "cache", "redis: { persistent: true }\n")
	if err := f.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"backup", "restore"} {
		_, _, events := call(f.admin, "POST", "/admin/apps/cache/redis/"+action, "", "")
		if outcome(events).Status != "ok" {
			t.Errorf("redis %s: %+v", action, events)
		}
		f.ops.Wait()
	}
}

func TestServiceEnv(t *testing.T) {
	f := newFixture(t, false)
	writeApp(t, f.appsDir, "full", "postgres: { version: 17 }\nredis: { persistent: false }\n")
	if _, err := f.load(); err != nil {
		t.Fatal(err)
	}
	app, _ := f.App("full")
	env, err := f.ServiceEnv(context.Background(), app)
	if err != nil || !strings.HasPrefix(env["DATABASE_URL"], "postgres://app:") || env["REDIS_URL"] != services.RedisURL {
		t.Fatalf("env = %v, %v", env, err)
	}
	f.sim.failOn = "zymple-full-redis"
	if _, err := f.ServiceEnv(context.Background(), app); err == nil || !strings.Contains(err.Error(), "redis") {
		t.Errorf("redis failure: %v", err)
	}
	f.sim.failOn = "zymple-full-postgres"
	if _, err := f.ServiceEnv(context.Background(), app); err == nil || !strings.Contains(err.Error(), "postgres") {
		t.Errorf("postgres failure: %v", err)
	}
}

func TestReload(t *testing.T) {
	f := newFixture(t, false)
	f.deploy(t, digestA)
	ctx := context.Background()

	// A broken definition is reported and does not disturb the others.
	writeFile(t, filepath.Join(f.appsDir, "broken", "app.yml"), "name: broken\n")
	code, _, events := call(f.admin, "POST", "/admin/reload", "", "")
	if code != 200 || outcome(events).Status != "fail" || !strings.Contains(outcome(events).Message, "broken") {
		t.Fatalf("reload with a broken app: %d %+v", code, events)
	}
	f.ops.Wait()
	if _, ok := f.App("shop"); !ok {
		t.Fatal("a broken neighbour unloaded a healthy app")
	}
	if err := os.RemoveAll(filepath.Join(f.appsDir, "broken")); err != nil {
		t.Fatal(err)
	}

	// A bad tokens file is reported, and the app then accepts no token.
	writeFile(t, filepath.Join(f.appsDir, "shop", "tokens"), "garbage\n")
	if err := f.Reload(ctx); err == nil || !strings.Contains(err.Error(), "tokens") {
		t.Fatalf("bad tokens file: %v", err)
	}
	if code, _, _ := call(f.network, "GET", "/api/v1/apps/shop/status", token, ""); code != 401 {
		t.Errorf("a bad tokens file must not leave old tokens valid: %d", code)
	}

	// Removing a definition stops the app and removes its proxy snippet.
	snippet := filepath.Join(f.Config.Caddy.SnippetsDir, "shop.caddy")
	if _, err := os.Stat(snippet); err != nil {
		t.Fatalf("snippet missing before removal: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(f.appsDir, "shop")); err != nil {
		t.Fatal(err)
	}
	f.fake.Reset()
	if err := f.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(snippet); !errors.Is(err, os.ErrNotExist) {
		t.Error("the removed app's snippet is still there")
	}
	if f.fake.Find("docker stop shop-1") == "" {
		t.Errorf("the removed app's containers were not stopped: %v", f.fake.Calls())
	}
	if len(f.AppNames()) != 0 {
		t.Errorf("apps = %v", f.AppNames())
	}

	// Failures while removing are reported.
	writeApp(t, f.appsDir, "temp", "")
	if err := f.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(f.appsDir, "temp")); err != nil {
		t.Fatal(err)
	}
	f.sim.failOn = "docker ps"
	if err := f.Reload(ctx); err == nil {
		t.Error("a failure while stopping a removed app was not reported")
	}
	f.sim.failOn = ""

	// A missing apps directory is an error, not an empty app list.
	f.Config.AppsDir = filepath.Join(f.appsDir, "missing")
	if err := f.Reload(ctx); err == nil {
		t.Error("a missing apps directory was accepted")
	}
}

// Regression: a definition that failed validation used to drop the app from
// the loaded set, and reload then treated it as removed: it stopped the
// containers and deleted the proxy snippet of a running app over a typo.
func TestReloadKeepsARunningAppWhoseDefinitionBroke(t *testing.T) {
	f := newFixture(t, false)
	f.deploy(t, digestA)
	snippet := filepath.Join(f.Config.Caddy.SnippetsDir, "shop.caddy")
	before, err := os.ReadFile(snippet)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.appsDir, "shop", "app.yml"), "name: shop\nbogus_key: 1\n")
	f.fake.Reset()
	err = f.Reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bogus_key") {
		t.Fatalf("the broken definition was not reported: %v", err)
	}
	app, ok := f.App("shop")
	if !ok || len(app.Domains) != 1 || app.Domains[0] != "shop.example.com" {
		t.Fatalf("the running app lost its previous definition: %+v, %v", app, ok)
	}
	if call := f.fake.Find("docker stop"); call != "" {
		t.Errorf("the running app was stopped: %s", call)
	}
	if call := f.fake.Find("docker rm"); call != "" {
		t.Errorf("a container of the running app was removed: %s", call)
	}
	if after, err := os.ReadFile(snippet); err != nil || string(after) != string(before) {
		t.Errorf("the proxy snippet changed: %q, %v", after, err)
	}
	if code, _, _ := call(f.network, "GET", "/api/v1/apps/shop/status", token, ""); code != 200 {
		t.Errorf("the app's token stopped working: %d", code)
	}
	// Tokens are still re-read, so a revoked token is revoked at once.
	writeFile(t, filepath.Join(f.appsDir, "shop", "tokens"), "")
	_ = f.Reload(context.Background())
	if code, _, _ := call(f.network, "GET", "/api/v1/apps/shop/status", token, ""); code != 401 {
		t.Errorf("a removed token still works while the definition is broken: %d", code)
	}
	// Fixing the definition loads it, and removing the directory is still
	// what stops an app.
	writeApp(t, f.appsDir, "shop", "keep_releases: 7\n")
	if err := f.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if app, _ := f.App("shop"); app.KeepReleases != 7 {
		t.Errorf("the fixed definition was not loaded: %+v", app)
	}
	if err := os.RemoveAll(filepath.Join(f.appsDir, "shop")); err != nil {
		t.Fatal(err)
	}
	f.fake.Reset()
	if err := f.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.App("shop"); ok || f.fake.Find("docker stop shop-1") == "" {
		t.Errorf("a removed app was not stopped: %v", f.fake.Calls())
	}
}

func writeAppWithDomain(t *testing.T, appsDir, name, domain string) {
	t.Helper()
	writeApp(t, appsDir, name, "")
	writeFile(t, filepath.Join(appsDir, name, "app.yml"),
		fmt.Sprintf("name: %s\ndomains: [%s]\nweb: { service: web, port: 3000, drain: 0s }\n", name, domain))
}

// Regression: definitions load in name order and the first to claim a domain
// wins, so a new app that sorts earlier used to unload, and thereby stop,
// the running app that already served the domain.
func TestReloadDoesNotLetANewAppTakeARunningAppsDomain(t *testing.T) {
	f := newFixture(t, false)
	f.deploy(t, digestA)
	ctx := context.Background()
	snippet := filepath.Join(f.Config.Caddy.SnippetsDir, "shop.caddy")

	writeAppWithDomain(t, f.appsDir, "aaa", "shop.example.com")
	f.fake.Reset()
	err := f.Reload(ctx)
	if err == nil || !strings.Contains(err.Error(), "app aaa") || !strings.Contains(err.Error(), "not loaded") {
		t.Fatalf("the conflict was not reported: %v", err)
	}
	if _, ok := f.App("aaa"); ok {
		t.Error("the newcomer was loaded with a domain that is in use")
	}
	if _, ok := f.App("shop"); !ok || f.fake.Find("docker stop") != "" {
		t.Errorf("the running app was disturbed: %v", f.fake.Calls())
	}
	if _, err := os.Stat(snippet); err != nil {
		t.Errorf("the running app's snippet is gone: %v", err)
	}
	if code, _, _ := call(f.network, "GET", "/api/v1/apps/aaa/status", "zym_aaa_token", ""); code != 401 {
		t.Errorf("the rejected newcomer accepts its token: %d", code)
	}

	// The same when the app claiming the domain is itself already loaded:
	// it goes back to its own previous definition.
	writeAppWithDomain(t, f.appsDir, "aaa", "aaa.example.com")
	if err := f.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	writeAppWithDomain(t, f.appsDir, "aaa", "shop.example.com")
	err = f.Reload(ctx)
	if err == nil || !strings.Contains(err.Error(), "keeping the previous definition") {
		t.Fatalf("the conflict was not reported: %v", err)
	}
	aaa, ok := f.App("aaa")
	if !ok || aaa.Domains[0] != "aaa.example.com" {
		t.Errorf("aaa = %+v, %v; want its previous definition", aaa, ok)
	}
	if shop, ok := f.App("shop"); !ok || shop.Domains[0] != "shop.example.com" {
		t.Errorf("shop = %+v, %v", shop, ok)
	}
}

func TestReloadReportsReconcileFailures(t *testing.T) {
	f := newFixture(t, false)
	f.deploy(t, digestA)
	f.sim.failOn = "docker ps"
	if err := f.Reload(context.Background()); err == nil || !strings.Contains(err.Error(), "reconcile") {
		t.Fatalf("reconcile failure: %v", err)
	}
	f.sim.failOn = ""
	// A busy app is skipped, not reported.
	unlock, _ := f.Engine.Lock("shop")
	defer unlock()
	if err := f.Reload(context.Background()); err != nil {
		t.Errorf("a busy app made reload fail: %v", err)
	}
}

func TestStateErrorsSurface(t *testing.T) {
	f := newFixture(t, false)
	writeFile(t, filepath.Join(f.Store.Dir("shop"), "releases.json"), "{corrupt")
	for _, r := range [][3]string{
		{"GET", "/api/v1/apps/shop/releases", ""}, {"GET", "/api/v1/apps/shop/status", ""},
		{"GET", "/admin/apps", ""}, {"POST", "/admin/apps/shop/run", `{"command":["x"]}`},
	} {
		if code, _, _ := call(f.admin, r[0], r[1], "", r[2]); code != 500 {
			t.Errorf("%s %s with corrupt state = %d, want 500", r[0], r[1], code)
		}
	}
	if err := os.Remove(filepath.Join(f.Store.Dir("shop"), "releases.json")); err != nil {
		t.Fatal(err)
	}
	f.deploy(t, digestA)
	f.sim.failOn = "docker ps"
	if code, _, _ := call(f.admin, "GET", "/api/v1/apps/shop/status", "", ""); code != 500 {
		t.Errorf("status when docker fails = %d, want 500", code)
	}
}

func TestScheduledBackups(t *testing.T) {
	f := newFixture(t, true)
	writeApp(t, f.appsDir, "ledger", "postgres: { version: 17 }\nredis: { persistent: true }\n")
	if _, err := f.load(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	f.runDueBackups(ctx)
	if f.fake.Find("--stanza=ledger --type=full backup") == "" {
		t.Errorf("a cluster with no backup did not get a full one: %v", f.fake.Calls())
	}
	if f.fake.Find("backup /data") == "" {
		t.Error("persistent Redis was not backed up")
	}
	// Recent backups: nothing to do.
	f.fake.Reset()
	f.sim.infoJSON = fmt.Sprintf(`[{"backup":[{"type":"full","timestamp":{"stop":%d}}]}]`, time.Now().Unix())
	f.runDueBackups(ctx)
	if f.fake.Count(" backup") != 0 {
		t.Errorf("backups ran although none was due: %v", f.fake.Calls())
	}
	// Failures are logged, not fatal.
	f.sim.infoJSON = "not json"
	f.runDueBackups(ctx)
	f.sim.infoJSON = "[]"
	f.sim.failOn = "backup"
	if err := os.Remove(filepath.Join(f.Store.Dir("ledger"), "redis-last-backup")); err != nil {
		t.Fatal(err)
	}
	f.runDueBackups(ctx)

	// Without a repository the scheduler does nothing at all.
	plain := newFixture(t, false)
	done := make(chan struct{})
	go func() { plain.schedule(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("schedule did not return without a backup repository")
	}
	// With one, it stops when the daemon does.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	f.sim.failOn = ""
	f.schedule(cancelled)
}

func TestStreamSurvivesAClientThatLeaves(t *testing.T) {
	st := newStream()
	st.add(deploy.Event{Step: "start", Status: "ok"})
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { st.serve(rec, req); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel() // the client goes away while the operation is still running
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return when the client left")
	}
	// The operation keeps writing without blocking or panicking.
	st.add(deploy.Event{Step: "later", Status: "ok"})
	st.close()
	st.close()
	st.add(deploy.Event{Step: "ignored"})
	// The first event was delivered and released; the later one is kept for
	// a reader that never comes back, and nothing is added after close.
	if events, closed, _ := st.from(0); len(events) != 1 || events[0].Step != "later" || !closed {
		t.Errorf("events = %+v, closed = %v", events, closed)
	}
	w := st.writer("x")
	if n, err := w.Write([]byte("after close\n")); n != 12 || err != nil {
		t.Errorf("Write = %d, %v", n, err)
	}
}

func TestStreamReleasesDeliveredEvents(t *testing.T) {
	st := newStream()
	for i := 0; i < 1000; i++ {
		st.add(deploy.Event{Step: "log", Message: fmt.Sprint(i)})
	}
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { st.serve(rec, httptest.NewRequest("GET", "/", nil).WithContext(ctx)); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st.mu.Lock()
		held := len(st.events)
		st.mu.Unlock()
		if held == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d delivered events are still held in memory", held)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Events added after a release are still delivered, in order.
	st.add(deploy.Event{Step: "log", Message: "last"})
	st.close()
	<-done
	cancel()
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 1001 || !strings.Contains(lines[1000], `"last"`) || !strings.Contains(lines[999], `"999"`) {
		t.Fatalf("delivered %d lines, last %q", len(lines), lines[len(lines)-1])
	}
}

func TestLineWriter(t *testing.T) {
	st := newStream()
	w := st.writer("out")
	fmt.Fprint(w, "one\ntw")
	fmt.Fprint(w, "o\nthree")
	w.flush()
	w.flush()
	events, _, _ := st.from(0)
	if len(events) != 3 || events[0].Message != "one" || events[1].Message != "two" || events[2].Message != "three" {
		t.Fatalf("events = %+v", events)
	}
}

// Regression: output with no newline grew the line buffer without limit.
func TestLineWriterBoundsALineWithNoEnd(t *testing.T) {
	st := newStream()
	w := st.writer("out")
	chunk := bytes.Repeat([]byte("x"), 10<<10)
	for i := 0; i < 20; i++ { // 200 KiB with no newline
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
		if len(w.buf) >= maxLine {
			t.Fatalf("buffer holds %d bytes, limit %d", len(w.buf), maxLine)
		}
	}
	fmt.Fprint(w, "\nshort\n")
	w.flush()
	events, _, _ := st.from(0)
	total := 0
	for _, e := range events[:len(events)-1] {
		if len(e.Message) > maxLine {
			t.Fatalf("an event carries %d bytes", len(e.Message))
		}
		total += len(e.Message)
	}
	if total != 200<<10 || events[len(events)-1].Message != "short" {
		t.Errorf("%d events carrying %d bytes, last %q", len(events), total, events[len(events)-1].Message)
	}
	// A long line that does end is cut the same way, and nothing is lost.
	st = newStream()
	w = st.writer("out")
	fmt.Fprint(w, strings.Repeat("y", maxLine+5)+"\n")
	events, _, _ = st.from(0)
	if len(events) != 2 || len(events[0].Message) != maxLine || events[1].Message != "yyyyy" {
		t.Errorf("long line became %d events", len(events))
	}
}

// Regression: events for a reader that had left were kept without limit.
func TestStreamBoundsItsBacklog(t *testing.T) {
	st := newStream()
	for i := 0; i < maxBacklog+500; i++ {
		st.add(deploy.Event{Step: "log", Message: fmt.Sprint(i)})
	}
	st.add(deploy.Event{Step: "done", Status: "ok"})
	st.close()
	st.mu.Lock()
	held := len(st.events)
	st.mu.Unlock()
	if held != maxBacklog {
		t.Fatalf("stream holds %d events, limit %d", held, maxBacklog)
	}
	// A reader that arrives late gets the newest events, ending with the
	// outcome, and then the stream ends instead of repeating itself.
	rec := httptest.NewRecorder()
	st.serve(rec, httptest.NewRequest("GET", "/", nil))
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != maxBacklog || !strings.Contains(lines[0], `"501"`) || !strings.Contains(lines[len(lines)-1], `"done"`) {
		t.Fatalf("delivered %d lines, first %s, last %s", len(lines), lines[0], lines[len(lines)-1])
	}
	// Asking past the end is answered with nothing.
	if events, next, _, _ := st.read(1 << 30); len(events) != 0 || next != maxBacklog+501 {
		t.Errorf("read past the end = %d events, next %d", len(events), next)
	}
}

func TestActorDefaultsToAdmin(t *testing.T) {
	if got := actor(httptest.NewRequest("GET", "/", nil)); got != adminActor {
		t.Errorf("actor = %q", got)
	}
}

type errWriter struct{ header http.Header }

func (e *errWriter) Header() http.Header       { return e.header }
func (e *errWriter) WriteHeader(int)           {}
func (e *errWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestServeStopsOnWriteError(t *testing.T) {
	st := newStream()
	st.add(deploy.Event{Step: "a"})
	done := make(chan struct{})
	go func() {
		st.serve(&errWriter{header: http.Header{}}, httptest.NewRequest("GET", "/", nil))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop on a write error")
	}
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "zym")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestRunServesBothListenersAndShutsDown(t *testing.T) {
	f := newFixture(t, false)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.Config.Listen = l.Addr().String()
	l.Close()
	f.Config.Socket = filepath.Join(shortTempDir(t), "run", "zymple.sock")
	// A stale socket file from a crashed daemon must not block startup.
	writeFile(t, f.Config.Socket, "stale")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()

	unix := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", f.Config.Socket)
	}}}
	var resp *http.Response
	for i := 0; i < 100; i++ {
		if resp, err = unix.Get("http://zymple/admin/apps"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("operator socket never answered: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("operator socket: %d", resp.StatusCode)
	}
	info, err := os.Stat(f.Config.Socket)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	resp, err = http.Get("http://" + f.Config.Listen + "/admin/apps")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("the network listener serves operator routes: %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not shut down")
	}
}

func TestRunFailsWhenItCannotListen(t *testing.T) {
	f := newFixture(t, false)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	f.Config.Listen = l.Addr().String() // already taken
	f.Config.Socket = filepath.Join(shortTempDir(t), "zymple.sock")
	if err := f.Run(context.Background()); err == nil {
		t.Error("Run should fail when the port is taken")
	}
	f2 := newFixture(t, false)
	l2, _ := net.Listen("tcp", "127.0.0.1:0")
	f2.Config.Listen = l2.Addr().String()
	l2.Close()
	blocker := filepath.Join(shortTempDir(t), "file")
	writeFile(t, blocker, "x")
	f2.Config.Socket = filepath.Join(blocker, "zymple.sock")
	if err := f2.Run(context.Background()); err == nil {
		t.Error("Run should fail when the socket cannot be created")
	}
}

func TestNewReportsAnUnusableContainerdSocket(t *testing.T) {
	cfg := config.Default()
	cfg.Containerd.Socket = ""
	if _, err := New(context.Background(), &cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Error("want an error")
	}
}
