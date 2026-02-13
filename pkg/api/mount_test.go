package api

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseVolumeMountRelativeGuestPath(t *testing.T) {
	hostDir := t.TempDir()
	workspace := "/workspace"

	gotHost, gotGuest, readonly, err := ParseVolumeMount(hostDir+":subdir", workspace)
	require.NoError(t, err)

	assert.Equal(t, hostDir, gotHost, "host path")
	assert.Equal(t, "/workspace/subdir", gotGuest, "guest path")
	assert.False(t, readonly, "readonly")
}

func TestParseVolumeMountAbsoluteGuestPathWithinWorkspace(t *testing.T) {
	hostDir := t.TempDir()
	workspace := "/workspace"

	_, gotGuest, _, err := ParseVolumeMount(hostDir+":/workspace/data", workspace)
	require.NoError(t, err)
	assert.Equal(t, "/workspace/data", gotGuest, "guest path")
}

func TestParseVolumeMountAbsoluteGuestPathOutsideWorkspace(t *testing.T) {
	hostDir := t.TempDir()
	workspace := "/workspace/project"

	_, _, _, err := ParseVolumeMount(hostDir+":/workspace", workspace)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be within workspace")
}

func TestParseVolumeMountRelativePathCannotEscapeWorkspace(t *testing.T) {
	hostDir := t.TempDir()
	workspace := "/workspace/project"

	_, _, _, err := ParseVolumeMount(hostDir+":../escape", workspace)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be within workspace")
}

func TestParseVolumeMountWorkspacePrefixBoundary(t *testing.T) {
	hostDir := t.TempDir()
	workspace := "/workspace"

	_, _, _, err := ParseVolumeMount(hostDir+":/workspace2/data", workspace)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be within workspace")
}

func TestParseVolumeMountWorkspaceRootAllowsAbsolutePaths(t *testing.T) {
	hostDir := t.TempDir()
	workspace := "/"

	_, gotGuest, _, err := ParseVolumeMount(hostDir+":/etc/data", workspace)
	require.NoError(t, err)
	assert.Equal(t, filepath.Clean("/etc/data"), gotGuest, "guest path")
}

func TestValidateVFSMountsWithinWorkspaceAllowsDescendants(t *testing.T) {
	err := ValidateVFSMountsWithinWorkspace(
		map[string]MountConfig{
			"/workspace/project/data": {Type: "memory"},
			"/workspace/project/logs": {Type: "memory"},
		},
		"/workspace/project",
	)
	require.NoError(t, err)
}

func TestValidateVFSMountsWithinWorkspaceRejectsOutside(t *testing.T) {
	err := ValidateVFSMountsWithinWorkspace(
		map[string]MountConfig{
			"/workspace": {Type: "memory"},
		},
		"/workspace/project",
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be within workspace")
}

func TestValidateVFSMountsWithinWorkspaceRejectsRelative(t *testing.T) {
	err := ValidateVFSMountsWithinWorkspace(
		map[string]MountConfig{
			"project/data": {Type: "memory"},
		},
		"/workspace",
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be absolute")
}
