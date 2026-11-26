// Package runtime drives Docker and docker compose for app containers.
package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gokaykucuk/zymple/internal/appdef"
	"github.com/gokaykucuk/zymple/internal/execx"
	"github.com/gokaykucuk/zymple/internal/releases"
)

const (
	composeServiceLabel = "com.docker.compose.service"
	// composeOneOffLabel is "True" on containers started by `compose run`
	// and "False" on a service's own containers.
	composeOneOffLabel = "com.docker.compose.oneoff"
	flagFormat         = "--format"
	flagNoDeps         = "--no-deps"
	// noValue is what docker prints for a tag or digest an image lacks.
	noValue = "<none>"
)

// Docker implements the container operations the deploy engine needs.
type Docker struct {
	Runner execx.Runner
	// StateDir is Zymple's state root; rendered compose files live under it.
	StateDir string
}

// Container is one app container as Docker reports it.
type Container struct {
	ID      string
	Release int
	Running bool
	Service string
}

func (d *Docker) composePath(app string, release int) string {
	return filepath.Join(d.StateDir, "apps", app, "releases", strconv.Itoa(release), "compose.yml")
}

// HasCompose reports whether the rendered compose file of a release exists.
func (d *Docker) HasCompose(app string, release int) bool {
	_, err := os.Stat(d.composePath(app, release))
	return err == nil
}

// WriteCompose stores the rendered compose file of a release.
func (d *Docker) WriteCompose(app string, release int, data []byte) error {
	return releases.WriteFileAtomic(d.composePath(app, release), data, 0o600)
}

// RemoveCompose deletes the rendered compose file of a release.
func (d *Docker) RemoveCompose(app string, release int) error {
	return os.RemoveAll(filepath.Dir(d.composePath(app, release)))
}

// composeEnv quiets compose's warning about "orphan" containers. Each
// release's web service has its own name, so the previous release always
// looks like an orphan to compose; Zymple removes it itself after the drain.
func composeEnv() []string {
	return append(os.Environ(), "COMPOSE_IGNORE_ORPHANS=true")
}

// run streams a docker compose command's output to out.
func (d *Docker) run(ctx context.Context, out io.Writer, args []string) error {
	cmd := execx.Cmd{Name: "docker", Args: args, Stdout: out, Stderr: out, Env: composeEnv()}
	if err := d.Runner.Run(ctx, cmd); err != nil {
		return execx.Wrap(cmd, err, "")
	}
	return nil
}

func (d *Docker) compose(app string, release int, args ...string) []string {
	base := []string{"compose", "-p", appdef.Project(app), "-f", d.composePath(app, release)}
	if len(args) > 0 && args[0] == "run" {
		// Keep compose's own progress lines out of the command's output.
		base = append(base, "--progress", "quiet")
	}
	return append(base, args...)
}

// ImageExists reports whether the digest is present in the app's repository.
func (d *Docker) ImageExists(ctx context.Context, app, digest string) (bool, error) {
	_, err := execx.Output(ctx, d.Runner, "docker", "image", "inspect", flagFormat, "{{.Id}}", appdef.ImageRef(app, digest))
	if err == nil {
		return true, nil
	}
	if strings.Contains(strings.ToLower(err.Error()), "no such image") {
		return false, nil
	}
	return false, err
}

// EnsureNetwork creates the app's network if it does not exist.
func (d *Docker) EnsureNetwork(ctx context.Context, app string) error {
	name := appdef.Network(app)
	if _, err := execx.Output(ctx, d.Runner, "docker", "network", "inspect", flagFormat, "{{.Id}}", name); err == nil {
		return nil
	}
	_, err := execx.Output(ctx, d.Runner, "docker", "network", "create", "--label", appdef.LabelApp+"="+app, name)
	return err
}

// RunRelease runs the app's release command in a one-off container.
func (d *Docker) RunRelease(ctx context.Context, app *appdef.App, release int, out io.Writer) error {
	args := d.compose(app.Name, release, "run", "--rm", flagNoDeps, "-T",
		appdef.WebService(app.Web.Service, release))
	return d.run(ctx, out, append(args, app.Release...))
}

// Exec runs an arbitrary command in a one-off container of the current release.
func (d *Docker) Exec(ctx context.Context, app *appdef.App, release int, command []string, out io.Writer) error {
	args := d.compose(app.Name, release, "run", "--rm", flagNoDeps, "-T",
		appdef.WebService(app.Web.Service, release))
	return d.run(ctx, out, append(args, command...))
}

// StartWeb starts the web container of a release.
func (d *Docker) StartWeb(ctx context.Context, app *appdef.App, release int, out io.Writer) error {
	args := d.compose(app.Name, release, "up", "-d", flagNoDeps, appdef.WebService(app.Web.Service, release))
	return d.run(ctx, out, args)
}

// Containers lists an app's service containers with the given role.
//
// One-off containers are left out. `compose run` gives them the labels of
// the service they were started from, so the release command and
// `zymple run` would otherwise be mistaken for the web container itself.
func (d *Docker) Containers(ctx context.Context, app, role string) ([]Container, error) {
	return d.list(ctx, app, role, false)
}

// labelFilter is the `docker ps` flag that keeps containers with a label.
func labelFilter(key, value string) []string {
	return []string{"--filter", "label=" + key + "=" + value}
}

func (d *Docker) list(ctx context.Context, app, role string, oneOffs bool) ([]Container, error) {
	filters := labelFilter(appdef.LabelRole, role)
	if !oneOffs {
		filters = append(filters, labelFilter(composeOneOffLabel, "False")...)
	}
	return d.ps(ctx, app, filters)
}

// ps lists the app's containers, running or not, that pass the filters.
func (d *Docker) ps(ctx context.Context, app string, filters []string) ([]Container, error) {
	format := fmt.Sprintf(`{{.ID}} {{.State}} {{.Label %q}} {{.Label %q}}`, appdef.LabelRelease, composeServiceLabel)
	args := append([]string{"ps", "-a"}, labelFilter(appdef.LabelApp, app)...)
	args = append(args, filters...)
	out, err := execx.Output(ctx, d.Runner, "docker", append(args, flagFormat, format)...)
	if err != nil {
		return nil, err
	}
	var list []Container
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		release, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		c := Container{ID: fields[0], Running: fields[1] == "running", Release: release}
		if len(fields) > 3 {
			c.Service = fields[3]
		}
		list = append(list, c)
	}
	return list, nil
}

func (d *Docker) webIDs(ctx context.Context, app string, release int) ([]string, error) {
	list, err := d.Containers(ctx, app, appdef.RoleWeb)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, c := range list {
		if c.Release == release {
			ids = append(ids, c.ID)
		}
	}
	return ids, nil
}

// StopWeb stops a release's web container gracefully, then removes it.
func (d *Docker) StopWeb(ctx context.Context, app string, release int) error {
	ids, err := d.webIDs(ctx, app, release)
	if err != nil || len(ids) == 0 {
		return err
	}
	if _, err := execx.Output(ctx, d.Runner, "docker", append([]string{"stop"}, ids...)...); err != nil {
		return err
	}
	_, err = execx.Output(ctx, d.Runner, "docker", append([]string{"rm", "-f"}, ids...)...)
	return err
}

// RemoveWeb force-removes a release's web container.
func (d *Docker) RemoveWeb(ctx context.Context, app string, release int) error {
	ids, err := d.webIDs(ctx, app, release)
	if err != nil || len(ids) == 0 {
		return err
	}
	_, err = execx.Output(ctx, d.Runner, "docker", append([]string{"rm", "-f"}, ids...)...)
	return err
}

// RemoveOneOffs force-removes the app's one-off containers of the releases
// match selects. One-offs are what `compose run` starts: the release command
// and `zymple run`. Containers() leaves them out, so nothing else removes
// one that a crash of the daemon left running.
func (d *Docker) RemoveOneOffs(ctx context.Context, app string, match func(release int) bool) error {
	list, err := d.ps(ctx, app, labelFilter(composeOneOffLabel, "True"))
	if err != nil {
		return err
	}
	var ids []string
	for _, c := range list {
		if match(c.Release) {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	_, err = execx.Output(ctx, d.Runner, "docker", append([]string{"rm", "-f"}, ids...)...)
	return err
}

// StartContainers starts stopped containers, for example after a host reboot.
func (d *Docker) StartContainers(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := execx.Output(ctx, d.Runner, "docker", append([]string{"start"}, ids...)...)
	return err
}

// UpServices brings the non-web services to a release and removes containers
// of services that the compose file no longer declares.
func (d *Docker) UpServices(ctx context.Context, app *appdef.App, release int, out io.Writer) error {
	others := app.OtherServices()
	wanted := make(map[string]bool, len(others))
	for _, name := range others {
		wanted[name] = true
	}
	existing, err := d.Containers(ctx, app.Name, appdef.RoleService)
	if err != nil {
		return err
	}
	var stale []string
	for _, c := range existing {
		if !wanted[c.Service] {
			stale = append(stale, c.ID)
		}
	}
	if len(stale) > 0 {
		if _, err := execx.Output(ctx, d.Runner, "docker", append([]string{"rm", "-f"}, stale...)...); err != nil {
			return err
		}
	}
	if len(others) == 0 {
		return nil
	}
	args := d.compose(app.Name, release, append([]string{"up", "-d", flagNoDeps}, others...)...)
	return d.run(ctx, out, args)
}

// StopAll stops every container of the app: web, services, and one-off
// commands that are still running.
func (d *Docker) StopAll(ctx context.Context, app string) error {
	return d.eachContainer(ctx, app, "stop", true)
}

// StartAll starts the app's web and service containers again. A one-off
// command that was stopped is not run a second time.
func (d *Docker) StartAll(ctx context.Context, app string) error {
	return d.eachContainer(ctx, app, "start", false)
}

func (d *Docker) eachContainer(ctx context.Context, app, verb string, oneOffs bool) error {
	var ids []string
	for _, role := range []string{appdef.RoleService, appdef.RoleWeb} {
		list, err := d.list(ctx, app, role, oneOffs)
		if err != nil {
			return err
		}
		for _, c := range list {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	_, err := execx.Output(ctx, d.Runner, "docker", append([]string{verb}, ids...)...)
	return err
}

// ReleaseTagPrefix starts every tag Zymple itself puts on an image.
const ReleaseTagPrefix = "release-"

// ReleaseTag is the local tag that pins a release's image against pruning.
func ReleaseTag(release int) string { return ReleaseTagPrefix + strconv.Itoa(release) }

// ParseReleaseTag returns the release a tag pins. Only the exact text
// ReleaseTag produces counts: "release-1.4.0" and "release-07" are tags
// somebody else chose.
func ParseReleaseTag(tag string) (int, bool) {
	rest, ok := strings.CutPrefix(tag, ReleaseTagPrefix)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 || ReleaseTag(n) != tag {
		return 0, false
	}
	return n, true
}

// Pin tags a release's image so image pruning cannot remove it.
func (d *Docker) Pin(ctx context.Context, app, digest string, release int) error {
	_, err := execx.Output(ctx, d.Runner, "docker", "tag",
		appdef.ImageRef(app, digest), appdef.Repository(app)+":"+ReleaseTag(release))
	return err
}

// Repin gives a release's image its name back after something outside
// Zymple removed it, typically `docker image prune -a`. It works only while
// the image content is still on the host, for example because a running
// container uses it.
func (d *Docker) Repin(ctx context.Context, app, digest string, release int) error {
	_, err := execx.Output(ctx, d.Runner, "docker", "tag", digest, appdef.Repository(app)+":"+ReleaseTag(release))
	return err
}

// Tag is one tag in an app's repository and the digest it points at.
type Tag struct {
	Name string
	// Digest is "" when Docker does not report one.
	Digest string
}

// Tags lists the tags in an app's repository.
func (d *Docker) Tags(ctx context.Context, app string) ([]Tag, error) {
	out, err := execx.Output(ctx, d.Runner, "docker", "images", "--digests",
		flagFormat, "{{.Tag}} {{.Digest}}", appdef.Repository(app))
	if err != nil {
		return nil, err
	}
	var tags []Tag
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == noValue {
			continue
		}
		tag := Tag{Name: fields[0]}
		if len(fields) > 1 && fields[1] != noValue {
			tag.Digest = fields[1]
		}
		tags = append(tags, tag)
	}
	return tags, nil
}

// RemoveTag removes one tag from an app's repository.
func (d *Docker) RemoveTag(ctx context.Context, app, tag string) error {
	_, err := execx.Output(ctx, d.Runner, "docker", "rmi", appdef.Repository(app)+":"+tag)
	return err
}

// Logs writes the logs of an app's containers for one compose service.
func (d *Docker) Logs(ctx context.Context, app, role, service string, tail int, follow bool, out io.Writer) error {
	list, err := d.Containers(ctx, app, role)
	if err != nil {
		return err
	}
	var id string
	for _, c := range list {
		if c.Running && (service == "" || c.Service == service) {
			id = c.ID
		}
	}
	if id == "" {
		return fmt.Errorf("no running container for that service")
	}
	args := []string{"logs", "--tail", strconv.Itoa(tail)}
	if follow {
		args = append(args, "-f")
	}
	return execx.Stream(ctx, d.Runner, out, "docker", append(args, id)...)
}
