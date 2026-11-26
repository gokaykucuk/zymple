package deploy

import (
	"errors"
	"strings"
	"testing"

	"github.com/gokaykucuk/zymple/internal/appdef"
	"github.com/gokaykucuk/zymple/internal/runtime"
)

// imagesApp is an app with one extra image, run by its own service.
func imagesApp(t *testing.T, extra string) *appdef.App {
	t.Helper()
	app, err := appdef.Parse(
		[]byte("name: shop\ndomains: [shop.example.com]\nweb: { service: web, port: 3000, healthcheck: /healthz, drain: 7s }\nimages: [ocr]\n"+extra),
		[]byte("services:\n  web:\n    image: ${IMAGE}\n  worker:\n    image: ${IMAGE}\n  ocr:\n    image: ${IMAGE_OCR}\n"))
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func (w *world) tag(name string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tags[name]
}

func TestAReleasePinsEveryImage(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	app := imagesApp(t, "")
	rel, err := e.DeployImages(ctx, app, digestA, map[string]string{"ocr": digestB}, "ci", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Digest != digestA || rel.Images["ocr"] != digestB {
		t.Errorf("release = %+v", rel)
	}
	if w.calls("ImageExists") != 2 {
		t.Errorf("each image must be verified: %v", w.log)
	}
	if w.tag("release-1") != digestA || w.tag("release-1-ocr") != digestB {
		t.Errorf("tags = %v", w.tags)
	}
	st, _ := e.Store.Load("shop")
	if cur, _ := st.CurrentRelease(); cur.Images["ocr"] != digestB {
		t.Errorf("the extra image was not recorded: %+v", cur)
	}
}

func TestADeployKeepsTheImagesItDoesNotName(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	app := imagesApp(t, "")
	if _, err := e.DeployImages(ctx, app, digestA, map[string]string{"ocr": digestB}, "ci", nil); err != nil {
		t.Fatal(err)
	}

	// Only the app's image: the extra one stays.
	rel, err := e.Deploy(ctx, app, digestC, "ci", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Digest != digestC || rel.Images["ocr"] != digestB || w.tag("release-2-ocr") != digestB {
		t.Errorf("release = %+v, tags = %v", rel, w.tags)
	}

	// Only the extra image: the app's image stays, and the release command
	// still runs, because this is a deploy and not a rollback.
	w.resetLog()
	rel, err = e.DeployImages(ctx, imagesApp(t, "release: [\"bin/migrate\"]\n"), "", map[string]string{"ocr": digestA}, "ci", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Digest != digestC || rel.Images["ocr"] != digestA {
		t.Errorf("release = %+v", rel)
	}
	if w.calls("RunRelease") != 1 {
		t.Errorf("the release command did not run: %v", w.log)
	}
}

func TestADeployNeedsEveryImageOnce(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	app := imagesApp(t, "")
	_, err := e.Deploy(ctx, app, digestA, "ci", nil)
	if err == nil || !strings.Contains(err.Error(), `image "ocr" has not been deployed`) {
		t.Fatalf("error = %v", err)
	}
	if w.calls("StartWeb") != 0 || w.calls("WriteCompose") != 0 {
		t.Errorf("nothing may start without the whole set: %v", w.log)
	}
	// Nothing at all to take the app's own image from.
	if _, err := e.DeployImages(ctx, app, "", map[string]string{"ocr": digestB}, "ci", nil); !errors.Is(err, ErrNoRelease) {
		t.Errorf("error = %v, want ErrNoRelease", err)
	}
}

func TestDeployRejectsImagesTheAppDoesNotDeclare(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	if _, err := e.DeployImages(ctx, imagesApp(t, ""), digestA, map[string]string{"ocr": digestB, "pdf": digestC}, "ci", nil); !errors.Is(err, ErrUnknownImage) {
		t.Errorf("error = %v, want ErrUnknownImage", err)
	}
	if _, err := e.DeployImages(ctx, testApp(t, ""), digestA, map[string]string{"ocr": digestB}, "ci", nil); !errors.Is(err, ErrUnknownImage) {
		t.Errorf("an app without extra images: error = %v, want ErrUnknownImage", err)
	}
	if w.calls("ImageExists") != 0 {
		t.Errorf("the request must be refused before anything runs: %v", w.log)
	}
}

func TestDeployRejectsAnExtraImageThatWasNotPushed(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	app := imagesApp(t, "")
	missing := "sha256:" + strings.Repeat("d", 64)
	_, err := e.DeployImages(ctx, app, digestA, map[string]string{"ocr": missing}, "ci", nil)
	if !errors.Is(err, ErrUnknownDigest) || !strings.Contains(err.Error(), `image "ocr"`) {
		t.Fatalf("error = %v", err)
	}
	if w.calls("StartWeb") != 0 {
		t.Errorf("a release started without its image: %v", w.log)
	}
	if _, err := e.DeployImages(ctx, app, digestA, map[string]string{"ocr": "latest"}, "ci", nil); err == nil || !strings.Contains(err.Error(), "not a sha256 digest") {
		t.Errorf("a tag was accepted as an extra image: %v", err)
	}
}

func TestRollbackBringsBackTheWholeSet(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	app := imagesApp(t, "")
	if _, err := e.DeployImages(ctx, app, digestA, map[string]string{"ocr": digestB}, "ci", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DeployImages(ctx, app, digestC, map[string]string{"ocr": digestC}, "ci", nil); err != nil {
		t.Fatal(err)
	}
	rel, err := e.Rollback(ctx, app, 0, "ci", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Number != 3 || rel.Digest != digestA || rel.Images["ocr"] != digestB || rel.RolledBackFrom != 2 {
		t.Errorf("release = %+v", rel)
	}
	if w.tag("release-3-ocr") != digestB {
		t.Errorf("tags = %v", w.tags)
	}

	// Redeploy keeps the set too.
	rel, err = e.Redeploy(ctx, app, "admin", nil)
	if err != nil || rel.Digest != digestA || rel.Images["ocr"] != digestB {
		t.Errorf("redeploy = %+v, %v", rel, err)
	}
}

// A release made before the app declared the image has none to bring back;
// the current one's is kept.
func TestRollbackToAReleaseFromBeforeTheImage(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	mustDeploy(t, e, w, digestA)
	app := imagesApp(t, "")
	if _, err := e.Redeploy(ctx, app, "admin", nil); err == nil || !strings.Contains(err.Error(), "has not been deployed") {
		t.Fatalf("redeploy without the new image: %v", err)
	}
	if _, err := e.DeployImages(ctx, app, digestB, map[string]string{"ocr": digestC}, "ci", nil); err != nil {
		t.Fatal(err)
	}
	rel, err := e.Rollback(ctx, app, 1, "ci", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Digest != digestA || rel.Images["ocr"] != digestC {
		t.Errorf("release = %+v", rel)
	}

	// The other way round: the definition dropped the image again.
	rel, err = e.Rollback(ctx, testApp(t, ""), 0, "ci", nil)
	if err != nil || len(rel.Images) != 0 {
		t.Errorf("release = %+v, %v", rel, err)
	}
}

func TestPruneRemovesTheTagsOfExtraImages(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	app := imagesApp(t, "keep_releases: 1\n")
	// A push tag on the extra image of the release being made goes at once.
	w.tags["push-ocr"] = digestB
	if _, err := e.DeployImages(ctx, app, digestA, map[string]string{"ocr": digestB}, "ci", nil); err != nil {
		t.Fatal(err)
	}
	if w.hasTag("push-ocr") {
		t.Errorf("the push tag of a deployed extra image was left: %v", w.tags)
	}
	if _, err := e.DeployImages(ctx, app, digestB, map[string]string{"ocr": digestC}, "ci", nil); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{runtime.ReleaseTag(1), runtime.ImageTag(1, "ocr")} {
		if w.hasTag(gone) {
			t.Errorf("tag %s outlived its release: %v", gone, w.tags)
		}
	}
	for _, kept := range []string{runtime.ReleaseTag(2), runtime.ImageTag(2, "ocr")} {
		if !w.hasTag(kept) {
			t.Errorf("tag %s of the kept release is gone: %v", kept, w.tags)
		}
	}
}

func TestReconcileRestoresTheNameOfAnExtraImage(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	app := imagesApp(t, "")
	if _, err := e.DeployImages(ctx, app, digestA, map[string]string{"ocr": digestB}, "ci", nil); err != nil {
		t.Fatal(err)
	}
	delete(w.images, digestB)
	w.content[digestB] = true
	w.resetLog()
	if err := e.Reconcile(ctx, app); err != nil {
		t.Fatal(err)
	}
	if !w.has("Repin 1 ocr") || !w.images[digestB] {
		t.Errorf("the extra image did not get its name back: %v", w.log)
	}

	// Gone for good: reconcile says which image.
	delete(w.images, digestB)
	delete(w.content, digestB)
	if err := e.Reconcile(ctx, app); !errors.Is(err, ErrImageGone) || !strings.Contains(err.Error(), `image "ocr"`) {
		t.Errorf("error = %v", err)
	}
}

func TestAnExtraImageThatIsGoneStopsARollback(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	app := imagesApp(t, "")
	if _, err := e.DeployImages(ctx, app, digestA, map[string]string{"ocr": digestB}, "ci", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DeployImages(ctx, app, digestC, map[string]string{"ocr": digestC}, "ci", nil); err != nil {
		t.Fatal(err)
	}
	delete(w.images, digestB)
	before := w.port
	if _, err := e.Rollback(ctx, app, 1, "ci", nil); !errors.Is(err, ErrImageGone) {
		t.Fatalf("error = %v, want ErrImageGone", err)
	}
	if w.port != before {
		t.Error("traffic moved during a failed rollback")
	}
}

// Declaring an image does not disturb the running release: reconcile keeps
// the web container converged and says what is missing.
func TestReconcileReportsAnImageThatWasNeverDeployed(t *testing.T) {
	w := newWorld()
	e := newEngine(t, w)
	mustDeploy(t, e, w, digestA)
	port := w.port
	w.resetLog()
	err := e.Reconcile(ctx, imagesApp(t, ""))
	if err == nil || !strings.Contains(err.Error(), `image "ocr" has not been deployed`) {
		t.Fatalf("error = %v", err)
	}
	if w.port != port || len(w.runningWebs()) != 1 || w.calls("RemoveWeb") != 0 || w.calls("UpServices") != 0 {
		t.Errorf("the running release was disturbed: %v", w.log)
	}
}
