package appdef

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ImagePlaceholder is the only image a service may name. Zymple replaces it
// with the pinned digest of the release.
const ImagePlaceholder = "${IMAGE}"

var (
	serviceRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	volumeRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
)

// allowedServiceKeys is an allow-list. Anything not named here is rejected,
// which is what keeps privileged mode, host namespaces, devices, capabilities,
// bind mounts, published ports and builds out of app definitions.
var allowedServiceKeys = map[string]bool{
	"image":             true,
	"command":           true,
	"entrypoint":        true,
	"environment":       true,
	"healthcheck":       true,
	"restart":           true,
	"stop_grace_period": true,
	"stop_signal":       true,
	"working_dir":       true,
	"user":              true,
	"init":              true,
	"read_only":         true,
	"tmpfs":             true,
	"mem_limit":         true,
	"cpus":              true,
	"pids_limit":        true,
	"volumes":           true,
}

// reservedEnv are variables Zymple injects; apps may not set them.
var reservedEnv = map[string]bool{"DATABASE_URL": true, "REDIS_URL": true}

// Compose is a validated compose document.
type Compose struct {
	// Services maps a service name to its settings.
	Services map[string]map[string]any
	// Volumes lists the declared named volumes.
	Volumes []string
}

// ServiceNames returns the service names in sorted order.
func (c *Compose) ServiceNames() []string {
	names := make([]string, 0, len(c.Services))
	for name := range c.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ParseCompose validates a compose file against Zymple's restricted subset.
func ParseCompose(data []byte) (*Compose, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	if err := rejectAliases(&root); err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := root.Decode(&doc); err != nil {
		return nil, err
	}
	for key := range doc {
		if key != "services" && key != "volumes" {
			return nil, fmt.Errorf("top-level key %q is not allowed", key)
		}
	}
	c := &Compose{Services: map[string]map[string]any{}}
	volumes, err := parseVolumes(doc["volumes"])
	if err != nil {
		return nil, err
	}
	c.Volumes = volumes
	services, ok := doc["services"].(map[string]any)
	if !ok || len(services) == 0 {
		return nil, errors.New("services must be a non-empty mapping")
	}
	declared := make(map[string]bool, len(volumes))
	for _, v := range volumes {
		declared[v] = true
	}
	for name, raw := range services {
		if !serviceRe.MatchString(name) {
			return nil, fmt.Errorf("service name %q is not allowed", name)
		}
		svc, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("service %s must be a mapping", name)
		}
		if err := validateService(svc, declared); err != nil {
			return nil, fmt.Errorf("service %s: %w", name, err)
		}
		c.Services[name] = svc
	}
	return c, nil
}

// rejectAliases refuses YAML anchors, aliases and merge keys. They are the
// usual way to smuggle a forbidden setting past a reviewer.
func rejectAliases(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return errors.New("YAML anchors and aliases are not allowed")
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "<<" {
				return errors.New("YAML merge keys are not allowed")
			}
		}
	}
	for _, child := range n.Content {
		if err := rejectAliases(child); err != nil {
			return err
		}
	}
	return nil
}

func parseVolumes(raw any) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("volumes must be a mapping")
	}
	names := make([]string, 0, len(m))
	for name, opts := range m {
		if !volumeRe.MatchString(name) {
			return nil, fmt.Errorf("volume name %q is not allowed", name)
		}
		if opts != nil {
			if om, ok := opts.(map[string]any); !ok || len(om) != 0 {
				return nil, fmt.Errorf("volume %s must not set options", name)
			}
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func validateService(svc map[string]any, declaredVolumes map[string]bool) error {
	for key := range svc {
		if !allowedServiceKeys[key] {
			return fmt.Errorf("key %q is not allowed", key)
		}
	}
	if image, _ := svc["image"].(string); image != ImagePlaceholder {
		return fmt.Errorf("image must be exactly %s", ImagePlaceholder)
	}
	if err := rejectInterpolation(svc, true); err != nil {
		return err
	}
	if _, err := Environment(svc); err != nil {
		return err
	}
	if err := validateUser(svc["user"]); err != nil {
		return err
	}
	return validateServiceVolumes(svc["volumes"], declaredVolumes)
}

// rejectInterpolation refuses "$" anywhere except the image placeholder, so
// compose variable substitution cannot change what was reviewed.
func rejectInterpolation(v any, top bool) error {
	switch t := v.(type) {
	case string:
		if strings.Contains(t, "$") {
			return errors.New(`"$" is not allowed; compose interpolation is disabled`)
		}
	case map[string]any:
		for key, val := range t {
			if strings.Contains(key, "$") {
				return errors.New(`"$" is not allowed in keys`)
			}
			if top && key == "image" {
				continue
			}
			if err := rejectInterpolation(val, false); err != nil {
				return err
			}
		}
	case []any:
		for _, val := range t {
			if err := rejectInterpolation(val, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateUser(raw any) error {
	if raw == nil {
		return nil
	}
	user := fmt.Sprint(raw)
	if user == "0" || user == "root" || strings.HasPrefix(user, "0:") || strings.HasPrefix(user, "root:") {
		return errors.New("user must not be root; omit it to use the image's user")
	}
	return nil
}

func validateServiceVolumes(raw any, declared map[string]bool) error {
	if raw == nil {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return errors.New("volumes must be a list")
	}
	for _, item := range list {
		spec, ok := item.(string)
		if !ok {
			return errors.New(`volumes must use the short "name:/path" form`)
		}
		if err := validateVolumeSpec(spec, declared); err != nil {
			return err
		}
	}
	return nil
}

func validateVolumeSpec(spec string, declared map[string]bool) error {
	parts := strings.Split(spec, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return fmt.Errorf("volume %q must be name:/path or name:/path:ro", spec)
	}
	if !volumeRe.MatchString(parts[0]) || !declared[parts[0]] {
		return fmt.Errorf("volume %q must name a volume declared under volumes; host paths are not allowed", spec)
	}
	if !strings.HasPrefix(parts[1], "/") {
		return fmt.Errorf("volume %q must mount at an absolute path", spec)
	}
	if len(parts) == 3 && parts[2] != "ro" && parts[2] != "rw" {
		return fmt.Errorf("volume %q has an unsupported mode", spec)
	}
	return nil
}

func decodeEnvironment(raw any) (map[string]string, error) {
	env := map[string]string{}
	switch t := raw.(type) {
	case nil:
	case map[string]any:
		for key, val := range t {
			if val == nil {
				return nil, fmt.Errorf("environment %s must have a value", key)
			}
			env[key] = fmt.Sprint(val)
		}
	case []any:
		for _, item := range t {
			s, ok := item.(string)
			key, val, found := strings.Cut(s, "=")
			if !ok || !found {
				return nil, errors.New("environment list entries must be KEY=VALUE")
			}
			env[key] = val
		}
	default:
		return nil, errors.New("environment must be a mapping or a list")
	}
	return env, nil
}

// Environment returns a service's environment as a map. Compose allows both
// a mapping and a list of KEY=VALUE strings.
func Environment(svc map[string]any) (map[string]string, error) {
	env, err := decodeEnvironment(svc["environment"])
	if err != nil {
		return nil, err
	}
	for key := range env {
		if key == "" || strings.ContainsAny(key, " =\n") {
			return nil, fmt.Errorf("environment key %q is invalid", key)
		}
		if reservedEnv[key] {
			return nil, fmt.Errorf("environment %s is set by zymple", key)
		}
	}
	return env, nil
}
