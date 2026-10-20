package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExactFUSEMountpoints_LegacyModeNil(t *testing.T) {
	// Legacy mode (exact_destinations=false) must not request any extra mounts.
	cfg := &api.Config{VFS: &api.VFSConfig{
		Workspace: "/workspace",
		Mounts: map[string]api.MountConfig{
			"/workspace/repo": {Type: api.MountTypeHostFS, HostPath: t.TempDir()},
		},
	}}
	assert.Nil(t, exactFUSEMountpoints(cfg))
}

func TestExactFUSEMountpoints_DirectoryDestinations(t *testing.T) {
	repo := t.TempDir()
	worktree := t.TempDir()
	cfg := &api.Config{VFS: &api.VFSConfig{
		ExactDestinations: true,
		Mounts: map[string]api.MountConfig{
			"/opt/project":   {Type: api.MountTypeHostFS, HostPath: repo},
			"/home/u/wt":     {Type: api.MountTypeHostFS, HostPath: worktree},
			"/workspace/cfg": {Type: api.MountTypeHostFS, HostPath: t.TempDir()},
		},
	}}

	got := exactFUSEMountpoints(cfg)
	require.ElementsMatch(t, []string{"/opt/project", "/home/u/wt", "/workspace/cfg"}, got)
}

func TestExactFUSEMountpoints_WorkspaceAndNestedDeduped(t *testing.T) {
	cfg := &api.Config{VFS: &api.VFSConfig{
		ExactDestinations: true,
		Workspace:         "/workspace",
		Mounts: map[string]api.MountConfig{
			// Single-file destination under the workspace must NOT spawn its own
			// FUSE mount; it is served by the workspace tree.
			"/workspace/runs/1/progress.txt": {Type: api.MountTypeHostFS, HostPath: writeFile(t)},
			// A directory destination nested beneath the workspace is collapsed.
			"/workspace/runs": {Type: api.MountTypeHostFS, HostPath: t.TempDir()},
			// A genuine exact directory destination outside workspace.
			"/opt/project": {Type: api.MountTypeHostFS, HostPath: t.TempDir()},
		},
	}}

	got := exactFUSEMountpoints(cfg)
	require.ElementsMatch(t, []string{"/workspace", "/opt/project"}, got)
}

func TestSelectNonNestedMountpoints_CollapsesAndDedupes(t *testing.T) {
	got := selectNonNestedMountpoints([]string{
		"/opt/project",
		"/opt/project/sub", // nested, dropped
		"/opt",             // is a prefix (ancestor) of /opt/project, kept as shallower
		"/workspace",
		"/workspace", // duplicate, deduped
		"/root/.tamandua/wt",
		"/",             // guest root, dropped
		"relative/path", // non-absolute, dropped
	})
	require.ElementsMatch(t, []string{"/opt", "/workspace", "/root/.tamandua/wt"}, got)
}

func writeFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "progress.txt")
	require.NoError(t, os.WriteFile(p, []byte("x"), 0644))
	return p
}
