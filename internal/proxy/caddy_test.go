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

func newCaddy(t *testing.T, fake *execxtest.Fake) *Caddy {
	t.Helper()
	return &Caddy{Dir: t.TempDir(), Reload: []string{"caddy", "reload"}, TLS: TLSAuto, Runner: fake}
}

func TestSnippet(t *testing.T) {
	c := &Caddy{TLS: TLSAuto}
	got := c.Snippet([]string{"a.example.com", "b.example.com"}, 20001)
	if !strings.Contains(got, "a.example.com, b.example.com {\n\treverse_proxy 127.0.0.1:20001\n}") {
		t.Errorf("auto snippet:\n%s", got)
	}
	c.TLS = TLSOff
	if got := c.Snippet([]string{"a.example.com"}, 1); !strings.Contains(got, "http://a.example.com {") {
		t.Errorf("off snippet:\n%s", got)
	}
	c.TLS = TLSInternal
	if got := c.Snippet([]string{"a.example.com"}, 1); !strings.Contains(got, "\ttls internal\n") {
		t.Errorf("internal snippet:\n%s", got)
	}
}

func TestSwitch(t *testing.T) {
	fake := &execxtest.Fake{}
	c := newCaddy(t, fake)
	ctx := context.Background()
	if err := c.Switch(ctx, "shop", []string{"shop.example.com"}, 20001); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(c.Dir, "shop.caddy"))
	if !strings.Contains(string(data), "127.0.0.1:20001") || fake.Count("caddy reload") != 1 {
		t.Fatalf("first switch: %q, calls %v", data, fake.Calls())
	}
	// Switching to what is already live must not reload Caddy.
	if err := c.Switch(ctx, "shop", []string{"shop.example.com"}, 20001); err != nil || fake.Count("caddy reload") != 1 {
		t.Fatalf("unchanged switch reloaded: %v, %v", err, fake.Calls())
	}
	if err := c.Switch(ctx, "shop", []string{"shop.example.com"}, 20002); err != nil || fake.Count("caddy reload") != 2 {
		t.Fatalf("second switch: %v, %v", err, fake.Calls())
	}
}

func TestSwitchRestoresPreviousSnippetWhenReloadFails(t *testing.T) {
	fake := &execxtest.Fake{}
	c := newCaddy(t, fake)
	ctx := context.Background()
	if err := c.Switch(ctx, "shop", []string{"shop.example.com"}, 20001); err != nil {
		t.Fatal(err)
	}
	failures := 1
	fake.Handler = func(execx.Cmd) (string, error) {
		if failures > 0 {
			failures--
			return "", errors.New("adapting config: syntax error")
		}
		return "", nil
	}
	err := c.Switch(ctx, "shop", []string{"shop.example.com"}, 20002)
	if err == nil || !strings.Contains(err.Error(), "previous snippet restored") {
		t.Fatalf("want a restored error, got %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(c.Dir, "shop.caddy"))
	if !strings.Contains(string(data), "127.0.0.1:20001") {
		t.Fatalf("previous snippet was not restored: %q", data)
	}

	// With no previous snippet, the failed one is removed.
	failures = 1
	if err := c.Switch(ctx, "fresh", []string{"fresh.example.com"}, 20003); err == nil {
		t.Fatal("want an error")
	}
	if _, err := os.Stat(filepath.Join(c.Dir, "fresh.caddy")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed first snippet was left behind: %v", err)
	}

	// If the restoring reload fails too, both failures are reported.
	failures = 2
	err = c.Switch(ctx, "shop", []string{"shop.example.com"}, 20004)
	if err == nil || !strings.Contains(err.Error(), "also failed") {
		t.Fatalf("want both failures reported, got %v", err)
	}
}

func TestSwitchAndRemoveErrors(t *testing.T) {
	fake := &execxtest.Fake{}
	c := newCaddy(t, fake)
	ctx := context.Background()
	c.Reload = nil
	if err := c.Switch(ctx, "shop", []string{"shop.example.com"}, 1); err == nil {
		t.Error("a missing reload command should be an error")
	}
	c.Reload = []string{"caddy", "reload"}
	// A directory where the snippet should be makes it unreadable.
	if err := os.Mkdir(filepath.Join(c.Dir, "dir.caddy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.Switch(ctx, "dir", []string{"dir.example.com"}, 1); err == nil {
		t.Error("an unreadable snippet should be an error")
	}
	if err := c.Remove(ctx, "absent"); err != nil {
		t.Errorf("removing a missing snippet: %v", err)
	}
	if err := c.Switch(ctx, "gone", []string{"gone.example.com"}, 2); err != nil {
		t.Fatal(err)
	}
	before := fake.Count("caddy reload")
	if err := c.Remove(ctx, "gone"); err != nil || fake.Count("caddy reload") != before+1 {
		t.Errorf("Remove: %v, calls %v", err, fake.Calls())
	}
	if err := os.WriteFile(filepath.Join(c.Dir, "dir.caddy", "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(ctx, "dir"); err == nil {
		t.Error("removing a non-empty directory should fail")
	}
}
