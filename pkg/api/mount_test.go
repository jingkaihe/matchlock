package api

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseVolumeMountRelativeGuestPath(t *testing.T) {
	hostDir := t.TempDir()
	workspace := "/workspace"

	gotHost, gotGuest, readonly, err := ParseVolumeMount(hostDir+":subdir", workspace)
	if err != nil {
		t.Fatalf("ParseVolumeMount: %v", err)
	}

	if gotHost != hostDir {
		t.Errorf("host path = %q, want %q", gotHost, hostDir)
	}
	if gotGuest != "/workspace/subdir" {
		t.Errorf("guest path = %q, want %q", gotGuest, "/workspace/subdir")
	}
	if readonly {
		t.Errorf("readonly = true, want false")
	}
}

func TestParseVolumeMountAbsoluteGuestPathWithinWorkspace(t *testing.T) {
	hostDir := t.TempDir()
	workspace := "/workspace"

	_, gotGuest, _, err := ParseVolumeMount(hostDir+":/workspace/data", workspace)
	if err != nil {
		t.Fatalf("ParseVolumeMount: %v", err)
	}
	if gotGuest != "/workspace/data" {
		t.Errorf("guest path = %q, want %q", gotGuest, "/workspace/data")
	}
}

func TestParseVolumeMountAbsoluteGuestPathOutsideWorkspace(t *testing.T) {
	hostDir := t.TempDir()
	workspace := "/workspace/project"

	_, _, _, err := ParseVolumeMount(hostDir+":/workspace", workspace)
	if err == nil {
		t.Fatal("expected error for absolute guest path outside workspace")
	}
	if !strings.Contains(err.Error(), "must be within workspace") {
		t.Fatalf("error = %q, want to contain %q", err.Error(), "must be within workspace")
	}
}

func TestParseVolumeMountRelativePathCannotEscapeWorkspace(t *testing.T) {
	hostDir := t.TempDir()
	workspace := "/workspace/project"

	_, _, _, err := ParseVolumeMount(hostDir+":../escape", workspace)
	if err == nil {
		t.Fatal("expected error for relative guest path escaping workspace")
	}
	if !strings.Contains(err.Error(), "must be within workspace") {
		t.Fatalf("error = %q, want to contain %q", err.Error(), "must be within workspace")
	}
}

func TestParseVolumeMountWorkspacePrefixBoundary(t *testing.T) {
	hostDir := t.TempDir()
	workspace := "/workspace"

	_, _, _, err := ParseVolumeMount(hostDir+":/workspace2/data", workspace)
	if err == nil {
		t.Fatal("expected error for guest path outside workspace prefix boundary")
	}
	if !strings.Contains(err.Error(), "must be within workspace") {
		t.Fatalf("error = %q, want to contain %q", err.Error(), "must be within workspace")
	}
}

func TestParseVolumeMountWorkspaceRootAllowsAbsolutePaths(t *testing.T) {
	hostDir := t.TempDir()
	workspace := "/"

	_, gotGuest, _, err := ParseVolumeMount(hostDir+":/etc/data", workspace)
	if err != nil {
		t.Fatalf("ParseVolumeMount: %v", err)
	}
	if gotGuest != filepath.Clean("/etc/data") {
		t.Errorf("guest path = %q, want %q", gotGuest, filepath.Clean("/etc/data"))
	}
}

func TestValidateVFSMountsWithinWorkspaceAllowsDescendants(t *testing.T) {
	err := ValidateVFSMountsWithinWorkspace(
		map[string]MountConfig{
			"/workspace/project/data": {Type: "memory"},
			"/workspace/project/logs": {Type: "memory"},
		},
		"/workspace/project",
	)
	if err != nil {
		t.Fatalf("ValidateVFSMountsWithinWorkspace: %v", err)
	}
}

func TestValidateVFSMountsWithinWorkspaceRejectsOutside(t *testing.T) {
	err := ValidateVFSMountsWithinWorkspace(
		map[string]MountConfig{
			"/workspace": {Type: "memory"},
		},
		"/workspace/project",
	)
	if err == nil {
		t.Fatal("expected error for mount path outside workspace")
	}
	if !strings.Contains(err.Error(), "must be within workspace") {
		t.Fatalf("error = %q, want to contain %q", err.Error(), "must be within workspace")
	}
}

func TestValidateVFSMountsWithinWorkspaceRejectsRelative(t *testing.T) {
	err := ValidateVFSMountsWithinWorkspace(
		map[string]MountConfig{
			"project/data": {Type: "memory"},
		},
		"/workspace",
	)
	if err == nil {
		t.Fatal("expected error for relative mount path")
	}
	if !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("error = %q, want to contain %q", err.Error(), "must be absolute")
	}
}
