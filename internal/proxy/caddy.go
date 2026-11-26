// Package proxy points Caddy at an app's current web container by writing a
// per-app Caddyfile snippet and asking Caddy to reload gracefully.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gokaykucuk/zymple/internal/execx"
	"github.com/gokaykucuk/zymple/internal/releases"
)

// TLS modes for app sites.
const (
	TLSAuto     = "auto"     // public certificates through ACME
	TLSInternal = "internal" // Caddy's local CA, for private networks
	TLSOff      = "off"      // plain HTTP, for tests and for TLS terminated upstream
)

// Caddy manages snippets in Dir. The base Caddyfile imports Dir/*.caddy.
type Caddy struct {
	Dir string
	// Reload is the command that makes Caddy re-read its configuration.
	Reload []string
	TLS    string
	Runner execx.Runner
}

func (c *Caddy) path(app string) string { return filepath.Join(c.Dir, app+".caddy") }

// Snippet renders the site block for an app. Domains are validated hostnames,
// so nothing here can break out of the block.
func (c *Caddy) Snippet(domains []string, port int) string {
	sites := make([]string, len(domains))
	for i, d := range domains {
		if c.TLS == TLSOff {
			d = "http://" + d
		}
		sites[i] = d
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Managed by zymple. Do not edit.\n%s {\n", strings.Join(sites, ", "))
	if c.TLS == TLSInternal {
		b.WriteString("\ttls internal\n")
	}
	fmt.Fprintf(&b, "\treverse_proxy 127.0.0.1:%d\n}\n", port)
	return b.String()
}

// Switch sends an app's traffic to port. If Caddy rejects the reload, the
// previous snippet is put back and Caddy is reloaded again.
func (c *Caddy) Switch(ctx context.Context, app string, domains []string, port int) error {
	path := c.path(app)
	previous, err := os.ReadFile(path)
	hadPrevious := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	next := c.Snippet(domains, port)
	if hadPrevious && string(previous) == next {
		return nil
	}
	if err := releases.WriteFileAtomic(path, []byte(next), 0o644); err != nil {
		return err
	}
	reloadErr := c.reload(ctx)
	if reloadErr == nil {
		return nil
	}
	var restoreErr error
	if hadPrevious {
		restoreErr = releases.WriteFileAtomic(path, previous, 0o644)
	} else {
		restoreErr = os.Remove(path)
	}
	if restoreErr == nil {
		restoreErr = c.reload(ctx)
	}
	if restoreErr != nil {
		return fmt.Errorf("caddy reload failed: %w; restoring the previous snippet also failed: %v", reloadErr, restoreErr)
	}
	return fmt.Errorf("caddy reload failed, previous snippet restored: %w", reloadErr)
}

// Remove deletes an app's snippet and reloads Caddy.
func (c *Caddy) Remove(ctx context.Context, app string) error {
	err := os.Remove(c.path(app))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return c.reload(ctx)
}

func (c *Caddy) reload(ctx context.Context) error {
	if len(c.Reload) == 0 {
		return errors.New("no caddy reload command configured")
	}
	_, err := execx.Output(ctx, c.Runner, c.Reload[0], c.Reload[1:]...)
	return err
}
