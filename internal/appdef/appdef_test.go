package appdef

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const validApp = `name: shop
domains: [shop.example.com, www.shop.example.com]
compose: compose.yml
web:
  service: web
  port: 3000
  healthcheck: /healthz
  health_timeout: 45s
  drain: 10s
release: ["bin/migrate", "--all"]
keep_releases: 3
postgres: { version: 17, pitr_days: 30 }
redis: { persistent: true }
env: secrets/apps/shop.env
`

const validCompose = `services:
  web:
    image: ${IMAGE}
    environment:
      MODE: production
    volumes: ["uploads:/data/uploads", "cache:/cache:ro"]
  worker:
    image: ${IMAGE}
    command: ["bin/worker"]
    environment: ["QUEUE=default"]
volumes:
  uploads:
  cache: {}
`

func TestParseValid(t *testing.T) {
	app, err := Parse([]byte(validApp), []byte(validCompose))
	if err != nil {
		t.Fatal(err)
	}
	if app.Name != "shop" || len(app.Domains) != 2 || app.Web.Port != 3000 ||
		time.Duration(app.Web.HealthTimeout) != 45*time.Second || time.Duration(app.Web.Drain) != 10*time.Second ||
		app.KeepReleases != 3 || app.Postgres.PITRDays != 30 || !app.Redis.Persistent {
		t.Fatalf("unexpected app: %+v", app)
	}
	if got := app.OtherServices(); len(got) != 1 || got[0] != "worker" {
		t.Errorf("OtherServices = %v", got)
	}
	if got := app.Compose.Volumes; len(got) != 2 || got[0] != "cache" {
		t.Errorf("Volumes = %v", got)
	}
}

func TestParseDefaults(t *testing.T) {
	app, err := Parse([]byte("name: shop\ndomains: [shop.example.com]\nweb: { service: web, port: 80 }\npostgres: { version: 16 }\n"),
		[]byte("services:\n  web:\n    image: ${IMAGE}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if app.KeepReleases != DefaultKeepReleases || time.Duration(app.Web.HealthTimeout) != DefaultHealthTimeout ||
		time.Duration(app.Web.Drain) != DefaultDrain || app.Web.Healthcheck != "/" ||
		app.ComposeFile != "compose.yml" || app.Postgres.PITRDays != DefaultPITRDays {
		t.Fatalf("defaults not applied: %+v", app)
	}
}

func TestParseRejectsBadAppDefinitions(t *testing.T) {
	base := map[string]any{}
	if err := yaml.Unmarshal([]byte(validApp), &base); err != nil {
		t.Fatal(err)
	}
	mutate := func(fn func(m map[string]any)) []byte {
		m := map[string]any{}
		_ = yaml.Unmarshal([]byte(validApp), &m)
		fn(m)
		out, _ := yaml.Marshal(m)
		return out
	}
	web := func(m map[string]any) map[string]any { return m["web"].(map[string]any) }
	cases := map[string][]byte{
		"unknown key":         mutate(func(m map[string]any) { m["privileged"] = true }),
		"uppercase name":      mutate(func(m map[string]any) { m["name"] = "Shop" }),
		"one-letter name":     mutate(func(m map[string]any) { m["name"] = "s" }),
		"name with slash":     mutate(func(m map[string]any) { m["name"] = "a/b" }),
		"no domains":          mutate(func(m map[string]any) { m["domains"] = []string{} }),
		"wildcard domain":     mutate(func(m map[string]any) { m["domains"] = []string{"*.example.com"} }),
		"domain with path":    mutate(func(m map[string]any) { m["domains"] = []string{"example.com/x"} }),
		"domain with brace":   mutate(func(m map[string]any) { m["domains"] = []string{"example.com {\n respond ok\n}"} }),
		"domain with space":   mutate(func(m map[string]any) { m["domains"] = []string{"a.com b.com"} }),
		"bare hostname":       mutate(func(m map[string]any) { m["domains"] = []string{"localhost"} }),
		"compose path":        mutate(func(m map[string]any) { m["compose"] = "../other/compose.yml" }),
		"compose dotdot":      mutate(func(m map[string]any) { m["compose"] = ".." }),
		"bad service name":    mutate(func(m map[string]any) { web(m)["service"] = "Web Service" }),
		"missing web service": mutate(func(m map[string]any) { web(m)["service"] = "api" }),
		"port zero":           mutate(func(m map[string]any) { web(m)["port"] = 0 }),
		"port too large":      mutate(func(m map[string]any) { web(m)["port"] = 70000 }),
		"healthcheck url":     mutate(func(m map[string]any) { web(m)["healthcheck"] = "http://evil.example/" }),
		"healthcheck spaces":  mutate(func(m map[string]any) { web(m)["healthcheck"] = "/a b" }),
		"bad duration":        mutate(func(m map[string]any) { web(m)["health_timeout"] = "soon" }),
		"numeric duration":    mutate(func(m map[string]any) { web(m)["health_timeout"] = []int{1} }),
		"timeout too long":    mutate(func(m map[string]any) { web(m)["health_timeout"] = "11m" }),
		"timeout too short":   mutate(func(m map[string]any) { web(m)["health_timeout"] = "10ms" }),
		"negative drain":      mutate(func(m map[string]any) { web(m)["drain"] = "-1s" }),
		"drain too long":      mutate(func(m map[string]any) { web(m)["drain"] = "6m" }),
		"empty release arg":   mutate(func(m map[string]any) { m["release"] = []string{"bin/migrate", ""} }),
		"keep too many":       mutate(func(m map[string]any) { m["keep_releases"] = 51 }),
		"keep negative":       mutate(func(m map[string]any) { m["keep_releases"] = -1 }),
		"old postgres":        mutate(func(m map[string]any) { m["postgres"] = map[string]any{"version": 9} }),
		"pitr too long":       mutate(func(m map[string]any) { m["postgres"] = map[string]any{"version": 17, "pitr_days": 400} }),
		"not yaml":            []byte("name: [unterminated"),
	}
	for name, data := range cases {
		if _, err := Parse(data, []byte(validCompose)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse([]byte(validApp), []byte("services: {}")); err == nil {
		t.Error("empty compose accepted")
	}
	big := make([]byte, maxFileSize+1)
	if _, err := Parse(big, []byte(validCompose)); err == nil {
		t.Error("oversized definition accepted")
	}
}

// escapeAttempts is the corpus of compose files that must never load. Each
// one tries to reach the host, another app, or the Docker daemon.
var escapeAttempts = map[string]string{
	"privileged":                         "services:\n  web:\n    image: ${IMAGE}\n    privileged: true\n",
	"host network":                       "services:\n  web:\n    image: ${IMAGE}\n    network_mode: host\n",
	"host pid":                           "services:\n  web:\n    image: ${IMAGE}\n    pid: host\n",
	"host ipc":                           "services:\n  web:\n    image: ${IMAGE}\n    ipc: host\n",
	"host userns":                        "services:\n  web:\n    image: ${IMAGE}\n    userns_mode: host\n",
	"cap_add":                            "services:\n  web:\n    image: ${IMAGE}\n    cap_add: [SYS_ADMIN]\n",
	"devices":                            "services:\n  web:\n    image: ${IMAGE}\n    devices: [\"/dev/sda:/dev/sda\"]\n",
	"security_opt":                       "services:\n  web:\n    image: ${IMAGE}\n    security_opt: [\"seccomp:unconfined\"]\n",
	"docker socket":                      "services:\n  web:\n    image: ${IMAGE}\n    volumes: [\"/var/run/docker.sock:/var/run/docker.sock\"]\n",
	"host bind mount":                    "services:\n  web:\n    image: ${IMAGE}\n    volumes: [\"/etc:/host-etc\"]\n",
	"relative bind mount":                "services:\n  web:\n    image: ${IMAGE}\n    volumes: [\"./data:/data\"]\n",
	"home bind mount":                    "services:\n  web:\n    image: ${IMAGE}\n    volumes: [\"~/.ssh:/ssh\"]\n",
	"long-form bind":                     "services:\n  web:\n    image: ${IMAGE}\n    volumes:\n      - type: bind\n        source: /\n        target: /host\n",
	"undeclared volume":                  "services:\n  web:\n    image: ${IMAGE}\n    volumes: [\"data:/data\"]\n",
	"volume with driver":                 "services:\n  web:\n    image: ${IMAGE}\n    volumes: [\"data:/data\"]\nvolumes:\n  data:\n    driver_opts: { type: none, o: bind, device: / }\n",
	"external volume":                    "services:\n  web:\n    image: ${IMAGE}\n    volumes: [\"data:/data\"]\nvolumes:\n  data:\n    external: true\n",
	"volume mode":                        "services:\n  web:\n    image: ${IMAGE}\n    volumes: [\"data:/data:z\"]\nvolumes:\n  data:\n",
	"volume relative path":               "services:\n  web:\n    image: ${IMAGE}\n    volumes: [\"data:data\"]\nvolumes:\n  data:\n",
	"volumes_from":                       "services:\n  web:\n    image: ${IMAGE}\n    volumes_from: [other]\n",
	"build":                              "services:\n  web:\n    image: ${IMAGE}\n    build: .\n",
	"published ports":                    "services:\n  web:\n    image: ${IMAGE}\n    ports: [\"80:80\"]\n",
	"other image":                        "services:\n  web:\n    image: alpine\n",
	"image with suffix":                  "services:\n  web:\n    image: ${IMAGE}-debug\n",
	"no image":                           "services:\n  web:\n    command: [\"true\"]\n",
	"second image":                       "services:\n  web:\n    image: ${IMAGE}\n  side:\n    image: docker:dind\n",
	"interpolated value":                 "services:\n  web:\n    image: ${IMAGE}\n    command: [\"sh\", \"-c\", \"echo ${HOME}\"]\n",
	"interpolated key":                   "services:\n  web:\n    image: ${IMAGE}\n    environment:\n      ${KEY}: value\n",
	"interpolated user":                  "services:\n  web:\n    image: ${IMAGE}\n    user: ${UID}\n",
	"root user":                          "services:\n  web:\n    image: ${IMAGE}\n    user: root\n",
	"uid zero":                           "services:\n  web:\n    image: ${IMAGE}\n    user: \"0:0\"\n",
	"numeric zero user":                  "services:\n  web:\n    image: ${IMAGE}\n    user: 0\n",
	"anchor":                             "x: &a\n  privileged: true\nservices:\n  web:\n    image: ${IMAGE}\n",
	"alias":                              "services:\n  web: &w\n    image: ${IMAGE}\n  other: *w\n",
	"merge key":                          "services:\n  web:\n    <<: { privileged: true }\n    image: ${IMAGE}\n",
	"extends":                            "services:\n  web:\n    image: ${IMAGE}\n    extends: { file: /etc/other.yml, service: x }\n",
	"env_file":                           "services:\n  web:\n    image: ${IMAGE}\n    env_file: [/etc/shadow]\n",
	"networks":                           "services:\n  web:\n    image: ${IMAGE}\n    networks: [other]\n",
	"top-level networks":                 "services:\n  web:\n    image: ${IMAGE}\nnetworks:\n  default: { external: true, name: zymple-other }\n",
	"top-level include":                  "include: [/etc/other.yml]\nservices:\n  web:\n    image: ${IMAGE}\n",
	"top-level secrets":                  "services:\n  web:\n    image: ${IMAGE}\nsecrets:\n  s: { file: /etc/shadow }\n",
	"top-level name":                     "name: zymple-other\nservices:\n  web:\n    image: ${IMAGE}\n",
	"labels":                             "services:\n  web:\n    image: ${IMAGE}\n    labels: { zymple.app: other }\n",
	"container_name":                     "services:\n  web:\n    image: ${IMAGE}\n    container_name: zymple-other-postgres\n",
	"depends_on":                         "services:\n  web:\n    image: ${IMAGE}\n    depends_on: [web]\n",
	"sysctls":                            "services:\n  web:\n    image: ${IMAGE}\n    sysctls: { net.ipv4.ip_forward: 1 }\n",
	"cgroup_parent":                      "services:\n  web:\n    image: ${IMAGE}\n    cgroup_parent: /\n",
	"pull_policy":                        "services:\n  web:\n    image: ${IMAGE}\n    pull_policy: always\n",
	"extra_hosts":                        "services:\n  web:\n    image: ${IMAGE}\n    extra_hosts: [\"postgres:1.2.3.4\"]\n",
	"reserved env":                       "services:\n  web:\n    image: ${IMAGE}\n    environment: { DATABASE_URL: postgres://elsewhere }\n",
	"reserved env list":                  "services:\n  web:\n    image: ${IMAGE}\n    environment: [\"REDIS_URL=redis://elsewhere\"]\n",
	"env without value":                  "services:\n  web:\n    image: ${IMAGE}\n    environment: { PASSTHROUGH: }\n",
	"env list passthrough":               "services:\n  web:\n    image: ${IMAGE}\n    environment: [PASSTHROUGH]\n",
	"env wrong type":                     "services:\n  web:\n    image: ${IMAGE}\n    environment: 5\n",
	"bad service name":                   "services:\n  Web_Service!:\n    image: ${IMAGE}\n",
	"service not a mapping":              "services:\n  web: [1, 2]\n",
	"services not mapping":               "services: [web]\n",
	"volumes not mapping":                "services:\n  web:\n    image: ${IMAGE}\nvolumes: [data]\n",
	"bad volume name":                    "services:\n  web:\n    image: ${IMAGE}\nvolumes:\n  ../x:\n",
	"service volumes map":                "services:\n  web:\n    image: ${IMAGE}\n    volumes: { a: b }\nvolumes:\n  a:\n",
	"user double zero":                   "services:\n  web:\n    image: ${IMAGE}\n    user: \"00\"\n",
	"user plus zero":                     "services:\n  web:\n    image: ${IMAGE}\n    user: \"+0\"\n",
	"user double zero with group":        "services:\n  web:\n    image: ${IMAGE}\n    user: \"00:5\"\n",
	"user hex zero":                      "services:\n  web:\n    image: ${IMAGE}\n    user: \"0x0\"\n",
	"user space zero":                    "services:\n  web:\n    image: ${IMAGE}\n    user: \" 0\"\n",
	"user zero trailing space":           "services:\n  web:\n    image: ${IMAGE}\n    user: \"0 \"\n",
	"user zero zero":                     "services:\n  web:\n    image: ${IMAGE}\n    user: \"0:0\"\n",
	"user root with gid":                 "services:\n  web:\n    image: ${IMAGE}\n    user: \"root:1\"\n",
	"user minus zero":                    "services:\n  web:\n    image: ${IMAGE}\n    user: \"-0\"\n",
	"user float zero string":             "services:\n  web:\n    image: ${IMAGE}\n    user: \"0.0\"\n",
	"user float zero":                    "services:\n  web:\n    image: ${IMAGE}\n    user: 0.0\n",
	"user int zero":                      "services:\n  web:\n    image: ${IMAGE}\n    user: 0\n",
	"user yaml octal zero":               "services:\n  web:\n    image: ${IMAGE}\n    user: 00\n",
	"user yaml hex zero":                 "services:\n  web:\n    image: ${IMAGE}\n    user: 0x0\n",
	"user signed group":                  "services:\n  web:\n    image: ${IMAGE}\n    user: \"1000:+0\"\n",
	"user padded group":                  "services:\n  web:\n    image: ${IMAGE}\n    user: \"1000:00\"\n",
	"user empty":                         "services:\n  web:\n    image: ${IMAGE}\n    user: \"\"\n",
	"user empty before group":            "services:\n  web:\n    image: ${IMAGE}\n    user: \":0\"\n",
	"user empty group":                   "services:\n  web:\n    image: ${IMAGE}\n    user: \"1000:\"\n",
	"user three parts":                   "services:\n  web:\n    image: ${IMAGE}\n    user: \"1000:1000:0\"\n",
	"user uppercase root":                "services:\n  web:\n    image: ${IMAGE}\n    user: \"ROOT\"\n",
	"user uid overflow":                  "services:\n  web:\n    image: ${IMAGE}\n    user: \"4294967296\"\n",
	"user wraps to zero":                 "services:\n  web:\n    image: ${IMAGE}\n    user: 4294967296\n",
	"user mapping":                       "services:\n  web:\n    image: ${IMAGE}\n    user: { name: root }\n",
	"user list":                          "services:\n  web:\n    image: ${IMAGE}\n    user: [root]\n",
	"user bool":                          "services:\n  web:\n    image: ${IMAGE}\n    user: true\n",
	"user float":                         "services:\n  web:\n    image: ${IMAGE}\n    user: 1e3\n",
	"user yaml octal":                    "services:\n  web:\n    image: ${IMAGE}\n    user: 01000\n",
	"user yaml hex":                      "services:\n  web:\n    image: ${IMAGE}\n    user: 0x3E8\n",
	"user yaml new octal":                "services:\n  web:\n    image: ${IMAGE}\n    user: 0o17\n",
	"user yaml binary":                   "services:\n  web:\n    image: ${IMAGE}\n    user: 0b11\n",
	"user yaml signed":                   "services:\n  web:\n    image: ${IMAGE}\n    user: +1\n",
	"user yaml underscore":               "services:\n  web:\n    image: ${IMAGE}\n    user: 1_000\n",
	"user yaml octal with group":         "services:\n  web:\n    image: ${IMAGE}\n    user: 01000:5\n",
	"user null":                          "services:\n  web:\n    image: ${IMAGE}\n    user: null\n",
	"user tilde":                         "services:\n  web:\n    image: ${IMAGE}\n    user: ~\n",
	"user blank":                         "services:\n  web:\n    image: ${IMAGE}\n    user:\n",
	"user tagged binary":                 "services:\n  web:\n    image: ${IMAGE}\n    user: !!binary cm9vdA==\n",
	"user tagged string zero":            "services:\n  web:\n    image: ${IMAGE}\n    user: !!str 0\n",
	"user uid above docker limit":        "services:\n  web:\n    image: ${IMAGE}\n    user: \"2147483648\"\n",
	"user uid above limit unquoted":      "services:\n  web:\n    image: ${IMAGE}\n    user: 2147483648\n",
	"user uid 32-bit max":                "services:\n  web:\n    image: ${IMAGE}\n    user: \"4294967295\"\n",
	"user gid above docker limit":        "services:\n  web:\n    image: ${IMAGE}\n    user: \"1000:2147483648\"\n",
	"user gid 32-bit max":                "services:\n  web:\n    image: ${IMAGE}\n    user: \"1000:4294967295\"\n",
	"non-string key hides interpolation": "services:\n  web:\n    image: ${IMAGE}\n    healthcheck:\n      1: x\n      test: [\"CMD\", \"${HOME}\"]\n",
	"non-string key in a list":           "services:\n  web:\n    image: ${IMAGE}\n    command: [{ 1: x }]\n",
	"empty document":                     "",
	"not yaml":                           "services: [unterminated",
}

func TestComposeEscapeAttemptsAreRejected(t *testing.T) {
	for name, compose := range escapeAttempts {
		if c, err := ParseCompose([]byte(compose)); err == nil {
			t.Errorf("%s: accepted: %+v", name, c)
		}
	}
}

func TestUserAllowList(t *testing.T) {
	for _, user := range []string{`1000`, `"1000"`, `'1000'`, `"1000:1000"`, `"1000:0"`, `1000:1000`, `nobody`, `"app:app"`, `"_svc:100"`,
		`1`, `2147483647`, `"2147483647"`, `"2147483647:2147483647"`, `"app:2147483647"`} {
		c, err := ParseCompose([]byte("services:\n  web:\n    image: ${IMAGE}\n    user: " + user + "\n"))
		if err != nil {
			t.Errorf("user %s was refused: %v", user, err)
			continue
		}
		if got, want := c.Services["web"]["user"], strings.Trim(user, `"'`); got != want {
			t.Errorf("user %s is kept as %#v, want the string %q", user, got, want)
		}
	}
}

// Regression: an unquoted user was decoded by YAML before it was checked, so
// 01000 passed as 512 and 0x3E8 as 1000. The text in the file is what is
// checked, and it is what the rendered file carries, as a string.
func TestUserIsCheckedAndRenderedAsWritten(t *testing.T) {
	const app = "name: shop\ndomains: [a.example.com]\nweb: { service: web, port: 80 }\n"
	compose := func(user string) []byte {
		return []byte("services:\n  web:\n    image: ${IMAGE}\n    user: " + user + "\n")
	}
	for _, user := range []string{"01000", "0x3E8", "0o17", "0b11", "+1", "1_000"} {
		if c, err := ParseCompose(compose(user)); err == nil {
			t.Errorf("user %s was accepted as %#v", user, c.Services["web"]["user"])
		}
	}
	for _, user := range []string{"1000", `"1000:0"`, "2147483647"} {
		parsed, err := Parse([]byte(app), compose(user))
		if err != nil {
			t.Fatalf("user %s was refused: %v", user, err)
		}
		parsed.Dir = t.TempDir()
		out, err := parsed.Render(RenderInput{Release: 1, Digest: "sha256:" + strings.Repeat("c", 64), Port: 20000})
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Services map[string]map[string]any `yaml:"services"`
		}
		if err := yaml.Unmarshal(out, &doc); err != nil {
			t.Fatal(err)
		}
		if got, want := doc.Services["web-r1"]["user"], strings.Trim(user, `"`); got != want {
			t.Errorf("user %s is rendered as %#v, want the string %q:\n%s", user, got, want, out)
		}
	}
}

// Regression: a service named like a release of the web service replaced the
// web service in the rendered file, and one named like a managed service
// shadowed its network alias.
func TestReservedServiceNames(t *testing.T) {
	const app = "name: shop\ndomains: [a.example.com]\nweb: { service: web, port: 80 }\n"
	compose := func(name string) []byte {
		return []byte("services:\n  web:\n    image: ${IMAGE}\n  " + name + ":\n    image: ${IMAGE}\n")
	}
	cases := []struct {
		extra, service string
		ok             bool
	}{
		{"", "web-r1", false},
		{"", "web-r250", false},
		{"", "web-r", true},
		{"", "web-rx1", true},
		{"", "worker-r1", true},
		{"", "postgres", true},
		{"", "redis", true},
		{"postgres: { version: 17 }\n", "postgres", false},
		{"postgres: { version: 17 }\n", "postgres-drill-x", false},
		{"postgres: { version: 17 }\n", "postgres-exporter", true},
		{"postgres: { version: 17 }\n", "redis", true},
		{"redis: {}\n", "redis", false},
		{"redis: {}\n", "redis-worker", true},
	}
	for _, c := range cases {
		_, err := Parse([]byte(app+c.extra), compose(c.service))
		if c.ok && err != nil {
			t.Errorf("%q with %q was refused: %v", c.service, c.extra, err)
		}
		if !c.ok && (err == nil || !strings.Contains(err.Error(), "reserved")) {
			t.Errorf("%q with %q: %v, want a reserved-name error", c.service, c.extra, err)
		}
	}
}

func TestRender(t *testing.T) {
	app, err := Parse([]byte(validApp), []byte(validCompose))
	if err != nil {
		t.Fatal(err)
	}
	app.Dir = t.TempDir()
	if app.EnvFile() != "" {
		t.Fatal("EnvFile should be empty when the file is absent")
	}
	envPath := filepath.Join(app.Dir, "env")
	if err := os.WriteFile(envPath, []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("c", 64)
	data, err := app.Render(RenderInput{Release: 7, Digest: digest, Port: 20123,
		Env: map[string]string{"DATABASE_URL": "postgres://app:p$ss@postgres/app"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "p$ss") || !strings.Contains(string(data), "p$$ss") {
		t.Errorf("\"$\" was not escaped for compose:\n%s", data)
	}
	var doc struct {
		Name     string                    `yaml:"name"`
		Services map[string]map[string]any `yaml:"services"`
		Networks map[string]map[string]any `yaml:"networks"`
		Volumes  map[string]any            `yaml:"volumes"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Name != "zymple-shop" || doc.Networks["default"]["name"] != "zymple-shop" || doc.Networks["default"]["external"] != true {
		t.Errorf("project or network wrong: %+v %+v", doc.Name, doc.Networks)
	}
	if len(doc.Volumes) != 2 {
		t.Errorf("volumes = %v", doc.Volumes)
	}
	web, ok := doc.Services["web-r7"]
	if !ok || doc.Services["web"] != nil {
		t.Fatalf("web service was not renamed per release: %v", doc.Services)
	}
	worker := doc.Services["worker"]
	for name, svc := range map[string]map[string]any{"web": web, "worker": worker} {
		if svc["image"] != "zymple/shop@"+digest || svc["pull_policy"] != "never" || svc["restart"] != "unless-stopped" {
			t.Errorf("%s: image, pull policy or restart wrong: %v", name, svc)
		}
		if opts, _ := svc["security_opt"].([]any); len(opts) != 1 || opts[0] != "no-new-privileges:true" {
			t.Errorf("%s: security_opt = %v", name, svc["security_opt"])
		}
		labels := svc["labels"].(map[string]any)
		if labels[LabelApp] != "shop" || labels[LabelRelease] != "7" {
			t.Errorf("%s: labels = %v", name, labels)
		}
		files := svc["env_file"].([]any)
		if entry := files[0].(map[string]any); entry["path"] != envPath || entry["format"] != "raw" {
			t.Errorf("%s: env_file = %v", name, files)
		}
	}
	if ports := web["ports"].([]any); ports[0] != "127.0.0.1:20123:3000" {
		t.Errorf("web ports = %v", ports)
	}
	if web["labels"].(map[string]any)[LabelRole] != RoleWeb || worker["labels"].(map[string]any)[LabelRole] != RoleService {
		t.Error("role labels wrong")
	}
	if worker["ports"] != nil {
		t.Error("worker must not publish ports")
	}
	if env := worker["environment"].(map[string]any); env["QUEUE"] != "default" || env["DATABASE_URL"] == nil {
		t.Errorf("worker environment = %v", env)
	}
	if env := web["environment"].(map[string]any); env["MODE"] != "production" {
		t.Errorf("web environment = %v", env)
	}
}

func TestNames(t *testing.T) {
	if Repository("shop") != "zymple/shop" || Network("shop") != "zymple-shop" || Project("shop") != "zymple-shop" ||
		WebService("web", 3) != "web-r3" || ImageRef("shop", "sha256:x") != "zymple/shop@sha256:x" {
		t.Fatal("naming helpers changed")
	}
	for _, bad := range []string{"", "a", "A1", "a_b", "-ab", "ab-", strings.Repeat("a", 33)} {
		if ValidName(bad) {
			t.Errorf("ValidName(%q) = true", bad)
		}
	}
}

func writeApp(t *testing.T, root, dir, appYAML, compose string) {
	t.Helper()
	path := filepath.Join(root, dir)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "app.yml"), []byte(appYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if compose != "" {
		if err := os.WriteFile(filepath.Join(path, "compose.yml"), []byte(compose), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadAll(t *testing.T) {
	root := t.TempDir()
	simple := func(name, domain string) string {
		return "name: " + name + "\ndomains: [" + domain + "]\nweb: { service: web, port: 80 }\n"
	}
	compose := "services:\n  web:\n    image: ${IMAGE}\n"
	writeApp(t, root, "alpha", simple("alpha", "alpha.example.com"), compose)
	writeApp(t, root, "beta", simple("beta", "beta.example.com"), compose)
	writeApp(t, root, "clash", simple("clash", "alpha.example.com"), compose)
	writeApp(t, root, "renamed", simple("other", "renamed.example.com"), compose)
	writeApp(t, root, "unsafe", simple("unsafe", "unsafe.example.com"), "services:\n  web:\n    image: ${IMAGE}\n    privileged: true\n")
	writeApp(t, root, "nocompose", simple("nocompose", "nc.example.com"), "")
	writeApp(t, root, "badyaml", "name: [", compose)
	writeApp(t, root, "escape", simple("escape", "escape.example.com")+"compose: ../alpha/compose.yml\n", compose)
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("not an app"), 0o644); err != nil {
		t.Fatal(err)
	}

	apps, err := LoadAll(root)
	if len(apps) != 2 || apps["alpha"] == nil || apps["beta"] == nil {
		t.Fatalf("want alpha and beta loaded, got %v", apps)
	}
	if apps["alpha"].Dir != filepath.Join(root, "alpha") || apps["alpha"].TokensFile() != filepath.Join(root, "alpha", "tokens") {
		t.Errorf("Dir or TokensFile wrong: %+v", apps["alpha"])
	}
	if err == nil {
		t.Fatal("broken definitions were not reported")
	}
	for _, want := range []string{"clash", "renamed", "unsafe", "nocompose", "badyaml", "escape"} {
		if !strings.Contains(err.Error(), "app "+want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
	if _, err := LoadAll(filepath.Join(root, "missing")); err == nil {
		t.Error("a missing apps directory should be an error")
	}
	if _, err := Load(filepath.Join(root, "missing")); err == nil {
		t.Error("a missing app directory should be an error")
	}
	big := filepath.Join(root, "big")
	writeApp(t, root, "big", strings.Repeat("#", maxFileSize+1), compose)
	if _, err := Load(big); err == nil {
		t.Error("an oversized app.yml should be an error")
	}
}

// Regression: apps load in name order, so a new app whose name sorted first
// took a domain away from the app already serving it.
func TestLoadAllPreferringKeepsTheIncumbent(t *testing.T) {
	root := t.TempDir()
	simple := func(name string) string {
		return "name: " + name + "\ndomains: [shop.example.com]\nweb: { service: web, port: 80 }\n"
	}
	compose := "services:\n  web:\n    image: ${IMAGE}\n"
	writeApp(t, root, "aaa", simple("aaa"), compose)
	writeApp(t, root, "shop", simple("shop"), compose)

	apps, err := LoadAllPreferring(root, func(name string) bool { return name == "shop" })
	if len(apps) != 1 || apps["shop"] == nil {
		t.Fatalf("want only the incumbent shop loaded, got %v", apps)
	}
	if err == nil || !strings.Contains(err.Error(), "app aaa: a domain is already used by app shop") {
		t.Errorf("the newcomer was not reported: %v", err)
	}

	for name, load := range map[string]func() (map[string]*App, error){
		"LoadAll":             func() (map[string]*App, error) { return LoadAll(root) },
		"nil preference":      func() (map[string]*App, error) { return LoadAllPreferring(root, nil) },
		"no incumbent at all": func() (map[string]*App, error) { return LoadAllPreferring(root, func(string) bool { return false }) },
	} {
		apps, err := load()
		if len(apps) != 1 || apps["aaa"] == nil {
			t.Errorf("%s: want name order to hold and aaa loaded, got %v", name, apps)
		}
		if err == nil || !strings.Contains(err.Error(), "app shop: a domain is already used by app aaa") {
			t.Errorf("%s: shop was not reported: %v", name, err)
		}
	}
}

// allowedRendered are the keys Render may add on top of the allow-list.
var allowedRendered = map[string]bool{
	"pull_policy": true, "security_opt": true, "env_file": true, "labels": true, "ports": true, "networks": true,
}

func FuzzParseCompose(f *testing.F) {
	f.Add([]byte(validCompose))
	for _, attempt := range escapeAttempts {
		f.Add([]byte(attempt))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := ParseCompose(data)
		if err != nil {
			return
		}
		// Whatever the parser accepts must stay inside the allow-list, name
		// only the placeholder image, and mount only declared volumes.
		for name, svc := range c.Services {
			if svc["image"] != ImagePlaceholder {
				t.Fatalf("service %s accepted with image %v", name, svc["image"])
			}
			for key := range svc {
				if !allowedServiceKeys[key] {
					t.Fatalf("service %s accepted with key %q", name, key)
				}
			}
			if user, set := svc["user"]; set {
				if _, ok := user.(string); !ok {
					t.Fatalf("service %s accepted with a user that is not a string: %#v", name, user)
				}
				uid, _, _ := strings.Cut(fmt.Sprint(user), ":")
				if n, err := strconv.Atoi(strings.TrimSpace(uid)); (err == nil && n == 0) || strings.TrimSpace(uid) == "root" {
					t.Fatalf("service %s accepted with root user %q", name, user)
				}
			}
			if vols, ok := svc["volumes"].([]any); ok {
				for _, v := range vols {
					if s, _ := v.(string); strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "~") {
						t.Fatalf("service %s accepted a host path %q", name, s)
					}
				}
			}
		}
		web := c.ServiceNames()[0]
		app := &App{Name: "fuzz", Web: Web{Service: web, Port: 80}, Compose: c}
		out, err := app.Render(RenderInput{Release: 1, Digest: "sha256:" + strings.Repeat("a", 64), Port: 20000})
		if err != nil {
			t.Fatalf("accepted compose does not render: %v", err)
		}
		var doc struct {
			Services map[string]map[string]any `yaml:"services"`
		}
		if err := yaml.Unmarshal(out, &doc); err != nil {
			t.Fatalf("rendered compose is not valid YAML: %v", err)
		}
		for name, svc := range doc.Services {
			for key := range svc {
				if !allowedServiceKeys[key] && !allowedRendered[key] {
					t.Fatalf("rendered service %s has key %q", name, key)
				}
			}
		}
	})
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(validApp))
	f.Fuzz(func(t *testing.T, data []byte) {
		app, err := Parse(data, []byte(validCompose))
		if err != nil {
			return
		}
		if !ValidName(app.Name) || len(app.Domains) == 0 || app.Web.Port < 1 || app.Web.Port > 65535 {
			t.Fatalf("accepted an invalid app: %+v", app)
		}
		for _, d := range app.Domains {
			if strings.ContainsAny(d, " \t\n{}/:*") {
				t.Fatalf("accepted domain %q", d)
			}
		}
	})
}

func TestServiceEnvNames(t *testing.T) {
	compose := "services:\n  web:\n    image: ${IMAGE}\n"
	base := "name: shop\ndomains: [shop.example.com]\nweb: { service: web, port: 80 }\n"
	parse := func(extra, composeYAML string) (*App, error) {
		return Parse([]byte(base+extra), []byte(composeYAML))
	}

	app, err := parse("postgres: { version: 17 }\nredis: { persistent: false }\n", compose)
	if err != nil {
		t.Fatal(err)
	}
	if app.Postgres.Env != "DATABASE_URL" || app.Redis.Env != "REDIS_URL" {
		t.Fatalf("default names = %q, %q", app.Postgres.Env, app.Redis.Env)
	}
	if got := app.InjectedEnv(); len(got) != 2 {
		t.Fatalf("InjectedEnv = %v", got)
	}

	app, err = parse("postgres: { version: 17, env: SHOP_DATABASE_URL }\nredis: { persistent: false, env: SHOP_CACHE_URL }\n", compose)
	if err != nil {
		t.Fatal(err)
	}
	if got := app.InjectedEnv(); len(got) != 2 || got[0] != "SHOP_DATABASE_URL" || got[1] != "SHOP_CACHE_URL" {
		t.Fatalf("InjectedEnv = %v", got)
	}
	if plain, _ := parse("", compose); len(plain.InjectedEnv()) != 0 {
		t.Error("an app without managed services injects nothing")
	}

	for name, extra := range map[string]string{
		"lowercase":        "postgres: { version: 17, env: database_url }\n",
		"with equals sign": "postgres: { version: 17, env: \"A=B\" }\n",
		"with a space":     "redis: { env: \"MY URL\" }\n",
		"leading digit":    "redis: { env: 1URL }\n",
		"shell syntax":     "redis: { env: \"$(id)\" }\n",
		"same for both":    "postgres: { version: 17, env: URL }\nredis: { env: URL }\n",
	} {
		if _, err := parse(extra, compose); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// The app may not set a variable Zymple injects, under any name.
	custom := "postgres: { version: 17, env: SHOP_DATABASE_URL }\n"
	for name, c := range map[string]string{
		"mapping": "services:\n  web:\n    image: ${IMAGE}\n    environment: { SHOP_DATABASE_URL: postgres://elsewhere }\n",
		"list":    "services:\n  web:\n    image: ${IMAGE}\n  worker:\n    image: ${IMAGE}\n    environment: [\"SHOP_DATABASE_URL=postgres://elsewhere\"]\n",
	} {
		if _, err := parse(custom, c); err == nil || !strings.Contains(err.Error(), "set by zymple") {
			t.Errorf("%s: a compose file overriding the injected variable was accepted: %v", name, err)
		}
	}
}
