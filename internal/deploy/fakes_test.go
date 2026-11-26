package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gokaykucuk/zymple/internal/appdef"
	"github.com/gokaykucuk/zymple/internal/releases"
	"github.com/gokaykucuk/zymple/internal/runtime"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

var errInjected = errors.New("injected failure")

// world is a fake host: it records every operation in order and can be told
// to fail any of them.
type world struct {
	mu       sync.Mutex
	log      []string
	fail     map[string]error
	images   map[string]bool // digests present in the repository
	content  map[string]bool // digests whose content survives without a name
	webs     map[int]bool    // release -> running
	compose  map[int]bool
	tags     map[string]string // tag -> digest
	oneoffs  map[string]int    // one-off container id -> release
	port     int               // where the proxy points
	hook     map[string]func()
	nextPort int
	released []int
	slept    []time.Duration
	// releaseHangs makes the release command run until it is cancelled.
	releaseHangs bool
}

func newWorld() *world {
	return &world{
		fail: map[string]error{}, images: map[string]bool{digestA: true, digestB: true, digestC: true},
		content: map[string]bool{}, webs: map[int]bool{}, compose: map[int]bool{}, tags: map[string]string{},
		oneoffs: map[string]int{},
		hook:    map[string]func(){}, nextPort: 20000,
	}
}

func (w *world) op(name string, args ...any) error {
	w.mu.Lock()
	w.log = append(w.log, strings.TrimSpace(name+" "+strings.TrimSpace(fmt.Sprintln(args...))))
	err := w.fail[name]
	hook := w.hook[name]
	w.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (w *world) calls(name string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, l := range w.log {
		if l == name || strings.HasPrefix(l, name+" ") {
			n++
		}
	}
	return n
}

func (w *world) has(entry string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, l := range w.log {
		if l == entry {
			return true
		}
	}
	return false
}

func (w *world) order() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	names := make([]string, len(w.log))
	for i, l := range w.log {
		names[i] = strings.Fields(l)[0]
	}
	return strings.Join(names, " ")
}

func (w *world) resetLog() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.log = nil
}

func (w *world) runningWebs() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []int
	for n := range w.webs {
		out = append(out, n)
	}
	return out
}

// Runtime.

func (w *world) ImageExists(_ context.Context, _ string, digest string) (bool, error) {
	if err := w.op("ImageExists"); err != nil {
		return false, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.images[digest], nil
}

func (w *world) Repin(_ context.Context, _ string, digest string, release int) error {
	if err := w.op("Repin", release); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.content[digest] {
		return errors.New("no such image")
	}
	w.images[digest] = true
	return nil
}

func (w *world) EnsureNetwork(context.Context, string) error { return w.op("EnsureNetwork") }

func (w *world) HasCompose(_ string, release int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.compose[release]
}

func (w *world) WriteCompose(_ string, release int, _ []byte) error {
	if err := w.op("WriteCompose", release); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.compose[release] = true
	return nil
}

func (w *world) RemoveCompose(_ string, release int) error {
	err := w.op("RemoveCompose", release)
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.compose, release)
	return err
}

func (w *world) RunRelease(ctx context.Context, _ *appdef.App, release int, out io.Writer) error {
	fmt.Fprint(out, "migrating\nstill migrating\r\npartial")
	if err := w.op("RunRelease", release); err != nil {
		return err
	}
	w.mu.Lock()
	hangs := w.releaseHangs
	w.mu.Unlock()
	if hangs {
		// Like a killed docker client: it returns only when its context ends.
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (w *world) StartWeb(_ context.Context, _ *appdef.App, release int, _ io.Writer) error {
	if err := w.op("StartWeb", release); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.webs[release] = true
	return nil
}

func (w *world) StopWeb(_ context.Context, _ string, release int) error {
	if err := w.op("StopWeb", release); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.webs, release)
	return nil
}

func (w *world) RemoveWeb(_ context.Context, _ string, release int) error {
	err := w.op("RemoveWeb", release)
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.webs, release)
	return err
}

func (w *world) Containers(_ context.Context, _, _ string) ([]runtime.Container, error) {
	if err := w.op("Containers"); err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var list []runtime.Container
	for n, running := range w.webs {
		list = append(list, runtime.Container{ID: fmt.Sprintf("c%d", n), Release: n, Running: running})
	}
	return list, nil
}

// RemoveOneOffs removes the one-off containers match selects. Containers
// never lists them, as with the real runtime.
func (w *world) RemoveOneOffs(_ context.Context, _ string, match func(int) bool) error {
	if err := w.op("RemoveOneOffs"); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, release := range w.oneoffs {
		if match(release) {
			delete(w.oneoffs, id)
		}
	}
	return nil
}

func (w *world) hasOneOff(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.oneoffs[id]
	return ok
}

func (w *world) addOneOff(id string, release int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.oneoffs[id] = release
}

func (w *world) StartContainers(_ context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return w.op("StartContainers", strings.Join(ids, ","))
}

func (w *world) UpServices(_ context.Context, _ *appdef.App, release int, _ io.Writer) error {
	return w.op("UpServices", release)
}

func (w *world) Pin(_ context.Context, _ string, digest string, release int) error {
	if err := w.op("Pin", release); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tags[runtime.ReleaseTag(release)] = digest
	return nil
}

func (w *world) Tags(context.Context, string) ([]runtime.Tag, error) {
	if err := w.op("Tags"); err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var tags []runtime.Tag
	for tag, digest := range w.tags {
		tags = append(tags, runtime.Tag{Name: tag, Digest: digest})
	}
	return tags, nil
}

func (w *world) hasTag(tag string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.tags[tag]
	return ok
}

// indexOf returns the position of the first log entry equal to entry, or -1.
func (w *world) indexOf(entry string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, l := range w.log {
		if l == entry {
			return i
		}
	}
	return -1
}

func (w *world) RemoveTag(_ context.Context, _ string, tag string) error {
	if err := w.op("RemoveTag", tag); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.tags, tag)
	return nil
}

// Proxy, Health and Ports.

func (w *world) Switch(_ context.Context, _ string, _ []string, port int) error {
	if err := w.op("Switch", port); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.port = port
	return nil
}

func (w *world) Wait(_ context.Context, port int, path string, _ time.Duration) error {
	return w.op("Health", port, path)
}

func (w *world) Acquire() (int, error) {
	if err := w.op("Acquire"); err != nil {
		return 0, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.nextPort++
	return w.nextPort, nil
}

func (w *world) Release(port int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.released = append(w.released, port)
}

func (w *world) wasReleased(port int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range w.released {
		if p == port {
			return true
		}
	}
	return false
}

func testApp(t *testing.T, extra string) *appdef.App {
	t.Helper()
	app, err := appdef.Parse(
		[]byte("name: shop\ndomains: [shop.example.com]\nweb: { service: web, port: 3000, healthcheck: /healthz, drain: 7s }\nrelease: [\"bin/migrate\"]\n"+extra),
		[]byte("services:\n  web:\n    image: ${IMAGE}\n  worker:\n    image: ${IMAGE}\n"))
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func newEngine(t *testing.T, w *world) *Engine {
	t.Helper()
	return &Engine{
		Store:   &releases.Store{Root: t.TempDir()},
		Runtime: w,
		Proxy:   w,
		Health:  w,
		Ports:   w,
		Env: func(context.Context, *appdef.App) (map[string]string, error) {
			return map[string]string{"DATABASE_URL": "postgres://x"}, w.op("Env")
		},
		Sleep: func(_ context.Context, d time.Duration) {
			w.mu.Lock()
			w.slept = append(w.slept, d)
			w.mu.Unlock()
		},
		Now: func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
}

// events collects a deploy's event stream.
type events struct {
	mu   sync.Mutex
	list []Event
}

func (e *events) sink(ev Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, ev)
}

func (e *events) last() Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.list) == 0 {
		return Event{}
	}
	return e.list[len(e.list)-1]
}

func (e *events) messages() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var b strings.Builder
	for _, ev := range e.list {
		fmt.Fprintf(&b, "%s/%s:%s\n", ev.Step, ev.Status, ev.Message)
	}
	return b.String()
}

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}
