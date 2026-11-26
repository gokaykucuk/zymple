package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	bolt "go.etcd.io/bbolt"

	"github.com/gokaykucuk/zymple/internal/auth"
)

const testNamespace = "moby"

// inProcess is containerd's real content, image and lease stores running
// inside the test, on a temporary directory.
type inProcess struct{ db *metadata.DB }

func (s inProcess) ContentStore() content.Store   { return s.db.ContentStore() }
func (s inProcess) ImageService() images.Store    { return metadata.NewImageStore(s.db) }
func (s inProcess) LeasesService() leases.Manager { return metadata.NewLeaseManager(s.db) }

func newStores(t *testing.T) inProcess {
	t.Helper()
	dir := t.TempDir()
	cs, err := local.NewStore(filepath.Join(dir, "content"))
	if err != nil {
		t.Fatal(err)
	}
	bdb, err := bolt.Open(filepath.Join(dir, "meta.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bdb.Close() })
	db := metadata.NewDB(bdb, cs, nil)
	if err := db.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return inProcess{db: db}
}

type registryFixture struct {
	t      *testing.T
	url    string
	stores inProcess
}

func newRegistry(t *testing.T) *registryFixture {
	t.Helper()
	stores := newStores(t)
	handler, err := NewHandler(context.Background(), stores, testNamespace)
	if err != nil {
		t.Fatal(err)
	}
	tokens := map[string]auth.Set{}
	for _, app := range []string{"shop", "other"} {
		line, _ := auth.HashLine("ci", "zym_"+app)
		set, _ := auth.Parse([]byte(line))
		tokens[app] = set
	}
	guard := &Guard{Next: handler, Tokens: func(app string) (auth.Set, bool) {
		set, ok := tokens[app]
		return set, ok
	}}
	srv := httptest.NewServer(guard)
	t.Cleanup(srv.Close)
	return &registryFixture{t: t, url: srv.URL, stores: stores}
}

func (f *registryFixture) do(app, method, target string, body []byte, contentType string) *http.Response {
	f.t.Helper()
	if strings.HasPrefix(target, "/") {
		target = f.url + target
	}
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.SetBasicAuth(app, "zym_"+app)
	// Like docker, say which manifest formats are understood.
	req.Header.Set("Accept", v1.MediaTypeImageManifest+", "+v1.MediaTypeImageIndex)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// upload pushes one blob the way docker does: start, send, commit.
func (f *registryFixture) upload(app string, data []byte, chunked bool) digest.Digest {
	f.t.Helper()
	dgst := digest.FromBytes(data)
	resp := f.do(app, "POST", "/v2/zymple/"+app+"/blobs/uploads/", nil, "")
	if resp.StatusCode != http.StatusAccepted {
		f.t.Fatalf("start upload: %d", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if chunked {
		half := len(data) / 2
		for _, chunk := range [][]byte{data[:half], data[half:]} {
			resp = f.do(app, "PATCH", location, chunk, "application/octet-stream")
			if resp.StatusCode != http.StatusAccepted {
				f.t.Fatalf("upload chunk: %d", resp.StatusCode)
			}
			location = resp.Header.Get("Location")
		}
		data = nil
	}
	sep := "?"
	if strings.Contains(location, "?") {
		sep = "&"
	}
	resp = f.do(app, "PUT", location+sep+"digest="+dgst.String(), data, "application/octet-stream")
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		f.t.Fatalf("commit upload: %d %s", resp.StatusCode, body)
	}
	return dgst
}

func ociManifest(config, layer []byte) []byte {
	m := v1.Manifest{
		MediaType: v1.MediaTypeImageManifest,
		Config:    v1.Descriptor{MediaType: v1.MediaTypeImageConfig, Digest: digest.FromBytes(config), Size: int64(len(config))},
		Layers:    []v1.Descriptor{{MediaType: v1.MediaTypeImageLayerGzip, Digest: digest.FromBytes(layer), Size: int64(len(layer))}},
	}
	m.SchemaVersion = 2
	data, _ := json.Marshal(m)
	return data
}

func TestPushLandsInTheContainerdImageStore(t *testing.T) {
	f := newRegistry(t)
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	layer := bytes.Repeat([]byte("layer data "), 5000)

	if resp := f.do("shop", "GET", "/v2/", nil, ""); resp.StatusCode != 200 {
		t.Fatalf("version check: %d", resp.StatusCode)
	}
	layerDigest := f.upload("shop", layer, true)
	configDigest := f.upload("shop", config, false)

	// Existence checks, which are how a client skips layers the host has.
	if resp := f.do("shop", "HEAD", "/v2/zymple/shop/blobs/"+layerDigest.String(), nil, ""); resp.StatusCode != 200 ||
		resp.Header.Get("Content-Length") != fmt.Sprint(len(layer)) {
		t.Fatalf("HEAD existing blob: %d, length %s", resp.StatusCode, resp.Header.Get("Content-Length"))
	}
	missing := digest.FromString("never uploaded")
	if resp := f.do("shop", "HEAD", "/v2/zymple/shop/blobs/"+missing.String(), nil, ""); resp.StatusCode != 404 {
		t.Fatalf("HEAD missing blob: %d", resp.StatusCode)
	}
	// Uploading a blob that already exists succeeds without storing it twice.
	if again := f.upload("shop", layer, false); again != layerDigest {
		t.Fatalf("re-upload gave %s", again)
	}

	manifest := ociManifest(config, layer)
	manifestDigest := digest.FromBytes(manifest)
	resp := f.do("shop", "PUT", "/v2/zymple/shop/manifests/build-7", manifest, v1.MediaTypeImageManifest)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Docker-Content-Digest") != manifestDigest.String() {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT manifest: %d %s", resp.StatusCode, body)
	}
	for _, ref := range []string{"build-7", manifestDigest.String()} {
		if resp := f.do("shop", "HEAD", "/v2/zymple/shop/manifests/"+ref, nil, ""); resp.StatusCode != 200 {
			t.Errorf("HEAD manifest %s: %d", ref, resp.StatusCode)
		}
	}
	if resp := f.do("shop", "HEAD", "/v2/zymple/shop/manifests/no-such-tag", nil, ""); resp.StatusCode != 404 {
		t.Errorf("HEAD unknown tag: %d", resp.StatusCode)
	}

	// The image is now in containerd under the name Docker resolves.
	ctx := namespaces.WithNamespace(context.Background(), testNamespace)
	img, err := f.stores.ImageService().Get(ctx, "docker.io/zymple/shop:build-7")
	if err != nil {
		t.Fatalf("image not in the image store: %v", err)
	}
	if img.Target.Digest != manifestDigest || img.Target.MediaType != v1.MediaTypeImageManifest {
		t.Errorf("image target = %+v", img.Target)
	}
	// The manifest blob is labelled with its children, so containerd's
	// garbage collector keeps the layers once the upload leases expire.
	info, err := f.stores.ContentStore().Info(ctx, manifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for key, value := range info.Labels {
		if strings.HasPrefix(key, "containerd.io/gc.ref.content") {
			refs = append(refs, value)
		}
	}
	if len(refs) != 2 || !strings.Contains(strings.Join(refs, " "), layerDigest.String()) || !strings.Contains(strings.Join(refs, " "), configDigest.String()) {
		t.Errorf("garbage-collection labels = %v", info.Labels)
	}

	// Pushing the same tag again moves it.
	second := ociManifest(config, layer[:len(layer)/2])
	f.upload("shop", layer[:len(layer)/2], false)
	if resp := f.do("shop", "PUT", "/v2/zymple/shop/manifests/build-7", second, v1.MediaTypeImageManifest); resp.StatusCode != http.StatusCreated {
		t.Fatalf("re-tag: %d", resp.StatusCode)
	}
	img, _ = f.stores.ImageService().Get(ctx, "docker.io/zymple/shop:build-7")
	if img.Target.Digest != digest.FromBytes(second) {
		t.Errorf("the tag did not move: %s", img.Target.Digest)
	}
}

func TestPushRejectsWhatItMust(t *testing.T) {
	f := newRegistry(t)
	data := []byte("some layer")

	// A blob whose content does not match the digest the client claims.
	resp := f.do("shop", "POST", "/v2/zymple/shop/blobs/uploads/", nil, "")
	location := resp.Header.Get("Location")
	wrong := digest.FromString("something else")
	resp = f.do("shop", "PUT", location+"&digest="+wrong.String(), data, "application/octet-stream")
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Errorf("mismatched digest: %d, want a 4xx", resp.StatusCode)
	}
	ctx := namespaces.WithNamespace(context.Background(), testNamespace)
	if _, err := f.stores.ContentStore().Info(ctx, wrong); err == nil {
		t.Error("content was stored under a digest it does not match")
	}

	// A manifest that is not a manifest.
	if resp := f.do("shop", "PUT", "/v2/zymple/shop/manifests/bad", []byte(`{"not":"a manifest"}`), v1.MediaTypeImageManifest); resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Errorf("garbage manifest: %d, want a 4xx", resp.StatusCode)
	}

	// A manifest whose layers were never uploaded must not become an image.
	orphan := ociManifest([]byte(`{"missing":"config"}`), []byte("missing layer"))
	resp = f.do("shop", "PUT", "/v2/zymple/shop/manifests/orphan", orphan, v1.MediaTypeImageManifest)
	if resp.StatusCode < 400 {
		t.Errorf("manifest with missing blobs: %d, want an error", resp.StatusCode)
	}
	if _, err := f.stores.ImageService().Get(ctx, "docker.io/zymple/shop:orphan"); err == nil {
		t.Error("an image was created from a manifest whose blobs are missing")
	}

	// The guard: no reads, no other repository, no catalog, no deletes.
	uploaded := f.upload("shop", data, false)
	for name, c := range map[string][2]string{
		"read own blob":            {"GET", "/v2/zymple/shop/blobs/" + uploaded.String()},
		"read own manifest":        {"GET", "/v2/zymple/shop/manifests/latest"},
		"delete own blob":          {"DELETE", "/v2/zymple/shop/blobs/" + uploaded.String()},
		"probe another app's blob": {"HEAD", "/v2/zymple/other/blobs/" + uploaded.String()},
		"upload into another app":  {"POST", "/v2/zymple/other/blobs/uploads/"},
		"tag list":                 {"GET", "/v2/zymple/shop/tags/list"},
		"catalog":                  {"GET", "/v2/_catalog"},
		"library namespace":        {"POST", "/v2/postgres/blobs/uploads/"},
	} {
		if resp := f.do("shop", c[0], c[1], nil, ""); resp.StatusCode != 403 {
			t.Errorf("%s: %s %s = %d, want 403", name, c[0], c[1], resp.StatusCode)
		}
	}

	// A cross-repository mount is turned into a plain upload, never a mount.
	resp = f.do("other", "POST", "/v2/zymple/other/blobs/uploads/?mount="+uploaded.String()+"&from=zymple/shop", nil, "")
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("mount request: %d, want 202 (an upload session), not 201 (a mount)", resp.StatusCode)
	}

	// Cancelling an upload works.
	resp = f.do("shop", "POST", "/v2/zymple/shop/blobs/uploads/", nil, "")
	if resp := f.do("shop", "DELETE", resp.Header.Get("Location"), nil, ""); resp.StatusCode >= 400 {
		t.Errorf("cancel upload: %d", resp.StatusCode)
	}
}

func TestNewHandlerAndDialReportBadInput(t *testing.T) {
	if _, err := Dial(""); err == nil {
		t.Error("an empty socket path was accepted")
	}
	if stores, err := Dial(filepath.Join(t.TempDir(), "absent.sock")); err != nil || stores == nil {
		t.Errorf("Dial is lazy and should not fail for a missing socket: %v", err)
	}
	if h, err := NewHandler(context.Background(), newStores(t), ""); err == nil || h != nil {
		t.Errorf("an empty namespace was accepted: %v", err)
	}
	if h, err := NewHandler(context.Background(), nil, testNamespace); err == nil || h != nil {
		t.Errorf("missing stores were accepted: %v", err)
	}
}

// Regression: app "other" could replay app "shop"'s manifest, or put its own
// config on shop's layers, and get an image in its own repository without
// ever holding the content.
func TestOneAppCannotAdoptAnothersImage(t *testing.T) {
	f := newRegistry(t)
	ctx := namespaces.WithNamespace(context.Background(), testNamespace)
	config, layer := []byte(`{"architecture":"amd64","os":"linux"}`), []byte("shop's private layer")
	f.upload("shop", config, false)
	layerDigest := f.upload("shop", layer, false)
	manifest := ociManifest(config, layer)
	if resp := f.do("shop", "PUT", "/v2/zymple/shop/manifests/v1", manifest, v1.MediaTypeImageManifest); resp.StatusCode != http.StatusCreated {
		t.Fatalf("shop's own push: %d", resp.StatusCode)
	}

	// The shared content store no longer answers for another app's blobs,
	// so there is no existence or size oracle either.
	if resp := f.do("other", "HEAD", "/v2/zymple/other/blobs/"+layerDigest.String(), nil, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("HEAD of shop's layer through other: %d, want 404", resp.StatusCode)
	}
	if resp := f.do("other", "HEAD", "/v2/zymple/other/manifests/"+digest.FromBytes(manifest).String(), nil, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("HEAD of shop's manifest through other: %d, want 404", resp.StatusCode)
	}
	if resp := f.do("other", "PUT", "/v2/zymple/other/manifests/stolen", manifest, v1.MediaTypeImageManifest); resp.StatusCode < 400 {
		t.Errorf("replayed manifest: %d, want an error", resp.StatusCode)
	}
	evil := []byte(`{"architecture":"amd64","os":"linux","config":{"Entrypoint":["cat","/secret"]}}`)
	f.upload("other", evil, false)
	if resp := f.do("other", "PUT", "/v2/zymple/other/manifests/mixed", ociManifest(evil, layer), v1.MediaTypeImageManifest); resp.StatusCode < 400 {
		t.Errorf("own config on shop's layer: %d, want an error", resp.StatusCode)
	}
	for _, tag := range []string{"stolen", "mixed"} {
		if _, err := f.stores.ImageService().Get(ctx, "docker.io/zymple/other:"+tag); err == nil {
			t.Errorf("image zymple/other:%s was created from shop's content", tag)
		}
	}

	// Uploading the same bytes is fine: then the app really has the content.
	f.upload("other", config, false)
	f.upload("other", layer, false)
	if resp := f.do("other", "PUT", "/v2/zymple/other/manifests/own", manifest, v1.MediaTypeImageManifest); resp.StatusCode != http.StatusCreated {
		t.Errorf("push after uploading every blob: %d", resp.StatusCode)
	}
}

// Regression: HEAD /manifests/<digest> read the named blob into memory even
// when it was a multi-gigabyte layer.
func TestManifestLookupDoesNotReadLayers(t *testing.T) {
	f := newRegistry(t)
	big := bytes.Repeat([]byte("0123456789abcdef"), (5<<20)/16)
	dgst := f.upload("shop", big, false)
	if resp := f.do("shop", "HEAD", "/v2/zymple/shop/manifests/"+dgst.String(), nil, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("HEAD manifests/<layer digest>: %d, want 404", resp.StatusCode)
	}
}
