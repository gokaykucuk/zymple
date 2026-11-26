package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gokaykucuk/zymple/internal/appdef"
	"github.com/gokaykucuk/zymple/internal/deploy"
	"github.com/gokaykucuk/zymple/internal/registry"
	"github.com/gokaykucuk/zymple/internal/releases"
	"github.com/gokaykucuk/zymple/internal/services"
)

const (
	maxBody     = 64 << 10
	adminActor  = "admin"
	maxLogTail  = 5000
	defaultTail = 200
	// maxLogStreams bounds the log streams open at once for one app.
	maxLogStreams = 8
)

type ctxKey int

const actorKey ctxKey = 0

// NetworkHandler serves the routes Caddy forwards from the internet: the
// registry and the token-authenticated deploy API.
func (s *Server) NetworkHandler() http.Handler {
	mux := http.NewServeMux()
	guard := &registry.Guard{Next: s.Registry, Tokens: s.Tokens}
	mux.Handle("/v2/", guard)
	mux.Handle("/v2", guard)
	s.appRoutes(mux, s.requireToken)
	return mux
}

// AdminHandler serves the unix socket. Reaching the socket is the
// authentication, so it also carries the operator-only routes.
func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	asAdmin := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			next(w, r.WithContext(context.WithValue(r.Context(), actorKey, adminActor)))
		}
	}
	s.appRoutes(mux, asAdmin)
	mux.HandleFunc("GET /admin/apps", s.handleApps)
	mux.HandleFunc("POST /admin/reload", s.handleReload)
	mux.HandleFunc("POST /admin/apps/{app}/redeploy", asAdmin(s.withApp(s.handleRedeploy)))
	mux.HandleFunc("POST /admin/apps/{app}/run", s.withApp(s.handleRun))
	mux.HandleFunc("GET /admin/apps/{app}/pg/info", s.withApp(s.handlePGInfo))
	mux.HandleFunc("POST /admin/apps/{app}/pg/backup", s.withApp(s.handlePGBackup))
	mux.HandleFunc("POST /admin/apps/{app}/pg/restore", s.withApp(s.handlePGRestore))
	mux.HandleFunc("POST /admin/apps/{app}/pg/import", s.withApp(s.handlePGImport))
	mux.HandleFunc("DELETE /admin/apps/{app}/pg/drills/{drill}", s.withApp(s.handlePGDrillRemove))
	mux.HandleFunc("POST /admin/apps/{app}/redis/backup", s.withApp(s.handleRedisBackup))
	mux.HandleFunc("POST /admin/apps/{app}/redis/restore", s.withApp(s.handleRedisRestore))
	return mux
}

func (s *Server) appRoutes(mux *http.ServeMux, wrap func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("POST /api/v1/apps/{app}/deploys", wrap(s.withApp(s.handleDeploy)))
	mux.HandleFunc("POST /api/v1/apps/{app}/rollbacks", wrap(s.withApp(s.handleRollback)))
	mux.HandleFunc("GET /api/v1/apps/{app}/releases", wrap(s.withApp(s.handleReleases)))
	mux.HandleFunc("GET /api/v1/apps/{app}/status", wrap(s.withApp(s.handleStatus)))
	mux.HandleFunc("GET /api/v1/apps/{app}/logs", wrap(s.withApp(s.handleLogs)))
}

// requireToken authenticates a bearer token against the app in the path.
// Unknown apps and wrong tokens get the same answer.
func (s *Server) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("app")
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		set, known := s.Tokens(name)
		id, valid := set.Verify(token)
		if !ok || !known || !valid || !appdef.ValidName(name) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="zymple"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid app or token"})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), actorKey, id)))
	}
}

type appHandler func(w http.ResponseWriter, r *http.Request, app *appdef.App)

func (s *Server) withApp(next appHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app, ok := s.App(r.PathValue("app"))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such app"})
			return
		}
		next(w, r, app)
	}
}

func actor(r *http.Request) string {
	if v, ok := r.Context().Value(actorKey).(string); ok {
		return v
	}
	return adminActor
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return false
	}
	return true
}

func (s *Server) baseContext() context.Context {
	if s.base != nil {
		return s.base
	}
	return context.Background()
}

// detached runs an operation that must finish even if the client leaves,
// streaming its events to whoever is still listening.
func (s *Server) detached(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, st *stream) error) {
	st := newStream()
	s.ops.Add(1)
	go func() {
		defer s.ops.Done()
		defer st.close()
		ctx := context.WithoutCancel(s.baseContext())
		if err := fn(ctx, st); err != nil {
			st.add(deploy.Event{Step: "done", Status: "fail", Message: err.Error()})
			return
		}
		st.add(deploy.Event{Step: "done", Status: "ok"})
	}()
	st.serve(w, r)
}

// attached runs an operation that stops when the client leaves.
func (s *Server) attached(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, out io.Writer) error) {
	st := newStream()
	go func() {
		defer st.close()
		out := st.writer("output")
		err := fn(r.Context(), out)
		out.flush()
		if err != nil {
			st.add(deploy.Event{Step: "done", Status: "fail", Message: err.Error()})
			return
		}
		st.add(deploy.Event{Step: "done", Status: "ok"})
	}()
	st.serve(w, r)
}

// engineRun adapts an engine call, which emits its own "done" event.
func (s *Server) engineRun(w http.ResponseWriter, r *http.Request, app *appdef.App,
	fn func(ctx context.Context, sink deploy.Sink) (releases.Release, error)) {
	if s.Engine.Busy(app.Name) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": deploy.ErrBusy.Error()})
		return
	}
	st := newStream()
	s.ops.Add(1)
	go func() {
		defer s.ops.Done()
		defer st.close()
		ctx := context.WithoutCancel(s.baseContext())
		done := false
		rel, err := fn(ctx, func(e deploy.Event) {
			done = done || e.Step == "done"
			st.add(e)
		})
		if err != nil {
			s.Log.Error("deploy failed", "app", app.Name, "error", err)
			// The engine reports its own outcome, except for failures that
			// happen before it starts.
			if !done {
				st.add(deploy.Event{Step: "done", Status: "fail", Message: err.Error()})
			}
			return
		}
		s.Log.Info("release is live", "app", app.Name, "release", rel.Number, "digest", rel.Digest, "by", rel.By)
	}()
	st.serve(w, r)
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request, app *appdef.App) {
	var body struct {
		// Digest is the app's own image. It may be left out when Images
		// names at least one extra image; the app's image then stays.
		Digest string            `json:"digest"`
		Images map[string]string `json:"images"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if msg := checkDeployImages(app, body.Digest, body.Images); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	by := actor(r)
	s.engineRun(w, r, app, func(ctx context.Context, sink deploy.Sink) (releases.Release, error) {
		return s.Engine.DeployImages(ctx, app, body.Digest, body.Images, by, sink)
	})
}

// checkDeployImages returns what is wrong with the images of a deploy
// request, "" if nothing.
func checkDeployImages(app *appdef.App, digest string, images map[string]string) string {
	const want = "must be sha256:<64 hex characters>"
	if digest == "" && len(images) == 0 {
		return "digest " + want
	}
	if digest != "" && !deploy.ValidDigest(digest) {
		return "digest " + want
	}
	for name, d := range images {
		if !app.HasImage(name) {
			return "the app declares no image of that name"
		}
		if !deploy.ValidDigest(d) {
			return "the digest of image " + name + " " + want
		}
	}
	return ""
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request, app *appdef.App) {
	var body struct {
		Release int `json:"release"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Release < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "release must not be negative"})
		return
	}
	by := actor(r)
	s.engineRun(w, r, app, func(ctx context.Context, sink deploy.Sink) (releases.Release, error) {
		return s.Engine.Rollback(ctx, app, body.Release, by, sink)
	})
}

func (s *Server) handleRedeploy(w http.ResponseWriter, r *http.Request, app *appdef.App) {
	by := actor(r)
	s.engineRun(w, r, app, func(ctx context.Context, sink deploy.Sink) (releases.Release, error) {
		return s.Engine.Redeploy(ctx, app, by, sink)
	})
}

func (s *Server) handleReleases(w http.ResponseWriter, _ *http.Request, app *appdef.App) {
	st, err := s.Store.Load(app.Name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	list := st.Releases
	if list == nil {
		list = []releases.Release{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"current": st.Current, "releases": list})
}

// Status is the answer to "what is this app running right now".
type Status struct {
	App        string            `json:"app"`
	Current    *releases.Release `json:"current"`
	Deploying  bool              `json:"deploying"`
	WebRunning bool              `json:"web_running"`
	LastError  string            `json:"last_error,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request, app *appdef.App) {
	st, err := s.Store.Load(app.Name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	status := Status{App: app.Name, Deploying: s.Engine.Busy(app.Name), LastError: st.LastError}
	if cur, ok := st.CurrentRelease(); ok {
		status.Current = &cur
		webs, err := s.Docker.Containers(r.Context(), app.Name, appdef.RoleWeb)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		for _, c := range webs {
			if c.Release == cur.Number && c.Running {
				status.WebRunning = true
			}
		}
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request, app *appdef.App) {
	q := r.URL.Query()
	tail := defaultTail
	if raw := q.Get("tail"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > maxLogTail {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tail must be between 0 and " + strconv.Itoa(maxLogTail)})
			return
		}
		tail = n
	}
	service := q.Get("service")
	role := appdef.RoleWeb
	if service != "" && service != app.Web.Service {
		if _, ok := app.Compose.Services[service]; !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such service"})
			return
		}
		role = appdef.RoleService
	} else {
		service = ""
	}
	follow := q.Get("follow") == "true"
	done, ok := s.logSlot(app.Name)
	if !ok {
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many log streams are open for this app"})
		return
	}
	defer done()
	s.attached(w, r, func(ctx context.Context, out io.Writer) error {
		return s.Docker.Logs(ctx, app.Name, role, service, tail, follow, out)
	})
}

// logSlot takes one of the app's log stream slots. Every open stream is a
// `docker logs` process and a backlog of its own, and a token holder can open
// as many as they like and read none of them, so the number is bounded.
func (s *Server) logSlot(app string) (release func(), ok bool) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logStreams[app] >= maxLogStreams {
		return nil, false
	}
	if s.logStreams == nil {
		s.logStreams = map[string]int{}
	}
	s.logStreams[app]++
	return func() {
		s.logMu.Lock()
		defer s.logMu.Unlock()
		if s.logStreams[app]--; s.logStreams[app] <= 0 {
			delete(s.logStreams, app)
		}
	}, true
}

func (s *Server) handleApps(w http.ResponseWriter, _ *http.Request) {
	type row struct {
		Name    string   `json:"name"`
		Domains []string `json:"domains"`
		Current int      `json:"current"`
	}
	rows := []row{}
	for _, name := range s.AppNames() {
		app, _ := s.App(name)
		st, err := s.Store.Load(name)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		rows = append(rows, row{Name: name, Domains: app.Domains, Current: st.Current})
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	s.detached(w, r, func(ctx context.Context, _ *stream) error { return s.Reload(ctx) })
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request, app *appdef.App) {
	var body struct {
		Command []string `json:"command"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if len(body.Command) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "command must not be empty"})
		return
	}
	st, err := s.Store.Load(app.Name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	cur, ok := st.CurrentRelease()
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the app has no release yet"})
		return
	}
	s.attached(w, r, func(ctx context.Context, out io.Writer) error {
		return s.Docker.Exec(ctx, app, cur.Number, body.Command, out)
	})
}

func (s *Server) requirePostgres(w http.ResponseWriter, app *appdef.App) bool {
	if app.Postgres == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this app has no Postgres"})
		return false
	}
	return true
}

func (s *Server) handlePGInfo(w http.ResponseWriter, r *http.Request, app *appdef.App) {
	if !s.requirePostgres(w, app) {
		return
	}
	s.attached(w, r, func(ctx context.Context, out io.Writer) error {
		return s.Postgres.Info(ctx, app.Name, out)
	})
}

func (s *Server) handlePGBackup(w http.ResponseWriter, r *http.Request, app *appdef.App) {
	var body struct {
		Type string `json:"type"`
	}
	if !readJSON(w, r, &body) || !s.requirePostgres(w, app) {
		return
	}
	if body.Type == "" {
		body.Type = "diff"
	}
	s.detached(w, r, func(ctx context.Context, st *stream) error {
		out := st.writer("backup")
		defer out.flush()
		return s.Postgres.Backup(ctx, app.Name, body.Type, out)
	})
}

func (s *Server) handlePGRestore(w http.ResponseWriter, r *http.Request, app *appdef.App) {
	var opts struct {
		Time string `json:"time"`
		Into string `json:"into"`
	}
	if !readJSON(w, r, &opts) || !s.requirePostgres(w, app) {
		return
	}
	restore := services.RestoreOptions{Time: opts.Time, Into: opts.Into}
	if err := restore.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.detached(w, r, func(ctx context.Context, st *stream) error {
		out := st.writer("restore")
		defer out.flush()
		if restore.Into != "" {
			return s.Postgres.Restore(ctx, app, restore, out)
		}
		return s.restoreInPlace(ctx, app, restore, st, out)
	})
}

// restoreInPlace stops the app, restores its database and starts it again.
// It holds the deploy lock so no deploy can run against a half-restored
// database.
func (s *Server) restoreInPlace(ctx context.Context, app *appdef.App, opts services.RestoreOptions, st *stream, out io.Writer) error {
	unlock, err := s.Engine.Lock(app.Name)
	if err != nil {
		return err
	}
	defer unlock()
	st.add(deploy.Event{Step: "restore", Status: "log", Message: "stopping the app's containers"})
	if err := s.Docker.StopAll(ctx, app.Name); err != nil {
		return err
	}
	restoreErr := s.Postgres.Restore(ctx, app, opts, out)
	st.add(deploy.Event{Step: "restore", Status: "log", Message: "starting the app's containers"})
	if err := s.Docker.StartAll(ctx, app.Name); err != nil {
		return errors.Join(restoreErr, err)
	}
	return restoreErr
}

func (s *Server) handlePGImport(w http.ResponseWriter, r *http.Request, app *appdef.App) {
	var body struct {
		File    string `json:"file"`
		Jobs    int    `json:"jobs"`
		NoStart bool   `json:"no_start"`
	}
	if !readJSON(w, r, &body) || !s.requirePostgres(w, app) {
		return
	}
	opts := services.ImportOptions{File: body.File, Jobs: body.Jobs}
	if err := opts.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.detached(w, r, func(ctx context.Context, st *stream) error {
		out := st.writer("import")
		defer out.flush()
		return s.importDatabase(ctx, app, opts, body.NoStart, st, out)
	})
}

// importDatabase stops the app, replaces its database with a dump and starts
// the app again. It holds the deploy lock throughout. If the import fails
// after the database was dropped, the app stays stopped: its database is then
// empty or half loaded, and serving from it would be worse than not serving.
func (s *Server) importDatabase(ctx context.Context, app *appdef.App, opts services.ImportOptions, noStart bool, st *stream, out io.Writer) error {
	unlock, err := s.Engine.Lock(app.Name)
	if err != nil {
		return err
	}
	defer unlock()
	note := func(msg string) { st.add(deploy.Event{Step: "import", Status: "log", Message: msg}) }
	note("stopping the app's containers")
	if err := s.Docker.StopAll(ctx, app.Name); err != nil {
		return err
	}
	if err := s.Postgres.Import(ctx, app, opts, out); err != nil {
		if errors.Is(err, services.ErrDatabaseUntouched) {
			note("starting the app's containers again; nothing was changed")
			return errors.Join(err, s.Docker.StartAll(ctx, app.Name))
		}
		return fmt.Errorf("%w; the app's containers are left stopped because its database is incomplete", err)
	}
	if noStart {
		note("the app's containers are left stopped; start them with `zymple reload`")
		return nil
	}
	note("starting the app's containers")
	return s.Docker.StartAll(ctx, app.Name)
}

func (s *Server) handlePGDrillRemove(w http.ResponseWriter, r *http.Request, app *appdef.App) {
	if !s.requirePostgres(w, app) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := s.Postgres.RemoveDrill(ctx, app.Name, r.PathValue("drill")); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"removed": r.PathValue("drill")})
}

func (s *Server) handleRedisBackup(w http.ResponseWriter, r *http.Request, app *appdef.App) {
	s.detached(w, r, func(ctx context.Context, st *stream) error {
		out := st.writer("backup")
		defer out.flush()
		return s.Redis.Backup(ctx, app, out)
	})
}

func (s *Server) handleRedisRestore(w http.ResponseWriter, r *http.Request, app *appdef.App) {
	s.detached(w, r, func(ctx context.Context, st *stream) error {
		out := st.writer("restore")
		defer out.flush()
		return s.Redis.Restore(ctx, app, out)
	})
}
