// Package server is the Zymple daemon: the registry and deploy API behind
// Caddy, and the operator API on a unix socket.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/gokaykucuk/zymple/internal/appdef"
	"github.com/gokaykucuk/zymple/internal/auth"
	"github.com/gokaykucuk/zymple/internal/config"
	"github.com/gokaykucuk/zymple/internal/deploy"
	"github.com/gokaykucuk/zymple/internal/execx"
	"github.com/gokaykucuk/zymple/internal/proxy"
	"github.com/gokaykucuk/zymple/internal/registry"
	"github.com/gokaykucuk/zymple/internal/releases"
	"github.com/gokaykucuk/zymple/internal/runtime"
	"github.com/gokaykucuk/zymple/internal/services"
)

// AppProxy is what the server needs from the reverse proxy.
type AppProxy interface {
	deploy.Proxy
	Remove(ctx context.Context, app string) error
}

// Server holds the daemon's state.
type Server struct {
	Config   *config.Config
	Log      *slog.Logger
	Store    *releases.Store
	Docker   *runtime.Docker
	Proxy    AppProxy
	Engine   *deploy.Engine
	Postgres *services.Postgres
	Redis    *services.Redis
	// Registry serves /v2/ without authentication; the server guards it.
	Registry http.Handler

	mu     sync.RWMutex
	apps   map[string]*appdef.App
	tokens map[string]auth.Set

	// ops tracks operations that outlive their request.
	ops sync.WaitGroup
	// base is cancelled when the daemon shuts down.
	base context.Context
}

// New wires a server that runs real commands.
func New(ctx context.Context, cfg *config.Config, log *slog.Logger) (*Server, error) {
	runner := execx.OS{}
	stores, err := registry.Dial(cfg.Containerd.Socket)
	if err != nil {
		return nil, err
	}
	reg, err := registry.NewHandler(ctx, stores, cfg.Containerd.Namespace)
	if err != nil {
		return nil, err
	}
	s := &Server{
		Config: cfg,
		Log:    log,
		Store:  &releases.Store{Root: cfg.StateDir},
		Docker: &runtime.Docker{Runner: runner, StateDir: cfg.StateDir},
		Proxy:  &proxy.Caddy{Dir: cfg.Caddy.SnippetsDir, Reload: cfg.Caddy.Reload, TLS: cfg.Caddy.TLS, Runner: runner},
		Postgres: &services.Postgres{Runner: runner, Config: cfg, Sleep: deploy.SleepContext, Now: time.Now,
			Warn: func(msg string) { log.Warn(msg) }},
		Redis:    &services.Redis{Runner: runner, Config: cfg},
		Registry: reg,
	}
	s.Engine = &deploy.Engine{
		Store:   s.Store,
		Runtime: s.Docker,
		Proxy:   s.Proxy,
		Health:  deploy.HTTPHealth{},
		Ports:   &deploy.PortRange{Low: cfg.Ports.Low, High: cfg.Ports.High},
		Env:     s.ServiceEnv,
		Sleep:   deploy.SleepContext,
		Now:     time.Now,
	}
	return s, nil
}

// ServiceEnv starts an app's managed services and returns their URLs.
func (s *Server) ServiceEnv(ctx context.Context, app *appdef.App) (map[string]string, error) {
	env := map[string]string{}
	if app.Postgres != nil {
		url, err := s.Postgres.Ensure(ctx, app)
		if err != nil {
			return nil, fmt.Errorf("postgres: %w", err)
		}
		env["DATABASE_URL"] = url
	}
	if app.Redis != nil {
		url, err := s.Redis.Ensure(ctx, app)
		if err != nil {
			return nil, fmt.Errorf("redis: %w", err)
		}
		env["REDIS_URL"] = url
	}
	return env, nil
}

// App returns a loaded app definition.
func (s *Server) App(name string) (*appdef.App, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	app, ok := s.apps[name]
	return app, ok
}

// AppNames returns the loaded app names in sorted order.
func (s *Server) AppNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.apps))
	for name := range s.apps {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Tokens implements registry.TokenLookup.
func (s *Server) Tokens(app string) (auth.Set, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	set, ok := s.tokens[app]
	return set, ok
}

// Reload re-reads the app definitions and converges every app. Definitions
// that fail validation are reported and leave the running app untouched.
func (s *Server) Reload(ctx context.Context) error {
	removed, loadErr := s.load()
	return errors.Join(loadErr, s.converge(ctx, removed))
}

// load reads the definitions and tokens from disk. It returns the names of
// apps whose definition disappeared.
//
// An app that is already loaded keeps its previous definition when the new
// one cannot be used: a typo in app.yml must not take a running app down.
// Only an app whose directory is gone counts as removed.
func (s *Server) load() ([]string, error) {
	loaded, loadErr := appdef.LoadAll(s.Config.AppsDir)
	if loaded == nil {
		return nil, loadErr
	}
	s.mu.RLock()
	previous := s.apps
	s.mu.RUnlock()

	apps, keepErrs := keepPrevious(previous, loaded, s.Config.AppsDir)
	errs := append([]error{loadErr}, keepErrs...)
	tokens := make(map[string]auth.Set, len(apps))
	for name, app := range apps {
		set, err := auth.Load(app.TokensFile())
		if err != nil {
			errs = append(errs, fmt.Errorf("app %s: tokens: %w", name, err))
		}
		tokens[name] = set
	}

	s.mu.Lock()
	s.apps, s.tokens = apps, tokens
	s.mu.Unlock()

	s.reservePorts(apps)
	var removed []string
	for name := range previous {
		if _, still := apps[name]; !still {
			removed = append(removed, name)
		}
	}
	sort.Strings(removed)
	return removed, errors.Join(errs...)
}

// reservePorts marks the ports of recorded releases as in use.
func (s *Server) reservePorts(apps map[string]*appdef.App) {
	ports, ok := s.Engine.Ports.(*deploy.PortRange)
	if !ok {
		return
	}
	for name := range apps {
		st, err := s.Store.Load(name)
		if err != nil {
			continue
		}
		for _, r := range st.Releases {
			ports.Reserve(r.Port)
		}
	}
}

// keepPrevious merges freshly loaded definitions with the ones already in
// use. A previously loaded app that is missing from loaded, but whose
// directory still exists, failed to load and keeps its previous definition.
// A new definition may then claim a domain that such a kept definition still
// serves; the definition in use wins, and the new one is put back to its own
// previous definition, or left out if the app is new.
func keepPrevious(previous, loaded map[string]*appdef.App, appsDir string) (map[string]*appdef.App, []error) {
	apps := make(map[string]*appdef.App, len(loaded)+len(previous))
	for name, app := range loaded {
		apps[name] = app
	}
	kept := map[string]bool{}
	for name, old := range previous {
		if _, ok := apps[name]; !ok && isDir(filepath.Join(appsDir, name)) {
			apps[name] = old
			kept[name] = true
		}
	}
	var errs []error
	// Each pass moves at least one app from its new definition to its
	// previous one, and the previous definitions never conflicted with each
	// other, so this ends.
	for len(kept) > 0 {
		name, owner := newDefinitionInConflict(apps, kept)
		if name == "" {
			break
		}
		if old, had := previous[name]; had {
			apps[name] = old
			kept[name] = true
			errs = append(errs, fmt.Errorf("app %s: a domain is still served by app %s; keeping the previous definition", name, owner))
			continue
		}
		delete(apps, name)
		errs = append(errs, fmt.Errorf("app %s: a domain is still served by app %s; not loaded", name, owner))
	}
	return apps, errs
}

// newDefinitionInConflict finds an app with a new definition that claims a
// domain of a kept definition. Names are visited in sorted order so the
// result does not depend on map iteration.
func newDefinitionInConflict(apps map[string]*appdef.App, kept map[string]bool) (name, owner string) {
	owners := map[string]string{}
	for keptName := range kept {
		for _, d := range apps[keptName].Domains {
			owners[d] = keptName
		}
	}
	names := make([]string, 0, len(apps))
	for n := range apps {
		if !kept[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		for _, d := range apps[n].Domains {
			if o, ok := owners[d]; ok {
				return n, o
			}
		}
	}
	return "", ""
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// converge makes the host match the loaded definitions.
func (s *Server) converge(ctx context.Context, removed []string) error {
	var errs []error
	for _, name := range removed {
		// The definition is gone: stop serving it, keep its data.
		s.Log.Warn("app definition removed; stopping its containers", "app", name)
		if err := s.Proxy.Remove(ctx, name); err != nil {
			errs = append(errs, fmt.Errorf("app %s: %w", name, err))
		}
		if err := s.Docker.StopAll(ctx, name); err != nil {
			errs = append(errs, fmt.Errorf("app %s: %w", name, err))
		}
	}
	for _, name := range s.AppNames() {
		app, _ := s.App(name)
		if err := s.Engine.Reconcile(ctx, app); err != nil && !errors.Is(err, deploy.ErrBusy) {
			errs = append(errs, fmt.Errorf("app %s: reconcile: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// Run serves until ctx is cancelled, then waits for running operations.
func (s *Server) Run(ctx context.Context) error {
	s.base = ctx
	// Definitions load before the listeners open, so requests are never
	// answered from an empty app list. Converging can take minutes when a
	// database has to start, so it runs behind the listeners.
	if _, err := s.load(); err != nil {
		s.Log.Error("some app definitions are invalid", "error", err)
	}

	network, err := net.Listen("tcp", s.Config.Listen)
	if err != nil {
		return err
	}
	admin, err := listenUnix(s.Config.Socket)
	if err != nil {
		network.Close()
		return err
	}
	servers := []*http.Server{
		{Handler: s.NetworkHandler(), ReadHeaderTimeout: 15 * time.Second},
		{Handler: s.AdminHandler(), ReadHeaderTimeout: 15 * time.Second},
	}
	errc := make(chan error, 2)
	go func() { errc <- servers[0].Serve(network) }()
	go func() { errc <- servers[1].Serve(admin) }()
	s.ops.Add(1)
	go func() {
		defer s.ops.Done()
		if err := s.converge(ctx, nil); err != nil {
			s.Log.Error("startup convergence reported problems", "error", err)
		}
		s.Log.Info("startup convergence finished")
		s.schedule(ctx)
	}()
	s.Log.Info("zymple is serving", "listen", s.Config.Listen, "socket", s.Config.Socket, "apps", len(s.AppNames()))

	select {
	case err = <-errc:
	case <-ctx.Done():
		err = nil
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutdownCtx)
	}
	// A deploy in flight is allowed to finish; killing it mid-switch would
	// only leave work for the next start.
	s.ops.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func listenUnix(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// Only root may use the operator API.
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// schedule takes database backups when they are due.
func (s *Server) schedule(ctx context.Context) {
	if s.Config.Backup == nil {
		return
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		s.runDueBackups(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) runDueBackups(ctx context.Context) {
	for _, name := range s.AppNames() {
		app, _ := s.App(name)
		if app.Postgres != nil {
			s.backupPostgresIfDue(ctx, app)
		}
		if app.Redis != nil && app.Redis.Persistent {
			s.backupRedisIfDue(ctx, app)
		}
	}
}

func (s *Server) backupPostgresIfDue(ctx context.Context, app *appdef.App) {
	kind, err := s.Postgres.DueBackup(ctx, app.Name)
	if err != nil {
		s.Log.Error("checking backups failed", "app", app.Name, "error", err)
		return
	}
	if kind == "" {
		return
	}
	s.Log.Info("starting scheduled backup", "app", app.Name, "type", kind)
	out := &logSink{log: s.Log, app: app.Name}
	if err := s.Postgres.Backup(ctx, app.Name, kind, out); err != nil {
		s.Log.Error("scheduled backup failed", "app", app.Name, "type", kind, "error", err)
	}
}

func (s *Server) backupRedisIfDue(ctx context.Context, app *appdef.App) {
	marker := filepath.Join(s.Store.Dir(app.Name), "redis-last-backup")
	if info, err := os.Stat(marker); err == nil && time.Since(info.ModTime()) < 24*time.Hour {
		return
	}
	out := &logSink{log: s.Log, app: app.Name}
	if err := s.Redis.Backup(ctx, app, out); err != nil {
		s.Log.Error("scheduled redis backup failed", "app", app.Name, "error", err)
		return
	}
	if err := releases.WriteFileAtomic(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		s.Log.Error("recording redis backup failed", "app", app.Name, "error", err)
	}
}

type logSink struct {
	log *slog.Logger
	app string
}

func (l *logSink) Write(p []byte) (int, error) {
	l.log.Info("backup", "app", l.app, "output", string(p))
	return len(p), nil
}
