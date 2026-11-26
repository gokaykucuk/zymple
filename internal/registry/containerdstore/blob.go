package containerdstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/errdefs"
	"github.com/distribution/distribution/v3"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	octetStream = "application/octet-stream"
	// ownerLabelPrefix starts the content label that records which
	// repositories a blob was pushed to.
	ownerLabelPrefix = "zymple.io/pushed-to."
	// manifestLabelPrefix starts the content label that records which
	// repositories verified a blob as a manifest.
	manifestLabelPrefix = "zymple.io/manifest-of."
	// maxInlineBlob bounds what Get reads into memory. It matches the size
	// the registry accepts for a manifest.
	maxInlineBlob = 4 << 20
)

// blobStore implements distribution.BlobStore on the containerd content
// store for one repository.
//
// The content store itself is shared by every repository and by Docker, so
// the store keeps its own record of ownership: a blob is labelled with each
// repository that uploaded its bytes, and a repository sees only blobs that
// carry its label. Without that, knowing a digest would be enough to build
// an image out of another app's layers.
type blobStore struct {
	reg  *Registry
	repo string
}

func ownerLabel(repo string) string { return ownerLabelPrefix + repo }

// manifestLabel marks a blob as a manifest the repository's manifest service
// verified and stored. Uploading a manifest's bytes as an ordinary blob does
// not earn it.
func manifestLabel(repo string) string { return manifestLabelPrefix + repo }

// info describes a blob this repository owns. A blob that exists but was
// never pushed to the repository is reported as unknown.
func (b *blobStore) info(ctx context.Context, dgst digest.Digest) (content.Info, error) {
	info, err := b.reg.stores.ContentStore().Info(b.reg.ctx(ctx), dgst)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return content.Info{}, distribution.ErrBlobUnknown
		}
		return content.Info{}, fmt.Errorf("stat blob %s: %w", dgst, err)
	}
	if info.Labels[ownerLabel(b.repo)] == "" {
		return content.Info{}, distribution.ErrBlobUnknown
	}
	return info, nil
}

// claim records that the repository uploaded the blob's bytes.
func (b *blobStore) claim(ctx context.Context, dgst digest.Digest) error {
	return b.label(ctx, dgst, ownerLabel(b.repo))
}

// isManifest reports whether the repository verified the blob as a manifest.
func (b *blobStore) isManifest(ctx context.Context, dgst digest.Digest) (bool, error) {
	info, err := b.info(ctx, dgst)
	if errors.Is(err, distribution.ErrBlobUnknown) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Labels[manifestLabel(b.repo)] != "", nil
}

func (b *blobStore) label(ctx context.Context, dgst digest.Digest, key string) error {
	info := content.Info{Digest: dgst, Labels: map[string]string{key: "1"}}
	if _, err := b.reg.stores.ContentStore().Update(b.reg.ctx(ctx), info, "labels."+key); err != nil {
		return fmt.Errorf("label blob %s: %w", dgst, err)
	}
	return nil
}

var _ distribution.BlobStore = (*blobStore)(nil)

// hold puts the given content under a lease that expires on its own. The
// returned function gives the lease up early.
func (b *blobStore) hold(ctx context.Context, refs []v1.Descriptor) (release func(), err error) {
	ctx = b.reg.ctx(ctx)
	manager := b.reg.stores.LeasesService()
	lease, err := manager.Create(ctx, leases.WithRandomID(), leases.WithExpiration(leaseExpiration))
	if err != nil {
		return nil, fmt.Errorf("create containerd lease: %w", err)
	}
	release = func() { _ = manager.Delete(context.WithoutCancel(ctx), lease) }
	for _, ref := range refs {
		if ref.Digest.Validate() != nil {
			continue
		}
		if err := manager.AddResource(ctx, lease, leases.Resource{ID: ref.Digest.String(), Type: "content"}); err != nil {
			release()
			return nil, fmt.Errorf("lease blob %s: %w", ref.Digest, err)
		}
	}
	return release, nil
}

// Stat describes a blob, or returns distribution.ErrBlobUnknown.
func (b *blobStore) Stat(ctx context.Context, dgst digest.Digest) (v1.Descriptor, error) {
	info, err := b.info(ctx, dgst)
	if err != nil {
		return v1.Descriptor{}, err
	}
	return v1.Descriptor{MediaType: octetStream, Digest: info.Digest, Size: info.Size}, nil
}

// Get returns a blob's content. It is meant for small blobs such as
// manifests: anything larger is reported as unknown instead of being read
// into memory.
func (b *blobStore) Get(ctx context.Context, dgst digest.Digest) ([]byte, error) {
	info, err := b.info(ctx, dgst)
	if err != nil {
		return nil, err
	}
	if info.Size > maxInlineBlob {
		return nil, distribution.ErrBlobUnknown
	}
	data, err := content.ReadBlob(b.reg.ctx(ctx), b.reg.stores.ContentStore(), v1.Descriptor{Digest: dgst})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, distribution.ErrBlobUnknown
		}
		return nil, fmt.Errorf("read blob %s: %w", dgst, err)
	}
	return data, nil
}

// Open returns a reader over a blob.
func (b *blobStore) Open(ctx context.Context, dgst digest.Digest) (io.ReadSeekCloser, error) {
	if _, err := b.info(ctx, dgst); err != nil {
		return nil, err
	}
	ra, err := b.reg.stores.ContentStore().ReaderAt(b.reg.ctx(ctx), v1.Descriptor{Digest: dgst})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, distribution.ErrBlobUnknown
		}
		return nil, fmt.Errorf("open blob %s: %w", dgst, err)
	}
	return &blobReader{SectionReader: io.NewSectionReader(ra, 0, ra.Size()), ra: ra}, nil
}

// Put stores a small blob, such as a manifest, in one call.
func (b *blobStore) Put(ctx context.Context, mediaType string, blob []byte) (desc v1.Descriptor, err error) {
	writer, err := newBlobWriter(ctx, b, "")
	if err != nil {
		return v1.Descriptor{}, err
	}
	defer func() {
		if err != nil {
			_ = writer.Cancel(ctx)
		}
		_ = writer.Close()
	}()
	if _, err = writer.Write(blob); err != nil {
		return v1.Descriptor{}, err
	}
	return writer.Commit(ctx, v1.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(blob), Size: int64(len(blob))})
}

// Create starts a blob upload.
func (b *blobStore) Create(ctx context.Context, _ ...distribution.BlobCreateOption) (distribution.BlobWriter, error) {
	return newBlobWriter(ctx, b, "")
}

// Resume continues the upload with the given id.
func (b *blobStore) Resume(ctx context.Context, id string) (distribution.BlobWriter, error) {
	return newBlobWriter(ctx, b, id)
}

// ServeBlob writes a blob to an HTTP response.
func (b *blobStore) ServeBlob(ctx context.Context, w http.ResponseWriter, r *http.Request, dgst digest.Digest) error {
	desc, err := b.Stat(ctx, dgst)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", desc.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(desc.Size, 10))
	w.Header().Set("Docker-Content-Digest", dgst.String())
	w.Header().Set("Etag", dgst.String())
	if r.Method == http.MethodHead {
		return nil
	}
	reader, err := b.Open(ctx, dgst)
	if err != nil {
		return err
	}
	defer reader.Close()
	_, err = io.CopyN(w, reader, desc.Size)
	return err
}

// Delete is not supported. Removing an image in containerd removes its blobs.
func (b *blobStore) Delete(context.Context, digest.Digest) error { return distribution.ErrUnsupported }

type blobReader struct {
	*io.SectionReader
	ra content.ReaderAt
}

func (r *blobReader) Close() error { return r.ra.Close() }
