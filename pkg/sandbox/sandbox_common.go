package sandbox

import (
	"context"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	sandboxnet "github.com/jingkaihe/matchlock/pkg/net"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/jingkaihe/matchlock/pkg/vfs"
	"github.com/jingkaihe/matchlock/pkg/vm"
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

func execCommand(ctx context.Context, machine vm.Machine, config *api.Config, caPool *sandboxnet.CAPool, pol *policy.Engine, command string, opts *api.ExecOptions) (*api.ExecResult, error) {
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

	return machine.Exec(ctx, command, opts)
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
