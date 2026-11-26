// Package config loads the server configuration file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/gokaykucuk/zymple/internal/proxy"
)

// DefaultPath is where the server looks for its configuration.
const DefaultPath = "/etc/zymple/zymple.yml"

// Default images for the managed services. They are pinned by digest: these
// containers see app data and backup credentials, and a tag can be moved.
// Bump the version and the digest together.
const (
	DefaultRedisImage  = "redis:7.4.12-alpine@sha256:7f7d30b7fdd09467bac58198a1f166dd69789d348d4fe859cccb20e0cf287fbe"
	DefaultResticImage = "restic/restic:0.17.3@sha256:8f5a62b422a2cb1277ea0dd6e826fe1acf649e5b9f02d60e5268d5fd1976255a"
)

// Caddy configures the reverse proxy integration.
type Caddy struct {
	SnippetsDir string   `yaml:"snippets_dir"`
	Reload      []string `yaml:"reload"`
	TLS         string   `yaml:"tls"`
}

// Containerd locates the image store Docker uses.
type Containerd struct {
	Socket    string `yaml:"socket"`
	Namespace string `yaml:"namespace"`
}

// Ports is the loopback port range for web containers.
type Ports struct {
	Low  int `yaml:"low"`
	High int `yaml:"high"`
}

// S3 is an S3-compatible backup repository.
type S3 struct {
	Endpoint  string `yaml:"endpoint"`
	Bucket    string `yaml:"bucket"`
	Region    string `yaml:"region"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	// PathStyle selects path-style URLs, which most non-AWS services need.
	PathStyle bool `yaml:"path_style"`
}

// Backup configures where database backups go.
type Backup struct {
	// Type is "s3" or "posix" (a directory on the host, such as a mount).
	Type string `yaml:"type"`
	// Path is the host directory for type posix, or the key prefix for s3.
	Path string `yaml:"path"`
	S3   S3     `yaml:"s3"`
	// CipherPass encrypts the repository. Required for Redis backups.
	CipherPass string `yaml:"cipher_pass"`
}

// Config is the server configuration.
type Config struct {
	Listen     string     `yaml:"listen"`
	Socket     string     `yaml:"socket"`
	AppsDir    string     `yaml:"apps_dir"`
	StateDir   string     `yaml:"state_dir"`
	Caddy      Caddy      `yaml:"caddy"`
	Containerd Containerd `yaml:"containerd"`
	Ports      Ports      `yaml:"ports"`
	Backup     *Backup    `yaml:"backup"`
	// PostgresImages maps a major version to an image reference.
	PostgresImages map[int]string `yaml:"postgres_images"`
	RedisImage     string         `yaml:"redis_image"`
	ResticImage    string         `yaml:"restic_image"`
}

var safeValueRe = regexp.MustCompile(`^[A-Za-z0-9._:/@+=-]*$`)

// Default returns the configuration used when a key is absent.
func Default() Config {
	return Config{
		Listen:      "127.0.0.1:7080",
		Socket:      "/run/zymple/zymple.sock",
		AppsDir:     "/etc/zymple/apps",
		StateDir:    "/var/lib/zymple",
		Caddy:       Caddy{SnippetsDir: "/etc/caddy/zymple", Reload: []string{"systemctl", "reload", "caddy"}, TLS: proxy.TLSAuto},
		Containerd:  Containerd{Socket: "/run/containerd/containerd.sock", Namespace: "moby"},
		Ports:       Ports{Low: 20000, High: 29999},
		RedisImage:  DefaultRedisImage,
		ResticImage: DefaultResticImage,
	}
}

// PostgresImage returns the image for a major version.
func (c *Config) PostgresImage(major int) string {
	if image, ok := c.PostgresImages[major]; ok {
		return image
	}
	return fmt.Sprintf("ghcr.io/gokaykucuk/zymple-postgres:%d", major)
}

// Parse reads a configuration document over the defaults.
func Parse(data []byte) (*Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Load reads the configuration file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Backup != nil && (cfg.Backup.S3.SecretKey != "" || cfg.Backup.CipherPass != "") {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("%s holds backup secrets and is readable by group or others; chmod 600 it", path)
		}
	}
	return cfg, nil
}

func (c *Config) validate() error {
	for name, dir := range map[string]string{
		"socket": c.Socket, "apps_dir": c.AppsDir, "state_dir": c.StateDir, "caddy.snippets_dir": c.Caddy.SnippetsDir,
	} {
		if !filepath.IsAbs(dir) {
			return fmt.Errorf("%s must be an absolute path", name)
		}
	}
	if !strings.HasPrefix(c.Listen, "127.0.0.1:") && !strings.HasPrefix(c.Listen, "[::1]:") && !strings.HasPrefix(c.Listen, "localhost:") {
		return errors.New("listen must be a loopback address; Caddy is the only public listener")
	}
	switch c.Caddy.TLS {
	case proxy.TLSAuto, proxy.TLSInternal, proxy.TLSOff:
	default:
		return fmt.Errorf("caddy.tls must be auto, internal or off")
	}
	if len(c.Caddy.Reload) == 0 {
		return errors.New("caddy.reload must not be empty")
	}
	if c.Ports.Low < 1024 || c.Ports.High > 65535 || c.Ports.Low > c.Ports.High {
		return errors.New("ports must satisfy 1024 <= low <= high <= 65535")
	}
	if c.Backup != nil {
		return c.Backup.validate()
	}
	return nil
}

func (b *Backup) validate() error {
	// These values are written into a pgbackrest ini file, one per line.
	for name, v := range map[string]string{
		"backup.path": b.Path, "backup.s3.endpoint": b.S3.Endpoint, "backup.s3.bucket": b.S3.Bucket,
		"backup.s3.region": b.S3.Region, "backup.s3.access_key": b.S3.AccessKey,
		"backup.s3.secret_key": b.S3.SecretKey, "backup.cipher_pass": b.CipherPass,
	} {
		if !safeValueRe.MatchString(v) {
			return fmt.Errorf("%s contains characters outside %s", name, safeValueRe)
		}
	}
	switch b.Type {
	case "posix":
		if !filepath.IsAbs(b.Path) {
			return errors.New("backup.path must be an absolute directory for type posix")
		}
	case "s3":
		if b.S3.Endpoint == "" || b.S3.Bucket == "" || b.S3.Region == "" || b.S3.AccessKey == "" || b.S3.SecretKey == "" {
			return errors.New("backup.s3 needs endpoint, bucket, region, access_key and secret_key")
		}
	default:
		return errors.New("backup.type must be s3 or posix")
	}
	return nil
}
