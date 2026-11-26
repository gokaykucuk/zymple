package appdef

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Labels put on every container Zymple starts.
const (
	LabelApp     = "zymple.app"
	LabelRelease = "zymple.release"
	LabelRole    = "zymple.role"
	RoleWeb      = "web"
	RoleService  = "service"
)

// Repository is the image repository of an app inside the embedded registry.
func Repository(app string) string { return "zymple/" + app }

// ImageRef is the digest-pinned image reference of a release.
func ImageRef(app, digest string) string { return Repository(app) + "@" + digest }

// Network is the Docker network shared by an app's containers.
func Network(app string) string { return "zymple-" + app }

// Project is the compose project name of an app.
func Project(app string) string { return "zymple-" + app }

// WebService is the compose service name of a release's web container.
// Each release gets its own name so two can run side by side.
func WebService(service string, release int) string {
	return fmt.Sprintf("%s-r%d", service, release)
}

// RenderInput is what a release adds to the reviewed compose file.
type RenderInput struct {
	Release int
	Digest  string
	// Images holds the digest of each extra image the app declares.
	Images map[string]string
	// Port is the loopback port the web container is published on.
	Port int
	// Env is injected into every service (DATABASE_URL, REDIS_URL).
	Env map[string]string
}

// Render produces the compose file Zymple actually runs for one release.
func (a *App) Render(in RenderInput) ([]byte, error) {
	services := map[string]any{}
	for name, svc := range a.Compose.Services {
		out := make(map[string]any, len(svc)+8)
		for key, val := range svc {
			out[key] = val
		}
		env, err := Environment(svc)
		if err != nil {
			return nil, err
		}
		for key, val := range in.Env {
			env[key] = val
		}
		out["environment"] = env
		digest, err := a.serviceDigest(name, in)
		if err != nil {
			return nil, err
		}
		out["image"] = ImageRef(a.Name, digest)
		out["pull_policy"] = "never"
		out["security_opt"] = []string{"no-new-privileges:true"}
		if _, ok := out["restart"]; !ok {
			out["restart"] = "unless-stopped"
		}
		if envFile := a.EnvFile(); envFile != "" {
			// "raw" stops compose from interpolating "$" inside secret values.
			out["env_file"] = []any{map[string]any{"path": envFile, "format": "raw"}}
		}
		labels := map[string]string{LabelApp: a.Name, LabelRelease: fmt.Sprint(in.Release), LabelRole: RoleService}
		target := name
		if name == a.Web.Service {
			labels[LabelRole] = RoleWeb
			target = WebService(name, in.Release)
			out["ports"] = []string{fmt.Sprintf("127.0.0.1:%d:%d", in.Port, a.Web.Port)}
			out["networks"] = map[string]any{"default": map[string]any{"aliases": []string{name}}}
		}
		out["labels"] = labels
		services[target] = out
	}
	doc := map[string]any{
		"name":     Project(a.Name),
		"services": services,
		"networks": map[string]any{"default": map[string]any{"name": Network(a.Name), "external": true}},
	}
	if len(a.Compose.Volumes) > 0 {
		volumes := map[string]any{}
		for _, v := range a.Compose.Volumes {
			volumes[v] = map[string]any{}
		}
		doc["volumes"] = volumes
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	// Compose interpolates "$" in every string; escape what is left so env
	// values pass through literally.
	return []byte(strings.ReplaceAll(string(data), "$", "$$")), nil
}

// serviceDigest is the digest a service runs in a release: the app's own
// image, or the extra image its placeholder names.
func (a *App) serviceDigest(service string, in RenderInput) (string, error) {
	image := a.Compose.Image(service)
	if image == "" {
		return in.Digest, nil
	}
	digest, ok := in.Images[image]
	if !ok {
		return "", fmt.Errorf("service %s: the release has no image %q", service, image)
	}
	return digest, nil
}

// OtherServices returns the compose services that are not the web service.
func (a *App) OtherServices() []string {
	var names []string
	for _, name := range a.Compose.ServiceNames() {
		if name != a.Web.Service {
			names = append(names, name)
		}
	}
	return names
}
