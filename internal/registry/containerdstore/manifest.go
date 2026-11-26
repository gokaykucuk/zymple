package containerdstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/distribution/distribution/v3"
	// Importing these packages also registers the manifest formats
	// UnmarshalManifest knows.
	"github.com/distribution/distribution/v3/manifest/manifestlist"
	"github.com/distribution/distribution/v3/manifest/ocischema"
	"github.com/distribution/distribution/v3/manifest/schema2"
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
// run, and one that points at blobs another app uploaded would hand that
// app's content to this one.
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
	if err := m.blobs.label(ctx, desc.Digest, manifestLabel(m.repo.Name())); err != nil {
		return "", err
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
	index, known := isIndex(manifest)
	if !known {
		return errcode.ErrorCodeManifestInvalid.WithDetail("unsupported manifest type")
	}
	refs := manifest.References()
	if len(refs) == 0 {
		return errcode.ErrorCodeManifestInvalid.WithDetail("the manifest references no content")
	}
	// Hold the referenced content while it is checked and until the tag that
	// follows makes it an image, so that pruning an older image at the same
	// moment cannot take a shared layer away in between.
	release, err := m.blobs.hold(ctx, refs)
	if err != nil {
		return err
	}
	var missing distribution.ErrManifestVerification
	for _, ref := range refs {
		ok, err := m.usable(ctx, ref, index)
		if err != nil {
			release()
			return err
		}
		if !ok {
			missing = append(missing, distribution.ErrManifestBlobUnknown{Digest: ref.Digest})
		}
	}
	if len(missing) > 0 {
		release()
		return missing
	}
	return nil
}

// usable reports whether a manifest in this repository may reference ref.
//
// An index may only name manifests this service verified and stored for the
// repository. Otherwise the JSON of a manifest that points at another app's
// layers could be uploaded as an ordinary blob, where nothing checks it, and
// then be reached through an index.
//
// An image manifest may only name blobs that were pushed to the repository,
// and none of them as a manifest: what interprets the image later follows
// the media type written here, not the one the content was checked as.
func (m *manifestService) usable(ctx context.Context, ref v1.Descriptor, index bool) (bool, error) {
	if ref.Digest.Validate() != nil {
		return false, nil
	}
	if index {
		return m.blobs.isManifest(ctx, ref.Digest)
	}
	if manifestMediaTypes[ref.MediaType] {
		return false, nil
	}
	_, err := m.blobs.Stat(ctx, ref.Digest)
	if errors.Is(err, distribution.ErrBlobUnknown) {
		return false, nil
	}
	return err == nil, err
}

var manifestMediaTypes = map[string]bool{
	v1.MediaTypeImageManifest:          true,
	v1.MediaTypeImageIndex:             true,
	schema2.MediaTypeManifest:          true,
	manifestlist.MediaTypeManifestList: true,
}

// isIndex tells an index from an image manifest. Any other kind of manifest
// is not known and is refused.
func isIndex(manifest distribution.Manifest) (index, known bool) {
	switch manifest.(type) {
	case *manifestlist.DeserializedManifestList, *ocischema.DeserializedImageIndex:
		return true, true
	case *schema2.DeserializedManifest, *ocischema.DeserializedManifest:
		return false, true
	}
	return false, false
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
