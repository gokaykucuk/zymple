package deploy

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gokaykucuk/zymple/internal/appdef"
	"github.com/gokaykucuk/zymple/internal/releases"
	"github.com/gokaykucuk/zymple/internal/runtime"
)

var ctx = context.Background()

func mustDeploy(t *testing.T, e *Engine, w *world, digest string) int {
	t.Helper()
	rel, err := e.Deploy(ctx, testApp(t, ""), digest, "ci", nil)
	if err != nil {
		t.Fatalf("deploy: %v\nlog: %s", err, w.order())
	}
	return rel.Number
}

func TestFirstDeploy(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	var ev events
	rel, err := e.Deploy(ctx, testApp(t, ""), digestA, "ci", ev.sink)
	if err != nil {
		t.Fatal(err)
	}
	want := "ImageExists Acquire EnsureNetwork Env WriteCompose RunRelease StartWeb Health Switch UpServices Pin Tags"
	if got := w.order(); got != want {
		t.Errorf("operation order:\n got %s\nwant %s", got, want)
	}
	if rel.Number != 1 || rel.Digest != digestA || rel.By != "ci" || rel.Port != 20001 || w.port != 20001 {
		t.Errorf("release = %+v, proxy port %d", rel, w.port)
	}
	if !w.has("Health 20001 /healthz") {
		t.Errorf("health check not run against the new port and path: %v", w.log)
	}
	st, _ := e.Store.Load("shop")
	if st.Current != 1 || st.Next != 2 || len(st.Releases) != 1 || st.LastError != "" {
		t.Errorf("state = %+v", st)
	}
	if last := ev.last(); last.Step != "done" || last.Status != "ok" || last.Release != 1 {
		t.Errorf("last event = %+v", last)
	}
	msgs := ev.messages()
	for _, want := range []string{"release/log:migrating", "release/log:still migrating", "verify/ok", "switch/ok"} {
		if !strings.Contains(msgs, want) {
			t.Errorf("events lack %q:\n%s", want, msgs)
		}
	}
	if len(w.slept) != 0 {
		t.Error("the first deploy has nothing to drain")
	}
	if e.Busy("shop") {
		t.Error("the lock was not released")
	}
}

func TestSecondDeployDrainsAndStopsTheOldRelease(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	mustDeploy(t, e, w, digestA)
	w.resetLog()
	mustDeploy(t, e, w, digestB)
	want := "ImageExists Acquire EnsureNetwork Env WriteCompose RunRelease StartWeb Health Switch StopWeb UpServices Pin Tags"
	if got := w.order(); got != want {
		t.Errorf("operation order:\n got %s\nwant %s", got, want)
	}
	if !w.has("StopWeb 1") || !w.has("UpServices 2") {
		t.Errorf("old release not stopped or services not moved: %v", w.log)
	}
	if len(w.slept) != 1 || w.slept[0] != 7*time.Second {
		t.Errorf("drain = %v, want one 7s sleep", w.slept)
	}
	if webs := w.runningWebs(); len(webs) != 1 || webs[0] != 2 {
		t.Errorf("running web releases = %v, want [2]", webs)
	}
}

// TestFailureBeforeTheSwitchLeavesTheOldReleaseServing injects a failure
// into every step up to and including the proxy switch.
func TestFailureBeforeTheSwitchLeavesTheOldReleaseServing(t *testing.T) {
	steps := []struct {
		op          string
		startedWeb  bool
		wantInError string
	}{
		{"ImageExists", false, "verify"},
		{"Acquire", false, "start"},
		{"EnsureNetwork", false, "start"},
		{"Env", false, "start"},
		{"WriteCompose", false, "start"},
		{"RunRelease", false, "release"},
		{"StartWeb", false, "start"},
		{"Health", true, "health"},
		{"Switch", true, "switch"},
	}
	for _, step := range steps {
		t.Run(step.op, func(t *testing.T) {
			w := newWorld()
			e := newEngine(t, w)
			mustDeploy(t, e, w, digestA)
			oldPort := w.port
			w.resetLog()
			w.fail[step.op] = errInjected

			var ev events
			_, err := e.Deploy(ctx, testApp(t, ""), digestB, "ci", ev.sink)
			if !errors.Is(err, errInjected) || !strings.Contains(err.Error(), step.wantInError) {
				t.Fatalf("error = %v, want injected failure in step %q", err, step.wantInError)
			}
			if w.port != oldPort {
				t.Errorf("proxy moved to %d although the deploy failed", w.port)
			}
			if webs := w.runningWebs(); len(webs) != 1 || webs[0] != 1 {
				t.Errorf("running web releases = %v, want only the old one", webs)
			}
			if w.compose[2] {
				t.Error("the failed attempt's compose file was left behind")
			}
			if w.calls("StopWeb") != 0 || w.calls("UpServices") != 0 || w.calls("Pin") != 0 {
				t.Errorf("post-switch work ran after a failure: %v", w.log)
			}
			if step.startedWeb && !w.has("RemoveWeb 2") {
				t.Errorf("the new container was not removed: %v", w.log)
			}
			st, _ := e.Store.Load("shop")
			if st.Current != 1 || len(st.Releases) != 1 || st.LastError == "" || st.Next != 3 {
				t.Errorf("state after failure = %+v", st)
			}
			if last := ev.last(); last.Step != "done" || last.Status != "fail" {
				t.Errorf("last event = %+v", last)
			}
			if w.calls("Acquire") == 1 && step.op != "Acquire" && !w.wasReleased(20002) {
				t.Error("the failed attempt's port was not returned")
			}
			if e.Busy("shop") {
				t.Fatal("the lock was not released")
			}
			// The same app deploys cleanly once the fault is gone, and the
			// failed attempt's number is never reused.
			delete(w.fail, step.op)
			if n := mustDeploy(t, e, w, digestB); n != 3 {
				t.Errorf("next release number = %d, want 3", n)
			}
			if st, _ := e.Store.Load("shop"); st.LastError != "" {
				t.Errorf("LastError not cleared after a success: %q", st.LastError)
			}
		})
	}
}

func TestCleanupFailureIsReportedWithTheOriginalError(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	mustDeploy(t, e, w, digestA)
	w.fail["Health"] = errInjected
	w.fail["RemoveOneOffs"] = errors.New("cannot reap")
	w.fail["RemoveWeb"] = errors.New("cannot remove")
	w.fail["RemoveCompose"] = errors.New("cannot delete")
	_, err := e.Deploy(ctx, testApp(t, ""), digestB, "ci", nil)
	if !errors.Is(err, errInjected) {
		t.Fatalf("error lost its cause: %v", err)
	}
	for _, want := range []string{"cannot reap", "cannot remove", "cannot delete"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks the cleanup failure %q: %v", want, err)
		}
	}
}

// TestFailureAfterTheSwitchDoesNotFailTheDeploy: once traffic has moved, the
// release is live whatever happens to the housekeeping.
func TestFailureAfterTheSwitchDoesNotFailTheDeploy(t *testing.T) {
	for _, op := range []string{"StopWeb", "UpServices", "Pin", "Tags", "RemoveTag", "RemoveCompose"} {
		t.Run(op, func(t *testing.T) {
			w := newWorld()
			e := newEngine(t, w)
			app := testApp(t, "keep_releases: 1\n")
			if _, err := e.Deploy(ctx, app, digestA, "ci", nil); err != nil {
				t.Fatal(err)
			}
			// The tag this deploy's own push left behind.
			w.tags["push-leftover"] = digestB
			w.fail[op] = errInjected
			var ev events
			rel, err := e.Deploy(ctx, app, digestB, "ci", ev.sink)
			if err != nil {
				t.Fatalf("deploy failed although the switch succeeded: %v", err)
			}
			if rel.Number != 2 || w.port != rel.Port {
				t.Errorf("release = %+v, proxy port %d", rel, w.port)
			}
			st, _ := e.Store.Load("shop")
			if st.Current != 2 {
				t.Errorf("current = %d, want 2", st.Current)
			}
			if !strings.Contains(ev.messages(), "injected failure") {
				t.Errorf("the problem was not reported:\n%s", ev.messages())
			}
			if last := ev.last(); last.Status != "ok" {
				t.Errorf("last event = %+v", last)
			}
		})
	}
}

func TestCommitFailureSwitchesBack(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	w := newWorld()
	e := newEngine(t, w)
	mustDeploy(t, e, w, digestA)
	oldPort := w.port
	dir := e.Store.Dir("shop")
	// Make the state unwritable at the moment the proxy switches.
	w.hook["Switch"] = func() { _ = os.Chmod(dir, 0o500) }
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, err := e.Deploy(ctx, testApp(t, ""), digestB, "ci", nil)
	_ = os.Chmod(dir, 0o700)
	if err == nil || !strings.Contains(err.Error(), "recording the release failed") {
		t.Fatalf("error = %v", err)
	}
	if w.port != oldPort {
		t.Errorf("proxy left on %d, want it switched back to %d", w.port, oldPort)
	}
	if webs := w.runningWebs(); len(webs) != 1 || webs[0] != 1 {
		t.Errorf("running web releases = %v, want only the old one", webs)
	}
	delete(w.hook, "Switch")
	st, _ := e.Store.Load("shop")
	if st.Current != 1 {
		t.Errorf("current = %d, want 1", st.Current)
	}
}

func TestStateFailures(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	w := newWorld()
	e := newEngine(t, w)
	mustDeploy(t, e, w, digestA)
	dir := e.Store.Dir("shop")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	w.resetLog()
	if _, err := e.Deploy(ctx, testApp(t, ""), digestB, "ci", nil); err == nil || w.calls("StartWeb") != 0 {
		t.Fatalf("a deploy that cannot reserve its number must not start: %v, %v", err, w.log)
	}
	_ = os.Chmod(dir, 0o700)
	if err := os.WriteFile(dir+"/releases.json", []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Deploy(ctx, testApp(t, ""), digestB, "ci", nil); err == nil {
		t.Error("corrupt state must fail the deploy")
	}
	if err := e.Reconcile(ctx, testApp(t, "")); err == nil {
		t.Error("corrupt state must fail reconcile")
	}
}

func TestDeployRejectsBadAndUnknownDigests(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	for _, bad := range []string{"", "latest", "sha256:abc", "sha256:" + strings.Repeat("G", 64), "md5:" + strings.Repeat("a", 64)} {
		if _, err := e.Deploy(ctx, testApp(t, ""), bad, "ci", nil); err == nil {
			t.Errorf("digest %q accepted", bad)
		}
	}
	if w.calls("ImageExists") != 0 {
		t.Error("an invalid digest reached the runtime")
	}
	// A digest whose content exists on the host, but was never pushed to
	// this app, must not be deployable: that would cross app boundaries.
	delete(w.images, digestC)
	w.content[digestC] = true
	_, err := e.Deploy(ctx, testApp(t, ""), digestC, "ci", nil)
	if !errors.Is(err, ErrUnknownDigest) {
		t.Fatalf("error = %v, want ErrUnknownDigest", err)
	}
	if w.calls("Repin") != 0 {
		t.Fatal("a caller-supplied digest was re-pinned by image id")
	}
}

func TestSkipsReleaseCommandWhenTheAppHasNone(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	app := testApp(t, "")
	app.Release = nil
	if _, err := e.Deploy(ctx, app, digestA, "ci", nil); err != nil {
		t.Fatal(err)
	}
	if w.calls("RunRelease") != 0 {
		t.Error("ran a release command that is not configured")
	}
}

func TestRollback(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	app := testApp(t, "")
	mustDeploy(t, e, w, digestA)
	mustDeploy(t, e, w, digestB)
	mustDeploy(t, e, w, digestC)
	w.resetLog()

	rel, err := e.Rollback(ctx, app, 0, "admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Number != 4 || rel.Digest != digestB || rel.RolledBackFrom != 3 || rel.By != "admin" {
		t.Errorf("default rollback = %+v, want release 4 running release 2's image", rel)
	}
	if w.calls("RunRelease") != 0 {
		t.Error("rollback ran the release command")
	}
	rel, err = e.Rollback(ctx, app, 1, "admin", nil)
	if err != nil || rel.Digest != digestA || rel.RolledBackFrom != 4 {
		t.Errorf("explicit rollback = %+v, %v", rel, err)
	}
	for _, target := range []int{99, rel.Number} {
		if _, err := e.Rollback(ctx, app, target, "admin", nil); !errors.Is(err, ErrNoRelease) {
			t.Errorf("rollback to %d: %v, want ErrNoRelease", target, err)
		}
	}
}

func TestRollbackWithNothingToRollBackTo(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	var ev events
	if _, err := e.Rollback(ctx, testApp(t, ""), 0, "ci", ev.sink); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("no releases: %v", err)
	}
	mustDeploy(t, e, w, digestA)
	if _, err := e.Rollback(ctx, testApp(t, ""), 0, "ci", nil); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("one release: %v", err)
	}
	if _, err := e.Redeploy(ctx, testApp(t, ""), "ci", nil); err != nil {
		t.Fatalf("redeploy: %v", err)
	}
}

func TestRollbackToAPrunedImage(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	app := testApp(t, "")
	mustDeploy(t, e, w, digestA)
	mustDeploy(t, e, w, digestB)

	// The name is gone but the content survives: the name is restored.
	delete(w.images, digestA)
	w.content[digestA] = true
	if _, err := e.Rollback(ctx, app, 1, "ci", nil); err != nil {
		t.Fatalf("rollback with surviving content: %v", err)
	}
	if !w.has("Repin 1") {
		t.Errorf("the image name was not restored: %v", w.log)
	}

	// The content is gone too: say so, and change nothing.
	delete(w.images, digestB)
	before := w.port
	_, err := e.Rollback(ctx, app, 2, "ci", nil)
	if !errors.Is(err, ErrImageGone) {
		t.Fatalf("error = %v, want ErrImageGone", err)
	}
	if w.port != before {
		t.Error("traffic moved during a failed rollback")
	}
	w.fail["ImageExists"] = errInjected
	if _, err := e.Rollback(ctx, app, 1, "ci", nil); !errors.Is(err, errInjected) {
		t.Errorf("runtime errors must surface: %v", err)
	}
}

func TestRedeploy(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	if _, err := e.Redeploy(ctx, testApp(t, ""), "admin", nil); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("redeploy with no release: %v", err)
	}
	mustDeploy(t, e, w, digestA)
	w.resetLog()
	rel, err := e.Redeploy(ctx, testApp(t, ""), "admin", nil)
	if err != nil || rel.Number != 2 || rel.Digest != digestA || rel.RolledBackFrom != 0 {
		t.Fatalf("redeploy = %+v, %v", rel, err)
	}
	if w.calls("RunRelease") != 0 || !w.has("StopWeb 1") {
		t.Errorf("redeploy should skip the release command and replace the container: %v", w.log)
	}
}

func TestPruneKeepsTheNewestReleases(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	app := testApp(t, "keep_releases: 2\n")
	// Left by the push of the last deploy below; its image ends up kept.
	w.tags["push-abc123"] = digestA
	// A release tag of a release that is not kept, however it got there.
	w.tags["release-99"] = digestC
	for _, d := range []string{digestA, digestB, digestC, digestA} {
		if _, err := e.Deploy(ctx, app, d, "ci", nil); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := e.Store.Load("shop")
	if len(st.Releases) != 2 || st.Releases[0].Number != 3 || st.Releases[1].Number != 4 {
		t.Fatalf("kept releases = %+v, want 3 and 4", st.Releases)
	}
	if len(w.tags) != 2 || !w.hasTag("release-3") || !w.hasTag("release-4") {
		t.Errorf("tags = %v, want release-3 and release-4 only", w.tags)
	}
	if w.compose[1] || w.compose[2] || !w.compose[3] || !w.compose[4] {
		t.Errorf("compose files = %v", w.compose)
	}
	if !w.wasReleased(20001) || !w.wasReleased(20002) || w.wasReleased(20004) {
		t.Errorf("released ports = %v", w.released)
	}
}

func TestPruneNeverDropsTheCurrentRelease(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	app := testApp(t, "keep_releases: 1\n")
	if _, err := e.Deploy(ctx, app, digestA, "ci", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Deploy(ctx, app, digestB, "ci", nil); err != nil {
		t.Fatal(err)
	}
	st, _ := e.Store.Load("shop")
	if len(st.Releases) != 1 || st.Releases[0].Number != st.Current {
		t.Fatalf("state = %+v", st)
	}
}

func TestOnlyOneOperationPerApp(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	app := testApp(t, "")
	entered, release := make(chan struct{}), make(chan struct{})
	w.hook["Health"] = func() {
		close(entered)
		<-release
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.Deploy(ctx, app, digestA, "ci", nil)
		done <- err
	}()
	<-entered
	if !e.Busy("shop") {
		t.Error("Busy should be true during a deploy")
	}
	var ev events
	if _, err := e.Deploy(ctx, app, digestB, "ci", ev.sink); !errors.Is(err, ErrBusy) {
		t.Errorf("second deploy: %v, want ErrBusy", err)
	}
	if _, err := e.Rollback(ctx, app, 0, "ci", nil); !errors.Is(err, ErrBusy) {
		t.Errorf("rollback: %v, want ErrBusy", err)
	}
	if err := e.Reconcile(ctx, app); !errors.Is(err, ErrBusy) {
		t.Errorf("reconcile: %v, want ErrBusy", err)
	}
	if _, err := e.Lock("shop"); !errors.Is(err, ErrBusy) {
		t.Errorf("Lock: %v, want ErrBusy", err)
	}
	// Another app is not affected.
	if unlock, err := e.Lock("other"); err != nil {
		t.Errorf("another app was blocked: %v", err)
	} else {
		unlock()
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	delete(w.hook, "Health")
	unlock, err := e.Lock("shop")
	if err != nil {
		t.Fatalf("Lock after the deploy: %v", err)
	}
	if !e.Busy("shop") {
		t.Error("Lock should make the app busy")
	}
	unlock()
	if e.Busy("shop") {
		t.Error("unlock did not release")
	}
}

func TestReconcile(t *testing.T) {
	app := testApp(t, "")
	t.Run("no release removes stray containers", func(t *testing.T) {
		w := newWorld()
		e := newEngine(t, w)
		w.webs[1] = true
		if err := e.Reconcile(ctx, app); err != nil {
			t.Fatal(err)
		}
		if len(w.webs) != 0 || w.calls("Switch") != 0 {
			t.Errorf("webs = %v, log = %v", w.webs, w.log)
		}
	})
	t.Run("crash before the commit converges back", func(t *testing.T) {
		w := newWorld()
		e := newEngine(t, w)
		mustDeploy(t, e, w, digestA)
		w.webs[2] = true // a half-started release that never committed
		w.port = 29999
		w.resetLog()
		if err := e.Reconcile(ctx, app); err != nil {
			t.Fatal(err)
		}
		if webs := w.runningWebs(); len(webs) != 1 || webs[0] != 1 {
			t.Errorf("webs = %v, want only release 1", webs)
		}
		if w.port != 20001 || !w.has("UpServices 1") {
			t.Errorf("proxy port = %d, log = %v", w.port, w.log)
		}
		if w.calls("StartWeb") != 0 || w.calls("WriteCompose") != 0 {
			t.Errorf("a healthy current release was restarted: %v", w.log)
		}
	})
	// Regression: the uncommitted container used to be removed first, and
	// the proxy stayed on its port while the app's database was started.
	t.Run("traffic moves back before the uncommitted container is removed", func(t *testing.T) {
		w := newWorld()
		e := newEngine(t, w)
		mustDeploy(t, e, w, digestA)
		w.webs[2] = true
		w.port = 29999
		w.resetLog()
		if err := e.Reconcile(ctx, app); err != nil {
			t.Fatal(err)
		}
		sw, rm, env := w.indexOf("Switch 20001"), w.indexOf("RemoveWeb 2"), w.indexOf("Env")
		if sw < 0 || rm < 0 || env < 0 || sw > rm || sw > env {
			t.Errorf("want Switch 20001 before RemoveWeb 2 and Env, got %v", w.log)
		}
	})
	t.Run("a failed switch back removes nothing", func(t *testing.T) {
		w := newWorld()
		e := newEngine(t, w)
		mustDeploy(t, e, w, digestA)
		w.webs[2] = true
		w.port = 29999
		w.fail["Switch"] = errInjected
		if err := e.Reconcile(ctx, app); !errors.Is(err, errInjected) {
			t.Fatalf("error = %v", err)
		}
		if w.calls("RemoveWeb") != 0 || len(w.runningWebs()) != 2 {
			t.Errorf("the container traffic may still be on was removed: %v", w.log)
		}
		if e.Busy("shop") {
			t.Error("lock not released")
		}
	})
	t.Run("several containers of one stale release are removed once", func(t *testing.T) {
		ws := classifyWebs([]runtime.Container{
			{ID: "a", Release: 2, Running: true}, {ID: "b", Release: 2}, {ID: "c", Release: 1}, {ID: "d", Release: 1, Running: true},
		}, 1)
		if len(ws.stale) != 1 || ws.stale[0] != 2 || len(ws.stopped) != 1 || ws.stopped[0] != "c" || !ws.found || !ws.running {
			t.Errorf("classifyWebs = %+v", ws)
		}
	})
	t.Run("crash after the commit converges forward", func(t *testing.T) {
		w := newWorld()
		e := newEngine(t, w)
		mustDeploy(t, e, w, digestA)
		mustDeploy(t, e, w, digestB)
		w.webs[1] = true // the old release that was never stopped
		if err := e.Reconcile(ctx, app); err != nil {
			t.Fatal(err)
		}
		if webs := w.runningWebs(); len(webs) != 1 || webs[0] != 2 {
			t.Errorf("webs = %v, want only release 2", webs)
		}
	})
	t.Run("stopped container is started", func(t *testing.T) {
		w := newWorld()
		e := newEngine(t, w)
		mustDeploy(t, e, w, digestA)
		w.webs[1] = false
		w.resetLog()
		if err := e.Reconcile(ctx, app); err != nil {
			t.Fatal(err)
		}
		if !w.has("StartContainers c1") || w.calls("StartWeb") != 0 {
			t.Errorf("log = %v", w.log)
		}
	})
	t.Run("missing container is recreated", func(t *testing.T) {
		w := newWorld()
		e := newEngine(t, w)
		mustDeploy(t, e, w, digestA)
		delete(w.webs, 1)
		w.resetLog()
		if err := e.Reconcile(ctx, app); err != nil {
			t.Fatal(err)
		}
		if !w.has("WriteCompose 1") || !w.has("StartWeb 1") {
			t.Errorf("log = %v", w.log)
		}
	})
	t.Run("pruned image name is restored", func(t *testing.T) {
		w := newWorld()
		e := newEngine(t, w)
		mustDeploy(t, e, w, digestA)
		delete(w.images, digestA)
		w.content[digestA] = true
		if err := e.Reconcile(ctx, app); err != nil {
			t.Fatal(err)
		}
		if !w.images[digestA] {
			t.Error("the image name was not restored")
		}
		delete(w.images, digestA)
		delete(w.content, digestA)
		if err := e.Reconcile(ctx, app); !errors.Is(err, ErrImageGone) {
			t.Errorf("error = %v, want ErrImageGone", err)
		}
	})
	t.Run("every failure surfaces", func(t *testing.T) {
		for _, op := range []string{"Containers", "RemoveOneOffs", "RemoveWeb", "ImageExists", "EnsureNetwork", "Env", "StartContainers", "Switch", "UpServices"} {
			w := newWorld()
			e := newEngine(t, w)
			mustDeploy(t, e, w, digestA)
			w.webs[1] = false
			w.webs[5] = true
			w.fail[op] = errInjected
			if err := e.Reconcile(ctx, app); !errors.Is(err, errInjected) {
				t.Errorf("%s: error = %v", op, err)
			}
			if e.Busy("shop") {
				t.Errorf("%s: lock not released", op)
			}
		}
		for _, op := range []string{"WriteCompose", "StartWeb"} {
			w := newWorld()
			e := newEngine(t, w)
			mustDeploy(t, e, w, digestA)
			delete(w.webs, 1)
			w.fail[op] = errInjected
			if err := e.Reconcile(ctx, app); !errors.Is(err, errInjected) {
				t.Errorf("%s: error = %v", op, err)
			}
		}
	})
}

func TestValidDigest(t *testing.T) {
	if !ValidDigest(digestA) || ValidDigest(digestA+"0") || ValidDigest("sha512:"+strings.Repeat("a", 64)) {
		t.Fatal("ValidDigest is wrong")
	}
}

func FuzzValidDigest(f *testing.F) {
	f.Add(digestA)
	f.Add("sha256:../../etc")
	f.Fuzz(func(t *testing.T, s string) {
		if !ValidDigest(s) {
			return
		}
		if len(s) != 71 || strings.ContainsAny(s, " /@\n$-") {
			t.Fatalf("accepted %q", s)
		}
	})
}

// Regression: cleanup removed every tag that was not a kept release tag,
// including the tag of an image another CI job had just pushed and was about
// to deploy. That deploy then failed with an unknown digest.
func TestCleanupLeavesAPendingPushAlone(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	now := time.Unix(1_700_000_000, 0)
	e.Now = func() time.Time { return now }
	app := testApp(t, "")
	const pending = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	w.tags["push-own"] = digestA     // this deploy's own push
	w.tags["push-pending"] = pending // another job's push, not deployed yet
	w.tags["push-nodigest"] = ""     // docker reported no digest
	if _, err := e.Deploy(ctx, app, digestA, "ci", nil); err != nil {
		t.Fatal(err)
	}
	if w.hasTag("push-own") {
		t.Error("the deploy's own push tag should go at once: the release tag protects the image")
	}
	if !w.hasTag("push-pending") || !w.hasTag("push-nodigest") {
		t.Fatalf("a tag cleanup cannot account for was removed immediately: %v", w.tags)
	}

	// Still inside the grace period.
	now = now.Add(pushTagGrace - time.Minute)
	if _, err := e.Deploy(ctx, app, digestB, "ci", nil); err != nil {
		t.Fatal(err)
	}
	if !w.hasTag("push-pending") {
		t.Fatal("the pending push was removed before the grace period ended")
	}

	// The other job deploys: its tag is now redundant and goes at once.
	w.images[pending] = true
	if _, err := e.Deploy(ctx, app, pending, "ci", nil); err != nil {
		t.Fatal(err)
	}
	if w.hasTag("push-pending") {
		t.Error("a deployed push tag was kept")
	}

	// An abandoned push is removed once it has been seen for the grace period.
	now = now.Add(2 * time.Minute)
	if _, err := e.Deploy(ctx, app, digestA, "ci", nil); err != nil {
		t.Fatal(err)
	}
	if w.hasTag("push-nodigest") {
		t.Error("an abandoned tag outlived the grace period")
	}
	if st, _ := e.Store.Load("shop"); len(st.PendingTags) != 0 {
		t.Errorf("first-seen times of removed tags were kept: %v", st.PendingTags)
	}
}

// Regression: the first-seen times lived in the Engine, so every restart of
// the daemon started the grace period again and an abandoned push tag could
// stay for ever.
func TestPendingTagGraceSurvivesARestart(t *testing.T) {
	w := newWorld()
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	app := testApp(t, "")
	w.tags["push-abandoned"] = "sha256:" + strings.Repeat("d", 64)

	first := newEngine(t, w)
	first.Now = clock
	if _, err := first.Deploy(ctx, app, digestA, "ci", nil); err != nil {
		t.Fatal(err)
	}
	if !w.hasTag("push-abandoned") {
		t.Fatal("the tag was removed inside the grace period")
	}
	st, err := first.Store.Load("shop")
	if err != nil {
		t.Fatal(err)
	}
	if seen, ok := st.PendingTags["push-abandoned"]; !ok || !seen.Equal(now) {
		t.Fatalf("first-seen time not recorded in the release state: %v", st.PendingTags)
	}

	// The daemon restarts: a new Engine over the same state directory.
	second := newEngine(t, w)
	second.Store = &releases.Store{Root: first.Store.Root}
	second.Now = clock
	now = now.Add(pushTagGrace + time.Minute)
	if _, err := second.Deploy(ctx, app, digestB, "ci", nil); err != nil {
		t.Fatal(err)
	}
	if w.hasTag("push-abandoned") {
		t.Error("an abandoned tag outlived the grace period because the daemon restarted")
	}
	if st, _ := second.Store.Load("shop"); len(st.PendingTags) != 0 {
		t.Errorf("the removed tag is still recorded: %v", st.PendingTags)
	}
}

// A tag whose removal fails keeps its first-seen time, so the next cleanup
// tries again instead of waiting another grace period.
func TestPendingTagRemovalIsRetried(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	now := time.Unix(1_700_000_000, 0)
	e.Now = func() time.Time { return now }
	app := testApp(t, "")
	w.tags["push-abandoned"] = ""
	if _, err := e.Deploy(ctx, app, digestA, "ci", nil); err != nil {
		t.Fatal(err)
	}
	now = now.Add(pushTagGrace)
	w.fail["RemoveTag"] = errInjected
	if _, err := e.Deploy(ctx, app, digestB, "ci", nil); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.Store.Load("shop"); len(st.PendingTags) != 1 {
		t.Fatalf("a tag that could not be removed was forgotten: %v", st.PendingTags)
	}
	delete(w.fail, "RemoveTag")
	now = now.Add(time.Second)
	if _, err := e.Deploy(ctx, app, digestC, "ci", nil); err != nil {
		t.Fatal(err)
	}
	if w.hasTag("push-abandoned") {
		t.Error("the removal was not retried by the next cleanup")
	}
}

// Regression: every tag starting with "release-" was taken for one of
// Zymple's own and removed at once, a pushed "release-1.4.0" included.
func TestCleanupOnlyOwnsExactReleaseTags(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	now := time.Unix(1_700_000_000, 0)
	e.Now = func() time.Time { return now }
	app := testApp(t, "")
	const pending = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	foreign := []string{"release-1.4.0", "release-07", "release-0", "release--3", "release-+4", "release-", "release-9x"}
	for _, tag := range foreign {
		w.tags[tag] = pending
	}
	w.tags["release-99"] = pending // Zymple's form, but no such release is kept
	if _, err := e.Deploy(ctx, app, digestA, "ci", nil); err != nil {
		t.Fatal(err)
	}
	for _, tag := range foreign {
		if !w.hasTag(tag) {
			t.Errorf("%s is not a tag Zymple makes, but it was removed at once", tag)
		}
	}
	if w.hasTag("release-99") || !w.hasTag("release-1") {
		t.Errorf("tags = %v, want release-99 gone and release-1 kept", w.tags)
	}
	// They are still cleaned up once the grace period is over.
	now = now.Add(pushTagGrace)
	if _, err := e.Deploy(ctx, app, digestB, "ci", nil); err != nil {
		t.Fatal(err)
	}
	for _, tag := range foreign {
		if w.hasTag(tag) {
			t.Errorf("%s outlived the grace period", tag)
		}
	}
}

// Regression: a push tag was removed at once when its digest matched any
// kept release. A pending re-push of an older kept image lost its tag, and
// then its image when that release was pruned, so its deploy failed with an
// unknown digest.
func TestCleanupKeepsAPushOfAnOlderKeptImage(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	app := testApp(t, "keep_releases: 3\n")
	const third = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	w.images[third] = true
	for _, d := range []string{digestA, digestB, third, digestA} {
		if _, err := e.Deploy(ctx, app, d, "ci", nil); err != nil {
			t.Fatal(err)
		}
	}
	// Another job pushes release 3's image again and has not deployed yet.
	w.tags["push-again"] = third
	if _, err := e.Deploy(ctx, app, digestB, "ci", nil); err != nil {
		t.Fatal(err)
	}
	st, _ := e.Store.Load("shop")
	if len(st.Releases) != 3 || st.Releases[0].Number != 3 || st.Releases[0].Digest != third || st.Current != 5 {
		t.Fatalf("state = %+v, want releases 3, 4 and 5 kept", st)
	}
	if !w.hasTag("push-again") {
		t.Fatal("a pending push of a kept release's image lost its tag")
	}
	// Release 3 is pruned by the next deploy; the push tag still holds the
	// image for the job that is about to deploy it.
	if _, err := e.Deploy(ctx, app, digestC, "ci", nil); err != nil {
		t.Fatal(err)
	}
	if w.hasTag("release-3") || !w.hasTag("push-again") {
		t.Fatalf("tags = %v, want release-3 gone and push-again kept", w.tags)
	}
	// Deploying it makes the tag redundant, and it goes at once.
	if _, err := e.Deploy(ctx, app, third, "ci", nil); err != nil {
		t.Fatal(err)
	}
	if w.hasTag("push-again") {
		t.Error("the push tag of the release just made was kept")
	}
}

// Regression: one-off containers are not listed by Containers, so the
// release command of a failed attempt was never removed and could keep
// migrating beside the release that stayed live.
func TestAbortRemovesTheReleaseCommand(t *testing.T) {
	for _, op := range []string{"RunRelease", "StartWeb", "Health", "Switch"} {
		t.Run(op, func(t *testing.T) {
			w := newWorld()
			e := newEngine(t, w)
			mustDeploy(t, e, w, digestA)
			w.addOneOff("console", 1) // a `zymple run` on the live release
			w.hook["RunRelease"] = func() { w.addOneOff("migrate", 2) }
			w.fail[op] = errInjected
			if _, err := e.Deploy(ctx, testApp(t, ""), digestB, "ci", nil); !errors.Is(err, errInjected) {
				t.Fatalf("error = %v", err)
			}
			if w.hasOneOff("migrate") {
				t.Error("the release command of the aborted release was left behind")
			}
			if !w.hasOneOff("console") {
				t.Error("a one-off of the live release was removed")
			}
		})
	}
}

// Failure injection: the release command never returns. Without a limit it
// held the app's lock for good, and no deploy, rollback or restore could run
// until the daemon was restarted.
func TestAHangingReleaseCommandIsAborted(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	mustDeploy(t, e, w, digestA)
	app := testApp(t, "")
	app.ReleaseTimeout = appdef.Duration(20 * time.Millisecond)
	w.releaseHangs = true
	w.hook["RunRelease"] = func() { w.addOneOff("migrate", 2) }
	_, err := e.Deploy(ctx, app, digestB, "ci", nil)
	if err == nil || !strings.Contains(err.Error(), "the release command did not finish within 20ms") {
		t.Fatalf("error = %v, want the release timeout", err)
	}
	if w.hasOneOff("migrate") {
		t.Error("the hanging release command was left running")
	}
	if got := w.runningWebs(); len(got) != 1 || got[0] != 1 {
		t.Errorf("running webs = %v, want the old release only", got)
	}
	if e.Busy("shop") {
		t.Error("the app is still locked")
	}
	w.releaseHangs = false
	if _, err := e.Deploy(ctx, testApp(t, ""), digestB, "ci", nil); err != nil {
		t.Errorf("the next deploy failed: %v", err)
	}
}

func TestReleaseTimeoutFallsBackToTheDefault(t *testing.T) {
	if got := releaseTimeout(&appdef.App{}); got != appdef.DefaultReleaseTimeout {
		t.Errorf("release timeout of an app without one = %s", got)
	}
}

// Failure injection: the daemon dies during the release step, so nothing
// removes the release command's container. Reconcile at the next start does,
// before it starts anything.
func TestReconcileRemovesAnOrphanedReleaseCommand(t *testing.T) {
	app := testApp(t, "")
	w := newWorld()
	e := newEngine(t, w)
	mustDeploy(t, e, w, digestA)
	w.addOneOff("console", 1)
	w.hook["RunRelease"] = func() { w.addOneOff("migrate", 2) }
	w.fail["RunRelease"] = errInjected
	// The crash: the attempt's own cleanup never happens.
	w.fail["RemoveOneOffs"] = errors.New("daemon killed")
	if _, err := e.Deploy(ctx, app, digestB, "ci", nil); !errors.Is(err, errInjected) {
		t.Fatalf("error = %v", err)
	}
	if !w.hasOneOff("migrate") {
		t.Fatal("the test did not leave an orphaned one-off")
	}
	delete(w.fail, "RunRelease")
	delete(w.fail, "RemoveOneOffs")
	w.webs[1] = false // the host rebooted
	w.resetLog()

	restarted := newEngine(t, w)
	restarted.Store = &releases.Store{Root: e.Store.Root}
	if err := restarted.Reconcile(ctx, app); err != nil {
		t.Fatal(err)
	}
	if w.hasOneOff("migrate") {
		t.Error("the orphaned release command of release 2 is still there")
	}
	if !w.hasOneOff("console") {
		t.Error("a one-off of the current release was removed")
	}
	rm := w.indexOf("RemoveOneOffs")
	for _, start := range []string{"Env", "StartContainers c1", "UpServices 1"} {
		if at := w.indexOf(start); rm < 0 || at < 0 || rm > at {
			t.Errorf("want RemoveOneOffs before %s, got %v", start, w.log)
		}
	}
	if w.calls("StartWeb") != 0 || w.calls("RemoveWeb") != 0 {
		t.Errorf("a one-off was taken for a web container: %v", w.log)
	}

	// With no release at all, every one-off is a leftover.
	bare := newWorld()
	bare.addOneOff("migrate", 1)
	if err := newEngine(t, bare).Reconcile(ctx, app); err != nil {
		t.Fatal(err)
	}
	if bare.hasOneOff("migrate") {
		t.Error("a one-off of an app without a release was kept")
	}
}

// Regression: a line without a newline was cut at exactly 64 KiB, in the
// middle of a multi-byte character, and both halves became invalid UTF-8.
func TestLogWriterCutsOnACharacterBoundary(t *testing.T) {
	for _, char := range []string{"é", "€", "𝄞"} {
		for pad := 0; pad < len(char); pad++ {
			var ev events
			w := &logWriter{sink: ev.sink, step: "release"}
			input := strings.Repeat("a", pad) + strings.Repeat(char, 3*maxLogLine/len(char))
			_, _ = w.Write([]byte(input + "\n"))
			var got strings.Builder
			for _, e := range ev.list {
				if !utf8.ValidString(e.Message) {
					t.Fatalf("%q with %d bytes before it: a line was cut inside a character", char, pad)
				}
				if len(e.Message) > maxLogLine {
					t.Fatalf("a line of %d bytes is over the limit", len(e.Message))
				}
				got.WriteString(e.Message)
			}
			if got.String() != input {
				t.Fatalf("%q with %d bytes before it: output was lost or changed", char, pad)
			}
		}
	}
	// Bytes that are not UTF-8 at all are still cut at the limit.
	var ev events
	w := &logWriter{sink: ev.sink}
	_, _ = w.Write([]byte(strings.Repeat("\x80", maxLogLine+10)))
	if len(ev.list) != 1 || len(ev.list[0].Message) != maxLogLine {
		t.Fatalf("invalid bytes: %d events", len(ev.list))
	}
	// A character still arriving at the limit waits for its last bytes.
	ev = events{}
	w = &logWriter{sink: ev.sink}
	_, _ = w.Write([]byte(strings.Repeat("a", maxLogLine-1) + "\xe2\x82"))
	_, _ = w.Write([]byte("\xac\n"))
	if len(ev.list) != 2 || ev.list[0].Message != strings.Repeat("a", maxLogLine-1) || ev.list[1].Message != "€" {
		t.Fatalf("a character split across writes was not kept whole: %d events", len(ev.list))
	}
}

func TestLogWriterBoundsALineWithoutNewline(t *testing.T) {
	var ev events
	w := &logWriter{sink: ev.sink, step: "release", release: 3}
	chunk := strings.Repeat("x", 10<<10)
	for i := 0; i < 20; i++ { // 200 KiB and never a newline
		if n, err := w.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
		if w.buf.Len() >= maxLogLine {
			t.Fatalf("buffer holds %d bytes after write %d", w.buf.Len(), i)
		}
	}
	if len(ev.list) != 3 {
		t.Fatalf("events = %d, want 3 full lines", len(ev.list))
	}
	for _, e := range ev.list {
		if len(e.Message) != maxLogLine || e.Step != "release" || e.Release != 3 {
			t.Errorf("event = %s/%d with %d bytes", e.Step, e.Release, len(e.Message))
		}
	}
	// Ordinary lines still split on the newline, with "\r" trimmed.
	_, _ = w.Write([]byte("\nnext\r\n"))
	if got := ev.list[len(ev.list)-1].Message; got != "next" {
		t.Errorf("last line = %q", got)
	}
}
