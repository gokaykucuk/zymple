package runtime

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gokaykucuk/zymple/internal/appdef"
	"github.com/gokaykucuk/zymple/internal/execx"
	"github.com/gokaykucuk/zymple/internal/execx/execxtest"
)

const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func testApp(t *testing.T) *appdef.App {
	t.Helper()
	app, err := appdef.Parse(
		[]byte("name: shop\ndomains: [shop.example.com]\nweb: { service: web, port: 3000 }\nrelease: [\"bin/migrate\", \"--all\"]\n"),
		[]byte("services:\n  web:\n    image: ${IMAGE}\n  worker:\n    image: ${IMAGE}\n  cron:\n    image: ${IMAGE}\n"))
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// psHandler answers `docker ps` with the given lines.
func psHandler(lines ...string) func(execx.Cmd) (string, error) {
	return func(c execx.Cmd) (string, error) {
		if len(c.Args) > 0 && c.Args[0] == "ps" {
			return strings.Join(lines, "\n") + "\n", nil
		}
		return "", nil
	}
}

func TestComposeFiles(t *testing.T) {
	d := &Docker{Runner: &execxtest.Fake{}, StateDir: t.TempDir()}
	if d.HasCompose("shop", 3) {
		t.Fatal("no compose file yet")
	}
	if err := d.WriteCompose("shop", 3, []byte("name: x\n")); err != nil || !d.HasCompose("shop", 3) {
		t.Fatalf("WriteCompose: %v", err)
	}
	if err := d.RemoveCompose("shop", 3); err != nil || d.HasCompose("shop", 3) {
		t.Fatalf("RemoveCompose: %v", err)
	}
}

func TestImageExists(t *testing.T) {
	fake := &execxtest.Fake{}
	d := &Docker{Runner: fake}
	ctx := context.Background()
	if ok, err := d.ImageExists(ctx, "shop", digest); !ok || err != nil {
		t.Fatalf("present image: %v, %v", ok, err)
	}
	if got := fake.Find("image inspect"); !strings.HasSuffix(got, "zymple/shop@"+digest) {
		t.Errorf("inspected %q", got)
	}
	fake.Handler = func(execx.Cmd) (string, error) { return "", errors.New("Error response from daemon: No such image: x") }
	if ok, err := d.ImageExists(ctx, "shop", digest); ok || err != nil {
		t.Fatalf("missing image: %v, %v", ok, err)
	}
	fake.Handler = func(execx.Cmd) (string, error) { return "", errors.New("Cannot connect to the Docker daemon") }
	if ok, err := d.ImageExists(ctx, "shop", digest); ok || err == nil {
		t.Fatalf("daemon down must be an error, not a missing image: %v, %v", ok, err)
	}
}

func TestEnsureNetwork(t *testing.T) {
	fake := &execxtest.Fake{}
	d := &Docker{Runner: fake}
	if err := d.EnsureNetwork(context.Background(), "shop"); err != nil || fake.Count("network create") != 0 {
		t.Fatalf("existing network: %v, %v", err, fake.Calls())
	}
	fake.Handler = func(c execx.Cmd) (string, error) {
		if c.Args[1] == "inspect" {
			return "", errors.New("no such network")
		}
		return "", nil
	}
	if err := d.EnsureNetwork(context.Background(), "shop"); err != nil {
		t.Fatal(err)
	}
	if got := fake.Find("network create"); !strings.HasSuffix(got, "zymple-shop") {
		t.Errorf("created %q", got)
	}
}

func TestComposeCommands(t *testing.T) {
	fake := &execxtest.Fake{}
	d := &Docker{Runner: fake, StateDir: "/state"}
	app := testApp(t)
	ctx := context.Background()
	var out bytes.Buffer
	prefix := "docker compose -p zymple-shop -f /state/apps/shop/releases/4/compose.yml "

	if err := d.RunRelease(ctx, app, 4, &out); err != nil {
		t.Fatal(err)
	}
	if got, want := fake.Calls()[0], prefix+"--progress quiet run --rm --no-deps -T web-r4 bin/migrate --all"; got != want {
		t.Errorf("RunRelease:\n got %s\nwant %s", got, want)
	}
	fake.Reset()
	if err := d.Exec(ctx, app, 4, []string{"printenv", "X"}, &out); err != nil {
		t.Fatal(err)
	}
	if got := fake.Calls()[0]; !strings.HasSuffix(got, "web-r4 printenv X") {
		t.Errorf("Exec: %s", got)
	}
	fake.Reset()
	if err := d.StartWeb(ctx, app, 4, &out); err != nil {
		t.Fatal(err)
	}
	if got, want := fake.Calls()[0], prefix+"up -d --no-deps web-r4"; got != want {
		t.Errorf("StartWeb:\n got %s\nwant %s", got, want)
	}
}

func TestContainersParsing(t *testing.T) {
	fake := &execxtest.Fake{Handler: psHandler(
		"abc running 4 web-r4",
		"def exited 3 web-r3",
		"",
		"bad line",
		"ghi running notanumber web",
		"jkl created 5",
	)}
	d := &Docker{Runner: fake}
	list, err := d.Containers(context.Background(), "shop", appdef.RoleWeb)
	if err != nil || len(list) != 3 {
		t.Fatalf("Containers = %+v, %v", list, err)
	}
	if !list[0].Running || list[0].Release != 4 || list[0].Service != "web-r4" || list[1].Running || list[2].Service != "" {
		t.Errorf("parsed wrong: %+v", list)
	}
	call := fake.Calls()[0]
	if !strings.Contains(call, "label=zymple.app=shop") || !strings.Contains(call, "label=zymple.role=web") {
		t.Errorf("filters missing: %s", call)
	}
	// Regression: `compose run` containers carry the service's labels and
	// were taken for the web container.
	if !strings.Contains(call, "label=com.docker.compose.oneoff=False") {
		t.Errorf("one-off containers are not filtered out: %s", call)
	}
	fake.Handler = func(execx.Cmd) (string, error) { return "", errors.New("daemon down") }
	if _, err := d.Containers(context.Background(), "shop", appdef.RoleWeb); err == nil {
		t.Error("want an error")
	}
}

func TestStopAndRemoveWeb(t *testing.T) {
	fake := &execxtest.Fake{Handler: psHandler("abc running 4 web-r4", "def running 3 web-r3")}
	d := &Docker{Runner: fake}
	ctx := context.Background()
	if err := d.StopWeb(ctx, "shop", 3); err != nil {
		t.Fatal(err)
	}
	if fake.Find("docker stop def") == "" || fake.Find("docker rm -f def") == "" || fake.Count("abc") != 0 {
		t.Errorf("StopWeb touched the wrong containers: %v", fake.Calls())
	}
	fake.Reset()
	if err := d.RemoveWeb(ctx, "shop", 4); err != nil || fake.Find("docker rm -f abc") == "" || fake.Count("docker stop") != 0 {
		t.Errorf("RemoveWeb: %v, %v", err, fake.Calls())
	}
	fake.Reset()
	if err := d.RemoveWeb(ctx, "shop", 9); err != nil || len(fake.Calls()) != 1 {
		t.Errorf("removing an absent release should do nothing: %v", fake.Calls())
	}
	if err := d.StopWeb(ctx, "shop", 9); err != nil {
		t.Error(err)
	}
	fake.Handler = func(c execx.Cmd) (string, error) {
		if c.Args[0] == "ps" {
			return "abc running 4 web-r4\n", nil
		}
		return "", errors.New("stop failed")
	}
	if err := d.StopWeb(ctx, "shop", 4); err == nil {
		t.Error("a failing stop must be reported")
	}
}

func TestStartContainers(t *testing.T) {
	fake := &execxtest.Fake{}
	d := &Docker{Runner: fake}
	if err := d.StartContainers(context.Background(), nil); err != nil || len(fake.Calls()) != 0 {
		t.Fatalf("nothing to start: %v", fake.Calls())
	}
	if err := d.StartContainers(context.Background(), []string{"a", "b"}); err != nil || fake.Calls()[0] != "docker start a b" {
		t.Fatalf("StartContainers: %v", fake.Calls())
	}
}

func TestUpServicesRemovesStaleServices(t *testing.T) {
	fake := &execxtest.Fake{Handler: psHandler("w1 running 3 worker", "old1 running 3 mailer")}
	d := &Docker{Runner: fake, StateDir: "/state"}
	var out bytes.Buffer
	if err := d.UpServices(context.Background(), testApp(t), 4, &out); err != nil {
		t.Fatal(err)
	}
	if fake.Find("docker rm -f old1") == "" {
		t.Errorf("stale service container was not removed: %v", fake.Calls())
	}
	if got := fake.Find("up -d"); !strings.HasSuffix(got, "up -d --no-deps cron worker") {
		t.Errorf("up command: %s", got)
	}

	// An app with only a web service has nothing to bring up.
	solo, err := appdef.Parse([]byte("name: solo\ndomains: [solo.example.com]\nweb: { service: web, port: 80 }\n"),
		[]byte("services:\n  web:\n    image: ${IMAGE}\n"))
	if err != nil {
		t.Fatal(err)
	}
	fake.Reset()
	fake.Handler = psHandler()
	if err := d.UpServices(context.Background(), solo, 1, &out); err != nil || fake.Count("up -d") != 0 {
		t.Errorf("web-only app ran compose: %v", fake.Calls())
	}
	fake.Handler = func(execx.Cmd) (string, error) { return "", errors.New("down") }
	if err := d.UpServices(context.Background(), solo, 1, &out); err == nil {
		t.Error("want an error when listing fails")
	}
}

func TestStopAllAndStartAll(t *testing.T) {
	fake := &execxtest.Fake{Handler: func(c execx.Cmd) (string, error) {
		if c.Args[0] != "ps" {
			return "", nil
		}
		if strings.Contains(c.String(), "role=service") {
			return "svc running 4 worker\n", nil
		}
		return "web running 4 web-r4\n", nil
	}}
	d := &Docker{Runner: fake}
	if err := d.StopAll(context.Background(), "shop"); err != nil || fake.Find("docker stop svc web") == "" {
		t.Fatalf("StopAll: %v, %v", err, fake.Calls())
	}
	// A one-off command still running is stopped with the app...
	for _, call := range fake.Calls() {
		if strings.HasPrefix(call, "docker ps") && strings.Contains(call, "oneoff") {
			t.Errorf("StopAll skipped one-off containers: %s", call)
		}
	}
	fake.Reset()
	if err := d.StartAll(context.Background(), "shop"); err != nil || fake.Find("docker start svc web") == "" {
		t.Fatalf("StartAll: %v, %v", err, fake.Calls())
	}
	// ...but never started a second time.
	if n := fake.Count("label=com.docker.compose.oneoff=False"); n != 2 {
		t.Errorf("StartAll must leave one-off containers alone: %v", fake.Calls())
	}
	fake.Reset()
	fake.Handler = psHandler()
	if err := d.StopAll(context.Background(), "shop"); err != nil || fake.Count("docker stop") != 0 {
		t.Errorf("nothing to stop: %v", fake.Calls())
	}
	fake.Handler = func(execx.Cmd) (string, error) { return "", errors.New("down") }
	if err := d.StopAll(context.Background(), "shop"); err == nil {
		t.Error("want an error")
	}
}

func TestTags(t *testing.T) {
	fake := &execxtest.Fake{Handler: func(execx.Cmd) (string, error) {
		return "release-1 " + digest + "\n<none> " + digest + "\n\npush-abc <none>\npush-def\n", nil
	}}
	d := &Docker{Runner: fake}
	ctx := context.Background()
	tags, err := d.Tags(ctx, "shop")
	want := []Tag{{Name: "release-1", Digest: digest}, {Name: "push-abc"}, {Name: "push-def"}}
	if err != nil || len(tags) != len(want) {
		t.Fatalf("Tags = %v, %v", tags, err)
	}
	for i := range want {
		if tags[i] != want[i] {
			t.Errorf("tag %d = %+v, want %+v", i, tags[i], want[i])
		}
	}
	if call := fake.Calls()[0]; call != "docker images --digests --format {{.Tag}} {{.Digest}} zymple/shop" {
		t.Errorf("listed tags with %q", call)
	}
	fake.Handler = nil
	if err := d.Pin(ctx, "shop", digest, 7, ""); err != nil || fake.Find("docker tag zymple/shop@"+digest+" zymple/shop:release-7") == "" {
		t.Errorf("Pin: %v", fake.Calls())
	}
	if err := d.Pin(ctx, "shop", digest, 7, "ocr"); err != nil || fake.Find("docker tag zymple/shop@"+digest+" zymple/shop:release-7-ocr") == "" {
		t.Errorf("Pin of an extra image: %v", fake.Calls())
	}
	if err := d.Repin(ctx, "shop", digest, 7, "ocr"); err != nil || fake.Find("docker tag "+digest+" zymple/shop:release-7-ocr") == "" {
		t.Errorf("Repin of an extra image: %v", fake.Calls())
	}
	if err := d.RemoveTag(ctx, "shop", "push-abc"); err != nil || fake.Find("docker rmi zymple/shop:push-abc") == "" {
		t.Errorf("RemoveTag: %v", fake.Calls())
	}
	fake.Handler = func(execx.Cmd) (string, error) { return "", errors.New("down") }
	if _, err := d.Tags(ctx, "shop"); err == nil {
		t.Error("want an error")
	}
}

func TestLogs(t *testing.T) {
	fake := &execxtest.Fake{Handler: func(c execx.Cmd) (string, error) {
		if c.Args[0] == "ps" {
			return "old exited 3 worker\nnew running 4 worker\nother running 4 cron\n", nil
		}
		return "line one\n", nil
	}}
	d := &Docker{Runner: fake}
	var out bytes.Buffer
	if err := d.Logs(context.Background(), "shop", appdef.RoleService, "worker", 50, true, &out); err != nil {
		t.Fatal(err)
	}
	if fake.Find("docker logs --tail 50 -f new") == "" || out.String() != "line one\n" {
		t.Errorf("Logs: %v, output %q", fake.Calls(), out.String())
	}
	if err := d.Logs(context.Background(), "shop", appdef.RoleService, "absent", 10, false, &out); err == nil {
		t.Error("want an error for a service with no running container")
	}
	fake.Handler = func(execx.Cmd) (string, error) { return "", errors.New("down") }
	if err := d.Logs(context.Background(), "shop", appdef.RoleWeb, "", 10, false, &out); err == nil {
		t.Error("want an error")
	}
}

func TestComposeCommandsReportFailuresAndQuietOrphanWarnings(t *testing.T) {
	var env []string
	fake := &execxtest.Fake{Handler: func(c execx.Cmd) (string, error) {
		env = c.Env
		return "", errors.New("compose failed")
	}}
	d := &Docker{Runner: fake, StateDir: "/state"}
	var out bytes.Buffer
	if err := d.StartWeb(context.Background(), testApp(t), 2, &out); err == nil || !strings.Contains(err.Error(), "docker compose") {
		t.Fatalf("error = %v", err)
	}
	found := false
	for _, kv := range env {
		found = found || kv == "COMPOSE_IGNORE_ORPHANS=true"
	}
	if !found {
		t.Error("compose runs without COMPOSE_IGNORE_ORPHANS")
	}
}

// Regression: Containers leaves one-off containers out, so nothing removed
// a release command that a crash of the daemon had left running.
func TestRemoveOneOffs(t *testing.T) {
	fake := &execxtest.Fake{Handler: psHandler(
		"mig running 5 web-r5",
		"con running 4 web-r4",
		"old exited 3 web-r3",
		"nolabel running",
	)}
	d := &Docker{Runner: fake}
	ctx := context.Background()
	if err := d.RemoveOneOffs(ctx, "shop", func(release int) bool { return release != 4 }); err != nil {
		t.Fatal(err)
	}
	ps := fake.Find("docker ps")
	if !strings.Contains(ps, "label=zymple.app=shop") || !strings.Contains(ps, "label=com.docker.compose.oneoff=True") {
		t.Errorf("one-offs of the app are not selected: %s", ps)
	}
	if strings.Contains(ps, "oneoff=False") || !strings.Contains(ps, " -a ") {
		t.Errorf("wrong listing: %s", ps)
	}
	if fake.Find("docker rm -f mig old") == "" || fake.Count("con") != 0 {
		t.Errorf("removed the wrong containers: %v", fake.Calls())
	}
	fake.Reset()
	if err := d.RemoveOneOffs(ctx, "shop", func(int) bool { return false }); err != nil || len(fake.Calls()) != 1 {
		t.Errorf("nothing selected should remove nothing: %v, %v", err, fake.Calls())
	}
	fake.Handler = func(c execx.Cmd) (string, error) {
		if c.Args[0] == "ps" {
			return "mig running 5 web-r5\n", nil
		}
		return "", errors.New("rm failed")
	}
	if err := d.RemoveOneOffs(ctx, "shop", func(int) bool { return true }); err == nil {
		t.Error("a failing removal must be reported")
	}
	fake.Handler = func(execx.Cmd) (string, error) { return "", errors.New("daemon down") }
	if err := d.RemoveOneOffs(ctx, "shop", func(int) bool { return true }); err == nil {
		t.Error("a failing listing must be reported")
	}
}

// Regression: any tag starting with "release-" counted as Zymple's own.
func TestParseReleaseTag(t *testing.T) {
	for _, n := range []int{1, 7, 10, 123456} {
		if got, ok := ParseReleaseTag(ReleaseTag(n)); !ok || got != n {
			t.Errorf("ParseReleaseTag(%q) = %d, %v", ReleaseTag(n), got, ok)
		}
	}
	for _, tag := range []string{
		"", "release", "release-", "release-0", "release--1", "release-+1", "release-01", "release-1.4.0",
		"release-1 ", "release-1a", "release-0x10", "release-1_000", "Release-1", "push-1", "1",
		"release-99999999999999999999999999",
		"release-1-", "release-1-OCR", "release-1-1x", "release-1-ocr-x", "release-01-ocr", "release-0-ocr",
		"release-1-ocr ", "release-1-" + strings.Repeat("a", 32), "release--ocr", "release-1--ocr",
	} {
		if n, ok := ParseReleaseTag(tag); ok {
			t.Errorf("ParseReleaseTag(%q) = %d, want it refused", tag, n)
		}
	}
}

// The tags of a release's extra images are Zymple's own as well.
func TestParseReleaseTagOfAnExtraImage(t *testing.T) {
	for _, image := range []string{"ocr", "a", "a_b", "x9", strings.Repeat("a", 31)} {
		tag := ImageTag(12, image)
		if want := "release-12-" + image; tag != want {
			t.Errorf("ImageTag = %q, want %q", tag, want)
		}
		if n, ok := ParseReleaseTag(tag); !ok || n != 12 {
			t.Errorf("ParseReleaseTag(%q) = %d, %v", tag, n, ok)
		}
	}
	if ImageTag(3, "") != ReleaseTag(3) {
		t.Error("the app's own image keeps the plain release tag")
	}
}

// FuzzParseReleaseTag: tags come from whoever pushes to the registry.
func FuzzParseReleaseTag(f *testing.F) {
	for _, seed := range []string{"release-1", "release-1.4.0", "release-", "release-007", "latest", "release-3-ocr", "release-3-a_b", "release-3-"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, tag string) {
		n, ok := ParseReleaseTag(tag)
		if !ok {
			return
		}
		image := strings.TrimPrefix(strings.TrimPrefix(tag, ReleaseTag(n)), "-")
		if n < 1 || ImageTag(n, image) != tag || (image != "" && !appdef.ValidImageName(image)) {
			t.Fatalf("ParseReleaseTag(%q) = %d, which is not what ImageTag writes", tag, n)
		}
	})
}
