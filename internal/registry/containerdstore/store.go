// Package containerdstore is a registry storage backend that keeps images in
// a containerd image store, so that a pushed image is immediately usable by
// the Docker daemon on the same host with no second copy.
//
// It is derived from github.com/psviderski/unregistry
// (internal/storage/containerd), copyright its contributors and licensed
// under the Apache License 2.0; see LICENSE in this directory. Changes made
// for Zymple: it depends on three small containerd interfaces instead of the
// client, applies the containerd namespace itself, implements the
// distribution v3.1 interfaces, drops debug logging, and is tested against
// in-process containerd stores.
package containerdstore

import (
	"context"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/distribution/distribution/v3"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
)

// Stores is what the backend needs from containerd. *client.Client
// implements it.
type Stores interface {
	ContentStore() content.Store
	ImageService() images.Store
	LeasesService() leases.Manager
}

// Registry implements distribution.Namespace on top of containerd.
type Registry struct {
	stores    Stores
	namespace string
}

var _ distribution.Namespace = (*Registry)(nil)

// New returns a registry backed by the given containerd stores. namespace is
// the containerd namespace images are stored in; Docker uses "moby".
func New(stores Stores, namespace string) *Registry {
	return &Registry{stores: stores, namespace: namespace}
}

// ctx puts the containerd namespace on a request context.
func (r *Registry) ctx(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, r.namespace)
}

// Scope returns the global scope.
func (r *Registry) Scope() distribution.Scope { return distribution.GlobalScope }

// Repository returns the repository with the given name.
func (r *Registry) Repository(_ context.Context, name reference.Named) (distribution.Repository, error) {
	return &repository{reg: r, name: name}, nil
}

// Repositories is not supported: Zymple never lists the catalog.
func (r *Registry) Repositories(context.Context, []string, string) (int, error) {
	return 0, distribution.ErrUnsupported
}

// Blobs returns an enumerator that enumerates nothing. Garbage collection is
// containerd's job.
func (r *Registry) Blobs() distribution.BlobEnumerator { return unsupportedEnumerator{} }

// BlobStatter returns a statter over the whole content store.
func (r *Registry) BlobStatter() distribution.BlobStatter { return &blobStore{reg: r} }

type unsupportedEnumerator struct{}

func (unsupportedEnumerator) Enumerate(context.Context, func(digest.Digest) error) error {
	return distribution.ErrUnsupported
}

type repository struct {
	reg  *Registry
	name reference.Named
}

var _ distribution.Repository = (*repository)(nil)

func (r *repository) Named() reference.Named { return r.name }

func (r *repository) Manifests(context.Context, ...distribution.ManifestServiceOption) (distribution.ManifestService, error) {
	return &manifestService{repo: r.name, blobs: &blobStore{reg: r.reg}}, nil
}

func (r *repository) Blobs(context.Context) distribution.BlobStore { return &blobStore{reg: r.reg} }

func (r *repository) Tags(context.Context) distribution.TagService {
	return &tagService{reg: r.reg, repo: r.name}
}
