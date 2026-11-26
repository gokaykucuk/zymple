package containerdstore

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/errdefs"
	"github.com/distribution/distribution/v3"
	"github.com/distribution/reference"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// tagService maps tags to images in the containerd image store.
type tagService struct {
	reg  *Registry
	repo reference.Named
}

var _ distribution.TagService = (*tagService)(nil)

// imageName is the name containerd and Docker know the tag by, for example
// "docker.io/zymple/shop:release-3".
func (t *tagService) imageName(tag string) (string, error) {
	canonical, err := reference.ParseNormalizedNamed(t.repo.String())
	if err != nil {
		return "", err
	}
	ref, err := reference.WithTag(canonical, tag)
	if err != nil {
		return "", err
	}
	return ref.String(), nil
}

func (t *tagService) Get(ctx context.Context, tag string) (v1.Descriptor, error) {
	name, err := t.imageName(tag)
	if err != nil {
		return v1.Descriptor{}, distribution.ErrManifestUnknown{Name: t.repo.Name(), Tag: tag}
	}
	img, err := t.reg.stores.ImageService().Get(t.reg.ctx(ctx), name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return v1.Descriptor{}, distribution.ErrTagUnknown{Tag: tag}
		}
		return v1.Descriptor{}, fmt.Errorf("get image %s: %w", name, err)
	}
	return img.Target, nil
}

// Tag creates or moves a tag. desc must be a manifest or index whose content
// is already in the content store.
func (t *tagService) Tag(ctx context.Context, tag string, desc v1.Descriptor) error {
	name, err := t.imageName(tag)
	if err != nil {
		return err
	}
	ctx = t.reg.ctx(ctx)
	// Label every blob with its children, so containerd's garbage collector
	// keeps the whole image once the upload leases expire.
	store := t.reg.stores.ContentStore()
	label := images.SetChildrenMappedLabels(store, images.ChildrenHandler(store), nil)
	if err := images.Dispatch(ctx, label, nil, desc); err != nil {
		return fmt.Errorf("label content of image %s: %w", name, err)
	}
	img := images.Image{Name: name, Target: desc}
	service := t.reg.stores.ImageService()
	if _, err := service.Create(ctx, img); err != nil {
		if !errdefs.IsAlreadyExists(err) {
			return fmt.Errorf("create image %s: %w", name, err)
		}
		if _, err := service.Update(ctx, img); err != nil {
			return fmt.Errorf("update image %s: %w", name, err)
		}
	}
	return nil
}

// The remaining operations are not supported: Zymple never lists or removes
// tags through the registry.

func (t *tagService) Untag(context.Context, string) error { return distribution.ErrUnsupported }

func (t *tagService) All(context.Context) ([]string, error) { return nil, distribution.ErrUnsupported }

func (t *tagService) Lookup(context.Context, v1.Descriptor) ([]string, error) {
	return nil, distribution.ErrUnsupported
}

func (t *tagService) List(context.Context, int, string) ([]string, error) {
	return nil, distribution.ErrUnsupported
}
