// Package proxy points Caddy at an app's current web container.
//
// Each app has two files in Caddy's snippet directory: a Caddyfile snippet
// that names the app's domains, and a port file holding the loopback port of
// the release that is live. The snippet tells Caddy to read the port file on
// every request, so switching releases is an atomic rename of that file and
// Caddy is not reloaded.
//
// Reloading on every deploy would work most of the time, but a reload makes
// Caddy replace its listening sockets, and a connection that arrives in that
// instant can be dropped. Caddy is reloaded only when an app's snippet itself
// changes: on its first deploy, and when its domains change.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

func (c *Caddy) snippetPath(app string) string { return filepath.Join(c.Dir, app+".caddy") }

// PortPath is the file Caddy reads an app's upstream port from.
func (c *Caddy) PortPath(app string) string { return filepath.Join(c.Dir, app+".port") }

// Snippet renders the site block for an app. Domains are validated hostnames
// and the app name is validated too, so nothing here can break out of the
// block.
func (c *Caddy) Snippet(app string, domains []string) string {
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
	// {file.*} is evaluated for every request, which is what makes a switch
	// take effect without a reload. It goes through a variable because Caddy
	// cannot parse an upstream address that has a file path inside it.
	fmt.Fprintf(&b, "\tvars zymple_port {file.%s}\n", c.PortPath(app))
	b.WriteString("\treverse_proxy 127.0.0.1:{vars.zymple_port}\n}\n")
	return b.String()
}

// file is a file's previous state, for putting it back.
type file struct {
	path    string
	content []byte
	existed bool
}

func snapshot(path string) (file, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return file{path: path}, nil
	}
	if err != nil {
		return file{}, err
	}
	return file{path: path, content: content, existed: true}, nil
}

func (f file) restore() error {
	if !f.existed {
		err := os.Remove(f.path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return releases.WriteFileAtomic(f.path, f.content, 0o644)
}

// Switch sends an app's traffic to port.
//
// When the snippet is already in place this is one atomic rename and cannot
// fail halfway. When the snippet has to change, the port file is written
// first, then the snippet, then Caddy is reloaded; if Caddy rejects the
// reload both files are put back and Caddy is reloaded again.
func (c *Caddy) Switch(ctx context.Context, app string, domains []string, port int) error {
	oldSnippet, err := snapshot(c.snippetPath(app))
	if err != nil {
		return err
	}
	oldPort, err := snapshot(c.PortPath(app))
	if err != nil {
		return err
	}
	// No trailing newline: Caddy uses the file's content as the port.
	if err := releases.WriteFileAtomic(c.PortPath(app), []byte(strconv.Itoa(port)), 0o644); err != nil {
		return err
	}
	snippet := c.Snippet(app, domains)
	if oldSnippet.existed && string(oldSnippet.content) == snippet {
		return nil
	}
	if err := releases.WriteFileAtomic(c.snippetPath(app), []byte(snippet), 0o644); err != nil {
		return errors.Join(err, oldPort.restore())
	}
	reloadErr := c.reload(ctx)
	if reloadErr == nil {
		return nil
	}
	restoreErr := errors.Join(oldSnippet.restore(), oldPort.restore())
	if restoreErr == nil {
		restoreErr = c.reload(ctx)
	}
	if restoreErr != nil {
		return fmt.Errorf("caddy reload failed: %w; restoring the previous state also failed: %v", reloadErr, restoreErr)
	}
	return fmt.Errorf("caddy reload failed, previous state restored: %w", reloadErr)
}

// Remove deletes an app's snippet and port file and reloads Caddy.
func (c *Caddy) Remove(ctx context.Context, app string) error {
	err := os.Remove(c.snippetPath(app))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// Reload first, so Caddy stops reading the port file before it is gone.
	if err := c.reload(ctx); err != nil {
		return err
	}
	if err := os.Remove(c.PortPath(app)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (c *Caddy) reload(ctx context.Context) error {
	if len(c.Reload) == 0 {
		return errors.New("no caddy reload command configured")
	}
	_, err := execx.Output(ctx, c.Runner, c.Reload[0], c.Reload[1:]...)
	return err
}
