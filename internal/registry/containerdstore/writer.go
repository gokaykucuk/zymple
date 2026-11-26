package containerdstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/errdefs"
	"github.com/distribution/distribution/v3"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// leaseExpiration bounds how long uploaded content survives without an image
// referring to it, for example after an abandoned push. It has to outlast the
// slowest push: the first layer's lease must still hold when the manifest
// arrives.
const leaseExpiration = 24 * time.Hour

// blobWriter is a resumable upload into the containerd content store.
type blobWriter struct {
	reg   *Registry
	blobs *blobStore
	id    string
	// lease keeps the uploaded content from being garbage collected until an
	// image refers to it. It is deliberately not deleted after a successful
	// commit: the manifest that will reference the blob arrives later.
	lease  leases.Lease
	writer content.Writer
	size   int64
}

var _ distribution.BlobWriter = (*blobWriter)(nil)

func newUploadID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func newBlobWriter(ctx context.Context, blobs *blobStore, id string) (*blobWriter, error) {
	reg := blobs.reg
	if id == "" {
		var err error
		if id, err = newUploadID(); err != nil {
			return nil, err
		}
	}
	ctx = reg.ctx(ctx)
	manager := reg.stores.LeasesService()
	lease, err := manager.Create(ctx, leases.WithRandomID(), leases.WithExpiration(leaseExpiration))
	if err != nil {
		return nil, fmt.Errorf("create containerd lease: %w", err)
	}
	writer, err := content.OpenWriter(leases.WithLease(ctx, lease.ID), reg.stores.ContentStore(), content.WithRef("upload-"+id))
	if err != nil {
		_ = manager.Delete(ctx, lease)
		return nil, fmt.Errorf("open containerd content writer: %w", err)
	}
	status, err := writer.Status()
	if err != nil {
		_ = writer.Close()
		_ = manager.Delete(ctx, lease)
		return nil, fmt.Errorf("read containerd content writer status: %w", err)
	}
	return &blobWriter{reg: reg, blobs: blobs, id: id, lease: lease, writer: writer, size: status.Offset}, nil
}

func (w *blobWriter) ID() string           { return w.id }
func (w *blobWriter) StartedAt() time.Time { return w.lease.CreatedAt }
func (w *blobWriter) Size() int64          { return w.size }

// ReadFrom copies an upload chunk into the content store.
func (w *blobWriter) ReadFrom(r io.Reader) (int64, error) {
	n, err := io.Copy(w.writer, r)
	w.size += n
	if err != nil {
		return n, fmt.Errorf("copy to containerd content writer: %w", err)
	}
	return n, nil
}

func (w *blobWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.size += int64(n)
	if err != nil {
		return n, fmt.Errorf("write to containerd content writer: %w", err)
	}
	return n, nil
}

// Commit finishes the upload. containerd verifies the digest, so a
// successful commit proves the client held the bytes; only then is the blob
// recorded as belonging to the repository.
func (w *blobWriter) Commit(ctx context.Context, desc v1.Descriptor) (v1.Descriptor, error) {
	ctx = w.reg.ctx(ctx)
	manager := w.reg.stores.LeasesService()
	if err := w.writer.Commit(ctx, w.size, desc.Digest); err != nil {
		switch {
		case errdefs.IsAlreadyExists(err):
			// The host already had the content, perhaps only through an
			// image that is about to be pruned. Hold it under this upload's
			// lease until the manifest arrives.
			resource := leases.Resource{ID: desc.Digest.String(), Type: "content"}
			if err := manager.AddResource(ctx, w.lease, resource); err != nil {
				return v1.Descriptor{}, fmt.Errorf("lease existing blob: %w", err)
			}
		case errdefs.IsFailedPrecondition(err), errdefs.IsInvalidArgument(err):
			// The content does not match the digest or size the client
			// claimed. That is the client's error, not the server's.
			_ = manager.Delete(ctx, w.lease)
			return v1.Descriptor{}, distribution.ErrBlobInvalidDigest{Digest: desc.Digest, Reason: err}
		default:
			_ = manager.Delete(ctx, w.lease)
			return v1.Descriptor{}, fmt.Errorf("commit blob: %w", err)
		}
	}
	if err := w.blobs.claim(ctx, desc.Digest); err != nil {
		return v1.Descriptor{}, err
	}
	if desc.Size == 0 {
		desc.Size = w.size
	}
	if desc.MediaType == "" {
		desc.MediaType = octetStream
	}
	return desc, nil
}

// Cancel abandons the upload.
func (w *blobWriter) Cancel(ctx context.Context) error {
	return w.reg.stores.LeasesService().Delete(w.reg.ctx(ctx), w.lease)
}

// Close releases the writer. An upload that received no data also gives up
// its lease.
func (w *blobWriter) Close() error {
	err := w.writer.Close()
	if w.size == 0 {
		_ = w.reg.stores.LeasesService().Delete(w.reg.ctx(context.Background()), w.lease)
	}
	return err
}
