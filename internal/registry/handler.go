// Package registry serves the OCI registry routes, storing images directly
// in the host's containerd image store, and guards them with per-app tokens.
package registry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/containerd/containerd/v2/client"
	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/configuration"
	"github.com/distribution/distribution/v3/registry/handlers"
	middleware "github.com/distribution/distribution/v3/registry/middleware/registry"
	storagedriver "github.com/distribution/distribution/v3/registry/storage/driver"
	// The distribution app insists on a storage driver even though every
	// request is answered by the containerd backend.
	_ "github.com/distribution/distribution/v3/registry/storage/driver/inmemory"
	"github.com/sirupsen/logrus"

	"github.com/gokaykucuk/zymple/internal/registry/containerdstore"
)

const middlewareName = "zymple-containerd"

var registerOnce sync.Once

// register installs the containerd backend as a distribution "registry
// middleware", which is the extension point for replacing the storage.
func register() {
	registerOnce.Do(func() {
		err := middleware.Register(middlewareName, func(_ context.Context, _ distribution.Namespace,
			_ storagedriver.StorageDriver, options map[string]interface{}) (distribution.Namespace, error) {
			stores, ok := options["stores"].(containerdstore.Stores)
			namespace, _ := options["namespace"].(string)
			if !ok || namespace == "" {
				return nil, errors.New("containerd stores and namespace are required")
			}
			return containerdstore.New(stores, namespace), nil
		})
		if err != nil {
			panic(err)
		}
	})
}

// Dial connects to containerd. The connection is lazy: a wrong socket path
// shows up on the first request, not here.
func Dial(socket string) (containerdstore.Stores, error) {
	if socket == "" {
		return nil, errors.New("containerd socket path is required")
	}
	c, err := client.New(socket)
	if err != nil {
		return nil, fmt.Errorf("connect to containerd at %s: %w", socket, err)
	}
	return c, nil
}

// NewHandler returns an http.Handler for /v2/ backed by containerd.
// It applies no authentication; wrap it with Guard.
func NewHandler(ctx context.Context, stores containerdstore.Stores, namespace string) (h http.Handler, err error) {
	register()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	// distribution logs every request at info level through logrus.
	logrus.SetLevel(logrus.WarnLevel)
	cfg := &configuration.Configuration{
		Storage: configuration.Storage{
			"inmemory": configuration.Parameters{},
			"maintenance": configuration.Parameters{
				"uploadpurging": map[interface{}]interface{}{"enabled": false},
			},
		},
		Middleware: map[string][]configuration.Middleware{
			"registry": {{
				Name:    middlewareName,
				Options: configuration.Parameters{"stores": stores, "namespace": namespace},
			}},
		},
	}
	// Upload state is signed with this secret. One per process is enough:
	// an upload interrupted by a restart is simply started again.
	cfg.HTTP.Secret = hex.EncodeToString(secret)
	// The distribution app panics when it cannot be set up.
	defer func() {
		if r := recover(); r != nil {
			h, err = nil, fmt.Errorf("registry: %v", r)
		}
	}()
	return handlers.NewApp(ctx, cfg), nil
}
