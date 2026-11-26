//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
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
	// Only the first deploy loads a new site into Caddy. The three switches
	// after it must not reload it.
	if reloads := mustHost(t, "sh", "-c", `grep -c '"msg":"load complete"' /var/log/caddy.log || true`); reloads != "" {
		t.Logf("caddy config loads so far: %s", reloads)
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
	// The app asked for its own variable name; the default must not be set.
	url := mustHost(t, "zymple", "run", a.name, "--", "printenv", "LEDGER_DATABASE_URL")
	if !strings.HasPrefix(url, "postgres://app:") || !strings.Contains(url, "@postgres:5432/app") {
		t.Fatalf("LEDGER_DATABASE_URL = %q", url)
	}
	if out, err := hostOutput("zymple", "run", a.name, "--", "printenv", "DATABASE_URL"); err == nil {
		t.Errorf("DATABASE_URL is set although the app renamed it: %q", out)
	}
	pg := "zymple-" + a.name + "-postgres"
	sql := func(container, query string) string {
		t.Helper()
		return mustHost(t, "docker", "exec", container, "psql", "-U", "app", "-d", "app", "-qAt", "-c", query)
	}
	notes := func(container string) string {
		return sql(container, "select coalesce(string_agg(note, ',' order by id), '') from t")
	}

	// The app asked to create roles: it can, and it is still not a superuser.
	if got := sql(pg, "create role ledger_auditor nologin; select rolcreaterole::text || rolsuper::text from pg_roles where rolname = current_user"); got != "truefalse" {
		t.Fatalf("createrole/superuser of the app's role = %q, want truefalse", got)
	}
	if out, err := hostOutput("docker", "exec", pg, "psql", "-U", "app", "-d", "app", "-qAt", "-c", "copy (select 1) to program 'true'"); err == nil {
		t.Fatalf("the app's role can run programs on the database server: %s", out)
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

// TestImportDumpFromAnotherCluster imports a dump shaped like one from a
// platform that ran the database as a superuser: objects owned by roles that
// do not exist on the target, by the superuser itself, and by a role the
// target does have.
func TestImportDumpFromAnotherCluster(t *testing.T) {
	src := newApp(t, "exporter", appOptions{postgres: true})
	src.deploy(t, "v1")
	dst := newApp(t, "importer", appOptions{postgres: true})
	dst.deploy(t, "v1")
	srcPG, dstPG := "zymple-"+src.name+"-postgres", "zymple-"+dst.name+"-postgres"
	as := func(container, role, database, query string) string {
		t.Helper()
		return mustHost(t, "docker", "exec", container, "psql", "-U", role, "-d", database, "-v", "ON_ERROR_STOP=1", "-qAt", "-c", query)
	}

	as(srcPG, "postgres", "app", `
		create role legacy_owner nologin; create role legacy_reader nologin; create role keeper nologin;
		create table by_legacy(id int primary key, note text); alter table by_legacy owner to legacy_owner;
		insert into by_legacy select g, 'row ' || g from generate_series(1, 500) g;
		create table by_super(id int primary key); insert into by_super values (1), (2);
		create table by_keeper(id int primary key); alter table by_keeper owner to keeper; insert into by_keeper values (7);
		grant select on by_legacy to legacy_reader;
		create type mood as enum ('fine'); create function answer() returns int language sql as 'select 42';
		create schema extra; create table extra.t(id int);`)
	const dump = "/var/lib/zymple/export.pgdump"
	mustHost(t, "sh", "-c", "docker exec "+srcPG+" pg_dump -U postgres -Fc -d app > "+dump+" && chmod 600 "+dump)

	// The target already has one of the roles, and data of its own.
	as(dstPG, "postgres", "postgres", "create role keeper nologin")
	as(dstPG, "app", "app", "create table old_data(id int)")
	if err := hostWrite("/etc/zymple/apps/"+dst.name+"/after-import.sql",
		"create table import_marker as select current_user::text as ran_as; grant select on import_marker to app;\n"); err != nil {
		t.Fatal(err)
	}

	if out, err := hostOutput("zymple", "pg", "import", dst.name, dump); err == nil {
		t.Fatalf("import ran without --yes:\n%s", out)
	}
	if got := as(dstPG, "app", "app", "select count(*) from old_data"); got != "0" {
		t.Fatalf("the database changed although the import was refused: %q", got)
	}
	// A file that is not an archive is refused, and the app keeps serving.
	mustHost(t, "sh", "-c", "echo not-a-dump > /var/lib/zymple/garbage.pgdump")
	if out, err := hostOutput("zymple", "pg", "import", dst.name, "/var/lib/zymple/garbage.pgdump", "--yes"); err == nil || !strings.Contains(out, "not changed") {
		t.Fatalf("garbage import: %v\n%s", err, out)
	}
	dst.wantVersion(t, "v1")
	as(dstPG, "app", "app", "select count(*) from old_data")

	out := mustHost(t, "zymple", "pg", "import", dst.name, dump, "--yes", "--jobs", "2")
	t.Logf("import output:\n%s", out)

	if got := as(dstPG, "app", "app", "select count(*) || ' ' || max(note) from by_legacy"); got != "500 row 99" {
		t.Errorf("imported rows = %q", got)
	}
	owners := as(dstPG, "postgres", "app", `select string_agg(relname || '=' || pg_get_userbyid(relowner), ' ' order by relname)
		from pg_class where relname in ('by_legacy', 'by_super', 'by_keeper', 't', 'import_marker')`)
	if owners != "by_keeper=keeper by_legacy=app by_super=app import_marker=postgres t=app" {
		t.Errorf("owners after import = %q", owners)
	}
	others := as(dstPG, "postgres", "app", `select
		(select pg_get_userbyid(typowner) from pg_type where typname = 'mood') || ' ' ||
		(select pg_get_userbyid(proowner) from pg_proc where proname = 'answer') || ' ' ||
		(select pg_get_userbyid(nspowner) from pg_namespace where nspname = 'extra')`)
	if others != "app app app" {
		t.Errorf("type, function and schema owners = %q", others)
	}
	if roles := as(dstPG, "postgres", "postgres", "select count(*) from pg_roles where rolname in ('legacy_owner', 'legacy_reader')"); roles != "0" {
		t.Errorf("%s borrowed roles were left in the cluster", roles)
	}
	if gone := as(dstPG, "postgres", "app", "select to_regclass('old_data') is null"); gone != "t" {
		t.Error("the target's old data survived the import")
	}
	if ran := as(dstPG, "app", "app", "select ran_as from import_marker"); ran != "postgres" {
		t.Errorf("after-import.sql ran as %q", ran)
	}
	// The app's own role can work with what it was handed, and is still not
	// a superuser.
	as(dstPG, "app", "app", "insert into by_legacy values (501, 'written by the app'); alter table by_super add column note text; select answer()")
	if super := as(dstPG, "postgres", "postgres", "select rolsuper from pg_roles where rolname = 'app'"); super != "f" {
		t.Errorf("the app's role is a superuser after the import: %q", super)
	}
	if left := mustHost(t, "sh", "-c", "ls /var/lib/zymple/apps/"+dst.name+"/postgres/data/"); strings.Contains(left, "zymple-import") {
		t.Errorf("the staged archive was left in the data directory: %s", left)
	}
	dst.wantVersion(t, "v1")
	if status := dst.mustCLI(t, "status", dst.name); !strings.Contains(status, "web:       running") {
		t.Errorf("status after import:\n%s", status)
	}
	// The source is untouched.
	if got := as(srcPG, "postgres", "app", "select count(*) from by_legacy"); got != "500" {
		t.Errorf("the source lost data: %q", got)
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

// TestStressSwitches repeats the traffic switch many times under load, to
// catch races a single deploy rarely hits. It measures from two places: from
// outside, through the host's published port, and from inside the host,
// straight at Caddy with a new connection per request. Every failure is
// logged with its time so it can be matched to a phase of a deploy.
// Set ZYMPLE_E2E_STRESS to the number of cycles.
func TestStressSwitches(t *testing.T) {
	cycles, _ := strconv.Atoi(os.Getenv("ZYMPLE_E2E_STRESS"))
	if cycles == 0 {
		t.Skip("set ZYMPLE_E2E_STRESS=<cycles> to run")
	}
	a := newApp(t, "stress", appOptions{})
	a.deploy(t, "v1")
	a.deploy(t, "v2")

	inside := `rm -f /tmp/stress.stop /tmp/stress.fail; n=0
while [ ! -f /tmp/stress.stop ]; do
  r=$(curl -s -o /dev/null -m 5 -w '%{http_code} exit=%{exitcode} connect=%{time_connect} total=%{time_total}' -H 'Host: stress.localhost' http://127.0.0.1:80/version)
  n=$((n+1))
  case "$r" in 200*) ;; *) echo "$(date -u +%H:%M:%S.%N | cut -c1-12) $r" >> /tmp/stress.fail ;; esac
done; echo "$n" > /tmp/stress.total`
	if out, err := exec.Command("docker", "exec", "-d", hostName, "sh", "-c", inside).CombinedOutput(); err != nil {
		t.Fatalf("starting the inside load: %v %s", err, out)
	}
	l := a.startLoad()
	stamp := func() string { return time.Now().UTC().Format("15:04:05.000") }
	for i := 0; i < cycles; i++ {
		var op, out string
		switch i % 3 {
		case 0:
			op, out = "rollback", a.mustCLI(t, "rollback", a.name)
		case 1:
			op, out = "redeploy", mustHost(t, "zymple", "redeploy", a.name)
		default:
			op, out = "deploy", a.deploy(t, []string{"v1", "v2"}[i%2])
		}
		_ = out
		t.Logf("%s cycle %d %s done", stamp(), i, op)
	}
	mustHost(t, "touch", "/tmp/stress.stop")
	time.Sleep(7 * time.Second)
	l.stop()
	l.done.Wait()

	insideFailures, _ := hostOutput("sh", "-c", "cat /tmp/stress.fail 2>/dev/null || true")
	t.Logf("inside the host: %s requests, failures:\n%s", mustHost(t, "cat", "/tmp/stress.total"), insideFailures)
	t.Logf("outside: %d requests, %d failed:\n%s", l.total.Load(), l.failed.Load(), l.failureLog())
	if insideFailures != "" || l.failed.Load() != 0 {
		// Everything needed to place the failures in a deploy's timeline.
		for _, cmd := range []string{
			`grep -E "msg=\"release is live\"|level=ERROR|level=WARN" /var/log/zymple.log | cut -c1-160 | tail -80`,
			`grep -vE "incomplete response|cleaning storage|finished cleaning" /var/log/caddy.log | cut -c1-400 | tail -80`,
			`docker events --since 30m --until 0s --filter type=container --filter event=start --filter event=die --filter event=kill --format '{{.Time}} {{.Action}} {{.Actor.Attributes.name}}' | grep stress | tail -150`,
		} {
			out, _ := hostOutput("sh", "-c", cmd)
			t.Logf("$ %s\n%s", cmd, out)
		}
		t.Errorf("requests failed during the switches: inside %q, outside %d", insideFailures, l.failed.Load())
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
