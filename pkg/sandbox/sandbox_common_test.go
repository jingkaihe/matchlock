package sandbox

import (
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/vfs"
)

func TestBuildVFSProvidersAddsWorkspaceRootForNestedMounts(t *testing.T) {
	workspace := "/workspace"
	config := &api.Config{
		VFS: &api.VFSConfig{
			Mounts: map[string]api.MountConfig{
				"/workspace/not_exist_folder": {Type: "memory"},
			},
		},
	}

	providers := buildVFSProviders(config, workspace)
	if _, ok := providers[workspace]; !ok {
		t.Fatalf("expected workspace mount %q to exist", workspace)
	}
	if _, ok := providers["/workspace/not_exist_folder"]; !ok {
		t.Fatal("expected nested mount to exist")
	}

	router := vfs.NewMountRouter(providers)
	if _, err := router.Stat(workspace); err != nil {
		t.Fatalf("expected workspace root to resolve, got error: %v", err)
	}
}

func TestBuildVFSProvidersKeepsExplicitWorkspaceMount(t *testing.T) {
	workspace := "/workspace"
	config := &api.Config{
		VFS: &api.VFSConfig{
			Mounts: map[string]api.MountConfig{
				workspace: {Type: "memory"},
			},
		},
	}

	providers := buildVFSProviders(config, workspace)
	if got := len(providers); got != 1 {
		t.Fatalf("expected exactly one mount provider, got %d", got)
	}
}
