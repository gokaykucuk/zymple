// Package cli implements the zymple command line.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/gokaykucuk/zymple/internal/appdef"
	"github.com/gokaykucuk/zymple/internal/auth"
	"github.com/gokaykucuk/zymple/internal/client"
	"github.com/gokaykucuk/zymple/internal/config"
	"github.com/gokaykucuk/zymple/internal/deploy"
	"github.com/gokaykucuk/zymple/internal/execx"
	"github.com/gokaykucuk/zymple/internal/releases"
	"github.com/gokaykucuk/zymple/internal/services"
)

// Version is set at build time with -ldflags.
var Version = "dev"

const usage = `usage: zymple <command> [arguments]

From CI or a laptop (ZYMPLE_HOST and ZYMPLE_TOKEN set):
  deploy <app> --image <ref>     push a local image and release it
  deploy <app> --digest <sha>    release an image that is already on the host
                                 an extra image is <name>=<ref> or <name>=<sha>;
                                 both flags repeat, and what is left out stays
  rollback <app> [release]       re-activate an older release
  releases <app>                 list kept releases
  status <app>                   show the current release
  logs <app> [service] [-f]      show container logs

On the host (uses the operator socket):
  apps                           list apps
  reload                         re-read /etc/zymple and converge
  redeploy <app>                 new release of the current image
  run <app> -- <command...>      run a command in a one-off container
  pg info|backup|restore|psql|drill-rm <app>
  pg import <app> <dump-file> --yes   replace the database with a pg_dump archive
  redis backup <app>
  redis restore <app> --yes

Anywhere:
  token generate <id>            create a deploy token and its hash line
  server [--config <path>]       run the daemon
  version
`

// URL prefixes of the server's app routes.
const (
	apiApps   = "/api/v1/apps/"
	adminApps = "/admin/apps/"
)

// Env is everything the command line touches outside its arguments.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	Runner execx.Runner
	// Serve runs the daemon; it is injected so this package stays testable.
	Serve func(ctx context.Context, configPath string) error
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return usageError{fmt.Sprintf(format, args...)} }

// Run executes one command line and returns the process exit code.
func Run(ctx context.Context, args []string, env Env) int {
	err := dispatch(ctx, args, env)
	if err == nil {
		return 0
	}
	var ue usageError
	if errors.As(err, &ue) {
		fmt.Fprintf(env.Stderr, "zymple: %s\n\n%s", ue.msg, usage)
		return 2
	}
	fmt.Fprintf(env.Stderr, "zymple: %v\n", err)
	return 1
}

func dispatch(ctx context.Context, args []string, env Env) error {
	if len(args) == 0 {
		return usagef("no command given")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "--version":
		_, err := fmt.Fprintln(env.Stdout, "zymple", Version)
		return err
	case "help", "--help", "-h":
		_, err := io.WriteString(env.Stdout, usage)
		return err
	case "server":
		return cmdServer(ctx, rest, env)
	case "token":
		return cmdToken(rest, env)
	case "deploy":
		return cmdDeploy(ctx, rest, env)
	case "rollback":
		return cmdRollback(ctx, rest, env)
	case "releases":
		return cmdReleases(ctx, rest, env)
	case "status":
		return cmdStatus(ctx, rest, env)
	case "logs":
		return cmdLogs(ctx, rest, env)
	case "apps":
		return cmdApps(ctx, rest, env)
	case "reload":
		return adminStream(ctx, env, http.MethodPost, "/admin/reload", nil)
	case "redeploy":
		app, err := oneApp(rest)
		if err != nil {
			return err
		}
		return adminStream(ctx, env, http.MethodPost, adminApps+app+"/redeploy", nil)
	case "run":
		return cmdRun(ctx, rest, env)
	case "pg":
		return cmdPG(ctx, rest, env)
	case "redis":
		return cmdRedis(ctx, rest, env)
	}
	return usagef("unknown command %q", cmd)
}

func newFlags(name string, env Env) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parse accepts flags before and after positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usagef("%v", err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

func oneApp(args []string) (string, error) {
	if len(args) != 1 {
		return "", usagef("expected exactly one app name")
	}
	return appName(args[0])
}

func appName(s string) (string, error) {
	if !appdef.ValidName(s) {
		return "", usagef("%q is not a valid app name", s)
	}
	return s, nil
}

// connect picks the network API when ZYMPLE_HOST is set and the operator
// socket otherwise.
func connect(env Env) (*client.Client, error) {
	if host := env.Getenv("ZYMPLE_HOST"); host != "" {
		return client.New(host, env.Getenv("ZYMPLE_TOKEN"))
	}
	return adminClient(env), nil
}

func adminClient(env Env) *client.Client {
	socket := env.Getenv("ZYMPLE_SOCKET")
	if socket == "" {
		socket = config.Default().Socket
	}
	return client.NewUnix(socket)
}

func printer(env Env) func(deploy.Event) {
	return func(e deploy.Event) {
		switch {
		case e.Status == "log" || e.Step == "done":
			fmt.Fprintln(env.Stdout, printable(e.Message))
		case e.Status == "start":
			fmt.Fprintf(env.Stdout, "==> %s\n", printable(e.Step))
		case e.Status == "fail":
			fmt.Fprintf(env.Stdout, "==> %s failed: %s\n", printable(e.Step), printable(e.Message))
		}
	}
}

// colourRe matches the terminal sequences that only change colour and style.
var colourRe = regexp.MustCompile(`\x1b\[[0-9;]{0,32}m`)

// printable makes text from the server safe to print. Most of it is output
// of the app's own containers, which whoever builds the image controls, and
// it is printed on an operator's terminal or into a CI log. Colours are
// and line breaks are kept; every other control character is replaced, so that output cannot
// move the cursor, rewrite earlier lines, retitle the window or write to the
// clipboard.
func printable(s string) string {
	clean := func(part string) string {
		return strings.Map(func(r rune) rune {
			if r == '\t' || r == '\n' || (r >= 0x20 && r != 0x7f && (r < 0x80 || r > 0x9f)) {
				return r
			}
			return utf8.RuneError
		}, part)
	}
	var b strings.Builder
	last := 0
	for _, loc := range colourRe.FindAllStringIndex(s, -1) {
		b.WriteString(clean(s[last:loc[0]]))
		b.WriteString(s[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(clean(s[last:]))
	return b.String()
}

func adminStream(ctx context.Context, env Env, method, path string, body any) error {
	return adminClient(env).Stream(ctx, method, path, body, printer(env))
}

func cmdServer(ctx context.Context, args []string, env Env) error {
	fs := newFlags("server", env)
	path := fs.String("config", config.DefaultPath, "")
	if rest, err := parse(fs, args); err != nil || len(rest) != 0 {
		return usagef("usage: zymple server [--config <path>]")
	}
	return env.Serve(ctx, *path)
}

func cmdToken(args []string, env Env) error {
	if len(args) != 2 || args[0] != "generate" {
		return usagef("usage: zymple token generate <id>")
	}
	token, err := auth.Generate()
	if err != nil {
		return err
	}
	line, err := auth.HashLine(args[1], token)
	if err != nil {
		return usagef("%v", err)
	}
	_, err = fmt.Fprintf(env.Stdout, "token: %s\ntokens file line: %s\n", token, line)
	return err
}

// imageFlags collects a repeatable --image or --digest flag. A value is
// either the app's own image or <name>=<value> for one of its extra images.
type imageFlags map[string]string

func (f imageFlags) String() string { return "" }

func (f imageFlags) Set(value string) error {
	name, rest, named := strings.Cut(value, "=")
	if !named {
		name, rest = "", value
	} else if !appdef.ValidImageName(name) {
		return fmt.Errorf("%q is not an image name", name)
	}
	if rest == "" {
		return errors.New("the value is empty")
	}
	if _, dup := f[name]; dup {
		return fmt.Errorf("given twice for %s", imageLabel(name))
	}
	f[name] = rest
	return nil
}

func imageLabel(name string) string {
	if name == "" {
		return "the app's image"
	}
	return "image " + name
}

// deployBody is the request of POST /api/v1/apps/<app>/deploys.
type deployBody struct {
	Digest string            `json:"digest,omitempty"`
	Images map[string]string `json:"images,omitempty"`
}

func cmdDeploy(ctx context.Context, args []string, env Env) error {
	fs := newFlags("deploy", env)
	images, digests := imageFlags{}, imageFlags{}
	fs.Var(images, "image", "")
	fs.Var(digests, "digest", "")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	app, err := oneApp(rest)
	if err != nil {
		return err
	}
	if err := checkImageFlags(images, digests); err != nil {
		return err
	}
	c, err := connect(env)
	if err != nil {
		return err
	}
	if len(images) > 0 && c.Token == "" {
		return usagef("--image needs ZYMPLE_HOST and ZYMPLE_TOKEN; on the host use --digest")
	}
	pusher := client.Pusher{Runner: env.Runner}
	for _, name := range sortedKeys(images) {
		fmt.Fprintf(env.Stdout, "==> push %s\n", images[name])
		digest, err := pusher.Push(ctx, c.RegistryHost(), app, c.Token, images[name], env.Stdout)
		if err != nil {
			return err
		}
		if !deploy.ValidDigest(digest) {
			return fmt.Errorf("the push of %s reported %q, which is not a sha256 digest", imageLabel(name), digest)
		}
		digests[name] = digest
	}
	body := deployBody{Digest: digests[""]}
	for _, name := range sortedKeys(digests) {
		if name == "" {
			fmt.Fprintf(env.Stdout, "==> deploy %s\n", digests[name])
			continue
		}
		if body.Images == nil {
			body.Images = map[string]string{}
		}
		body.Images[name] = digests[name]
		fmt.Fprintf(env.Stdout, "==> deploy %s %s\n", name, digests[name])
	}
	return c.Stream(ctx, http.MethodPost, apiApps+app+"/deploys", body, printer(env))
}

// checkImageFlags refuses a deploy that names nothing, names an image both
// ways, or gives something other than a digest to --digest.
func checkImageFlags(images, digests imageFlags) error {
	if len(images)+len(digests) == 0 {
		return usagef("give --image or --digest")
	}
	for name := range images {
		if _, both := digests[name]; both {
			return usagef("give one of --image and --digest for %s", imageLabel(name))
		}
	}
	for name, digest := range digests {
		if !deploy.ValidDigest(digest) {
			return usagef("the digest of %s must be sha256:<64 hex characters>", imageLabel(name))
		}
	}
	return nil
}

func cmdRollback(ctx context.Context, args []string, env Env) error {
	if len(args) < 1 || len(args) > 2 {
		return usagef("usage: zymple rollback <app> [release]")
	}
	app, err := appName(args[0])
	if err != nil {
		return err
	}
	target := 0
	if len(args) == 2 {
		if target, err = strconv.Atoi(args[1]); err != nil || target < 1 {
			return usagef("release must be a positive number")
		}
	}
	c, err := connect(env)
	if err != nil {
		return err
	}
	return c.Stream(ctx, http.MethodPost, apiApps+app+"/rollbacks", map[string]int{"release": target}, printer(env))
}

func cmdReleases(ctx context.Context, args []string, env Env) error {
	app, err := oneApp(args)
	if err != nil {
		return err
	}
	c, err := connect(env)
	if err != nil {
		return err
	}
	var out struct {
		Current  int                `json:"current"`
		Releases []releases.Release `json:"releases"`
	}
	if err := c.JSON(ctx, http.MethodGet, apiApps+app+"/releases", nil, &out); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RELEASE\tDIGEST\tDEPLOYED\tBY\tNOTE")
	for i := len(out.Releases) - 1; i >= 0; i-- {
		r := out.Releases[i]
		var notes []string
		if r.Number == out.Current {
			notes = append(notes, "current")
		}
		if r.RolledBackFrom != 0 {
			notes = append(notes, fmt.Sprintf("rollback from %d", r.RolledBackFrom))
		}
		for _, name := range sortedKeys(r.Images) {
			notes = append(notes, printable(name+" "+shortDigest(r.Images[name])))
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", r.Number, printable(shortDigest(r.Digest)),
			r.Time.Format(time.RFC3339), printable(r.By), strings.Join(notes, ", "))
	}
	return tw.Flush()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func shortDigest(d string) string {
	if len(d) > 19 {
		return d[:19]
	}
	return d
}

func cmdStatus(ctx context.Context, args []string, env Env) error {
	app, err := oneApp(args)
	if err != nil {
		return err
	}
	c, err := connect(env)
	if err != nil {
		return err
	}
	var st struct {
		Current    *releases.Release `json:"current"`
		Deploying  bool              `json:"deploying"`
		WebRunning bool              `json:"web_running"`
		LastError  string            `json:"last_error"`
	}
	if err := c.JSON(ctx, http.MethodGet, apiApps+app+"/status", nil, &st); err != nil {
		return err
	}
	if st.Current == nil {
		fmt.Fprintln(env.Stdout, "release:   none")
	} else {
		fmt.Fprintf(env.Stdout, "release:   %d (%s, %s by %s)\n", st.Current.Number, printable(st.Current.Digest),
			st.Current.Time.Format(time.RFC3339), printable(st.Current.By))
		for _, name := range sortedKeys(st.Current.Images) {
			fmt.Fprintf(env.Stdout, "image:     %s (%s)\n", printable(name), printable(st.Current.Images[name]))
		}
		fmt.Fprintf(env.Stdout, "web:       %s\n", map[bool]string{true: "running", false: "NOT RUNNING"}[st.WebRunning])
	}
	fmt.Fprintf(env.Stdout, "deploying: %v\n", st.Deploying)
	if st.LastError != "" {
		// The error can quote what a container printed.
		fmt.Fprintf(env.Stdout, "last failed deploy: %s\n", printable(st.LastError))
	}
	if st.Current != nil && !st.WebRunning {
		return errors.New("the current release's web container is not running")
	}
	return nil
}

func cmdLogs(ctx context.Context, args []string, env Env) error {
	fs := newFlags("logs", env)
	follow := fs.Bool("f", false, "")
	tail := fs.Int("tail", 200, "")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) < 1 || len(rest) > 2 {
		return usagef("usage: zymple logs <app> [service] [-f] [--tail n]")
	}
	app, err := appName(rest[0])
	if err != nil {
		return err
	}
	q := url.Values{"tail": {strconv.Itoa(*tail)}, "follow": {strconv.FormatBool(*follow)}}
	if len(rest) == 2 {
		q.Set("service", rest[1])
	}
	c, err := connect(env)
	if err != nil {
		return err
	}
	return c.Stream(ctx, http.MethodGet, apiApps+app+"/logs?"+q.Encode(), nil, printer(env))
}

func cmdApps(ctx context.Context, args []string, env Env) error {
	if len(args) != 0 {
		return usagef("usage: zymple apps")
	}
	var rows []struct {
		Name    string   `json:"name"`
		Domains []string `json:"domains"`
		Current int      `json:"current"`
	}
	if err := adminClient(env).JSON(ctx, http.MethodGet, "/admin/apps", nil, &rows); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "APP\tRELEASE\tDOMAINS")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%d\t%s\n", r.Name, r.Current, strings.Join(r.Domains, ", "))
	}
	return tw.Flush()
}

func cmdRun(ctx context.Context, args []string, env Env) error {
	if len(args) < 3 || args[1] != "--" {
		return usagef("usage: zymple run <app> -- <command...>")
	}
	app, err := appName(args[0])
	if err != nil {
		return err
	}
	return adminStream(ctx, env, http.MethodPost, adminApps+app+"/run", map[string][]string{"command": args[2:]})
}

func cmdPG(ctx context.Context, args []string, env Env) error {
	if len(args) < 2 {
		return usagef("usage: zymple pg info|backup|restore|import|psql|drill-rm <app>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "info":
		app, err := oneApp(rest)
		if err != nil {
			return err
		}
		return adminStream(ctx, env, http.MethodGet, adminApps+app+"/pg/info", nil)
	case "backup":
		fs := newFlags("pg backup", env)
		kind := fs.String("type", "diff", "")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		app, err := oneApp(pos)
		if err != nil {
			return err
		}
		return adminStream(ctx, env, http.MethodPost, adminApps+app+"/pg/backup", map[string]string{"type": *kind})
	case "restore":
		fs := newFlags("pg restore", env)
		at := fs.String("time", "", "")
		into := fs.String("into", "", "")
		yes := fs.Bool("yes", false, "")
		pos, err := parse(fs, rest)
		if err != nil {
			return err
		}
		app, err := oneApp(pos)
		if err != nil {
			return err
		}
		if *into == "" && !*yes {
			return usagef("an in-place restore stops %s and replaces its database; pass --yes to confirm, or --into <name> for a drill", app)
		}
		return adminStream(ctx, env, http.MethodPost, adminApps+app+"/pg/restore", map[string]string{"time": *at, "into": *into})
	case "import":
		return cmdPGImport(ctx, rest, env)
	case "drill-rm":
		if len(rest) != 2 {
			return usagef("usage: zymple pg drill-rm <app> <name>")
		}
		app, err := appName(rest[0])
		if err != nil {
			return err
		}
		return adminClient(env).JSON(ctx, http.MethodDelete, adminApps+app+"/pg/drills/"+url.PathEscape(rest[1]), nil, nil)
	case "psql":
		app, err := oneApp(rest)
		if err != nil {
			return err
		}
		return env.Runner.Run(ctx, execx.Cmd{
			Name:  "docker",
			Args:  []string{"exec", "-it", services.PostgresContainer(app), "psql", "-U", "app", "app"},
			Stdin: env.Stdin, Stdout: env.Stdout, Stderr: env.Stderr,
		})
	}
	return usagef("unknown pg command %q", sub)
}

// cmdPGImport replaces an app's database with a pg_dump archive.
func cmdPGImport(ctx context.Context, args []string, env Env) error {
	fs := newFlags("pg import", env)
	yes := fs.Bool("yes", false, "")
	jobs := fs.Int("jobs", 0, "")
	noStart := fs.Bool("no-start", false, "")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usagef("usage: zymple pg import <app> <dump-file> --yes [--jobs n] [--no-start]")
	}
	app, err := appName(pos[0])
	if err != nil {
		return err
	}
	if !*yes {
		return usagef("an import stops %s and replaces its whole database with the dump; pass --yes to confirm", app)
	}
	// The daemon reads the file, so it needs a path that means the same there.
	file, err := filepath.Abs(pos[1])
	if err != nil {
		return err
	}
	body := map[string]any{"file": file, "jobs": *jobs, "no_start": *noStart}
	return adminStream(ctx, env, http.MethodPost, adminApps+app+"/pg/import", body)
}

func cmdRedis(ctx context.Context, args []string, env Env) error {
	const redisUsage = "usage: zymple redis backup <app> | redis restore <app> --yes"
	if len(args) < 2 || (args[0] != "backup" && args[0] != "restore") {
		return usagef(redisUsage)
	}
	fs := newFlags("redis "+args[0], env)
	yes := fs.Bool("yes", false, "")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef(redisUsage)
	}
	app, err := appName(pos[0])
	if err != nil {
		return err
	}
	if args[0] == "restore" && !*yes {
		return usagef("a restore stops the Redis of %s and replaces its data with the latest backup; pass --yes to confirm", app)
	}
	return adminStream(ctx, env, http.MethodPost, adminApps+app+"/redis/"+args[0], nil)
}

// DefaultEnv returns the environment of a real process.
func DefaultEnv(serve func(ctx context.Context, configPath string) error) Env {
	return Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv, Runner: execx.OS{}, Serve: serve}
}
