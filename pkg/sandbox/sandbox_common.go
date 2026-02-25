package sandbox

import (
	"bytes"
	"context"
	"io"
	"strings"
	"time"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
	sandboxnet "github.com/jingkaihe/matchlock/pkg/net"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/jingkaihe/matchlock/pkg/vfs"
	"github.com/jingkaihe/matchlock/pkg/vm"
	"github.com/jingkaihe/matchlock/pkg/vsock"
)

func buildVFSProviders(config *api.Config) map[string]vfs.Provider {
	vfsProviders := make(map[string]vfs.Provider)
	if config.VFS != nil && config.VFS.Mounts != nil {
		for path, mount := range config.VFS.Mounts {
			provider := createProvider(mount)
			vfsProviders[path] = provider
		}
	}
	return vfsProviders
}

func prepareExecEnv(config *api.Config, caPool *sandboxnet.CAPool, pol *policy.Engine) *api.ExecOptions {
	opts := &api.ExecOptions{
		// Matchlock defaults execution to image WORKDIR, falling back to the
		// configured workspace path when VFS is enabled.
		WorkingDir: config.GetWorkspace(),
		Env:        make(map[string]string),
	}

	if ic := config.ImageCfg; ic != nil {
		for k, v := range ic.Env {
			opts.Env[k] = v
		}
		if ic.WorkingDir != "" {
			opts.WorkingDir = ic.WorkingDir
		}
		if ic.User != "" {
			opts.User = ic.User
		}
	}
	for k, v := range config.Env {
		opts.Env[k] = v
	}

	if caPool != nil {
		certPath := "/etc/ssl/certs/matchlock-ca.crt"
		opts.Env["SSL_CERT_FILE"] = certPath
		opts.Env["REQUESTS_CA_BUNDLE"] = certPath
		opts.Env["CURL_CA_BUNDLE"] = certPath
		opts.Env["NODE_EXTRA_CA_CERTS"] = certPath
	}
	if pol != nil {
		for name, placeholder := range pol.GetPlaceholders() {
			opts.Env[name] = placeholder
		}
	}
	return opts
}

func prepareExecOptions(config *api.Config, caPool *sandboxnet.CAPool, pol *policy.Engine, opts *api.ExecOptions) *api.ExecOptions {
	if opts == nil {
		opts = &api.ExecOptions{}
	}
	if opts.Env == nil {
		opts.Env = make(map[string]string)
	}

	prepared := prepareExecEnv(config, caPool, pol)
	if opts.WorkingDir == "" {
		opts.WorkingDir = prepared.WorkingDir
	}
	if opts.User == "" {
		opts.User = prepared.User
	}
	for k, v := range prepared.Env {
		opts.Env[k] = v
	}

	return opts
}

func execCommand(ctx context.Context, machine vm.Machine, config *api.Config, caPool *sandboxnet.CAPool, pol *policy.Engine, command string, opts *api.ExecOptions) (*api.ExecResult, error) {
	opts = prepareExecOptions(config, caPool, pol, opts)
	return machine.Exec(ctx, command, opts)
}

func readFileTo(ctx context.Context, machine vm.Machine, path string, w io.Writer) (int64, error) {
	dialer, ok := machine.(vm.VsockDialer)
	if !ok {
		data, err := machine.ReadFile(ctx, path)
		if err != nil {
			return 0, err
		}
		return io.Copy(w, bytes.NewReader(data))
	}

	conn, err := dialer.DialVsock(vsock.ServicePortExec)
	if err != nil {
		return 0, err
	}

	counting := &countingWriter{w: w}
	var stderr bytes.Buffer
	quotedPath := shellSingleQuote(path)
	cmd := "if [ ! -f " + quotedPath + " ]; then echo 'read_file only supports regular files' >&2; exit 1; fi; cat < " + quotedPath
	result, err := vsock.ExecPipe(ctx, conn, cmd, &api.ExecOptions{
		Stdout: counting,
		Stderr: &stderr,
	})
	if err != nil {
		return counting.n, err
	}
	if result.ExitCode != 0 {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return counting.n, errx.With(vsock.ErrFileRemote, ": %s", msg)
		}
		return counting.n, errx.With(vsock.ErrFileRemote, ": exit code %d", result.ExitCode)
	}

	return counting.n, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func flushGuestDisks(machine vm.Machine) {
	if machine == nil {
		return
	}

	// Best-effort flush so raw disk mounts persist writes before VM stop.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = machine.Exec(ctx, "sync", &api.ExecOptions{})
}
