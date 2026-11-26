package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gokaykucuk/zymple/internal/appdef"
	"github.com/gokaykucuk/zymple/internal/execx"
)

var pushDigestRe = regexp.MustCompile(`digest: (sha256:[a-f0-9]{64})`)

// Pusher uploads a local image to a Zymple host with the docker CLI.
type Pusher struct {
	Runner execx.Runner
	// Environ is the base environment for docker; defaults to os.Environ.
	Environ func() []string
}

// Push tags a local image for the app's repository, pushes it and returns
// its digest. Layers the host already has are not uploaded again.
//
// Credentials go into a temporary docker config directory, so the user's own
// docker login state is never touched.
func (p *Pusher) Push(ctx context.Context, registryHost, app, token, image string, out io.Writer) (string, error) {
	if strings.HasPrefix(image, "-") || image == "" {
		return "", fmt.Errorf("image %q is not an image reference", image)
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	target := registryHost + "/" + appdef.Repository(app) + ":push-" + hex.EncodeToString(suffix)

	// A different config directory also hides the user's docker contexts, so
	// pin the daemon the user's CLI currently points at.
	dockerHost, err := execx.Output(ctx, p.Runner, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "zymple-push-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	cfg := map[string]any{"auths": map[string]any{
		registryHost: map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(app + ":" + token))},
	}}
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		return "", err
	}

	if _, err := execx.Output(ctx, p.Runner, "docker", "tag", image, target); err != nil {
		return "", err
	}
	defer func() {
		// Best effort: remove the temporary tag from the local daemon.
		_, _ = execx.Output(context.WithoutCancel(ctx), p.Runner, "docker", "rmi", target)
	}()

	environ := os.Environ
	if p.Environ != nil {
		environ = p.Environ
	}
	env := append(environ(), "DOCKER_HOST="+dockerHost)
	var captured bytes.Buffer
	w := io.MultiWriter(out, &captured)
	cmd := execx.Cmd{Name: "docker", Args: []string{"--config", dir, "push", target}, Stdout: w, Stderr: w, Env: env}
	if err := p.Runner.Run(ctx, cmd); err != nil {
		return "", execx.Wrap(cmd, err, "")
	}
	if m := pushDigestRe.FindStringSubmatch(captured.String()); m != nil {
		return m[1], nil
	}
	return "", errors.New("docker push succeeded but printed no digest")
}
