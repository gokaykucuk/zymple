// Package appdef loads and validates app definitions: the app.yml and
// compose.yml that the operator's infra repo renders onto the host.
package appdef

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Limits and defaults.
const (
	DefaultKeepReleases  = 5
	DefaultHealthTimeout = 60 * time.Second
	DefaultDrain         = 30 * time.Second
	// DefaultReleaseTimeout bounds the release command. A command that never
	// ends would otherwise hold the app's deploy lock for good.
	DefaultReleaseTimeout = time.Hour
	DefaultPITRDays       = 14
	maxFileSize           = 1 << 20
	// MaxImages is how many extra images an app may declare.
	MaxImages = 8
)

var (
	nameRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$`)
	domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	digitsRe = regexp.MustCompile(`^[0-9]+$`)
	pathRe   = regexp.MustCompile(`^/[A-Za-z0-9/_.\-?=&%]*$`)
	// An image name becomes part of a tag (release-<n>-<name>) and of a
	// compose placeholder (${IMAGE_<NAME>}), so it has no "-" and one case.
	imageNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,30}$`)
)

// ValidImageName reports whether s can name one of an app's extra images.
func ValidImageName(s string) bool { return imageNameRe.MatchString(s) }

// Duration is a time.Duration that unmarshals from a string such as "30s".
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	*d = Duration(v)
	return nil
}

// Web describes the one service that receives HTTP traffic.
type Web struct {
	Service       string   `yaml:"service"`
	Port          int      `yaml:"port"`
	Healthcheck   string   `yaml:"healthcheck"`
	HealthTimeout Duration `yaml:"health_timeout"`
	Drain         Duration `yaml:"drain"`
}

// Default names of the variables that carry the managed services' URLs.
const (
	DefaultPostgresEnv = "DATABASE_URL"
	DefaultRedisEnv    = "REDIS_URL"
)

var envNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// Postgres declares a managed Postgres cluster.
type Postgres struct {
	Version  int `yaml:"version"`
	PITRDays int `yaml:"pitr_days"`
	// Env is the variable the connection URL is injected as. Apps that read
	// their own name for it set this instead of renaming it in compose.
	Env string `yaml:"env"`
	// CreateRoles lets the app's database role create other roles, for apps
	// whose migrations separate privileges that way. It never makes the role
	// a superuser.
	CreateRoles bool `yaml:"create_roles"`
}

// Redis declares a managed Redis instance.
type Redis struct {
	Persistent bool `yaml:"persistent"`
	// Env is the variable the connection URL is injected as.
	Env string `yaml:"env"`
}

// App is one validated app definition.
type App struct {
	Name        string   `yaml:"name"`
	Domains     []string `yaml:"domains"`
	ComposeFile string   `yaml:"compose"`
	Web         Web      `yaml:"web"`
	Release     []string `yaml:"release"`
	// ReleaseTimeout is how long the release command may run before the
	// deploy is aborted.
	ReleaseTimeout Duration  `yaml:"release_timeout"`
	KeepReleases   int       `yaml:"keep_releases"`
	Postgres       *Postgres `yaml:"postgres"`
	Redis          *Redis    `yaml:"redis"`
	// Images names the images the app runs besides its own, such as a
	// document processor built from another Dockerfile. A compose service
	// uses one by naming ${IMAGE_<NAME>}. They are pushed to the app's
	// repository and pinned by each release together with the app's image.
	Images []string `yaml:"images"`
	// Env names the secrets file in the infra repo. On the host the file is
	// always <dir>/env, so the value is accepted and ignored.
	Env string `yaml:"env"`

	// Dir is the directory the definition was loaded from.
	Dir string `yaml:"-"`
	// Compose is the validated compose document.
	Compose *Compose `yaml:"-"`
}

// EnvFile returns the path of the app's env file, or "" if it has none.
func (a *App) EnvFile() string {
	p := filepath.Join(a.Dir, "env")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// TokensFile returns the path of the app's token hash file.
func (a *App) TokensFile() string { return filepath.Join(a.Dir, "tokens") }

// Parse validates an app definition and its compose file.
func Parse(appYAML, composeYAML []byte) (*App, error) {
	if len(appYAML) > maxFileSize || len(composeYAML) > maxFileSize {
		return nil, errors.New("definition too large")
	}
	var app App
	dec := yaml.NewDecoder(bytes.NewReader(appYAML))
	dec.KnownFields(true)
	if err := dec.Decode(&app); err != nil {
		return nil, fmt.Errorf("app.yml: %w", err)
	}
	app.applyDefaults()
	if err := app.validate(); err != nil {
		return nil, fmt.Errorf("app.yml: %w", err)
	}
	compose, err := ParseCompose(composeYAML)
	if err != nil {
		return nil, fmt.Errorf("compose: %w", err)
	}
	if err := app.checkInjectedEnv(compose); err != nil {
		return nil, fmt.Errorf("compose: %w", err)
	}
	if _, ok := compose.Services[app.Web.Service]; !ok {
		return nil, fmt.Errorf("app.yml: web.service %q is not a service in the compose file", app.Web.Service)
	}
	if err := app.checkServiceNames(compose); err != nil {
		return nil, fmt.Errorf("compose: %w", err)
	}
	if err := app.checkImages(compose); err != nil {
		return nil, fmt.Errorf("compose: %w", err)
	}
	app.Compose = compose
	return &app, nil
}

// checkServiceNames refuses compose service names that Zymple needs for
// itself: the per-release name of the web service, and the network aliases
// of the managed Postgres and Redis containers.
func (a *App) checkServiceNames(compose *Compose) error {
	for _, name := range compose.ServiceNames() {
		if reason := a.reservedService(name); reason != "" {
			return fmt.Errorf("service name %q is reserved: %s", name, reason)
		}
	}
	return nil
}

// checkImages ties the compose file's image placeholders to the images the
// app declares. The web service runs the app's own image, because that is
// the image the release command and `zymple run` use. An image nothing runs
// is refused: every deploy would have to supply it for no reason.
func (a *App) checkImages(compose *Compose) error {
	declared := make(map[string]bool, len(a.Images))
	for _, name := range a.Images {
		declared[name] = false
	}
	for _, service := range compose.ServiceNames() {
		name := compose.Image(service)
		if name == "" {
			continue
		}
		if _, ok := declared[name]; !ok {
			return fmt.Errorf("service %s: image %q is not listed under images in app.yml", service, name)
		}
		if service == a.Web.Service {
			return fmt.Errorf("service %s: the web service must use %s", service, ImagePlaceholder)
		}
		declared[name] = true
	}
	for _, name := range a.Images {
		if !declared[name] {
			return fmt.Errorf("image %q is declared in app.yml but no service uses %s", name, ImagePlaceholderFor(name))
		}
	}
	return nil
}

// HasImage reports whether the app declares an extra image of that name.
func (a *App) HasImage(name string) bool {
	for _, declared := range a.Images {
		if declared == name {
			return true
		}
	}
	return false
}

func (a *App) reservedService(name string) string {
	if rest, ok := strings.CutPrefix(name, a.Web.Service+"-r"); ok && digitsRe.MatchString(rest) {
		return "it is the name of a release of the web service"
	}
	if a.Postgres != nil && (name == "postgres" || strings.HasPrefix(name, "postgres-drill-")) {
		return "it is how the app reaches its managed Postgres"
	}
	if a.Redis != nil && name == "redis" {
		return "it is how the app reaches its managed Redis"
	}
	return ""
}

func (a *App) applyDefaults() {
	if a.KeepReleases == 0 {
		a.KeepReleases = DefaultKeepReleases
	}
	if a.Web.HealthTimeout == 0 {
		a.Web.HealthTimeout = Duration(DefaultHealthTimeout)
	}
	if a.Web.Drain == 0 {
		a.Web.Drain = Duration(DefaultDrain)
	}
	if a.Web.Healthcheck == "" {
		a.Web.Healthcheck = "/"
	}
	if a.ReleaseTimeout == 0 {
		a.ReleaseTimeout = Duration(DefaultReleaseTimeout)
	}
	if a.ComposeFile == "" {
		a.ComposeFile = "compose.yml"
	}
	if a.Postgres != nil && a.Postgres.PITRDays == 0 {
		a.Postgres.PITRDays = DefaultPITRDays
	}
	if a.Postgres != nil && a.Postgres.Env == "" {
		a.Postgres.Env = DefaultPostgresEnv
	}
	if a.Redis != nil && a.Redis.Env == "" {
		a.Redis.Env = DefaultRedisEnv
	}
}

func (a *App) validate() error {
	if !ValidName(a.Name) {
		return fmt.Errorf("name %q must match %s", a.Name, nameRe)
	}
	if err := validateDomains(a.Domains); err != nil {
		return err
	}
	if a.ComposeFile != filepath.Base(a.ComposeFile) || a.ComposeFile == "." || a.ComposeFile == ".." {
		return fmt.Errorf("compose %q must be a file name inside the app directory", a.ComposeFile)
	}
	if err := a.Web.validate(); err != nil {
		return err
	}
	for _, arg := range a.Release {
		if arg == "" {
			return errors.New("release command arguments must not be empty")
		}
	}
	if t := time.Duration(a.ReleaseTimeout); t < time.Second || t > 24*time.Hour {
		return errors.New("release_timeout must be between 1s and 24h")
	}
	if a.KeepReleases < 1 || a.KeepReleases > 50 {
		return errors.New("keep_releases must be between 1 and 50")
	}
	if err := a.Postgres.validate(); err != nil {
		return err
	}
	if err := validateImages(a.Images); err != nil {
		return err
	}
	if a.Redis != nil && !envNameRe.MatchString(a.Redis.Env) {
		return fmt.Errorf("redis.env %q must match %s", a.Redis.Env, envNameRe)
	}
	if a.Postgres != nil && a.Redis != nil && a.Postgres.Env == a.Redis.Env {
		return fmt.Errorf("postgres.env and redis.env are both %q", a.Redis.Env)
	}
	return nil
}

// InjectedEnv lists the variables Zymple sets for this app's managed
// services. The app's own configuration may not set them.
func (a *App) InjectedEnv() []string {
	var names []string
	if a.Postgres != nil {
		names = append(names, a.Postgres.Env)
	}
	if a.Redis != nil {
		names = append(names, a.Redis.Env)
	}
	return names
}

// checkInjectedEnv refuses a compose file that sets a variable Zymple
// injects: the app would silently talk to something other than its own
// database.
func (a *App) checkInjectedEnv(compose *Compose) error {
	for _, service := range compose.ServiceNames() {
		env, err := Environment(compose.Services[service])
		if err != nil {
			return err
		}
		for _, name := range a.InjectedEnv() {
			if _, set := env[name]; set {
				return fmt.Errorf("service %s: environment %s is set by zymple", service, name)
			}
		}
	}
	return nil
}

func validateImages(images []string) error {
	if len(images) > MaxImages {
		return fmt.Errorf("at most %d images may be declared", MaxImages)
	}
	seen := make(map[string]bool, len(images))
	for _, name := range images {
		if !ValidImageName(name) {
			return fmt.Errorf("image name %q must match %s", name, imageNameRe)
		}
		if seen[name] {
			return fmt.Errorf("image %q is declared twice", name)
		}
		seen[name] = true
	}
	return nil
}

func validateDomains(domains []string) error {
	if len(domains) == 0 {
		return errors.New("at least one domain is required")
	}
	for _, d := range domains {
		if len(d) > 253 || !domainRe.MatchString(d) {
			return fmt.Errorf("domain %q is not a plain lowercase hostname", d)
		}
	}
	return nil
}

func (p *Postgres) validate() error {
	if p == nil {
		return nil
	}
	if p.Version < 14 || p.Version > 18 {
		return errors.New("postgres.version must be between 14 and 18")
	}
	if p.PITRDays < 1 || p.PITRDays > 365 {
		return errors.New("postgres.pitr_days must be between 1 and 365")
	}
	if !envNameRe.MatchString(p.Env) {
		return fmt.Errorf("postgres.env %q must match %s", p.Env, envNameRe)
	}
	// Before Postgres 16 a role with CREATEROLE can grant itself the
	// predefined roles that read files and run programs on the server,
	// which is the access the app's role is denied on purpose.
	if p.CreateRoles && p.Version < 16 {
		return errors.New("postgres.create_roles needs postgres.version 16 or newer")
	}
	return nil
}

func (w *Web) validate() error {
	if !serviceRe.MatchString(w.Service) {
		return fmt.Errorf("web.service %q is not a valid service name", w.Service)
	}
	if w.Port < 1 || w.Port > 65535 {
		return errors.New("web.port must be between 1 and 65535")
	}
	if len(w.Healthcheck) > 256 || !pathRe.MatchString(w.Healthcheck) {
		return fmt.Errorf("web.healthcheck %q must be a URL path", w.Healthcheck)
	}
	if t := time.Duration(w.HealthTimeout); t < time.Second || t > 10*time.Minute {
		return errors.New("web.health_timeout must be between 1s and 10m")
	}
	if d := time.Duration(w.Drain); d < 0 || d > 5*time.Minute {
		return errors.New("web.drain must be between 0s and 5m")
	}
	return nil
}

// ValidName reports whether s is an acceptable app name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// Load reads one app directory.
func Load(dir string) (*App, error) {
	appYAML, err := readLimited(filepath.Join(dir, "app.yml"))
	if err != nil {
		return nil, err
	}
	// The compose file name comes from app.yml, so decode loosely first.
	var probe struct {
		Compose string `yaml:"compose"`
	}
	if err := yaml.Unmarshal(appYAML, &probe); err != nil {
		return nil, fmt.Errorf("app.yml: %w", err)
	}
	if probe.Compose == "" {
		probe.Compose = "compose.yml"
	}
	if probe.Compose != filepath.Base(probe.Compose) {
		return nil, fmt.Errorf("app.yml: compose %q must be a file name inside the app directory", probe.Compose)
	}
	composeYAML, err := readLimited(filepath.Join(dir, probe.Compose))
	if err != nil {
		return nil, err
	}
	app, err := Parse(appYAML, composeYAML)
	if err != nil {
		return nil, err
	}
	if app.Name != filepath.Base(dir) {
		return nil, fmt.Errorf("app.yml: name %q does not match directory %q", app.Name, filepath.Base(dir))
	}
	app.Dir = dir
	return app, nil
}

// LoadAll reads every app directory under root. A broken definition does not
// hide the others: valid apps are returned together with the joined errors.
func LoadAll(root string) (map[string]*App, error) {
	return LoadAllPreferring(root, nil)
}

// LoadAllPreferring is LoadAll, but directories for which incumbent reports
// true are loaded first. Two apps that claim one domain cannot both load, and
// the first one loaded keeps it, so an app that is already serving wins the
// conflict against a newcomer whatever their names sort as.
func LoadAllPreferring(root string, incumbent func(name string) bool) (map[string]*App, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	apps := make(map[string]*App)
	var errs []error
	domains := make(map[string]string)
	for _, name := range loadOrder(entries, incumbent) {
		app, err := Load(filepath.Join(root, name))
		if err != nil {
			errs = append(errs, fmt.Errorf("app %s: %w", name, err))
			continue
		}
		if owner := firstClaimed(domains, app); owner != "" {
			errs = append(errs, fmt.Errorf("app %s: a domain is already used by app %s", name, owner))
			continue
		}
		for _, d := range app.Domains {
			domains[d] = name
		}
		apps[name] = app
	}
	return apps, errors.Join(errs...)
}

// loadOrder returns the directory names with the incumbents first, each
// group in sorted order.
func loadOrder(entries []os.DirEntry, incumbent func(name string) bool) []string {
	var first, rest []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if incumbent != nil && incumbent(e.Name()) {
			first = append(first, e.Name())
		} else {
			rest = append(rest, e.Name())
		}
	}
	sort.Strings(first)
	sort.Strings(rest)
	return append(first, rest...)
}

func firstClaimed(domains map[string]string, app *App) string {
	for _, d := range app.Domains {
		if owner, ok := domains[d]; ok {
			return owner
		}
	}
	return ""
}

func readLimited(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxFileSize {
		return nil, fmt.Errorf("%s: larger than %d bytes", path, maxFileSize)
	}
	return os.ReadFile(path)
}
