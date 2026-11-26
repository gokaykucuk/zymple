package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gokaykucuk/zymple/internal/auth"
	"github.com/gokaykucuk/zymple/internal/deploy"
	"github.com/gokaykucuk/zymple/internal/execx"
	"github.com/gokaykucuk/zymple/internal/execx/execxtest"
	"github.com/gokaykucuk/zymple/internal/releases"
)

const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// recorder is a fake Zymple API that records what the CLI sent.
type recorder struct {
	mu       sync.Mutex
	requests []string
	bodies   []string
	fail     bool
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.requests = append(r.requests, req.Method+" "+req.URL.RequestURI())
	r.bodies = append(r.bodies, strings.TrimSpace(string(body)))
	fail := r.fail
	r.mu.Unlock()
	enc := json.NewEncoder(w)
	path := req.URL.Path
	switch {
	case strings.HasSuffix(path, "/releases"):
		_ = enc.Encode(map[string]any{"current": 2, "releases": []releases.Release{
			{Number: 1, Digest: digest, Time: time.Unix(100, 0).UTC(), By: "ci"},
			{Number: 2, Digest: digest, Time: time.Unix(200, 0).UTC(), By: "admin", RolledBackFrom: 1},
		}})
	case strings.HasSuffix(path, "/status"):
		st := map[string]any{"deploying": false, "web_running": !fail, "last_error": "health: status 500",
			"current": releases.Release{Number: 2, Digest: digest, Time: time.Unix(200, 0).UTC(), By: "ci"}}
		if strings.Contains(path, "/empty/") {
			st = map[string]any{"deploying": true}
		}
		_ = enc.Encode(st)
	case path == "/admin/apps":
		_ = enc.Encode([]map[string]any{{"name": "shop", "domains": []string{"shop.example.com", "www.shop.example.com"}, "current": 2}})
	case strings.Contains(path, "/pg/drills/"):
		_ = enc.Encode(map[string]string{"removed": "x"})
	default:
		_ = enc.Encode(deploy.Event{Step: "verify", Status: "start"})
		_ = enc.Encode(deploy.Event{Step: "release", Status: "log", Message: "migrating"})
		if fail {
			_ = enc.Encode(deploy.Event{Step: "health", Status: "fail", Message: "status 500"})
			_ = enc.Encode(deploy.Event{Step: "done", Status: "fail", Message: "health: status 500"})
			return
		}
		_ = enc.Encode(deploy.Event{Step: "done", Status: "ok", Message: "release 3 is live"})
	}
}

func (r *recorder) last() (string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) == 0 {
		return "", ""
	}
	return r.requests[len(r.requests)-1], r.bodies[len(r.bodies)-1]
}

type harness struct {
	api    *recorder
	fake   *execxtest.Fake
	env    map[string]string
	served string
}

// newHarness serves the fake API on both a TCP port (network mode) and a
// unix socket (operator mode).
func newHarness(t *testing.T) *harness {
	t.Helper()
	api := &recorder{}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	dir, err := os.MkdirTemp("/tmp", "zymcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "z.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	unix := &http.Server{Handler: api}
	go unix.Serve(l)
	t.Cleanup(func() { unix.Close() })
	return &harness{api: api, fake: &execxtest.Fake{}, env: map[string]string{
		"ZYMPLE_HOST": srv.URL, "ZYMPLE_TOKEN": "zym_token", "ZYMPLE_SOCKET": socket,
	}}
}

// run executes the CLI. With network false it uses the operator socket.
func (h *harness) run(network bool, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	env := Env{
		Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr, Runner: h.fake,
		Getenv: func(key string) string {
			if !network && key != "ZYMPLE_SOCKET" {
				return ""
			}
			return h.env[key]
		},
		Serve: func(_ context.Context, path string) error {
			h.served = path
			if path == "/fail" {
				return errors.New("cannot start")
			}
			return nil
		},
	}
	code := Run(context.Background(), args, env)
	return code, stdout.String(), stderr.String()
}

func TestVersionHelpAndUsageErrors(t *testing.T) {
	h := newHarness(t)
	if code, out, _ := h.run(true, "version"); code != 0 || out != "zymple dev\n" {
		t.Errorf("version: %d %q", code, out)
	}
	if code, out, _ := h.run(true, "--version"); code != 0 || !strings.Contains(out, "zymple") {
		t.Errorf("--version: %d %q", code, out)
	}
	for _, arg := range []string{"help", "--help", "-h"} {
		if code, out, _ := h.run(true, arg); code != 0 || !strings.Contains(out, "usage: zymple") {
			t.Errorf("%s: %d %q", arg, code, out)
		}
	}
	usageErrors := [][]string{
		{}, {"frobnicate"},
		{"deploy"}, {"deploy", "shop"}, {"deploy", "shop", "--image", "a", "--digest", digest}, {"deploy", "Shop", "--digest", digest},
		{"deploy", "shop", "--digest", "latest"}, {"deploy", "shop", "extra", "--digest", digest}, {"deploy", "shop", "--bogus"},
		{"rollback"}, {"rollback", "shop", "zero"}, {"rollback", "shop", "0"}, {"rollback", "shop", "1", "2"}, {"rollback", "BAD"},
		{"releases"}, {"releases", "a", "b"}, {"status"}, {"status", "x"},
		{"logs"}, {"logs", "shop", "web", "extra"}, {"logs", "shop", "--tail", "many"}, {"logs", "BAD"},
		{"apps", "extra"}, {"redeploy"}, {"redeploy", "BAD"},
		{"run", "shop"}, {"run", "shop", "ls"}, {"run", "shop", "--"}, {"run", "BAD", "--", "ls"},
		{"pg"}, {"pg", "info"}, {"pg", "frob", "shop"}, {"pg", "info", "BAD"}, {"pg", "backup"}, {"pg", "backup", "shop", "--bogus"},
		{"pg", "restore", "shop"}, {"pg", "restore", "BAD", "--yes"}, {"pg", "restore", "shop", "--bogus"},
		{"pg", "drill-rm", "shop"}, {"pg", "drill-rm", "BAD", "x"}, {"pg", "psql", "BAD"},
		{"redis"}, {"redis", "flush", "shop"}, {"redis", "backup", "BAD"},
		{"token"}, {"token", "generate"}, {"token", "generate", "BAD ID"}, {"token", "revoke", "x"},
		{"server", "extra"}, {"server", "--bogus"},
	}
	for _, args := range usageErrors {
		code, _, stderr := h.run(true, args...)
		if code != 2 || !strings.Contains(stderr, "usage: zymple") {
			t.Errorf("%v: exit %d, want 2 with usage; stderr: %.80q", args, code, stderr)
		}
	}
	if req, _ := h.api.last(); req != "" {
		t.Errorf("a usage error reached the server: %s", req)
	}
}

func TestTokenGenerate(t *testing.T) {
	h := newHarness(t)
	code, out, _ := h.run(true, "token", "generate", "ci")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var token, line string
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(l, "token: "); ok {
			token = v
		}
		if v, ok := strings.CutPrefix(l, "tokens file line: "); ok {
			line = v
		}
	}
	set, err := auth.Parse([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := set.Verify(token); !ok || id != "ci" {
		t.Errorf("the printed line does not verify the printed token: %q %v", id, ok)
	}
}

func TestDeploy(t *testing.T) {
	h := newHarness(t)
	code, out, stderr := h.run(true, "deploy", "shop", "--digest", digest)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if req, body := h.api.last(); req != "POST /api/v1/apps/shop/deploys" || body != `{"digest":"`+digest+`"}` {
		t.Errorf("request = %s %s", req, body)
	}
	for _, want := range []string{"==> deploy " + digest, "==> verify", "migrating", "release 3 is live"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	// Flags may come before the app name.
	if code, _, _ := h.run(true, "deploy", "--digest", digest, "shop"); code != 0 {
		t.Errorf("flags before the app name: exit %d", code)
	}

	// A failed deploy exits non-zero and says why.
	h.api.fail = true
	code, out, stderr = h.run(true, "deploy", "shop", "--digest", digest)
	if code != 1 || !strings.Contains(stderr, "health: status 500") || !strings.Contains(out, "==> health failed: status 500") {
		t.Errorf("failed deploy: exit %d\nstdout: %s\nstderr: %s", code, out, stderr)
	}
	h.api.fail = false

	// On the host, --digest works over the socket; --image does not.
	if code, _, stderr := h.run(false, "deploy", "shop", "--digest", digest); code != 0 {
		t.Errorf("deploy over the socket: exit %d: %s", code, stderr)
	}
	if code, _, stderr := h.run(false, "deploy", "shop", "--image", "shop:1"); code != 2 || !strings.Contains(stderr, "ZYMPLE_HOST") {
		t.Errorf("--image on the host: exit %d: %s", code, stderr)
	}
}

func TestDeployPushesTheImage(t *testing.T) {
	h := newHarness(t)
	h.fake.Handler = func(c execx.Cmd) (string, error) {
		if len(c.Args) > 2 && c.Args[2] == "push" {
			return "push-x: digest: " + digest + " size: 1\n", nil
		}
		return "unix:///var/run/docker.sock\n", nil
	}
	code, out, stderr := h.run(true, "deploy", "shop", "--image", "shop:build-7")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(out, "==> push shop:build-7") || h.fake.Find("docker tag shop:build-7") == "" {
		t.Errorf("the image was not pushed:\n%s\n%v", out, h.fake.Calls())
	}
	if _, body := h.api.last(); body != `{"digest":"`+digest+`"}` {
		t.Errorf("deployed %s, want the pushed digest", body)
	}
	h.fake.Handler = func(execx.Cmd) (string, error) { return "", errors.New("docker is not running") }
	if code, _, stderr := h.run(true, "deploy", "shop", "--image", "shop:build-7"); code != 1 || !strings.Contains(stderr, "docker is not running") {
		t.Errorf("failed push: exit %d: %s", code, stderr)
	}
}

func TestConnectionProblems(t *testing.T) {
	h := newHarness(t)
	h.env["ZYMPLE_TOKEN"] = ""
	if code, _, stderr := h.run(true, "status", "shop"); code != 1 || !strings.Contains(stderr, "ZYMPLE_TOKEN") {
		t.Errorf("missing token: exit %d: %s", code, stderr)
	}
	h.env["ZYMPLE_TOKEN"] = "t"
	h.env["ZYMPLE_HOST"] = "http://deploy.example.com"
	for _, args := range [][]string{
		{"status", "shop"}, {"releases", "shop"}, {"rollback", "shop"}, {"logs", "shop"}, {"deploy", "shop", "--digest", digest},
	} {
		if code, _, stderr := h.run(true, args...); code != 1 || !strings.Contains(stderr, "plain http") {
			t.Errorf("%v over plain http to a public host: exit %d: %s", args, code, stderr)
		}
	}
	// With no host and no socket override, the default socket path is used.
	env := Env{Stdout: io.Discard, Stderr: io.Discard, Getenv: func(string) string { return "" }}
	if c := adminClient(env); c == nil {
		t.Error("no default operator client")
	}
}

func TestRollbackReleasesStatusLogs(t *testing.T) {
	h := newHarness(t)
	if code, _, _ := h.run(true, "rollback", "shop"); code != 0 {
		t.Fatalf("rollback: exit %d", code)
	}
	if req, body := h.api.last(); req != "POST /api/v1/apps/shop/rollbacks" || body != `{"release":0}` {
		t.Errorf("rollback request = %s %s", req, body)
	}
	h.run(true, "rollback", "shop", "4")
	if _, body := h.api.last(); body != `{"release":4}` {
		t.Errorf("rollback to 4 sent %s", body)
	}

	code, out, _ := h.run(true, "releases", "shop")
	if code != 0 || !strings.Contains(out, "RELEASE") {
		t.Fatalf("releases: %d\n%s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "2 ") || !strings.Contains(lines[1], "current, rollback from 1") || !strings.HasPrefix(lines[2], "1 ") {
		t.Errorf("releases table (newest first, with notes):\n%s", out)
	}
	if !strings.Contains(lines[1], "sha256:111111111111") || strings.Contains(lines[1], digest) {
		t.Errorf("digests should be shortened:\n%s", out)
	}

	code, out, _ = h.run(true, "status", "shop")
	if code != 0 || !strings.Contains(out, "release:   2") || !strings.Contains(out, "web:       running") || !strings.Contains(out, "last failed deploy") {
		t.Errorf("status: %d\n%s", code, out)
	}
	h.api.fail = true
	if code, out, stderr := h.run(true, "status", "shop"); code != 1 || !strings.Contains(out, "NOT RUNNING") || !strings.Contains(stderr, "not running") {
		t.Errorf("status of a down app must exit non-zero: %d\n%s", code, out)
	}
	h.api.fail = false
	if code, out, _ := h.run(true, "status", "empty"); code != 0 || !strings.Contains(out, "release:   none") || !strings.Contains(out, "deploying: true") {
		t.Errorf("status of an undeployed app: %d\n%s", code, out)
	}

	if code, _, _ := h.run(true, "logs", "shop", "worker", "-f", "--tail", "50"); code != 0 {
		t.Fatalf("logs: exit %d", code)
	}
	if req, _ := h.api.last(); req != "GET /api/v1/apps/shop/logs?follow=true&service=worker&tail=50" {
		t.Errorf("logs request = %s", req)
	}
	h.run(true, "logs", "shop")
	if req, _ := h.api.last(); req != "GET /api/v1/apps/shop/logs?follow=false&tail=200" {
		t.Errorf("default logs request = %s", req)
	}
}

func TestOperatorCommands(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		args     []string
		wantReq  string
		wantBody string
	}{
		{[]string{"reload"}, "POST /admin/reload", ""},
		{[]string{"redeploy", "shop"}, "POST /admin/apps/shop/redeploy", ""},
		{[]string{"run", "shop", "--", "bin/console", "--verbose"}, "POST /admin/apps/shop/run", `{"command":["bin/console","--verbose"]}`},
		{[]string{"pg", "info", "shop"}, "GET /admin/apps/shop/pg/info", ""},
		{[]string{"pg", "backup", "shop"}, "POST /admin/apps/shop/pg/backup", `{"type":"diff"}`},
		{[]string{"pg", "backup", "shop", "--type", "full"}, "POST /admin/apps/shop/pg/backup", `{"type":"full"}`},
		{[]string{"pg", "restore", "shop", "--into", "drill"}, "POST /admin/apps/shop/pg/restore", `{"into":"drill","time":""}`},
		{[]string{"pg", "restore", "shop", "--yes", "--time", "2026-01-02 15:04:05+00"}, "POST /admin/apps/shop/pg/restore", `{"into":"","time":"2026-01-02 15:04:05+00"}`},
		{[]string{"pg", "drill-rm", "shop", "drill"}, "DELETE /admin/apps/shop/pg/drills/drill", ""},
		{[]string{"redis", "backup", "shop"}, "POST /admin/apps/shop/redis/backup", ""},
		{[]string{"redis", "restore", "shop"}, "POST /admin/apps/shop/redis/restore", ""},
	}
	for _, c := range cases {
		code, _, stderr := h.run(false, c.args...)
		if code != 0 {
			t.Errorf("%v: exit %d: %s", c.args, code, stderr)
			continue
		}
		req, body := h.api.last()
		if req != c.wantReq || body != c.wantBody {
			t.Errorf("%v:\n got %s %s\nwant %s %s", c.args, req, body, c.wantReq, c.wantBody)
		}
	}
	code, out, _ := h.run(false, "apps")
	if code != 0 || !strings.Contains(out, "shop") || !strings.Contains(out, "shop.example.com, www.shop.example.com") {
		t.Errorf("apps: %d\n%s", code, out)
	}
	// An in-place restore is refused without --yes, before anything is sent.
	before, _ := h.api.last()
	code, _, stderr := h.run(false, "pg", "restore", "shop", "--time", "2026-01-02 15:04:05+00")
	if after, _ := h.api.last(); code != 2 || !strings.Contains(stderr, "--yes") || after != before {
		t.Errorf("unconfirmed in-place restore: exit %d, request sent: %v", code, after != before)
	}

	// psql attaches the terminal to the database container directly.
	if code, _, _ := h.run(false, "pg", "psql", "shop"); code != 0 || h.fake.Find("docker exec -it zymple-shop-postgres psql -U app app") == "" {
		t.Errorf("psql: exit %d, %v", code, h.fake.Calls())
	}

	// With the daemon down, operator commands fail cleanly.
	h.env["ZYMPLE_SOCKET"] = "/nonexistent/zymple.sock"
	for _, args := range [][]string{{"apps"}, {"reload"}, {"pg", "drill-rm", "shop", "d"}} {
		if code, _, stderr := h.run(false, args...); code != 1 || stderr == "" {
			t.Errorf("%v with the daemon down: exit %d", args, code)
		}
	}
}

func TestServer(t *testing.T) {
	h := newHarness(t)
	if code, _, _ := h.run(true, "server"); code != 0 || h.served != "/etc/zymple/zymple.yml" {
		t.Errorf("server: exit %d, config %q", code, h.served)
	}
	if code, _, _ := h.run(true, "server", "--config", "/custom.yml"); code != 0 || h.served != "/custom.yml" {
		t.Errorf("server --config: exit %d, config %q", code, h.served)
	}
	if code, _, stderr := h.run(true, "server", "--config", "/fail"); code != 1 || !strings.Contains(stderr, "cannot start") {
		t.Errorf("failing server: exit %d: %s", code, stderr)
	}
}

func TestDefaultEnv(t *testing.T) {
	env := DefaultEnv(func(context.Context, string) error { return nil })
	if env.Stdout == nil || env.Stderr == nil || env.Stdin == nil || env.Runner == nil || env.Getenv == nil || env.Serve == nil {
		t.Fatalf("incomplete default environment: %+v", env)
	}
}

func TestShortDigest(t *testing.T) {
	if got := shortDigest("short"); got != "short" {
		t.Errorf("shortDigest(short) = %q", got)
	}
	if got := fmt.Sprint(shortDigest(digest)); len(got) != 19 {
		t.Errorf("shortDigest = %q", got)
	}
}

// Regression: container output was printed as it came, so an image could
// send terminal control sequences to whoever ran `zymple logs` or a deploy.
func TestPrintableKeepsColoursAndNothingElse(t *testing.T) {
	for in, want := range map[string]string{
		"plain text\twith a tab":            "plain text\twith a tab",
		"\x1b[1;31mred\x1b[0m":              "\x1b[1;31mred\x1b[0m",
		"clear\x1b[2J\x1b[H":                "clear\ufffd[2J\ufffd[H",
		"title\x1b]0;pwned\x07":             "title\ufffd]0;pwned\ufffd",
		"clipboard\x1b]52;c;ZWNobw==\x1b\\": "clipboard\ufffd]52;c;ZWNobw==\ufffd\\",
		"overwrite\rfake line\b":            "overwrite\ufffdfake line\ufffd",
		"c1 \u009b31m csi":                  "c1 \ufffd31m csi",
		"del\x7f nul\x00":                   "del\ufffd nul\ufffd",
		"héllo wörld ✓":                     "héllo wörld ✓",
	} {
		if got := printable(in); got != want {
			t.Errorf("printable(%q) = %q, want %q", in, got, want)
		}
	}
	var out bytes.Buffer
	print := printer(Env{Stdout: &out})
	print(deploy.Event{Step: "start", Status: "log", Message: "a\x1b[2Kb"})
	print(deploy.Event{Step: "health", Status: "fail", Message: "x\x1b[1A"})
	if strings.Contains(out.String(), "\x1b") {
		t.Errorf("an escape sequence reached the terminal: %q", out.String())
	}
}
