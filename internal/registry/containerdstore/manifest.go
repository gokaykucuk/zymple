package containerdstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/distribution/distribution/v3"
	// These packages register the manifest formats UnmarshalManifest knows.
	_ "github.com/distribution/distribution/v3/manifest/manifestlist"
	_ "github.com/distribution/distribution/v3/manifest/ocischema"
	_ "github.com/distribution/distribution/v3/manifest/schema2"
	"github.com/distribution/distribution/v3/registry/api/errcode"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// manifestService stores manifests as blobs in the content store.
type manifestService struct {
	repo  reference.Named
	blobs *blobStore
}

var _ distribution.ManifestService = (*manifestService)(nil)

func (m *manifestService) Exists(ctx context.Context, dgst digest.Digest) (bool, error) {
	_, err := m.blobs.Stat(ctx, dgst)
	if errors.Is(err, distribution.ErrBlobUnknown) {
		return false, nil
	}
	return err == nil, err
}

func (m *manifestService) Get(ctx context.Context, dgst digest.Digest, _ ...distribution.ManifestServiceOption) (distribution.Manifest, error) {
	blob, err := m.blobs.Get(ctx, dgst)
	if err != nil {
		if errors.Is(err, distribution.ErrBlobUnknown) {
			return nil, distribution.ErrManifestUnknownRevision{Name: m.repo.Name(), Revision: dgst}
		}
		return nil, err
	}
	return unmarshalManifest(blob)
}

// Put verifies a manifest and stores it. Verification matters because the
// next step turns the manifest into an image: one that is malformed, or that
// points at blobs which were never uploaded, would be an image that cannot
// run.
func (m *manifestService) Put(ctx context.Context, manifest distribution.Manifest, _ ...distribution.ManifestServiceOption) (digest.Digest, error) {
	mediaType, payload, err := manifest.Payload()
	if err != nil {
		return "", fmt.Errorf("read manifest payload: %w", err)
	}
	if err := m.verify(ctx, manifest, payload); err != nil {
		return "", err
	}
	desc, err := m.blobs.Put(ctx, mediaType, payload)
	if err != nil {
		return "", fmt.Errorf("store manifest: %w", err)
	}
	return desc.Digest, nil
}

func (m *manifestService) verify(ctx context.Context, manifest distribution.Manifest, payload []byte) error {
	var header struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(payload, &header); err != nil || header.SchemaVersion != 2 {
		return errcode.ErrorCodeManifestInvalid.WithDetail("schemaVersion must be 2")
	}
	refs := manifest.References()
	if len(refs) == 0 {
		return errcode.ErrorCodeManifestInvalid.WithDetail("the manifest references no content")
	}
	var missing distribution.ErrManifestVerification
	for _, ref := range refs {
		if err := ref.Digest.Validate(); err != nil {
			missing = append(missing, distribution.ErrManifestBlobUnknown{Digest: ref.Digest})
			continue
		}
		_, err := m.blobs.Stat(ctx, ref.Digest)
		switch {
		case errors.Is(err, distribution.ErrBlobUnknown):
			missing = append(missing, distribution.ErrManifestBlobUnknown{Digest: ref.Digest})
		case err != nil:
			return err
		}
	}
	if len(missing) > 0 {
		return missing
	}
	return nil
}

// Delete is not supported.
func (m *manifestService) Delete(context.Context, digest.Digest) error {
	return distribution.ErrUnsupported
}

// unmarshalManifest parses a stored manifest. The media type is not kept
// beside the blob, so it is taken from the document itself.
func unmarshalManifest(blob []byte) (distribution.Manifest, error) {
	var probe struct {
		MediaType string          `json:"mediaType"`
		Manifests json.RawMessage `json:"manifests"`
	}
	if err := json.Unmarshal(blob, &probe); err != nil {
		return nil, distribution.ErrManifestVerification{errors.New("manifest is not JSON")}
	}
	mediaType := probe.MediaType
	if mediaType == "" {
		mediaType = v1.MediaTypeImageManifest
		if probe.Manifests != nil {
			mediaType = v1.MediaTypeImageIndex
		}
	}
	manifest, _, err := distribution.UnmarshalManifest(mediaType, blob)
	if err != nil {
		return nil, distribution.ErrManifestVerification{err}
	}
	return manifest, nil
}
