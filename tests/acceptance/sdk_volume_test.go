//go:build acceptance

package acceptance

import (
	"errors"
	"strings"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSDKVolumeCreateListRemove(t *testing.T) {
	requireVolumeFormatTool(t)

	client, err := sdk.NewClient(matchlockConfig(t))
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() {
		_ = client.Close(0)
	})

	volumeName := uniqueVolumeName("acc-sdk-volume")
	t.Cleanup(func() {
		_ = client.VolumeRemove(volumeName)
	})

	info, err := client.VolumeCreate(volumeName, 16)
	require.NoError(t, err, "VolumeCreate")
	require.NotNil(t, info)
	assert.Equal(t, volumeName, info.Name)
	assert.Equal(t, "16.0 MB", info.Size)
	assert.NotEmpty(t, strings.TrimSpace(info.Path))

	volumes, err := client.VolumeList()
	require.NoError(t, err, "VolumeList")

	created, ok := findVolumeByName(volumes, volumeName)
	require.Truef(t, ok, "volume %q should exist in list: %#v", volumeName, volumes)
	assert.Equal(t, info.Size, created.Size)
	assert.Equal(t, info.Path, created.Path)

	err = client.VolumeRemove(volumeName)
	require.NoError(t, err, "VolumeRemove")

	volumes, err = client.VolumeList()
	require.NoError(t, err, "VolumeList after remove")

	_, ok = findVolumeByName(volumes, volumeName)
	assert.Falsef(t, ok, "volume %q should not exist in list: %#v", volumeName, volumes)
}

func TestSDKVolumeRemoveMissing(t *testing.T) {
	client, err := sdk.NewClient(matchlockConfig(t))
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() {
		_ = client.Close(0)
	})

	volumeName := uniqueVolumeName("acc-sdk-missing")
	err = client.VolumeRemove(volumeName)
	require.Error(t, err)
	assert.True(t, errors.Is(err, sdk.ErrVolumeCommand))
	assert.Contains(t, err.Error(), "named volume not found")
}

func findVolumeByName(volumes []sdk.VolumeInfo, name string) (sdk.VolumeInfo, bool) {
	for _, volume := range volumes {
		if volume.Name == name {
			return volume, true
		}
	}
	return sdk.VolumeInfo{}, false
}
