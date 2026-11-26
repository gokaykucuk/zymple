package proxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gokaykucuk/zymple/internal/execx"
	"github.com/gokaykucuk/zymple/internal/execx/execxtest"
)

var ctx = context.Background()

func newCaddy(t *testing.T, fake *execxtest.Fake) *Caddy {
	t.Helper()
	return &Caddy{Dir: t.TempDir(), Reload: []string{"caddy", "reload"}, TLS: TLSAuto, Runner: fake}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSnippet(t *testing.T) {
	c := &Caddy{Dir: "/etc/caddy/zymple", TLS: TLSAuto}
	got := c.Snippet("shop", []string{"a.example.com", "b.example.com"})
	want := "a.example.com, b.example.com {\n\tvars zymple_port {file./etc/caddy/zymple/shop.port}\n\treverse_proxy 127.0.0.1:{vars.zymple_port}\n}"
	if !strings.Contains(got, want) {
		t.Errorf("auto snippet:\n%s", got)
	}
	c.TLS = TLSOff
	if got := c.Snippet("shop", []string{"a.example.com"}); !strings.Contains(got, "http://a.example.com {") {
		t.Errorf("off snippet:\n%s", got)
	}
	c.TLS = TLSInternal
	if got := c.Snippet("shop", []string{"a.example.com"}); !strings.Contains(got, "\ttls internal\n") {
		t.Errorf("internal snippet:\n%s", got)
	}
}

func TestSwitchReloadsOnlyWhenTheSnippetChanges(t *testing.T) {
	fake := &execxtest.Fake{}
	c := newCaddy(t, fake)
	domains := []string{"shop.example.com"}

	// First deploy: the snippet is new, so Caddy must load it.
	if err := c.Switch(ctx, "shop", domains, 20001); err != nil {
		t.Fatal(err)
	}
	if read(t, c.PortPath("shop")) != "20001" || fake.Count("caddy reload") != 1 {
		t.Fatalf("first switch: port %q, calls %v", read(t, c.PortPath("shop")), fake.Calls())
	}
	if snippet := read(t, c.snippetPath("shop")); !strings.Contains(snippet, c.PortPath("shop")) {
		t.Fatalf("the snippet does not read the port file:\n%s", snippet)
	}

	// Every later deploy only replaces the port file. No reload, so no
	// listener is touched while traffic is flowing.
	for _, port := range []int{20002, 20003, 20002} {
		if err := c.Switch(ctx, "shop", domains, port); err != nil {
			t.Fatal(err)
		}
	}
	if read(t, c.PortPath("shop")) != "20002" {
		t.Errorf("port file = %q", read(t, c.PortPath("shop")))
	}
	if n := fake.Count("caddy reload"); n != 1 {
		t.Errorf("Caddy was reloaded %d times; a release switch must not reload it", n)
	}
	// The port file is replaced atomically: no temporary files are left.
	entries, _ := os.ReadDir(c.Dir)
	if len(entries) != 2 {
		t.Errorf("unexpected files in the snippet directory: %v", entries)
	}

	// A changed domain list is a snippet change and does reload.
	if err := c.Switch(ctx, "shop", []string{"shop.example.com", "www.example.com"}, 20004); err != nil {
		t.Fatal(err)
	}
	if fake.Count("caddy reload") != 2 || read(t, c.PortPath("shop")) != "20004" {
		t.Errorf("domain change: calls %v", fake.Calls())
	}
}

func TestSwitchRestoresEverythingWhenReloadFails(t *testing.T) {
	fake := &execxtest.Fake{}
	c := newCaddy(t, fake)
	if err := c.Switch(ctx, "shop", []string{"shop.example.com"}, 20001); err != nil {
		t.Fatal(err)
	}
	before := read(t, c.snippetPath("shop"))
	failures := 1
	fake.Handler = func(execx.Cmd) (string, error) {
		if failures > 0 {
			failures--
			return "", errors.New("adapting config: syntax error")
		}
		return "", nil
	}
	err := c.Switch(ctx, "shop", []string{"new.example.com"}, 20002)
	if err == nil || !strings.Contains(err.Error(), "previous state restored") {
		t.Fatalf("want a restored error, got %v", err)
	}
	if read(t, c.snippetPath("shop")) != before || read(t, c.PortPath("shop")) != "20001" {
		t.Fatalf("previous state was not restored: snippet %q, port %q", read(t, c.snippetPath("shop")), read(t, c.PortPath("shop")))
	}

	// With no previous state, both new files are removed.
	failures = 1
	if err := c.Switch(ctx, "fresh", []string{"fresh.example.com"}, 20003); err == nil {
		t.Fatal("want an error")
	}
	for _, path := range []string{c.snippetPath("fresh"), c.PortPath("fresh")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s was left behind: %v", path, err)
		}
	}

	// If the restoring reload fails too, both failures are reported.
	failures = 2
	err = c.Switch(ctx, "shop", []string{"other.example.com"}, 20004)
	if err == nil || !strings.Contains(err.Error(), "also failed") {
		t.Fatalf("want both failures reported, got %v", err)
	}
}

func TestSwitchErrors(t *testing.T) {
	fake := &execxtest.Fake{}
	c := newCaddy(t, fake)
	c.Reload = nil
	if err := c.Switch(ctx, "shop", []string{"shop.example.com"}, 1); err == nil {
		t.Error("a missing reload command should be an error")
	}
	c.Reload = []string{"caddy", "reload"}
	// A directory where a file should be makes it unreadable.
	if err := os.Mkdir(c.snippetPath("dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.Switch(ctx, "dir", []string{"dir.example.com"}, 1); err == nil {
		t.Error("an unreadable snippet should be an error")
	}
	if err := os.Mkdir(c.PortPath("pdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.Switch(ctx, "pdir", []string{"pdir.example.com"}, 1); err == nil {
		t.Error("an unreadable port file should be an error")
	}
	// A snippet directory that cannot be written to.
	blocker := filepath.Join(c.Dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	gone := &Caddy{Dir: filepath.Join(blocker, "sub"), Reload: c.Reload, Runner: fake}
	if err := gone.Switch(ctx, "shop", []string{"shop.example.com"}, 1); err == nil {
		t.Error("an unwritable directory should be an error")
	}
}

func TestRemove(t *testing.T) {
	fake := &execxtest.Fake{}
	c := newCaddy(t, fake)
	if err := c.Remove(ctx, "absent"); err != nil || len(fake.Calls()) != 0 {
		t.Errorf("removing a missing app: %v, %v", err, fake.Calls())
	}
	if err := c.Switch(ctx, "gone", []string{"gone.example.com"}, 2); err != nil {
		t.Fatal(err)
	}
	before := fake.Count("caddy reload")
	if err := c.Remove(ctx, "gone"); err != nil || fake.Count("caddy reload") != before+1 {
		t.Errorf("Remove: %v, calls %v", err, fake.Calls())
	}
	for _, path := range []string{c.snippetPath("gone"), c.PortPath("gone")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was not removed", path)
		}
	}
	// A failing reload is reported and leaves the port file for Caddy.
	if err := c.Switch(ctx, "busy", []string{"busy.example.com"}, 3); err != nil {
		t.Fatal(err)
	}
	fake.Handler = func(execx.Cmd) (string, error) { return "", errors.New("reload failed") }
	if err := c.Remove(ctx, "busy"); err == nil {
		t.Error("a failing reload was not reported")
	}
	if _, err := os.Stat(c.PortPath("busy")); err != nil {
		t.Error("the port file was removed although Caddy still uses it")
	}
	// Something that cannot be removed is reported.
	if err := os.MkdirAll(filepath.Join(c.snippetPath("stuck"), "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(ctx, "stuck"); err == nil {
		t.Error("removing a non-empty directory should fail")
	}
}
