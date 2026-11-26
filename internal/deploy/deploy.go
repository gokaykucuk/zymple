// Package deploy implements the release state machine: blue/green switches,
// rollback, and convergence after a crash or reboot.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gokaykucuk/zymple/internal/appdef"
	"github.com/gokaykucuk/zymple/internal/releases"
	"github.com/gokaykucuk/zymple/internal/runtime"
)

// Errors a caller can test for.
var (
	ErrBusy          = errors.New("a deploy is already in progress for this app")
	ErrUnknownDigest = errors.New("no image with that digest has been pushed for this app")
	ErrNoRelease     = errors.New("no such release")
	ErrImageGone     = errors.New("the image of that release is no longer on this host (`docker image prune -a` removes kept releases); push it again")
)

var digestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// ValidDigest reports whether s is a sha256 image digest.
func ValidDigest(s string) bool { return digestRe.MatchString(s) }

// Runtime is the container side of a deploy.
type Runtime interface {
	ImageExists(ctx context.Context, app, digest string) (bool, error)
	EnsureNetwork(ctx context.Context, app string) error
	HasCompose(app string, release int) bool
	WriteCompose(app string, release int, data []byte) error
	RemoveCompose(app string, release int) error
	RunRelease(ctx context.Context, app *appdef.App, release int, out io.Writer) error
	StartWeb(ctx context.Context, app *appdef.App, release int, out io.Writer) error
	StopWeb(ctx context.Context, app string, release int) error
	RemoveWeb(ctx context.Context, app string, release int) error
	Containers(ctx context.Context, app, role string) ([]runtime.Container, error)
	StartContainers(ctx context.Context, ids []string) error
	UpServices(ctx context.Context, app *appdef.App, release int, out io.Writer) error
	Pin(ctx context.Context, app, digest string, release int) error
	Repin(ctx context.Context, app, digest string, release int) error
	Tags(ctx context.Context, app string) ([]runtime.Tag, error)
	RemoveTag(ctx context.Context, app, tag string) error
}

// Proxy switches an app's traffic to a loopback port.
type Proxy interface {
	Switch(ctx context.Context, app string, domains []string, port int) error
}

// Health waits for a web container to answer its health check.
type Health interface {
	Wait(ctx context.Context, port int, path string, timeout time.Duration) error
}

// Ports hands out loopback ports for web containers.
type Ports interface {
	Acquire() (int, error)
	Release(port int)
}

// Event is one line of deploy progress.
type Event struct {
	// Step names the stage: verify, release, start, health, switch, drain,
	// services, cleanup, or done.
	Step string `json:"step"`
	// Status is start, ok, fail, or log.
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Release int    `json:"release,omitempty"`
}

// Sink receives deploy events. It must not block for long.
type Sink func(Event)

// Engine runs deploys. All fields are required.
type Engine struct {
	Store   *releases.Store
	Runtime Runtime
	Proxy   Proxy
	Health  Health
	Ports   Ports
	// Env returns the variables injected into every service, starting the
	// app's managed services first if needed.
	Env func(ctx context.Context, app *appdef.App) (map[string]string, error)
	// Sleep waits for the drain period; replaced in tests.
	Sleep func(ctx context.Context, d time.Duration)
	Now   func() time.Time

	mu    sync.Mutex
	locks map[string]bool
	// tagSeen records, per app, when cleanup first saw each tag it left
	// alone. It is only a grace-period clock, so losing it in a restart is
	// harmless.
	tagSeen map[string]map[string]time.Time
}

// pushTagGrace is how long cleanup leaves a tag it cannot account for. Such
// a tag is usually an image another CI job has pushed and is about to
// deploy; removing it would make that deploy fail with an unknown digest.
const pushTagGrace = time.Hour

// maxLogLine bounds one line of command output. Output without a newline is
// cut into lines of this size instead of growing in memory.
const maxLogLine = 64 << 10

func (e *Engine) tryLock(app string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.locks == nil {
		e.locks = map[string]bool{}
	}
	if e.locks[app] {
		return false
	}
	e.locks[app] = true
	return true
}

func (e *Engine) unlock(app string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.locks, app)
}

// Busy reports whether a deploy is running for the app.
func (e *Engine) Busy(app string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.locks[app]
}

// Lock takes the app's deploy lock for an operation outside this package,
// such as a database restore. It returns ErrBusy if a deploy is running.
func (e *Engine) Lock(app string) (unlock func(), err error) {
	if !e.tryLock(app) {
		return nil, ErrBusy
	}
	return func() { e.unlock(app) }, nil
}

// request describes one run of the state machine.
type request struct {
	app            *appdef.App
	digest         string
	by             string
	skipRelease    bool
	rolledBackFrom int
	// fromRelease is the recorded release the digest was taken from, 0 when
	// the digest came from the caller. Only a recorded digest may be
	// re-pinned by image id: doing that for a caller's digest would let one
	// app deploy an image that was only ever pushed to another app.
	fromRelease int
}

// Deploy releases a pushed image.
func (e *Engine) Deploy(ctx context.Context, app *appdef.App, digest, by string, sink Sink) (releases.Release, error) {
	return e.locked(ctx, request{app: app, digest: digest, by: by}, sink, nil)
}

// Rollback re-activates an older release's image without running the release
// command. target 0 means the release before the current one.
func (e *Engine) Rollback(ctx context.Context, app *appdef.App, target int, by string, sink Sink) (releases.Release, error) {
	pick := func(st *releases.State) (request, error) {
		rel, ok := pickRollback(st, target)
		if !ok {
			return request{}, ErrNoRelease
		}
		return request{app: app, digest: rel.Digest, by: by, skipRelease: true, rolledBackFrom: st.Current, fromRelease: rel.Number}, nil
	}
	return e.locked(ctx, request{app: app}, sink, pick)
}

// Redeploy starts a fresh release of the current image, so that changes to
// the app definition or its env file take effect without downtime.
func (e *Engine) Redeploy(ctx context.Context, app *appdef.App, by string, sink Sink) (releases.Release, error) {
	pick := func(st *releases.State) (request, error) {
		rel, ok := st.CurrentRelease()
		if !ok {
			return request{}, ErrNoRelease
		}
		return request{app: app, digest: rel.Digest, by: by, skipRelease: true, fromRelease: rel.Number}, nil
	}
	return e.locked(ctx, request{app: app}, sink, pick)
}

func pickRollback(st *releases.State, target int) (releases.Release, bool) {
	if target != 0 {
		if target == st.Current {
			return releases.Release{}, false
		}
		return st.Find(target)
	}
	best := releases.Release{}
	for _, r := range st.Releases {
		if r.Number < st.Current && r.Number > best.Number {
			best = r
		}
	}
	return best, best.Number != 0
}

func (e *Engine) locked(ctx context.Context, req request, sink Sink, pick func(*releases.State) (request, error)) (releases.Release, error) {
	if sink == nil {
		sink = func(Event) {
			// The caller does not want progress events.
		}
	}
	name := req.app.Name
	if !e.tryLock(name) {
		return releases.Release{}, ErrBusy
	}
	defer e.unlock(name)
	st, err := e.Store.Load(name)
	if err != nil {
		return releases.Release{}, err
	}
	if pick != nil {
		if req, err = pick(st); err != nil {
			return releases.Release{}, err
		}
	}
	rel, err := e.run(ctx, req, st, sink)
	if err != nil {
		st.LastError = err.Error()
		if saveErr := e.Store.Save(name, st); saveErr != nil {
			err = fmt.Errorf("%w (recording the failure also failed: %v)", err, saveErr)
		}
		sink(Event{Step: "done", Status: "fail", Message: err.Error()})
		return releases.Release{}, err
	}
	sink(Event{Step: "done", Status: "ok", Release: rel.Number, Message: fmt.Sprintf("release %d is live", rel.Number)})
	return rel, nil
}

// stepper reports the start and outcome of each step.
type stepper struct {
	sink    Sink
	release int
}

func (s stepper) do(step string, fn func() error) error {
	s.sink(Event{Step: step, Status: "start", Release: s.release})
	if err := fn(); err != nil {
		s.sink(Event{Step: step, Status: "fail", Release: s.release, Message: err.Error()})
		return fmt.Errorf("%s: %w", step, err)
	}
	s.sink(Event{Step: step, Status: "ok", Release: s.release})
	return nil
}

func (e *Engine) run(ctx context.Context, req request, st *releases.State, sink Sink) (releases.Release, error) {
	app := req.app
	none := releases.Release{}
	if !ValidDigest(req.digest) {
		return none, fmt.Errorf("verify: %q is not a sha256 digest", req.digest)
	}

	// Reserve the release number first so a failed attempt never shares a
	// number, a compose file or a container name with a later one.
	number := st.Next
	st.Next++
	if err := e.Store.Save(app.Name, st); err != nil {
		return none, err
	}
	steps := stepper{sink: sink, release: number}
	out := &logWriter{sink: sink, step: "release", release: number}

	if err := steps.do("verify", func() error {
		if req.fromRelease != 0 {
			return e.ensureImage(ctx, app.Name, req.digest, req.fromRelease)
		}
		ok, err := e.Runtime.ImageExists(ctx, app.Name, req.digest)
		if err != nil {
			return err
		}
		if !ok {
			return ErrUnknownDigest
		}
		return nil
	}); err != nil {
		return none, err
	}

	port, err := e.Ports.Acquire()
	if err != nil {
		return none, fmt.Errorf("start: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			e.Ports.Release(port)
		}
	}()

	if err := e.prepare(ctx, app, number, req.digest, port); err != nil {
		return none, fmt.Errorf("start: %w", err)
	}
	// From here on, a failure must leave nothing of this attempt behind.
	abort := func(err error) (releases.Release, error) {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if rmErr := e.Runtime.RemoveWeb(cleanupCtx, app.Name, number); rmErr != nil {
			err = fmt.Errorf("%w (removing the new container also failed: %v)", err, rmErr)
		}
		if rmErr := e.Runtime.RemoveCompose(app.Name, number); rmErr != nil {
			err = fmt.Errorf("%w (removing its compose file also failed: %v)", err, rmErr)
		}
		return none, err
	}

	if len(app.Release) > 0 && !req.skipRelease {
		if err := steps.do("release", func() error { return e.Runtime.RunRelease(ctx, app, number, out) }); err != nil {
			return abort(err)
		}
	}
	out.step = "start"
	if err := steps.do("start", func() error { return e.Runtime.StartWeb(ctx, app, number, out) }); err != nil {
		return abort(err)
	}
	if err := steps.do("health", func() error {
		return e.Health.Wait(ctx, port, app.Web.Healthcheck, time.Duration(app.Web.HealthTimeout))
	}); err != nil {
		return abort(err)
	}
	if err := steps.do("switch", func() error { return e.Proxy.Switch(ctx, app.Name, app.Domains, port) }); err != nil {
		return abort(err)
	}

	// Commit point. Once this is saved the new release is the truth, and a
	// crash after it converges forward instead of back.
	previous := st.Current
	rel := releases.Release{
		Number: number, Digest: req.digest, Port: port, Time: e.Now().UTC(), By: req.by,
		RolledBackFrom: req.rolledBackFrom,
	}
	st.Releases = append(st.Releases, rel)
	st.Current = number
	st.LastError = ""
	if err := e.Store.Save(app.Name, st); err != nil {
		st.Releases = st.Releases[:len(st.Releases)-1]
		st.Current = previous
		err = fmt.Errorf("switch: recording the release failed: %w", err)
		if old, ok := st.Find(previous); ok {
			if swErr := e.Proxy.Switch(ctx, app.Name, app.Domains, old.Port); swErr != nil {
				err = fmt.Errorf("%w (switching back also failed: %v)", err, swErr)
			}
		}
		return abort(err)
	}
	committed = true

	// The release is live. Nothing below can fail the deploy; problems are
	// reported and the next deploy or reconcile retries them.
	post := context.WithoutCancel(ctx)
	e.afterSwitch(post, app, st, previous, number, sink)
	return rel, nil
}

// ensureImage makes sure a recorded release's image still has its name in
// the app's repository, restoring the name when the content survived.
func (e *Engine) ensureImage(ctx context.Context, app, digest string, release int) error {
	ok, err := e.Runtime.ImageExists(ctx, app, digest)
	if err != nil || ok {
		return err
	}
	if err := e.Runtime.Repin(ctx, app, digest, release); err != nil {
		return ErrImageGone
	}
	ok, err = e.Runtime.ImageExists(ctx, app, digest)
	if err != nil {
		return err
	}
	if !ok {
		return ErrImageGone
	}
	return nil
}

func (e *Engine) prepare(ctx context.Context, app *appdef.App, number int, digest string, port int) error {
	if err := e.Runtime.EnsureNetwork(ctx, app.Name); err != nil {
		return err
	}
	env, err := e.Env(ctx, app)
	if err != nil {
		return err
	}
	data, err := app.Render(appdef.RenderInput{Release: number, Digest: digest, Port: port, Env: env})
	if err != nil {
		return err
	}
	return e.Runtime.WriteCompose(app.Name, number, data)
}

func (e *Engine) afterSwitch(ctx context.Context, app *appdef.App, st *releases.State, previous, number int, sink Sink) {
	steps := stepper{sink: sink, release: number}
	warn := func(step string, err error) {
		sink(Event{Step: step, Status: "log", Release: number, Message: "warning: " + err.Error()})
	}
	if previous != 0 {
		_ = steps.do("drain", func() error {
			e.Sleep(ctx, time.Duration(app.Web.Drain))
			return e.Runtime.StopWeb(ctx, app.Name, previous)
		})
	}
	out := &logWriter{sink: sink, step: "services", release: number}
	_ = steps.do("services", func() error { return e.Runtime.UpServices(ctx, app, number, out) })
	_ = steps.do("cleanup", func() error {
		if err := e.Runtime.Pin(ctx, app.Name, st.Releases[len(st.Releases)-1].Digest, number); err != nil {
			return err
		}
		return e.prune(ctx, app, st, warn)
	})
}

// prune keeps the newest keep_releases releases and removes the tags, compose
// files and ports of the rest, plus any tag left behind by a push.
func (e *Engine) prune(ctx context.Context, app *appdef.App, st *releases.State, warn func(string, error)) error {
	sort.Slice(st.Releases, func(i, j int) bool { return st.Releases[i].Number < st.Releases[j].Number })
	var kept []releases.Release
	excess := len(st.Releases) - app.KeepReleases
	for i, r := range st.Releases {
		if i < excess && r.Number != st.Current {
			e.Ports.Release(r.Port)
			if err := e.Runtime.RemoveCompose(app.Name, r.Number); err != nil {
				warn("cleanup", err)
			}
			continue
		}
		kept = append(kept, r)
	}
	st.Releases = kept
	if err := e.Store.Save(app.Name, st); err != nil {
		return err
	}
	tags, err := e.Runtime.Tags(ctx, app.Name)
	if err != nil {
		return err
	}
	for _, tag := range e.staleTags(app.Name, tags, kept) {
		if err := e.Runtime.RemoveTag(ctx, app.Name, tag); err != nil {
			warn("cleanup", err)
		}
	}
	return nil
}

// staleTags picks the tags cleanup removes now.
//
// A release tag of a release that is no longer kept goes at once, and so does
// any other tag on a kept release's image, because the release tag already
// protects that image. Every other tag is left for pushTagGrace from the
// moment it is first seen: it may belong to a push whose deploy has not
// started yet.
func (e *Engine) staleTags(app string, tags []runtime.Tag, kept []releases.Release) []string {
	keep := make(map[string]bool, len(kept))
	keptDigest := make(map[string]bool, len(kept))
	for _, r := range kept {
		keep[runtime.ReleaseTag(r.Number)] = true
		keptDigest[r.Digest] = true
	}
	now := e.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	before := e.tagSeen[app]
	waiting := map[string]time.Time{}
	var stale []string
	for _, tag := range tags {
		switch {
		case keep[tag.Name]:
		case strings.HasPrefix(tag.Name, runtime.ReleaseTagPrefix) || keptDigest[tag.Digest]:
			stale = append(stale, tag.Name)
		default:
			first, seen := before[tag.Name]
			if !seen {
				first = now
			}
			if now.Sub(first) >= pushTagGrace {
				stale = append(stale, tag.Name)
			} else {
				waiting[tag.Name] = first
			}
		}
	}
	if e.tagSeen == nil {
		e.tagSeen = map[string]map[string]time.Time{}
	}
	// Replacing the map forgets tags that are gone.
	e.tagSeen[app] = waiting
	return stale
}

// webState is what Reconcile found among an app's web containers.
type webState struct {
	// stale lists the releases, other than the current one, that still have
	// a web container.
	stale []int
	// stopped lists the current release's containers that are not running.
	stopped []string
	// found reports whether the current release has a container at all,
	// running whether one of them is running.
	found, running bool
}

func classifyWebs(webs []runtime.Container, current int) webState {
	var ws webState
	seen := map[int]bool{}
	for _, c := range webs {
		switch {
		case c.Release != current || current == 0:
			if !seen[c.Release] {
				seen[c.Release] = true
				ws.stale = append(ws.stale, c.Release)
			}
		case c.Running:
			ws.found, ws.running = true, true
		default:
			ws.found = true
			ws.stopped = append(ws.stopped, c.ID)
		}
	}
	return ws
}

// Reconcile makes the host match the recorded state: it points the proxy at
// the current release, removes web containers of releases that are not
// current, starts the current one if it is stopped and brings the other
// services up. It is run at startup, so a crash at any point of a deploy
// converges.
func (e *Engine) Reconcile(ctx context.Context, app *appdef.App) error {
	name := app.Name
	if !e.tryLock(name) {
		return ErrBusy
	}
	defer e.unlock(name)
	st, err := e.Store.Load(name)
	if err != nil {
		return err
	}
	webs, err := e.Runtime.Containers(ctx, name, appdef.RoleWeb)
	if err != nil {
		return err
	}
	ws := classifyWebs(webs, st.Current)
	cur, ok := st.CurrentRelease()
	// A crash between the proxy switch and the commit leaves the proxy on a
	// release that was never recorded. Move traffic back to the recorded one
	// before that container is removed; everything after this point can be
	// slow, starting a database included.
	if ok && ws.running {
		if err := e.Proxy.Switch(ctx, name, app.Domains, cur.Port); err != nil {
			return err
		}
	}
	for _, release := range ws.stale {
		if err := e.Runtime.RemoveWeb(ctx, name, release); err != nil {
			return err
		}
	}
	if !ok {
		return nil
	}
	stopped, currentFound := ws.stopped, ws.found
	if err := e.ensureImage(ctx, name, cur.Digest, cur.Number); err != nil {
		return err
	}
	if err := e.Runtime.EnsureNetwork(ctx, name); err != nil {
		return err
	}
	if !e.Runtime.HasCompose(name, cur.Number) || !currentFound {
		if err := e.prepare(ctx, app, cur.Number, cur.Digest, cur.Port); err != nil {
			return err
		}
	} else if _, err := e.Env(ctx, app); err != nil {
		return err
	}
	if err := e.Runtime.StartContainers(ctx, stopped); err != nil {
		return err
	}
	if !currentFound {
		if err := e.Runtime.StartWeb(ctx, app, cur.Number, io.Discard); err != nil {
			return err
		}
	}
	if err := e.Proxy.Switch(ctx, name, app.Domains, cur.Port); err != nil {
		return err
	}
	return e.Runtime.UpServices(ctx, app, cur.Number, io.Discard)
}

// logWriter turns command output into log events, one per line.
type logWriter struct {
	sink    Sink
	step    string
	release int
	buf     strings.Builder
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	text := w.buf.String()
	for {
		i := strings.IndexByte(text, '\n')
		if i < 0 && len(text) >= maxLogLine {
			// No newline in sight: emit what there is so a command that
			// never ends its line cannot grow the buffer without limit.
			i = maxLogLine
			w.emit(text[:i])
			text = text[i:]
			continue
		}
		if i < 0 {
			break
		}
		w.emit(text[:i])
		text = text[i+1:]
	}
	w.buf.Reset()
	w.buf.WriteString(text)
	return len(p), nil
}

func (w *logWriter) emit(line string) {
	if line = strings.TrimRight(line, "\r"); line != "" {
		w.sink(Event{Step: w.step, Status: "log", Release: w.release, Message: line})
	}
}
