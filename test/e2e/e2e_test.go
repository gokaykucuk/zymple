//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDeployRollbackWithZeroDowntime(t *testing.T) {
	a := newApp(t, "blue", appOptions{release: `["cat", "/www/version"]`})
	out := a.deploy(t, "v1")
	if !strings.Contains(out, "release 1 is live") {
		t.Fatalf("unexpected deploy output:\n%s", out)
	}
	a.wantVersion(t, "v1")

	l := a.startLoad()
	a.deploy(t, "v2")
	a.wantVersion(t, "v2")
	a.mustCLI(t, "rollback", a.name)
	a.wantVersion(t, "v1")
	mustHost(t, "zymple", "redeploy", a.name)
	mustHost(t, "zymple", "reload")
	l.finish(t)

	if webs := a.webContainers(t); len(webs) != 1 {
		t.Errorf("want exactly one web container after the deploys, got %v", webs)
	}
	releases := a.mustCLI(t, "releases", a.name)
	if !strings.Contains(releases, "rollback from 2") || !strings.Contains(releases, "current") {
		t.Errorf("releases output misses the rollback or the current marker:\n%s", releases)
	}
	if status := a.mustCLI(t, "status", a.name); !strings.Contains(status, "web:       running") {
		t.Errorf("unexpected status:\n%s", status)
	}
	// The worker follows the web service to the same image.
	worker := mustHost(t, "docker", "ps", "--filter", "label=zymple.app="+a.name,
		"--filter", "label=zymple.role=service", "--format", `{{.Label "zymple.release"}}`)
	if worker != "4" {
		t.Errorf("worker runs release %q, want 4", worker)
	}
}

func TestFailedHealthCheckKeepsOldRelease(t *testing.T) {
	a := newApp(t, "sick", appOptions{healthTimeout: "4s"})
	a.deploy(t, "v1")

	l := a.startLoad()
	out, err := a.cli("deploy", a.name, "--image", "zymple-e2e-app:bad")
	l.finish(t)
	if err == nil {
		t.Fatalf("deploying an unhealthy image succeeded:\n%s", out)
	}
	if !strings.Contains(out, "health") {
		t.Errorf("failure output does not name the health step:\n%s", out)
	}
	a.wantVersion(t, "v1")
	if webs := a.webContainers(t); len(webs) != 1 || !strings.Contains(webs[0], "web-r1") {
		t.Errorf("the failed attempt left containers behind: %v", webs)
	}
	status, _ := a.cli("status", a.name)
	if !strings.Contains(status, "last failed deploy") || !strings.Contains(status, "release:   1") {
		t.Errorf("status does not report the failure:\n%s", status)
	}
	// The app is still deployable afterwards.
	a.deploy(t, "v2")
	a.wantVersion(t, "v2")
}

func TestFailedReleaseCommandAbortsBeforeStart(t *testing.T) {
	a := newApp(t, "migr", appOptions{release: `["grep", "-q", "v1", "/www/version"]`})
	a.deploy(t, "v1")
	out, err := a.cli("deploy", a.name, "--image", "zymple-e2e-app:v2")
	if err == nil {
		t.Fatalf("deploy succeeded although the release command fails:\n%s", out)
	}
	if strings.Contains(out, "==> start") {
		t.Errorf("the web container was started after a failed release command:\n%s", out)
	}
	a.wantVersion(t, "v1")
}

func TestTokensAreScopedToTheirApp(t *testing.T) {
	a := newApp(t, "tenant-a", appOptions{})
	b := newApp(t, "tenant-b", appOptions{})
	a.deploy(t, "v1")
	b.deploy(t, "v1")

	// A's token cannot push to or deploy B.
	if out, err := a.cliWithToken(a.token, "deploy", b.name, "--image", "zymple-e2e-app:v2"); err == nil {
		t.Fatalf("app A's token deployed app B:\n%s", out)
	}
	b.wantVersion(t, "v1")
	if webs := b.webContainers(t); len(webs) != 1 {
		t.Errorf("app B changed: %v", webs)
	}

	request := func(method, path, user, pass, bearer string) int {
		t.Helper()
		req, err := http.NewRequest(method, baseURL()+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	cases := []struct {
		name               string
		method, path       string
		user, pass, bearer string
		want               int
	}{
		{"registry without credentials", "GET", "/v2/", "", "", "", 401},
		{"registry with a wrong token", "GET", "/v2/", a.name, "zym_wrong", "", 401},
		{"registry with B's token as A", "GET", "/v2/", a.name, b.token, "", 401},
		{"registry with own token", "GET", "/v2/", a.name, a.token, "", 200},
		{"upload into another app", "POST", "/v2/zymple/" + b.name + "/blobs/uploads/", a.name, a.token, "", 403},
		{"read own manifest", "GET", "/v2/zymple/" + a.name + "/manifests/release-1", a.name, a.token, "", 403},
		{"read another app's manifest", "GET", "/v2/zymple/" + b.name + "/manifests/release-1", a.name, a.token, "", 403},
		{"catalog", "GET", "/v2/_catalog", a.name, a.token, "", 403},
		{"repository outside the zymple namespace", "POST", "/v2/library/postgres/blobs/uploads/", a.name, a.token, "", 403},
		{"status without a token", "GET", "/api/v1/apps/" + a.name + "/status", "", "", "", 401},
		{"status of B with A's token", "GET", "/api/v1/apps/" + b.name + "/status", "", "", a.token, 401},
		{"status of an unknown app", "GET", "/api/v1/apps/nope/status", "", "", a.token, 401},
		{"own status", "GET", "/api/v1/apps/" + a.name + "/status", "", "", a.token, 200},
		{"operator route on the network listener", "GET", "/admin/apps", "", "", a.token, 404},
		{"operator reload on the network listener", "POST", "/admin/reload", "", "", a.token, 404},
	}
	for _, c := range cases {
		if got := request(c.method, c.path, c.user, c.pass, c.bearer); got != c.want {
			t.Errorf("%s: %s %s = %d, want %d", c.name, c.method, c.path, got, c.want)
		}
	}
}

func TestKeepReleasesPrunesAndRollbackNeedsNoRegistry(t *testing.T) {
	a := newApp(t, "keeper", appOptions{keep: 2})
	for _, v := range []string{"v1", "v2", "v1", "v2"} {
		a.deploy(t, v)
	}
	tags := strings.Fields(mustHost(t, "docker", "images", "--format", "{{.Tag}}", "zymple/"+a.name))
	if len(tags) != 2 || !contains(tags, "release-3") || !contains(tags, "release-4") {
		t.Fatalf("tags after 4 deploys with keep_releases 2 = %v, want release-3 and release-4", tags)
	}
	// A prune of dangling images must not remove what rollback needs.
	mustHost(t, "docker", "image", "prune", "-f")
	mustHost(t, "zymple", "rollback", a.name, "3")
	a.wantVersion(t, "v1")
	if out, err := a.cli("rollback", a.name, "1"); err == nil {
		t.Errorf("rollback to a pruned release succeeded:\n%s", out)
	}
}

func TestOnlyOneDeployAtATime(t *testing.T) {
	a := newApp(t, "busy", appOptions{drain: "6s"})
	a.deploy(t, "v1")
	first := make(chan error, 1)
	go func() {
		_, err := a.cli("deploy", a.name, "--image", "zymple-e2e-app:v2")
		first <- err
	}()
	// Wait until the first deploy holds the lock.
	if err := waitFor(30*time.Second, func() error {
		status, _ := a.cli("status", a.name)
		if strings.Contains(status, "deploying: true") {
			return nil
		}
		return fmt.Errorf("not deploying yet")
	}); err != nil {
		t.Fatal(err)
	}
	out, err := a.cli("rollback", a.name)
	if err == nil || !strings.Contains(out, "already in progress") {
		t.Errorf("second operation was not refused as busy: %v\n%s", err, out)
	}
	if err := <-first; err != nil {
		t.Fatalf("first deploy failed: %v", err)
	}
	a.wantVersion(t, "v2")
}

// deployUntil starts a deploy and returns once its output contains marker.
func (a app) deployUntil(t *testing.T, version, marker string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(cliPath, "deploy", a.name, "--image", "zymple-e2e-app:"+version)
	cmd.Env = append(os.Environ(), "ZYMPLE_HOST="+baseURL(), "ZYMPLE_TOKEN="+a.token)
	var out syncBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(60*time.Second, func() error {
		if strings.Contains(out.String(), marker) {
			return nil
		}
		return fmt.Errorf("deploy output never showed %q:\n%s", marker, out.String())
	}); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func TestCrashDuringDeployConverges(t *testing.T) {
	a := newApp(t, "crashy", appOptions{healthTimeout: "60s", drain: "15s"})
	a.deploy(t, "v1")
	l := a.startLoad()

	// Kill the daemon while a new container waits for its health check:
	// before the commit point, so it must converge back to release 1.
	cmd := a.deployUntil(t, "bad", "==> health")
	mustHost(t, "pkill", "-9", "-x", "zymple")
	_ = cmd.Wait()
	if webs := a.webContainers(t); len(webs) != 2 {
		t.Fatalf("expected the half-started container to exist before recovery, got %v", webs)
	}
	if err := startZymple(); err != nil {
		t.Fatal(err)
	}
	a.wantVersion(t, "v1")
	if webs := a.webContainers(t); len(webs) != 1 || !strings.Contains(webs[0], "web-r1") {
		t.Fatalf("after recovery want only release 1's container, got %v", webs)
	}

	// Kill it again while the old container drains: after the commit point,
	// so it must converge forward to the new release.
	cmd = a.deployUntil(t, "v2", "==> drain")
	mustHost(t, "pkill", "-9", "-x", "zymple")
	_ = cmd.Wait()
	if err := startZymple(); err != nil {
		t.Fatal(err)
	}
	a.wantVersion(t, "v2")
	webs := a.webContainers(t)
	if len(webs) != 1 || strings.Contains(webs[0], "web-r1") {
		t.Fatalf("after recovery want only the new release's container, got %v", webs)
	}
	l.finish(t)

	// And the app deploys normally afterwards.
	a.deploy(t, "v1")
	a.wantVersion(t, "v1")
}

func TestHostRebootBringsAppsBack(t *testing.T) {
	a := newApp(t, "phoenix", appOptions{})
	a.deploy(t, "v1")
	a.deploy(t, "v2")
	if out, err := exec.Command("docker", "restart", hostName).CombinedOutput(); err != nil {
		t.Fatalf("restarting the host: %v\n%s", err, out)
	}
	if err := waitFor(90*time.Second, func() error { return hostRun(nil, "docker", "info") }); err != nil {
		t.Fatal(err)
	}
	if err := startDaemons(); err != nil {
		t.Fatal(err)
	}
	a.wantVersion(t, "v2")
	if status := a.mustCLI(t, "status", a.name); !strings.Contains(status, "release:   2") {
		t.Errorf("status after reboot:\n%s", status)
	}
	a.deploy(t, "v1")
	a.wantVersion(t, "v1")
}

func TestUnsafeComposeIsRejectedWithoutAffectingOthers(t *testing.T) {
	good := newApp(t, "neighbour", appOptions{})
	good.deploy(t, "v1")

	dir := "/etc/zymple/apps/escape"
	files := map[string]string{
		dir + "/app.yml":     "name: escape\ndomains: [escape.localhost]\nweb: { service: web, port: 3000 }\n",
		dir + "/compose.yml": "services:\n  web:\n    image: ${IMAGE}\n    privileged: true\n    volumes: [\"/var/run/docker.sock:/var/run/docker.sock\"]\n",
	}
	for path, content := range files {
		if err := hostWrite(path, content); err != nil {
			t.Fatal(err)
		}
	}
	out, err := hostOutput("zymple", "reload")
	if err == nil || !strings.Contains(out, "privileged") {
		t.Errorf("reload did not report the unsafe definition: %v\n%s", err, out)
	}
	if apps := mustHost(t, "zymple", "apps"); strings.Contains(apps, "escape") {
		t.Errorf("the unsafe app was loaded:\n%s", apps)
	}
	good.wantVersion(t, "v1")
	mustHost(t, "rm", "-rf", dir)
	mustHost(t, "zymple", "reload")
}

func TestEnvFileAndRedeploy(t *testing.T) {
	a := newApp(t, "envy", appOptions{redis: true})
	a.deploy(t, "v1")
	// "$" in a secret must arrive untouched.
	if got := mustHost(t, "zymple", "run", a.name, "--", "printenv", "GREETING"); got != "hello $USER" {
		t.Errorf("GREETING = %q, want %q", got, "hello $USER")
	}
	if got := mustHost(t, "zymple", "run", a.name, "--", "printenv", "REDIS_URL"); got != "redis://redis:6379/0" {
		t.Errorf("REDIS_URL = %q", got)
	}
	if err := hostWrite("/etc/zymple/apps/"+a.name+"/env", "GREETING=changed\n"); err != nil {
		t.Fatal(err)
	}
	l := a.startLoad()
	mustHost(t, "zymple", "redeploy", a.name)
	l.finish(t)
	if got := mustHost(t, "zymple", "run", a.name, "--", "printenv", "GREETING"); got != "changed" {
		t.Errorf("GREETING after redeploy = %q, want changed", got)
	}
}

func TestPostgresPointInTimeRecovery(t *testing.T) {
	a := newApp(t, "ledger", appOptions{postgres: true})
	a.deploy(t, "v1")
	url := mustHost(t, "zymple", "run", a.name, "--", "printenv", "DATABASE_URL")
	if !strings.HasPrefix(url, "postgres://app:") || !strings.Contains(url, "@postgres:5432/app") {
		t.Fatalf("DATABASE_URL = %q", url)
	}
	pg := "zymple-" + a.name + "-postgres"
	sql := func(container, query string) string {
		t.Helper()
		return mustHost(t, "docker", "exec", container, "psql", "-U", "app", "-d", "app", "-qAt", "-c", query)
	}
	notes := func(container string) string {
		return sql(container, "select coalesce(string_agg(note, ',' order by id), '') from t")
	}

	sql(pg, "create table t(id int primary key, note text)")
	sql(pg, "insert into t values (1, 'before')")
	mustHost(t, "zymple", "pg", "backup", a.name, "--type", "full")
	sql(pg, "insert into t values (2, 'kept')")
	time.Sleep(2 * time.Second)
	target := sql(pg, "select to_char(now() at time zone 'utc', 'YYYY-MM-DD HH24:MI:SS') || '+00'")
	time.Sleep(2 * time.Second)
	sql(pg, "insert into t values (3, 'mistake')")

	if info := mustHost(t, "zymple", "pg", "info", a.name); !strings.Contains(info, "full backup") {
		t.Errorf("pg info shows no full backup:\n%s", info)
	}

	// A drill restores beside the live cluster, which keeps serving.
	mustHost(t, "zymple", "pg", "restore", a.name, "--time", target, "--into", "drill")
	drill := pg + "-drill-drill"
	if got := notes(drill); got != "before,kept" {
		t.Errorf("drill cluster holds %q, want before,kept", got)
	}
	if got := notes(pg); got != "before,kept,mistake" {
		t.Errorf("live cluster changed during the drill: %q", got)
	}
	mustHost(t, "zymple", "pg", "drill-rm", a.name, "drill")

	// An in-place restore needs explicit confirmation.
	if out, err := hostOutput("zymple", "pg", "restore", a.name, "--time", target); err == nil {
		t.Fatalf("in-place restore ran without --yes:\n%s", out)
	}
	mustHost(t, "zymple", "pg", "restore", a.name, "--time", target, "--yes")
	if got := notes(pg); got != "before,kept" {
		t.Errorf("after point-in-time restore the table holds %q, want before,kept", got)
	}
	a.wantVersion(t, "v1")

	// The restored cluster still accepts writes, archives and deploys.
	sql(pg, "insert into t values (4, 'after-restore')")
	mustHost(t, "zymple", "pg", "backup", a.name, "--type", "diff")
	a.deploy(t, "v2")
	a.wantVersion(t, "v2")
	if got := notes(pg); got != "before,kept,after-restore" {
		t.Errorf("after a deploy the table holds %q", got)
	}
}

// TestZAggressivePruneIsSurvivable runs last: `docker image prune -a` damages
// every app on the host. It removes the kept releases, and it strips the
// image name even from repositories whose containers are running. Zymple
// cannot prevent that, so this pins down what it guarantees instead.
func TestZAggressivePruneIsSurvivable(t *testing.T) {
	a := newApp(t, "pruned", appOptions{})
	twin := newApp(t, "pruned-twin", appOptions{})
	a.deploy(t, "v1")
	a.deploy(t, "v2")
	twin.deploy(t, "v2") // the same image in two apps: prune keeps one name
	l := a.startLoad()
	mustHost(t, "docker", "image", "prune", "-af")
	// Reload restores the names of the releases that are still running.
	mustHost(t, "zymple", "reload")
	mustHost(t, "zymple", "redeploy", a.name)
	mustHost(t, "zymple", "redeploy", twin.name)
	l.finish(t)
	a.wantVersion(t, "v2")
	twin.wantVersion(t, "v2")
	// Release 1's image name is gone. Its content may or may not survive,
	// depending on what else on the host still uses it. Either way a
	// rollback must end in a clean state: the old version live, or a clear
	// refusal with the current version untouched.
	out, err := a.cli("rollback", a.name, "1")
	if err != nil {
		if !strings.Contains(out, "no longer on this host") {
			t.Errorf("rollback to a pruned image failed without saying why:\n%s", out)
		}
		a.wantVersion(t, "v2")
		// Pushing the image again makes it deployable again.
		a.deploy(t, "v1")
	}
	a.wantVersion(t, "v1")
	if webs := a.webContainers(t); len(webs) != 1 {
		t.Errorf("want exactly one web container, got %v", webs)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// syncBuffer is a bytes.Buffer safe to read while a command writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
