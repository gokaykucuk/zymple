package containerdstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/distribution/distribution/v3"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	bolt "go.etcd.io/bbolt"
)

type inProcess struct{ db *metadata.DB }

func (s inProcess) ContentStore() content.Store   { return s.db.ContentStore() }
func (s inProcess) ImageService() images.Store    { return metadata.NewImageStore(s.db) }
func (s inProcess) LeasesService() leases.Manager { return metadata.NewLeaseManager(s.db) }

var ctx = context.Background()

func newRepo(t *testing.T) (distribution.Repository, *Registry, inProcess) {
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
	stores := inProcess{db: metadata.NewDB(bdb, cs, nil)}
	if err := stores.db.Init(ctx); err != nil {
		t.Fatal(err)
	}
	reg := New(stores, "moby")
	name, _ := reference.WithName("zymple/shop")
	repo, err := reg.Repository(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	return repo, reg, stores
}

func leaseCount(t *testing.T, stores inProcess) int {
	t.Helper()
	list, err := stores.LeasesService().List(namespaces.WithNamespace(ctx, "moby"))
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

func TestBlobRoundTrip(t *testing.T) {
	repo, reg, stores := newRepo(t)
	blobs := repo.Blobs(ctx)
	data := []byte("hello blob")
	dgst := digest.FromBytes(data)

	if _, err := blobs.Stat(ctx, dgst); !errors.Is(err, distribution.ErrBlobUnknown) {
		t.Fatalf("Stat of a missing blob: %v", err)
	}
	if _, err := blobs.Get(ctx, dgst); !errors.Is(err, distribution.ErrBlobUnknown) {
		t.Fatalf("Get of a missing blob: %v", err)
	}
	if _, err := blobs.Open(ctx, dgst); !errors.Is(err, distribution.ErrBlobUnknown) {
		t.Fatalf("Open of a missing blob: %v", err)
	}

	desc, err := blobs.Put(ctx, "text/plain", data)
	if err != nil || desc.Digest != dgst || desc.Size != int64(len(data)) || desc.MediaType != "text/plain" {
		t.Fatalf("Put = %+v, %v", desc, err)
	}
	if stat, err := blobs.Stat(ctx, dgst); err != nil || stat.Size != int64(len(data)) {
		t.Fatalf("Stat = %+v, %v", stat, err)
	}
	if stat, err := reg.BlobStatter().Stat(ctx, dgst); err != nil || stat.Digest != dgst {
		t.Fatalf("registry-wide Stat = %+v, %v", stat, err)
	}
	if got, err := blobs.Get(ctx, dgst); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get = %q, %v", got, err)
	}
	reader, err := blobs.Open(ctx, dgst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Seek(6, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(reader)
	if string(rest) != "blob" || reader.Close() != nil {
		t.Fatalf("Open/Seek read %q", rest)
	}
	// Storing the same content again is not an error.
	if _, err := blobs.Put(ctx, "text/plain", data); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if n := leaseCount(t, stores); n != 1 {
		t.Errorf("leases after one new blob and one duplicate = %d, want 1", n)
	}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := httptest.NewRecorder()
		if err := blobs.ServeBlob(ctx, rec, httptest.NewRequest(method, "/", nil), dgst); err != nil {
			t.Fatalf("ServeBlob %s: %v", method, err)
		}
		if rec.Header().Get("Docker-Content-Digest") != dgst.String() || rec.Header().Get("Content-Length") != "10" {
			t.Errorf("ServeBlob %s headers = %v", method, rec.Header())
		}
		if want := map[string]string{http.MethodGet: "hello blob", http.MethodHead: ""}[method]; rec.Body.String() != want {
			t.Errorf("ServeBlob %s body = %q", method, rec.Body.String())
		}
	}
	if err := blobs.ServeBlob(ctx, httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), digest.FromString("x")); !errors.Is(err, distribution.ErrBlobUnknown) {
		t.Errorf("ServeBlob of a missing blob: %v", err)
	}
}

func TestBlobWriter(t *testing.T) {
	repo, _, stores := newRepo(t)
	blobs := repo.Blobs(ctx)
	data := bytes.Repeat([]byte("0123456789"), 1000)
	dgst := digest.FromBytes(data)

	// Upload in two requests, as a client that resumes does.
	w, err := blobs.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := w.ID()
	if id == "" || w.StartedAt().IsZero() || w.Size() != 0 {
		t.Fatalf("new writer: id %q, started %v, size %d", id, w.StartedAt(), w.Size())
	}
	if n, err := w.ReadFrom(bytes.NewReader(data[:4000])); err != nil || n != 4000 {
		t.Fatalf("ReadFrom = %d, %v", n, err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w, err = blobs.Resume(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if w.Size() != 4000 {
		t.Fatalf("resumed writer size = %d, want 4000", w.Size())
	}
	if n, err := w.Write(data[4000:]); err != nil || n != 6000 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	desc, err := w.Commit(ctx, v1.Descriptor{Digest: dgst})
	if err != nil || desc.Size != int64(len(data)) || desc.MediaType != octetStream {
		t.Fatalf("Commit = %+v, %v", desc, err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got, _ := blobs.Get(ctx, dgst); !bytes.Equal(got, data) {
		t.Fatal("the committed blob does not match what was uploaded")
	}

	// A commit with the wrong digest is refused as the client's mistake and
	// stores nothing.
	w, _ = blobs.Create(ctx)
	_, _ = w.Write([]byte("tampered"))
	wrong := digest.FromString("claimed")
	var invalid distribution.ErrBlobInvalidDigest
	if _, err := w.Commit(ctx, v1.Descriptor{Digest: wrong}); !errors.As(err, &invalid) {
		t.Fatalf("digest mismatch: %v, want ErrBlobInvalidDigest", err)
	}
	_ = w.Close()
	if _, err := blobs.Stat(ctx, wrong); !errors.Is(err, distribution.ErrBlobUnknown) {
		t.Fatalf("content exists under a digest it does not match: %v", err)
	}

	// An upload that is cancelled, or never receives data, leaves no lease.
	before := leaseCount(t, stores)
	w, _ = blobs.Create(ctx)
	_, _ = w.Write([]byte("abandoned"))
	if err := w.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	w, _ = blobs.Create(ctx)
	_ = w.Close()
	if after := leaseCount(t, stores); after != before {
		t.Errorf("leases grew from %d to %d", before, after)
	}
}

func TestManifestsAndTags(t *testing.T) {
	repo, _, stores := newRepo(t)
	blobs := repo.Blobs(ctx)
	manifests, err := repo.Manifests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tags := repo.Tags(ctx)
	if repo.Named().Name() != "zymple/shop" {
		t.Fatalf("Named = %s", repo.Named())
	}

	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	layer := []byte("layer")
	_, _ = blobs.Put(ctx, v1.MediaTypeImageConfig, config)
	_, _ = blobs.Put(ctx, v1.MediaTypeImageLayerGzip, layer)
	manifest := []byte(`{"schemaVersion":2,"mediaType":"` + v1.MediaTypeImageManifest + `","config":{"mediaType":"` +
		v1.MediaTypeImageConfig + `","digest":"` + digest.FromBytes(config).String() + `","size":37},"layers":[{"mediaType":"` +
		v1.MediaTypeImageLayerGzip + `","digest":"` + digest.FromBytes(layer).String() + `","size":5}]}`)
	parsed, err := unmarshalManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	dgst, err := manifests.Put(ctx, parsed)
	if err != nil || dgst != digest.FromBytes(manifest) {
		t.Fatalf("manifest Put = %s, %v", dgst, err)
	}
	if ok, err := manifests.Exists(ctx, dgst); !ok || err != nil {
		t.Fatalf("Exists = %v, %v", ok, err)
	}
	if ok, err := manifests.Exists(ctx, digest.FromString("absent")); ok || err != nil {
		t.Fatalf("Exists of a missing manifest = %v, %v", ok, err)
	}
	got, err := manifests.Get(ctx, dgst)
	if err != nil || len(got.References()) != 2 {
		t.Fatalf("manifest Get = %v, %v", got, err)
	}
	var unknown distribution.ErrManifestUnknownRevision
	if _, err := manifests.Get(ctx, digest.FromString("absent")); !errors.As(err, &unknown) {
		t.Fatalf("Get of a missing manifest: %v", err)
	}
	// Manifests are verified before they are stored.
	missingLayer := []byte(strings.Replace(string(manifest), digest.FromBytes(layer).String(), digest.FromString("never uploaded").String(), 1))
	for name, doc := range map[string][]byte{
		"layer never uploaded": missingLayer,
		"wrong schema version": []byte(strings.Replace(string(manifest), `"schemaVersion":2`, `"schemaVersion":1`, 1)),
	} {
		bad, err := unmarshalManifest(doc)
		if err != nil {
			continue // refused even earlier
		}
		if _, err := manifests.Put(ctx, bad); err == nil {
			t.Errorf("%s: the manifest was accepted", name)
		}
		if ok, _ := manifests.Exists(ctx, digest.FromBytes(doc)); ok {
			t.Errorf("%s: the rejected manifest was stored anyway", name)
		}
	}
	// An image index is recognised by its content.
	index := []byte(`{"schemaVersion":2,"manifests":[{"mediaType":"` + v1.MediaTypeImageManifest + `","digest":"` + dgst.String() + `","size":` + strconv.Itoa(len(manifest)) + `}]}`)
	if parsedIndex, err := unmarshalManifest(index); err != nil {
		t.Errorf("image index: %v", err)
	} else if indexDigest, err := manifests.Put(ctx, parsedIndex); err != nil || indexDigest == "" {
		t.Errorf("image index Put: %v", err)
	}
	// A blob that is not a manifest is not returned as one.
	if _, err := manifests.Get(ctx, digest.FromBytes(layer)); err == nil {
		t.Fatal("a layer was returned as a manifest")
	}

	desc := v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: dgst, Size: int64(len(manifest))}
	if err := tags.Tag(ctx, "release-1", desc); err != nil {
		t.Fatal(err)
	}
	if got, err := tags.Get(ctx, "release-1"); err != nil || got.Digest != dgst {
		t.Fatalf("tag Get = %+v, %v", got, err)
	}
	var unknownTag distribution.ErrTagUnknown
	if _, err := tags.Get(ctx, "nope"); !errors.As(err, &unknownTag) {
		t.Fatalf("Get of a missing tag: %v", err)
	}
	if _, err := tags.Get(ctx, "not a tag!"); err == nil {
		t.Fatal("an invalid tag name was accepted")
	}
	if err := tags.Tag(ctx, "not a tag!", desc); err == nil {
		t.Fatal("an invalid tag name was accepted for tagging")
	}
	// Tagging something whose content is missing fails and creates nothing.
	ghost := v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromString("ghost"), Size: 5}
	if err := tags.Tag(ctx, "ghost", ghost); err == nil {
		t.Fatal("tagged a manifest that does not exist")
	}
	nsctx := namespaces.WithNamespace(ctx, "moby")
	if _, err := stores.ImageService().Get(nsctx, "docker.io/zymple/shop:ghost"); err == nil {
		t.Fatal("an image record was created for missing content")
	}
	if img, err := stores.ImageService().Get(nsctx, "docker.io/zymple/shop:release-1"); err != nil || img.Target.Digest != dgst {
		t.Fatalf("image record = %+v, %v", img, err)
	}
}

func TestUnsupportedOperations(t *testing.T) {
	repo, reg, _ := newRepo(t)
	manifests, _ := repo.Manifests(ctx)
	tags := repo.Tags(ctx)
	blobs := repo.Blobs(ctx)
	d := digest.FromString("x")
	checks := map[string]error{
		"blob delete":     blobs.Delete(ctx, d),
		"manifest delete": manifests.Delete(ctx, d),
		"untag":           tags.Untag(ctx, "x"),
		"enumerate":       reg.Blobs().Enumerate(ctx, func(digest.Digest) error { return nil }),
	}
	_, checks["all tags"] = tags.All(ctx)
	_, checks["lookup"] = tags.Lookup(ctx, v1.Descriptor{})
	_, checks["list tags"] = tags.List(ctx, 10, "")
	_, checks["catalog"] = reg.Repositories(ctx, nil, "")
	for what, err := range checks {
		if !errors.Is(err, distribution.ErrUnsupported) {
			t.Errorf("%s: %v, want ErrUnsupported", what, err)
		}
	}
	if reg.Scope() != distribution.GlobalScope {
		t.Error("scope is not global")
	}
	if _, err := unmarshalManifest([]byte("not json")); err == nil {
		t.Error("garbage was accepted as a manifest")
	}
}
