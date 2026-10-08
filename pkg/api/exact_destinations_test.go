package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateExactDestinationMount_AllowsWorkPaths(t *testing.T) {
	for _, p := range []string{
		"/opt/project",
		"/root/.tamandua/worktrees/sample-dev-target-tst-20260322",
		"/home/nietzsche/my-sample-repo",
		"/home/nietzsche/.tamandua/worktrees/sample-dev-target-tst-20260322",
		"/workspace/config/pi",
		"/workspace/runtime",
	} {
		err := ValidateExactDestinationMount(p)
		require.NoError(t, err, "expected %q to be a valid exact destination", p)
	}
}

func TestValidateExactDestinationMount_RejectsGuestOSRoots(t *testing.T) {
	for _, p := range []string{"/", "/etc", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/boot", "/dev", "/proc", "/sys", "/run"} {
		err := ValidateExactDestinationMount(p)
		require.Error(t, err, "expected %q to be rejected as a prohibited destination", p)
		assert.True(t, errors.Is(err, ErrProhibitedDestination), "want ErrProhibitedDestination for %q, got %v", p, err)
	}
}

func TestValidateExactDestinationMount_RejectsWithinGuestOSRoots(t *testing.T) {
	for _, p := range []string{"/etc/foo", "/usr/share/bar", "/dev/null"} {
		err := ValidateExactDestinationMount(p)
		require.Error(t, err, "expected %q to be rejected as a descendant of a prohibited root", p)
	}
}

func TestValidateExactDestinationMount_RejectsTrustedGuestRuntime(t *testing.T) {
	// /opt/matchlock is the trusted guest runtime where guest-init/agent/fused
	// are injected; a host mount over it or a subpath would corrupt it.
	for _, p := range []string{"/opt/matchlock", "/opt/matchlock/sub", "/opt/matchlock/guest-agent"} {
		err := ValidateExactDestinationMount(p)
		require.Error(t, err, "expected %q to be rejected as the trusted guest runtime", p)
		assert.True(t, errors.Is(err, ErrProhibitedDestination), "want ErrProhibitedDestination for %q, got %v", p, err)
	}
}

func TestValidateExactDestinationMount_AllowsSiblingsOfTrustedGuestRuntime(t *testing.T) {
	// /opt/matchlock must be reserved, but its siblings (and other /opt project
	// dirs) remain valid exact destinations.
	for _, p := range []string{"/opt/project", "/opt/matchlock2", "/opt/matchlock-testing"} {
		err := ValidateExactDestinationMount(p)
		require.NoError(t, err, "expected %q to remain a valid exact destination", p)
	}
}

func TestValidateExactDestinationMount_RejectsNonAbsolute(t *testing.T) {
	err := ValidateExactDestinationMount("relative/path")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrGuestPathNotAbs))
}

func TestValidateVFS_ExactDestinationsAllowsAbsoluteMounts(t *testing.T) {
	cfg := &Config{
		VFS: &VFSConfig{
			ExactDestinations: true,
			Mounts: map[string]MountConfig{
				"/opt/project":             {Type: MountTypeHostFS, HostPath: "/tmp/src"},
				"/root/.tamandua/worktree": {Type: MountTypeHostFS, HostPath: "/tmp/wt", Readonly: true},
				"/workspace/config":        {Type: MountTypeHostFS, HostPath: "/tmp/cfg"},
			},
		},
	}
	require.NoError(t, cfg.ValidateVFS())
}

func TestValidateVFS_ExactDestinationsRejectsGuestOSRootShadowing(t *testing.T) {
	cfg := &Config{
		VFS: &VFSConfig{
			ExactDestinations: true,
			Mounts: map[string]MountConfig{
				"/": {Type: MountTypeHostFS, HostPath: "/tmp/src"},
			},
		},
	}
	err := cfg.ValidateVFS()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prohibited")
}

func TestValidateVFS_ExactDestinationsRejectsWorkspaceShadowing(t *testing.T) {
	cfg := &Config{
		VFS: &VFSConfig{
			ExactDestinations: true,
			Workspace:         "/etc",
			Mounts: map[string]MountConfig{
				"/opt/project": {Type: MountTypeHostFS, HostPath: "/tmp/src"},
			},
		},
	}
	err := cfg.ValidateVFS()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prohibited")
}

func TestValidateVFS_ExactDestinationsOptionalWorkspace(t *testing.T) {
	// Workspace may be absent in exact-destination mode.
	cfg := &Config{
		VFS: &VFSConfig{
			ExactDestinations: true,
			Mounts: map[string]MountConfig{
				"/opt/project": {Type: MountTypeHostFS, HostPath: "/tmp/src"},
			},
		},
	}
	require.NoError(t, cfg.ValidateVFS())
}

func TestValidateExactDestinationMounts_RejectsNestedCollision(t *testing.T) {
	cfg := &Config{
		VFS: &VFSConfig{
			ExactDestinations: true,
			Mounts: map[string]MountConfig{
				"/opt/project":     {Type: MountTypeHostFS, HostPath: "/tmp/a"},
				"/opt/project/sub": {Type: MountTypeHostFS, HostPath: "/tmp/b"},
				"/root/worktree":   {Type: MountTypeHostFS, HostPath: "/tmp/c"},
			},
		},
	}
	err := cfg.ValidateVFS()
	require.Error(t, err, "nested destination must be rejected")
	assert.Contains(t, err.Error(), "/opt/project/sub")
	assert.Contains(t, err.Error(), "collides")
}

func TestValidateExactDestinationMounts_AllowsNonNestedPaths(t *testing.T) {
	cfg := &Config{
		VFS: &VFSConfig{
			ExactDestinations: true,
			Mounts: map[string]MountConfig{
				"/opt/project":             {Type: MountTypeHostFS, HostPath: "/tmp/a"},
				"/root/.tamandua/worktree": {Type: MountTypeHostFS, HostPath: "/tmp/b"},
				"/workspace/config":        {Type: MountTypeHostFS, HostPath: "/tmp/c"},
			},
		},
	}
	require.NoError(t, cfg.ValidateVFS(), "distinct non-nested destinations must be admitted")
}

func TestValidateExactDestinationMounts_RejectsSourceSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	require.NoError(t, os.Mkdir(target, 0755))
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(target, link))

	cfg := &Config{
		VFS: &VFSConfig{
			ExactDestinations: true,
			Mounts: map[string]MountConfig{
				"/opt/project": {Type: MountTypeHostFS, HostPath: link},
			},
		},
	}
	err := cfg.ValidateVFS()
	require.Error(t, err, "a symlink host source must be rejected")
	assert.Contains(t, err.Error(), "symlink")
}

func TestValidateExactDestinationMounts_AllowsRegularSourceDir(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		VFS: &VFSConfig{
			ExactDestinations: true,
			Mounts: map[string]MountConfig{
				"/opt/project": {Type: MountTypeHostFS, HostPath: dir},
			},
		},
	}
	require.NoError(t, cfg.ValidateVFS(), "a regular directory source must be admitted")
}

func TestValidateVFS_LegacyModeUnchanged(t *testing.T) {
	// A workspace-less mount set is still rejected unless exact destinations are
	// enabled, preserving the historical contract.
	cfg := &Config{
		VFS: &VFSConfig{
			Mounts: map[string]MountConfig{
				"/opt/project": {Type: MountTypeHostFS, HostPath: "/tmp/src"},
			},
		},
	}
	err := cfg.ValidateVFS()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "vfs.workspace is required")
}
