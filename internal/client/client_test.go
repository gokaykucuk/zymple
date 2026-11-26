package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gokaykucuk/zymple/internal/deploy"
	"github.com/gokaykucuk/zymple/internal/execx"
	"github.com/gokaykucuk/zymple/internal/execx/execxtest"
)

var ctx = context.Background()

func TestNewValidatesTheHost(t *testing.T) {
	for _, host := range []string{"https://deploy.example.com", "https://deploy.example.com/", "http://localhost:8080", "http://127.0.0.1:1", "http://app.localhost", "http://[::1]:9"} {
		if _, err := New(host, "t"); err != nil {
			t.Errorf("%s rejected: %v", host, err)
		}
	}
	for name, host := range map[string]string{
		"plain http to a public host": "http://deploy.example.com",
		"plain http to a private ip":  "http://10.0.0.5",
		"no scheme":                   "deploy.example.com",
		"other scheme":                "ftp://deploy.example.com",
		"empty":                       "",
		"unparseable":                 "http://[::1",
	} {
		if _, err := New(host, "t"); err == nil {
			t.Errorf("%s: %q accepted", name, host)
		}
	}
	if _, err := New("https://deploy.example.com", ""); err == nil {
		t.Error("an empty token was accepted")
	}
	c, _ := New("https://deploy.example.com:8443/", "t")
	if c.Base != "https://deploy.example.com:8443" || c.RegistryHost() != "deploy.example.com:8443" {
		t.Errorf("Base = %q, RegistryHost = %q", c.Base, c.RegistryHost())
	}
	if got := (&Client{Base: "://bad"}).RegistryHost(); got != "" {
		t.Errorf("RegistryHost of a bad base = %q", got)
	}
}

func testServer(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "zym_token")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestJSON(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			if r.Header.Get("Authorization") != "Bearer zym_token" {
				t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
			}
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			_ = json.NewEncoder(w).Encode(map[string]string{"echo": in["say"], "type": r.Header.Get("Content-Type")})
		case "/denied":
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"invalid app or token"}`)
		default:
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, "upstream is down")
		}
	})
	var out map[string]string
	if err := c.JSON(ctx, "POST", "/ok", map[string]string{"say": "hi"}, &out); err != nil || out["echo"] != "hi" || out["type"] != "application/json" {
		t.Fatalf("JSON = %v, %v", out, err)
	}
	if err := c.JSON(ctx, "GET", "/ok", nil, nil); err != nil {
		t.Errorf("JSON with no output: %v", err)
	}
	err := c.JSON(ctx, "GET", "/denied", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "invalid app or token") || !strings.Contains(err.Error(), "401") {
		t.Errorf("structured error = %v", err)
	}
	err = c.JSON(ctx, "GET", "/gateway", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "upstream is down") || !strings.Contains(err.Error(), "502") {
		t.Errorf("plain error = %v", err)
	}
	if err := c.JSON(ctx, "GET", "/ok", make(chan int), nil); err == nil {
		t.Error("an unencodable body was accepted")
	}
	if err := c.JSON(ctx, "bad method", "/ok", nil, nil); err == nil {
		t.Error("an invalid method was accepted")
	}
	dead, _ := New("http://127.0.0.1:1", "t")
	if err := dead.JSON(ctx, "GET", "/", nil, nil); err == nil {
		t.Error("a refused connection was not reported")
	}
}

func TestStream(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		switch r.URL.Path {
		case "/ok":
			_ = enc.Encode(deploy.Event{Step: "start", Status: "ok"})
			_ = enc.Encode(deploy.Event{Step: "done", Status: "ok", Message: "release 3 is live", Release: 3})
			_ = enc.Encode(deploy.Event{Step: "ignored"})
		case "/quiet":
			_ = enc.Encode(deploy.Event{Step: "done", Status: "ok"})
		case "/fail":
			_ = enc.Encode(deploy.Event{Step: "health", Status: "fail", Message: "status 500"})
			_ = enc.Encode(deploy.Event{Step: "done", Status: "fail", Message: "health: status 500"})
		case "/cut":
			_ = enc.Encode(deploy.Event{Step: "start", Status: "ok"})
		case "/garbage":
			fmt.Fprintln(w, "<html>proxy error</html>")
		case "/busy":
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":"a deploy is already in progress for this app"}`)
		}
	})
	var got []deploy.Event
	collect := func(e deploy.Event) { got = append(got, e) }

	if err := c.Stream(ctx, "POST", "/ok", nil, collect); err != nil || len(got) != 2 || got[1].Message != "release 3 is live" {
		t.Fatalf("ok stream: %v, %+v", err, got)
	}
	got = nil
	if err := c.Stream(ctx, "POST", "/quiet", nil, collect); err != nil || len(got) != 0 {
		t.Errorf("a done event with no message should not be printed: %v, %+v", err, got)
	}
	if err := c.Stream(ctx, "POST", "/fail", nil, collect); err == nil || err.Error() != "health: status 500" {
		t.Errorf("failed stream: %v", err)
	}
	// A stream that ends without an outcome is not a success.
	if err := c.Stream(ctx, "POST", "/cut", nil, collect); err == nil || !strings.Contains(err.Error(), "zymple status") {
		t.Errorf("cut stream: %v", err)
	}
	if err := c.Stream(ctx, "POST", "/garbage", nil, collect); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("garbage stream: %v", err)
	}
	if err := c.Stream(ctx, "POST", "/busy", nil, collect); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Errorf("busy: %v", err)
	}
}

func TestStreamReportsABrokenConnection(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		// Promise more than is sent, then drop the connection.
		w.Header().Set("Content-Length", "1000")
		fmt.Fprintln(w, `{"step":"start","status":"ok"}`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	})
	err := c.Stream(ctx, "POST", "/", nil, func(deploy.Event) {})
	if err == nil || !strings.Contains(err.Error(), "continues on the server") {
		t.Fatalf("error = %v", err)
	}
}

func TestUnixClient(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "zymc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "z.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("the socket client sent a token")
		}
		fmt.Fprint(w, `{"ok":true}`)
	})}
	go srv.Serve(l)
	defer srv.Close()
	var out map[string]bool
	if err := NewUnix(socket).JSON(ctx, "GET", "/admin/apps", nil, &out); err != nil || !out["ok"] {
		t.Fatalf("unix client: %v, %v", out, err)
	}
	if err := NewUnix(filepath.Join(dir, "missing.sock")).JSON(ctx, "GET", "/", nil, nil); err == nil {
		t.Error("a missing socket was not reported")
	}
}

func TestPush(t *testing.T) {
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	var pushCmd execx.Cmd
	var authSeen string
	fake := &execxtest.Fake{}
	fake.Handler = func(c execx.Cmd) (string, error) {
		switch {
		case c.Args[0] == "context":
			return "unix:///custom/docker.sock\n", nil
		case len(c.Args) > 2 && c.Args[2] == "push":
			pushCmd = c
			data, err := os.ReadFile(filepath.Join(c.Args[1], "config.json"))
			if err != nil {
				return "", err
			}
			var cfg struct {
				Auths map[string]struct{ Auth string } `json:"auths"`
			}
			if err := json.Unmarshal(data, &cfg); err != nil {
				return "", err
			}
			raw, _ := base64.StdEncoding.DecodeString(cfg.Auths["deploy.example.com"].Auth)
			authSeen = string(raw)
			return "layer: Pushed\npush-ab12: digest: " + digest + " size: 735\n", nil
		}
		return "", nil
	}
	p := &Pusher{Runner: fake, Environ: func() []string { return []string{"PATH=/bin"} }}
	var out bytes.Buffer
	got, err := p.Push(ctx, "deploy.example.com", "shop", "zym_secret", "shop:build-7", &out)
	if err != nil || got != digest {
		t.Fatalf("Push = %q, %v", got, err)
	}
	if authSeen != "shop:zym_secret" {
		t.Errorf("credentials in the temporary config = %q", authSeen)
	}
	target := pushCmd.Args[3]
	if !strings.HasPrefix(target, "deploy.example.com/zymple/shop:push-") {
		t.Errorf("push target = %q", target)
	}
	if fake.Find("docker tag shop:build-7 "+target) == "" || fake.Find("docker rmi "+target) == "" {
		t.Errorf("the image was not tagged and untagged around the push: %v", fake.Calls())
	}
	if len(pushCmd.Env) != 2 || pushCmd.Env[1] != "DOCKER_HOST=unix:///custom/docker.sock" {
		t.Errorf("push environment = %v", pushCmd.Env)
	}
	if _, err := os.Stat(pushCmd.Args[1]); !errors.Is(err, os.ErrNotExist) {
		t.Error("the temporary docker config with the token was left on disk")
	}
	if !strings.Contains(out.String(), "Pushed") {
		t.Errorf("push output was not passed through: %q", out.String())
	}
	for _, c := range fake.Calls() {
		if strings.Contains(c, "zym_secret") {
			t.Errorf("the token appears on a command line: %s", c)
		}
	}
}

func TestPushFailures(t *testing.T) {
	var out bytes.Buffer
	for _, image := range []string{"", "--help", "-v"} {
		if _, err := (&Pusher{Runner: &execxtest.Fake{}}).Push(ctx, "h", "shop", "t", image, &out); err == nil {
			t.Errorf("image %q accepted", image)
		}
	}
	for _, failOn := range []string{"context", "tag", "push"} {
		fake := &execxtest.Fake{Handler: func(c execx.Cmd) (string, error) {
			if strings.Contains(c.String(), " "+failOn+" ") {
				return "", errors.New("injected")
			}
			return "", nil
		}}
		if _, err := (&Pusher{Runner: fake}).Push(ctx, "h", "shop", "t", "img:1", &out); err == nil {
			t.Errorf("a failing %q was not reported", failOn)
		}
	}
	// A push that prints no digest must not be treated as a success.
	if _, err := (&Pusher{Runner: &execxtest.Fake{}}).Push(ctx, "h", "shop", "t", "img:1", &out); err == nil || !strings.Contains(err.Error(), "no digest") {
		t.Errorf("push without a digest: %v", err)
	}
}
