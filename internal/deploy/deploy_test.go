package deploy

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
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
	w.fail["RemoveWeb"] = errors.New("cannot remove")
	w.fail["RemoveCompose"] = errors.New("cannot delete")
	_, err := e.Deploy(ctx, testApp(t, ""), digestB, "ci", nil)
	if !errors.Is(err, errInjected) || !strings.Contains(err.Error(), "cannot remove") || !strings.Contains(err.Error(), "cannot delete") {
		t.Fatalf("error should carry the cause and both cleanup failures: %v", err)
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
			w.tags["push-leftover"] = true
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
	w.tags["push-abc123"] = true
	for _, d := range []string{digestA, digestB, digestC, digestA} {
		if _, err := e.Deploy(ctx, app, d, "ci", nil); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := e.Store.Load("shop")
	if len(st.Releases) != 2 || st.Releases[0].Number != 3 || st.Releases[1].Number != 4 {
		t.Fatalf("kept releases = %+v, want 3 and 4", st.Releases)
	}
	if len(w.tags) != 2 || !w.tags["release-3"] || !w.tags["release-4"] {
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
		for _, op := range []string{"Containers", "RemoveWeb", "ImageExists", "EnsureNetwork", "Env", "StartContainers", "Switch", "UpServices"} {
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
