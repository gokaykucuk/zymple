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
	"github.com/containerd/errdefs"
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
	// Nothing can look a blob up without going through its repository.
	if _, err := reg.BlobStatter().Stat(ctx, dgst); !errors.Is(err, distribution.ErrBlobUnknown) {
		t.Fatalf("registry-wide Stat: %v", err)
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
	// The duplicate keeps its lease too: it holds the existing content until
	// the manifest that needs it arrives.
	if n := leaseCount(t, stores); n != 2 {
		t.Errorf("leases after one new blob and one duplicate = %d, want 2", n)
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

// otherRepo opens a second repository on the same stores.
func otherRepo(t *testing.T, reg *Registry, name string) distribution.Repository {
	t.Helper()
	named, _ := reference.WithName(name)
	repo, err := reg.Repository(ctx, named)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func imageManifest(config, layer []byte) []byte {
	return []byte(`{"schemaVersion":2,"mediaType":"` + v1.MediaTypeImageManifest + `","config":{"mediaType":"` +
		v1.MediaTypeImageConfig + `","digest":"` + digest.FromBytes(config).String() + `","size":` + strconv.Itoa(len(config)) +
		`},"layers":[{"mediaType":"` + v1.MediaTypeImageLayerGzip + `","digest":"` + digest.FromBytes(layer).String() +
		`","size":` + strconv.Itoa(len(layer)) + `}]}`)
}

// Regression: the content store is shared, and a repository used to see every
// blob in it. Knowing another app's digests was enough to build an image out
// of its layers.
func TestBlobsBelongToTheRepositoryThatPushedThem(t *testing.T) {
	shop, reg, _ := newRepo(t)
	other := otherRepo(t, reg, "zymple/other")
	config, layer := []byte(`{"architecture":"amd64","os":"linux"}`), []byte("shop's private layer")
	for _, blob := range [][]byte{config, layer} {
		if _, err := shop.Blobs(ctx).Put(ctx, octetStream, blob); err != nil {
			t.Fatal(err)
		}
	}
	doc := imageManifest(config, layer)
	parsed, err := unmarshalManifest(doc)
	if err != nil {
		t.Fatal(err)
	}
	shopManifests, _ := shop.Manifests(ctx)
	manifestDigest, err := shopManifests.Put(ctx, parsed)
	if err != nil {
		t.Fatal(err)
	}

	blobs := other.Blobs(ctx)
	manifests, _ := other.Manifests(ctx)
	layerDigest := digest.FromBytes(layer)
	if _, err := blobs.Stat(ctx, layerDigest); !errors.Is(err, distribution.ErrBlobUnknown) {
		t.Errorf("another repository can stat the blob: %v", err)
	}
	if _, err := blobs.Get(ctx, layerDigest); !errors.Is(err, distribution.ErrBlobUnknown) {
		t.Errorf("another repository can read the blob: %v", err)
	}
	if _, err := blobs.Open(ctx, layerDigest); !errors.Is(err, distribution.ErrBlobUnknown) {
		t.Errorf("another repository can open the blob: %v", err)
	}
	if ok, _ := manifests.Exists(ctx, manifestDigest); ok {
		t.Error("another repository can see the manifest")
	}
	if _, err := manifests.Get(ctx, manifestDigest); err == nil {
		t.Error("another repository can read the manifest")
	}
	// Replaying the manifest, without having uploaded what it names.
	if _, err := manifests.Put(ctx, parsed); err == nil {
		t.Fatal("a manifest naming another repository's blobs was accepted")
	}
	// Its own config on top of the other repository's layer.
	evil := []byte(`{"architecture":"amd64","os":"linux","config":{"Entrypoint":["cat","/secret"]}}`)
	if _, err := blobs.Put(ctx, octetStream, evil); err != nil {
		t.Fatal(err)
	}
	mixed, _ := unmarshalManifest(imageManifest(evil, layer))
	if _, err := manifests.Put(ctx, mixed); err == nil {
		t.Fatal("a manifest mixing in another repository's layer was accepted")
	}
	// Uploading the bytes is what makes a blob the repository's own.
	for _, blob := range [][]byte{config, layer} {
		if _, err := blobs.Put(ctx, octetStream, blob); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manifests.Put(ctx, parsed); err != nil {
		t.Fatalf("a repository that uploaded every blob was refused: %v", err)
	}
	// And the first repository still owns them.
	if _, err := shop.Blobs(ctx).Stat(ctx, layerDigest); err != nil {
		t.Errorf("the first repository lost its blob: %v", err)
	}
}

// Regression: a manifest lookup by digest used to read the whole blob into
// memory, whatever its size.
func TestGetRefusesLargeBlobs(t *testing.T) {
	repo, _, _ := newRepo(t)
	big := bytes.Repeat([]byte("x"), maxInlineBlob+1)
	desc, err := repo.Blobs(ctx).Put(ctx, octetStream, big)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Blobs(ctx).Get(ctx, desc.Digest); !errors.Is(err, distribution.ErrBlobUnknown) {
		t.Errorf("Get of a blob over the limit: %v", err)
	}
	manifests, _ := repo.Manifests(ctx)
	var unknown distribution.ErrManifestUnknownRevision
	if _, err := manifests.Get(ctx, desc.Digest); !errors.As(err, &unknown) {
		t.Errorf("a large layer was looked up as a manifest: %v", err)
	}
}

// A rejected manifest gives its lease back; an accepted one keeps its content
// held until the tag arrives.
func TestManifestVerificationLeases(t *testing.T) {
	repo, _, stores := newRepo(t)
	config, layer := []byte(`{"os":"linux"}`), []byte("layer")
	_, _ = repo.Blobs(ctx).Put(ctx, octetStream, config)
	before := leaseCount(t, stores)
	manifests, _ := repo.Manifests(ctx)
	bad, _ := unmarshalManifest(imageManifest(config, layer))
	if _, err := manifests.Put(ctx, bad); err == nil {
		t.Fatal("a manifest with a missing layer was accepted")
	}
	if after := leaseCount(t, stores); after != before {
		t.Errorf("a rejected manifest left %d leases behind", after-before)
	}
}

func FuzzManifestPut(f *testing.F) {
	config, layer := []byte(`{"os":"linux"}`), []byte("layer")
	f.Add(imageManifest(config, layer))
	f.Add([]byte(`{"schemaVersion":2,"manifests":[{"mediaType":"` + v1.MediaTypeImageManifest + `","digest":"` +
		digest.FromBytes(layer).String() + `","size":5}]}`))
	f.Add([]byte(`{"schemaVersion":1}`))
	f.Add([]byte(`not json`))
	dir := f.TempDir()
	cs, err := local.NewStore(filepath.Join(dir, "content"))
	if err != nil {
		f.Fatal(err)
	}
	bdb, err := bolt.Open(filepath.Join(dir, "meta.db"), 0o600, nil)
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { bdb.Close() })
	stores := inProcess{db: metadata.NewDB(bdb, cs, nil)}
	if err := stores.db.Init(ctx); err != nil {
		f.Fatal(err)
	}
	reg := New(stores, "moby")
	// "shop" owns two blobs. "other" owns nothing, so no manifest it sends
	// may ever be stored.
	shopName, _ := reference.WithName("zymple/shop")
	shop, _ := reg.Repository(ctx, shopName)
	for _, blob := range [][]byte{config, layer} {
		if _, err := shop.Blobs(ctx).Put(ctx, octetStream, blob); err != nil {
			f.Fatal(err)
		}
	}
	otherName, _ := reference.WithName("zymple/other")
	other, _ := reg.Repository(ctx, otherName)
	manifests, _ := other.Manifests(ctx)
	f.Fuzz(func(t *testing.T, doc []byte) {
		parsed, err := unmarshalManifest(doc)
		if err != nil {
			return
		}
		if dgst, err := manifests.Put(ctx, parsed); err == nil {
			t.Fatalf("a repository that owns no blobs stored manifest %s: %s", dgst, doc)
		}
	})
}

// lenientStore answers a commit the way containerd's content service does
// over its API: when the host already has the expected digest it reports
// "already exists" and discards the upload without looking at the bytes.
// The in-process local store checks the bytes first, so only this wrapper
// shows what happens against a real daemon.
type lenientStore struct{ content.Store }

func (s lenientStore) Writer(ctx context.Context, opts ...content.WriterOpt) (content.Writer, error) {
	w, err := s.Store.Writer(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return lenientWriter{Writer: w, store: s.Store}, nil
}

type lenientWriter struct {
	content.Writer
	store content.Store
}

func (w lenientWriter) Commit(ctx context.Context, size int64, expected digest.Digest, opts ...content.Opt) error {
	if _, err := w.store.Info(ctx, expected); err == nil {
		_ = w.Writer.Close()
		return errdefs.ErrAlreadyExists
	}
	return w.Writer.Commit(ctx, size, expected, opts...)
}

type lenientStores struct{ inProcess }

func (s lenientStores) ContentStore() content.Store { return lenientStore{s.inProcess.ContentStore()} }

// Regression: ownership was granted on containerd's "already exists" answer,
// which does not mean the uploaded bytes matched. An empty or junk upload
// under another app's digest made that app's layer one's own.
func TestOwnershipNeedsTheBytesNotJustTheDigest(t *testing.T) {
	_, _, stores := newRepo(t)
	reg := New(lenientStores{stores}, "moby")
	shop := otherRepo(t, reg, "zymple/shop")
	other := otherRepo(t, reg, "zymple/other")
	layer := []byte("shop's private layer")
	dgst := digest.FromBytes(layer)
	if _, err := shop.Blobs(ctx).Put(ctx, octetStream, layer); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"empty": nil, "junk": []byte("not the layer")} {
		w, err := other.Blobs(ctx).Create(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) > 0 {
			if _, err := w.Write(body); err != nil {
				t.Fatal(err)
			}
		}
		var invalid distribution.ErrBlobInvalidDigest
		if _, err := w.Commit(ctx, v1.Descriptor{Digest: dgst}); !errors.As(err, &invalid) {
			t.Errorf("%s upload under shop's digest: %v, want an invalid digest error", name, err)
		}
		_ = w.Close()
		if _, err := other.Blobs(ctx).Stat(ctx, dgst); !errors.Is(err, distribution.ErrBlobUnknown) {
			t.Errorf("%s upload under shop's digest made the blob other's: %v", name, err)
		}
	}
	// A wrong size with the right bytes is refused too.
	w, _ := other.Blobs(ctx).Create(ctx)
	_, _ = w.Write(layer)
	if _, err := w.Commit(ctx, v1.Descriptor{Digest: dgst, Size: int64(len(layer)) + 1}); err == nil {
		t.Error("a wrong size was accepted")
	}
	_ = w.Close()
	// The real bytes are what makes it other's.
	w, _ = other.Blobs(ctx).Create(ctx)
	_, _ = w.Write(layer)
	if _, err := w.Commit(ctx, v1.Descriptor{Digest: dgst}); err != nil {
		t.Fatalf("uploading the real bytes: %v", err)
	}
	_ = w.Close()
	if _, err := other.Blobs(ctx).Stat(ctx, dgst); err != nil {
		t.Errorf("after uploading the real bytes: %v", err)
	}
}

func imageIndex(children ...[]byte) []byte {
	var entries []string
	for _, child := range children {
		entries = append(entries, `{"mediaType":"`+v1.MediaTypeImageManifest+`","digest":"`+digest.FromBytes(child).String()+
			`","size":`+strconv.Itoa(len(child))+`}`)
	}
	return []byte(`{"schemaVersion":2,"mediaType":"` + v1.MediaTypeImageIndex + `","manifests":[` + strings.Join(entries, ",") + `]}`)
}

// Regression: an index was only checked one level deep, and a manifest
// uploaded as an ordinary blob is never checked at all. Together they let a
// repository reach another app's layers through an index.
func TestIndexOnlyNamesVerifiedManifests(t *testing.T) {
	shop, reg, _ := newRepo(t)
	other := otherRepo(t, reg, "zymple/other")
	config, layer := []byte(`{"architecture":"amd64","os":"linux"}`), []byte("shop's private layer")
	for _, blob := range [][]byte{config, layer} {
		if _, err := shop.Blobs(ctx).Put(ctx, octetStream, blob); err != nil {
			t.Fatal(err)
		}
	}
	// other holds its own config and the bytes of a manifest naming shop's
	// layer, both pushed as plain blobs.
	evil := []byte(`{"architecture":"amd64","os":"linux","config":{"Entrypoint":["cat","/secret"]}}`)
	smuggled := imageManifest(evil, layer)
	for _, blob := range [][]byte{evil, smuggled} {
		if _, err := other.Blobs(ctx).Put(ctx, octetStream, blob); err != nil {
			t.Fatal(err)
		}
	}
	manifests, _ := other.Manifests(ctx)
	index, err := unmarshalManifest(imageIndex(smuggled))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifests.Put(ctx, index); err == nil {
		t.Fatal("an index naming a manifest that was never verified was accepted")
	}
	// An image manifest may not name a manifest as one of its blobs either.
	nested := []byte(strings.Replace(string(imageManifest(evil, smuggled)), v1.MediaTypeImageLayerGzip, v1.MediaTypeImageManifest, 1))
	if parsed, err := unmarshalManifest(nested); err == nil {
		if _, err := manifests.Put(ctx, parsed); err == nil {
			t.Fatal("an image manifest naming a manifest as a layer was accepted")
		}
	}
	// A manifest that went through Put is fine in an index.
	own := []byte("other's own layer")
	if _, err := other.Blobs(ctx).Put(ctx, octetStream, own); err != nil {
		t.Fatal(err)
	}
	child := imageManifest(evil, own)
	parsedChild, _ := unmarshalManifest(child)
	if _, err := manifests.Put(ctx, parsedChild); err != nil {
		t.Fatal(err)
	}
	good, _ := unmarshalManifest(imageIndex(child))
	if _, err := manifests.Put(ctx, good); err != nil {
		t.Fatalf("an index of verified manifests was refused: %v", err)
	}
}
