package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gokaykucuk/zymple/internal/appdef"
	"github.com/gokaykucuk/zymple/internal/config"
	"github.com/gokaykucuk/zymple/internal/execx"
)

// RedisURL is the connection string injected into apps that declare Redis.
const RedisURL = "redis://redis:6379/0"

// Redis manages one Redis instance per app.
type Redis struct {
	Runner execx.Runner
	Config *config.Config
}

// RedisContainer is the name of an app's Redis container.
func RedisContainer(app string) string { return "zymple-" + app + "-redis" }

func (r *Redis) dir(app string) string {
	return filepath.Join(r.Config.StateDir, "apps", app, "redis")
}

// Ensure makes the app's Redis container run with the wanted settings.
func (r *Redis) Ensure(ctx context.Context, app *appdef.App) (string, error) {
	name := RedisContainer(app.Name)
	image := r.Config.RedisImage
	mode := "cache"
	if app.Redis.Persistent {
		mode = "persistent"
	}
	want := configHash(image, mode)
	state, have := inspect(ctx, r.Runner, name)
	if state == "running" && have == want {
		return RedisURL, nil
	}
	if err := replace(ctx, r.Runner, name, state, redisStopSeconds); err != nil {
		return "", err
	}
	args := []string{"run", "-d", "--name", name, "--restart", "unless-stopped",
		"--network", appdef.Network(app.Name), "--network-alias", "redis",
		flagLabel, appdef.LabelApp + "=" + app.Name, flagLabel, appdef.LabelRole + "=" + roleRedis,
		flagLabel, labelConfig + "=" + want,
		"--security-opt", "no-new-privileges:true"}
	if app.Redis.Persistent {
		if err := os.MkdirAll(r.dir(app.Name), 0o750); err != nil {
			return "", err
		}
		args = append(args, "-v", r.dir(app.Name)+":/data", image, "redis-server", "--appendonly", "yes")
	} else {
		args = append(args, image, "redis-server", "--save", "", "--appendonly", "no")
	}
	if _, err := execx.Output(ctx, r.Runner, "docker", args...); err != nil {
		return "", err
	}
	return RedisURL, nil
}

// restic builds a restic invocation: the docker arguments, and the docker
// CLI environment that carries the secrets named by the `-e NAME` flags.
func (r *Redis) restic(app string, readOnly bool, args ...string) (argv, env []string, err error) {
	b := r.Config.Backup
	if b == nil {
		return nil, nil, errors.New("no backup repository is configured")
	}
	if b.CipherPass == "" {
		return nil, nil, errors.New("backup.cipher_pass is required for Redis backups")
	}
	data := r.dir(app) + ":/data"
	if readOnly {
		data += ":ro"
	}
	secrets := []string{"RESTIC_PASSWORD=" + b.CipherPass}
	run := []string{"run", "--rm", "-v", data, "-e", "RESTIC_PASSWORD", "--hostname", "zymple-" + app}
	if b.Type == "posix" {
		repo := filepath.Join(b.Path, app, "redis")
		if err := os.MkdirAll(repo, 0o750); err != nil {
			return nil, nil, err
		}
		run = append(run, "-v", repo+":/repo", "-e", "RESTIC_REPOSITORY=/repo")
	} else {
		repo := "s3:https://" + b.S3.Endpoint + "/" + b.S3.Bucket + filepath.Join("/", b.Path, app, "redis")
		secrets = append(secrets, "AWS_ACCESS_KEY_ID="+b.S3.AccessKey, "AWS_SECRET_ACCESS_KEY="+b.S3.SecretKey)
		run = append(run, "-e", "RESTIC_REPOSITORY="+repo,
			"-e", "AWS_ACCESS_KEY_ID", "-e", "AWS_SECRET_ACCESS_KEY",
			"-e", "AWS_DEFAULT_REGION="+b.S3.Region)
	}
	run = append(run, r.Config.ResticImage)
	return append(run, args...), secretEnv(secrets...), nil
}

// Backup snapshots a persistent Redis data directory into the repository.
func (r *Redis) Backup(ctx context.Context, app *appdef.App, out io.Writer) error {
	if app.Redis == nil || !app.Redis.Persistent {
		return errors.New("this app has no persistent Redis")
	}
	check, env, err := r.restic(app.Name, true, "cat", "config")
	if err != nil {
		return err
	}
	if _, err := dockerOutput(ctx, r.Runner, env, nil, check...); err != nil {
		init, _, _ := r.restic(app.Name, true, "init")
		if err := dockerStream(ctx, r.Runner, out, env, init...); err != nil {
			return err
		}
	}
	backup, _, _ := r.restic(app.Name, true, "backup", "/data")
	return dockerStream(ctx, r.Runner, out, env, backup...)
}

// Restore replaces a persistent Redis data directory with the latest
// snapshot. Redis is stopped for the duration and started again whatever the
// outcome: a failed restore must not also leave the app without Redis.
func (r *Redis) Restore(ctx context.Context, app *appdef.App, out io.Writer) error {
	if app.Redis == nil || !app.Redis.Persistent {
		return errors.New("this app has no persistent Redis")
	}
	// restic refuses "--target / --delete" without a filter, because it would
	// delete everything else under the target. The snapshot holds /data only.
	restore, env, err := r.restic(app.Name, false, "restore", "latest", "--target", "/", "--delete", "--include", "/data")
	if err != nil {
		return err
	}
	name := RedisContainer(app.Name)
	if _, err := execx.Output(ctx, r.Runner, "docker", "stop", "--time", redisStopSeconds, name); err != nil {
		return err
	}
	restoreErr := dockerStream(ctx, r.Runner, out, env, restore...)
	if restoreErr != nil {
		fmt.Fprintf(out, "restore failed; starting Redis again on the data that is there\n")
	}
	_, startErr := execx.Output(context.WithoutCancel(ctx), r.Runner, "docker", "start", name)
	return errors.Join(restoreErr, startErr)
}
