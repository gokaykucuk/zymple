package containerdstore

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/errdefs"
	"github.com/distribution/distribution/v3"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const octetStream = "application/octet-stream"

// blobStore implements distribution.BlobStore on the containerd content
// store. The content store is not divided by repository.
type blobStore struct {
	reg *Registry
}

var _ distribution.BlobStore = (*blobStore)(nil)

// Stat describes a blob, or returns distribution.ErrBlobUnknown.
func (b *blobStore) Stat(ctx context.Context, dgst digest.Digest) (v1.Descriptor, error) {
	info, err := b.reg.stores.ContentStore().Info(b.reg.ctx(ctx), dgst)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return v1.Descriptor{}, distribution.ErrBlobUnknown
		}
		return v1.Descriptor{}, fmt.Errorf("stat blob %s: %w", dgst, err)
	}
	return v1.Descriptor{MediaType: octetStream, Digest: info.Digest, Size: info.Size}, nil
}

// Get returns a blob's content. It is meant for small blobs such as manifests.
func (b *blobStore) Get(ctx context.Context, dgst digest.Digest) ([]byte, error) {
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
	writer, err := newBlobWriter(ctx, b.reg, "")
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
	return newBlobWriter(ctx, b.reg, "")
}

// Resume continues the upload with the given id.
func (b *blobStore) Resume(ctx context.Context, id string) (distribution.BlobWriter, error) {
	return newBlobWriter(ctx, b.reg, id)
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
