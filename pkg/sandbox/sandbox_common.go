package sandbox

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
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

// createProvider builds a VFS Provider for the given mount config.
// Returns ErrInvalidMountConfig if owner_uid or owner_gid are set on a
// non-host_fs mount type, since ownership overrides are only applied by the
// RealFSProvider and have no effect on memory or overlay mounts.
func createProvider(mount api.MountConfig) (vfs.Provider, error) {
	if (mount.OwnerUID != nil || mount.OwnerGID != nil) && mount.Type != api.MountTypeHostFS {
		return nil, errx.With(ErrInvalidMountConfig, ": owner_uid/owner_gid are only supported for host_fs mounts, got %q", mount.Type)
	}

	switch mount.Type {
	case api.MountTypeMemory:
		return vfs.NewMemoryProvider(), nil
	case api.MountTypeHostFS:
		p := vfs.NewRealFSProvider(mount.HostPath)
		if mount.OwnerUID != nil || mount.OwnerGID != nil {
			uid := uint32(0)
			gid := uint32(0)
			if mount.OwnerUID != nil {
				uid = *mount.OwnerUID
			}
			if mount.OwnerGID != nil {
				gid = *mount.OwnerGID
			}
			p = p.WithOwner(uid, gid)
		}
		if mount.Readonly {
			return vfs.NewReadonlyProvider(p), nil
		}
		return p, nil
	default:
		return vfs.NewMemoryProvider(), nil
	}
}

func buildVFSProviders(config *api.Config) (map[string]vfs.Provider, error) {
	vfsProviders := make(map[string]vfs.Provider)
	if config.VFS != nil && config.VFS.Mounts != nil {
		for path, mount := range config.VFS.Mounts {
			provider, err := createProvider(mount)
			if err != nil {
				return nil, err
			}
			vfsProviders[path] = provider
		}
	}
	return vfsProviders, nil
}

// exactFUSEMountpoints returns the guest directory paths that need their own
// FUSE mount so exact absolute work/repository paths are physically reachable in
// the guest. In legacy (non-exact) mode it returns nil: the single workspace
// FUSE mount already serves every mount beneath it, so no extra mount is needed.
//
// In exact mode it returns the workspace (if set) plus every exact destination
// whose guest path is a directory and is not nested beneath the workspace or
// another exact destination. A single-file destination that lives under the
// workspace (for example the run progress file) is served by the workspace tree;
// a single-file destination outside the workspace uses its parent directory as
// the mountpoint. Nested mountpoints are collapsed to the shallowest so a FUSE
// tree is never mounted on top of another FUSE tree (which would not work).
func exactFUSEMountpoints(config *api.Config) []string {
	if config == nil || config.VFS == nil || !config.VFS.ExactDestinations {
		return nil
	}
	workspace := config.VFS.Workspace
	candidates := make([]string, 0, len(config.VFS.Mounts)+1)
	if workspace != "" {
		candidates = append(candidates, workspace)
	}
	for guestPath, mount := range config.VFS.Mounts {
		if mount.Type == api.MountTypeHostFS && mount.HostPath != "" {
			if fi, err := os.Stat(mount.HostPath); err == nil && !fi.IsDir() {
				// A single-file destination is not a FUSE mountpoint itself; mount
				// its parent directory so the file is a child of the served tree.
				candidates = append(candidates, filepath.Dir(guestPath))
				continue
			}
		}
		candidates = append(candidates, guestPath)
	}
	return selectNonNestedMountpoints(candidates)
}

// selectNonNestedMountpoints keeps only the shallowest absolute guest paths so
// no mountpoint is nested inside another, deduplicates exact repeats, and drops
// any non-absolute or malformed entry.
func selectNonNestedMountpoints(candidates []string) []string {
	cleaned := make([]string, 0, len(candidates))
	for _, c := range candidates {
		cc := filepath.Clean(c)
		if !filepath.IsAbs(cc) {
			continue
		}
		if cc == "/" {
			// Never mount a FUSE tree over the guest root.
			continue
		}
		cleaned = append(cleaned, cc)
	}
	sort.Slice(cleaned, func(i, j int) bool {
		return len(cleaned[i]) < len(cleaned[j])
	})
	var result []string
	seen := make(map[string]bool)
	for _, c := range cleaned {
		if seen[c] {
			continue
		}
		nested := false
		for _, existing := range result {
			if existing == c || strings.HasPrefix(c, existing+"/") {
				nested = true
				break
			}
		}
		if nested {
			continue
		}
		seen[c] = true
		result = append(result, c)
	}
	return result
}

func buildExtraDiskConfigs(disks []api.DiskMount) ([]vm.DiskConfig, error) {
	extraDisks := make([]vm.DiskConfig, 0, len(disks))
	for _, d := range disks {
		if err := api.ValidateGuestMount(d.GuestMount); err != nil {
			return nil, errx.Wrap(ErrInvalidDiskCfg, err)
		}
		if d.ReadOnly && (d.OwnerUID != nil || d.OwnerGID != nil) {
			return nil, errx.With(ErrInvalidDiskCfg, ": ownership options require writable disk")
		}
		extraDisks = append(extraDisks, vm.DiskConfig{
			HostPath:   d.HostPath,
			GuestMount: d.GuestMount,
			ReadOnly:   d.ReadOnly,
			OwnerUID:   d.OwnerUID,
			OwnerGID:   d.OwnerGID,
		})
	}
	return extraDisks, nil
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
