//go:build e2e

// Package e2e runs Zymple inside a throwaway privileged Docker-in-Docker
// "server" with Caddy, and drives it from outside with the real CLI and the
// real docker client, the way CI would.
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	hostImage     = "zymple-e2e-host"
	hostName      = "zymple-e2e"
	postgresImage = "zymple-postgres:17"
)

var (
	repoRoot string
	cliPath  string
	hostPort int
)

func TestMain(m *testing.M) {
	code, err := setup(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e setup failed:", err)
		code = 1
	}
	if os.Getenv("ZYMPLE_E2E_KEEP") == "" {
		_ = exec.Command("docker", "rm", "-f", hostName).Run()
	}
	os.Exit(code)
}

func setup(m *testing.M) (int, error) {
	wd, err := os.Getwd()
	if err != nil {
		return 1, err
	}
	repoRoot = filepath.Join(wd, "..", "..")
	tmp, err := os.MkdirTemp("", "zymple-e2e-*")
	if err != nil {
		return 1, err
	}
	defer os.RemoveAll(tmp)

	cliPath = filepath.Join(tmp, "zymple")
	linux := filepath.Join(tmp, "zymple-linux")
	steps := []struct {
		env  []string
		args []string
	}{
		{nil, []string{"go", "build", "-o", cliPath, "./cmd/zymple"}},
		{[]string{"GOOS=linux", "GOARCH=" + runtime.GOARCH, "CGO_ENABLED=0"}, []string{"go", "build", "-o", linux, "./cmd/zymple"}},
		{nil, []string{"docker", "build", "-q", "-t", hostImage, "test/e2e/host"}},
		{nil, []string{"docker", "build", "-q", "--build-arg", "VERSION=v1", "-t", "zymple-e2e-app:v1", "test/e2e/app"}},
		{nil, []string{"docker", "build", "-q", "--build-arg", "VERSION=v2", "-t", "zymple-e2e-app:v2", "test/e2e/app"}},
		{nil, []string{"docker", "build", "-q", "--build-arg", "VERSION=bad", "--build-arg", "HEALTHY=0", "-t", "zymple-e2e-app:bad", "test/e2e/app"}},
	}
	for _, s := range steps {
		cmd := exec.Command(s.args[0], s.args[1:]...)
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(), s.env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return 1, fmt.Errorf("%s: %v\n%s", strings.Join(s.args, " "), err, out)
		}
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 1, err
	}
	hostPort = l.Addr().(*net.TCPAddr).Port
	l.Close()

	_ = exec.Command("docker", "rm", "-f", hostName).Run()
	if out, err := exec.Command("docker", "run", "-d", "--privileged", "--name", hostName,
		"-p", fmt.Sprintf("127.0.0.1:%d:80", hostPort), hostImage).CombinedOutput(); err != nil {
		return 1, fmt.Errorf("starting host: %v\n%s", err, out)
	}
	if err := waitFor(90*time.Second, func() error { return hostRun(nil, "docker", "info") }); err != nil {
		return 1, fmt.Errorf("docker in the host never came up: %w", err)
	}
	if out, _ := hostOutput("docker", "info", "--format", "{{.DriverStatus}}"); !strings.Contains(out, "io.containerd.snapshotter") {
		return 1, fmt.Errorf("the host's docker does not use the containerd image store: %s", out)
	}

	dockerfile, err := os.ReadFile(filepath.Join(repoRoot, "images", "postgres", "Dockerfile"))
	if err != nil {
		return 1, err
	}
	if err := hostRun(bytes.NewReader(dockerfile), "docker", "build", "-q", "-t", postgresImage, "-"); err != nil {
		return 1, fmt.Errorf("building the postgres image in the host: %w", err)
	}

	if out, err := exec.Command("docker", "cp", linux, hostName+":/usr/local/bin/zymple").CombinedOutput(); err != nil {
		return 1, fmt.Errorf("installing zymple: %v\n%s", err, out)
	}
	files := map[string]string{
		"/etc/zymple/zymple.yml": `caddy:
  reload: ["caddy", "reload", "--config", "/etc/caddy/Caddyfile"]
  tls: "off"
containerd:
  socket: /run/docker/containerd/containerd.sock
backup:
  type: posix
  path: /var/lib/zymple-backups
postgres_images:
  17: ` + postgresImage + "\n",
		"/etc/caddy/Caddyfile": "{\n\tauto_https off\n}\n:80 {\n\treverse_proxy 127.0.0.1:7080\n}\nimport /etc/caddy/zymple/*.caddy\n",
	}
	for path, content := range files {
		if err := hostWrite(path, content); err != nil {
			return 1, err
		}
	}
	if err := hostRun(nil, "mkdir", "-p", "/etc/zymple/apps", "/etc/caddy/zymple", "/var/lib/zymple"); err != nil {
		return 1, err
	}
	if err := startDaemons(); err != nil {
		return 1, err
	}
	return m.Run(), nil
}

// startDaemons starts Caddy and Zymple in the host and waits for Zymple to
// finish converging.
func startDaemons() error {
	script := `caddy start --config /etc/caddy/Caddyfile >>/var/log/caddy.log 2>&1
: > /var/log/zymple.log
(zymple server >>/var/log/zymple.log 2>&1 &)`
	if err := hostRun(nil, "sh", "-c", script); err != nil {
		return err
	}
	return waitConverged()
}

func startZymple() error {
	if err := hostRun(nil, "sh", "-c", ": > /var/log/zymple.log; (zymple server >>/var/log/zymple.log 2>&1 &)"); err != nil {
		return err
	}
	return waitConverged()
}

func waitConverged() error {
	err := waitFor(5*time.Minute, func() error {
		return hostRun(nil, "grep", "-q", "startup convergence finished", "/var/log/zymple.log")
	})
	if err != nil {
		log, _ := hostOutput("tail", "-30", "/var/log/zymple.log")
		return fmt.Errorf("zymple did not converge: %w\n%s", err, log)
	}
	return nil
}

func waitFor(timeout time.Duration, fn func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := fn()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func hostRun(stdin io.Reader, args ...string) error {
	full := []string{"exec"}
	if stdin != nil {
		full = append(full, "-i")
	}
	cmd := exec.Command("docker", append(append(full, hostName), args...)...)
	cmd.Stdin = stdin
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func hostOutput(args ...string) (string, error) {
	out, err := exec.Command("docker", append([]string{"exec", hostName}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func hostWrite(path, content string) error {
	script := fmt.Sprintf("mkdir -p %q && cat > %q", filepath.Dir(path), path)
	return hostRun(strings.NewReader(content), "sh", "-c", script)
}

// mustHost runs a command in the host and fails the test if it fails.
func mustHost(t *testing.T, args ...string) string {
	t.Helper()
	out, err := hostOutput(args...)
	if err != nil {
		t.Fatalf("host: %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// app is one app defined on the host for a test.
type app struct {
	name  string
	token string
}

type appOptions struct {
	healthTimeout string
	drain         string
	release       string
	keep          int
	postgres      bool
	redis         bool
	compose       string
	// images is the app's list of extra images, as written in app.yml.
	images string
}

const defaultCompose = `services:
  web:
    image: ${IMAGE}
    init: true
  worker:
    image: ${IMAGE}
    init: true
    command: ["sleep", "1000000"]
`

// newApp writes an app definition into the host and reloads Zymple.
func newApp(t *testing.T, name string, o appOptions) app {
	t.Helper()
	if o.healthTimeout == "" {
		o.healthTimeout = "20s"
	}
	if o.drain == "" {
		o.drain = "1s"
	}
	if o.compose == "" {
		o.compose = defaultCompose
	}
	yml := fmt.Sprintf("name: %s\ndomains: [%s.localhost]\nweb:\n  service: web\n  port: 3000\n  healthcheck: /healthz\n  health_timeout: %s\n  drain: %s\n",
		name, name, o.healthTimeout, o.drain)
	if o.release != "" {
		yml += "release: " + o.release + "\n"
	}
	if o.keep != 0 {
		yml += fmt.Sprintf("keep_releases: %d\n", o.keep)
	}
	if o.postgres {
		yml += "postgres: { version: 17, pitr_days: 7, env: LEDGER_DATABASE_URL, create_roles: true }\n"
	}
	if o.redis {
		yml += "redis: { persistent: false }\n"
	}
	if o.images != "" {
		yml += "images: " + o.images + "\n"
	}
	out, err := exec.Command(cliPath, "token", "generate", "ci").Output()
	if err != nil {
		t.Fatal(err)
	}
	var token, line string
	for _, l := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(l, "token: "); ok {
			token = v
		}
		if v, ok := strings.CutPrefix(l, "tokens file line: "); ok {
			line = v
		}
	}
	dir := "/etc/zymple/apps/" + name
	for path, content := range map[string]string{
		dir + "/app.yml":     yml,
		dir + "/compose.yml": o.compose,
		dir + "/tokens":      line + "\n",
		dir + "/env":         "GREETING=hello $USER\n",
	} {
		if err := hostWrite(path, content); err != nil {
			t.Fatal(err)
		}
	}
	mustHost(t, "zymple", "reload")
	return app{name: name, token: token}
}

func baseURL() string { return fmt.Sprintf("http://localhost:%d", hostPort) }

// cli runs the real CLI from outside the host, as CI would.
func (a app) cli(args ...string) (string, error) {
	return a.cliWithToken(a.token, args...)
}

func (a app) cliWithToken(token string, args ...string) (string, error) {
	cmd := exec.Command(cliPath, args...)
	cmd.Env = append(os.Environ(), "ZYMPLE_HOST="+baseURL(), "ZYMPLE_TOKEN="+token)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (a app) mustCLI(t *testing.T, args ...string) string {
	t.Helper()
	out, err := a.cli(args...)
	if err != nil {
		t.Fatalf("zymple %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (a app) deploy(t *testing.T, version string) string {
	t.Helper()
	return a.mustCLI(t, "deploy", a.name, "--image", "zymple-e2e-app:"+version)
}

// get fetches a path from the app through Caddy.
func (a app) get(path string) (int, string, error) {
	req, err := http.NewRequest(http.MethodGet, baseURL()+path, nil)
	if err != nil {
		return 0, "", err
	}
	req.Host = a.name + ".localhost"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(body)), err
}

func (a app) wantVersion(t *testing.T, want string) {
	t.Helper()
	code, body, err := a.get("/version")
	if err != nil || code != 200 || body != want {
		t.Fatalf("GET /version = %d %q, %v; want 200 %q", code, body, err, want)
	}
}

// webContainers lists the app's web containers in the host.
func (a app) webContainers(t *testing.T) []string {
	t.Helper()
	out := mustHost(t, "docker", "ps", "-a", "--filter", "label=zymple.app="+a.name,
		"--filter", "label=zymple.role=web", "--format", "{{.Names}}")
	return strings.Fields(out)
}

// load hammers the app until stopped and counts every request that was not a
// 200. Zero-downtime tests assert that the count stays at zero.
type load struct {
	total, failed atomic.Int64
	firstFailure  atomic.Value
	mu            sync.Mutex
	failures      []string
	stop          context.CancelFunc
	done          sync.WaitGroup
}

func (a app) startLoad() *load {
	ctx, cancel := context.WithCancel(context.Background())
	l := &load{stop: cancel}
	for i := 0; i < 4; i++ {
		l.done.Add(1)
		go func() {
			defer l.done.Done()
			for ctx.Err() == nil {
				code, _, err := a.get("/version")
				if ctx.Err() != nil {
					return
				}
				l.total.Add(1)
				if err != nil || code != 200 {
					l.failed.Add(1)
					l.firstFailure.CompareAndSwap(nil, fmt.Sprintf("status %d, error %v", code, err))
					l.mu.Lock()
					if len(l.failures) < 50 {
						l.failures = append(l.failures, fmt.Sprintf("%s status %d, error %v",
							time.Now().UTC().Format("15:04:05.000"), code, err))
					}
					l.mu.Unlock()
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
	}
	return l
}

// failureLog lists the recorded failures with their times.
func (l *load) failureLog() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.failures, "\n")
}

func (l *load) finish(t *testing.T) {
	t.Helper()
	l.stop()
	l.done.Wait()
	if l.total.Load() < 50 {
		t.Fatalf("load generator only made %d requests; the test proves nothing", l.total.Load())
	}
	if n := l.failed.Load(); n != 0 {
		t.Fatalf("%d of %d requests failed during the operation; first: %v", n, l.total.Load(), l.firstFailure.Load())
	}
	t.Logf("%d requests, 0 failed", l.total.Load())
}
