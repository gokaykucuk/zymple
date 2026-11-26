package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaults(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:7080" || cfg.Caddy.TLS != "auto" || cfg.Ports.Low != 20000 || cfg.Backup != nil {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if got := cfg.PostgresImage(17); got != "ghcr.io/gokaykucuk/zymple-postgres:17" {
		t.Errorf("default postgres image = %q", got)
	}
}

func TestParseOverrides(t *testing.T) {
	cfg, err := Parse([]byte(`
listen: "[::1]:9000"
caddy: { tls: internal, reload: ["caddy", "reload"] }
ports: { low: 30000, high: 30010 }
postgres_images: { 17: registry.example/pg:17 }
backup:
  type: s3
  path: prod
  cipher_pass: Secret123
  s3: { endpoint: s3.example.com, bucket: backups, region: eu, access_key: AK, secret_key: SK/+=, path_style: true }
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PostgresImage(17) != "registry.example/pg:17" || cfg.Backup.S3.Bucket != "backups" || !cfg.Backup.S3.PathStyle {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	if cfg.StateDir != "/var/lib/zymple" {
		t.Errorf("unset keys should keep defaults, got %q", cfg.StateDir)
	}
}

func TestParseRejects(t *testing.T) {
	s3 := "s3: { endpoint: e, bucket: b, region: r, access_key: a, secret_key: s }"
	cases := map[string]string{
		"unknown key":         "unknown: 1",
		"public listen":       "listen: 0.0.0.0:7080",
		"relative state dir":  "state_dir: var/lib",
		"relative socket":     "socket: zymple.sock",
		"bad tls":             "caddy: { tls: maybe }",
		"empty reload":        "caddy: { reload: [] }",
		"low port":            "ports: { low: 80, high: 90 }",
		"inverted ports":      "ports: { low: 30000, high: 20000 }",
		"bad backup type":     "backup: { type: ftp }",
		"posix without path":  "backup: { type: posix }",
		"posix relative path": "backup: { type: posix, path: backups }",
		"s3 incomplete":       "backup: { type: s3, s3: { endpoint: e } }",
		"newline in secret":   "backup: { type: s3, cipher_pass: \"a\\nrepo1-path=/\", " + s3 + " }",
		"space in bucket":     "backup: { type: s3, s3: { endpoint: e, bucket: \"b c\", region: r, access_key: a, secret_key: s } }",
		"not yaml":            "listen: [",
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted %q", name, doc)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zymple.yml")
	if _, err := Load(path); err == nil {
		t.Error("a missing file should be an error")
	}
	if err := os.WriteFile(path, []byte("listen: 127.0.0.1:1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || cfg.Listen != "127.0.0.1:1234" {
		t.Fatalf("Load = %+v, %v", cfg, err)
	}
	if err := os.WriteFile(path, []byte("bogus: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("error should name the file, got %v", err)
	}
}

func TestLoadRefusesReadableSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zymple.yml")
	doc := "backup: { type: posix, path: /backups, cipher_pass: Secret }\n"
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a world-readable config with secrets was accepted: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("a private config was refused: %v", err)
	}
	// Without secrets the mode does not matter.
	open := filepath.Join(t.TempDir(), "zymple.yml")
	if err := os.WriteFile(open, []byte("backup: { type: posix, path: /backups }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(open); err != nil {
		t.Fatalf("a config without secrets was refused: %v", err)
	}
}
