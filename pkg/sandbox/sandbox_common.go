package sandbox

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/jingkaihe/matchlock/pkg/api"
	sandboxnet "github.com/jingkaihe/matchlock/pkg/net"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/jingkaihe/matchlock/pkg/vfs"
	"github.com/jingkaihe/matchlock/pkg/vm"
)

func buildVFSProviders(config *api.Config, workspace string) map[string]vfs.Provider {
	vfsProviders := make(map[string]vfs.Provider)
	if config.VFS != nil && config.VFS.Mounts != nil {
		for path, mount := range config.VFS.Mounts {
			provider := createProvider(mount)
			if provider != nil {
				vfsProviders[path] = provider
			}
		}
	}

	cleanWorkspace := filepath.Clean(workspace)
	hasWorkspaceMount := false
	for path := range vfsProviders {
		if filepath.Clean(path) == cleanWorkspace {
			hasWorkspaceMount = true
			break
		}
	}

	// Keep a workspace root mount even when only nested mounts are configured.
	// Without this, guest-fused root lookups on /workspace fail with ENOENT.
	if !hasWorkspaceMount {
		vfsProviders[cleanWorkspace] = vfs.NewMemoryProvider()
	}

	return vfsProviders
}

func prepareExecEnv(config *api.Config, caPool *sandboxnet.CAPool, pol *policy.Engine) *api.ExecOptions {
	opts := &api.ExecOptions{
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

func writeFile(vfsRoot *vfs.MountRouter, path string, content []byte, mode uint32) error {
	if mode == 0 {
		mode = 0644
	}
	h, err := vfsRoot.Create(path, os.FileMode(mode))
	if err != nil {
		return err
	}
	defer h.Close()
	_, err = h.Write(content)
	return err
}

func readFile(vfsRoot *vfs.MountRouter, path string) ([]byte, error) {
	h, err := vfsRoot.Open(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer h.Close()

	info, err := vfsRoot.Stat(path)
	if err != nil {
		return nil, err
	}

	content := make([]byte, info.Size())
	_, err = h.Read(content)
	if err != nil {
		return nil, err
	}
	return content, nil
}

func readFileTo(vfsRoot *vfs.MountRouter, path string, w io.Writer) (int64, error) {
	h, err := vfsRoot.Open(path, os.O_RDONLY, 0)
	if err != nil {
		return 0, err
	}
	defer h.Close()
	return io.Copy(w, h)
}

func listFiles(vfsRoot *vfs.MountRouter, path string) ([]api.FileInfo, error) {
	entries, err := vfsRoot.ReadDir(path)
	if err != nil {
		return nil, err
	}

	result := make([]api.FileInfo, len(entries))
	for i, e := range entries {
		info, _ := e.Info()
		result[i] = api.FileInfo{
			Name:  e.Name(),
			Size:  info.Size(),
			Mode:  uint32(info.Mode()),
			IsDir: e.IsDir(),
		}
	}
	return result, nil
}
