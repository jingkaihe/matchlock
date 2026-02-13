package vfs

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadonlyProvider_Read(t *testing.T) {
	base := NewMemoryProvider()
	h, _ := base.Create("/file.txt", 0644)
	h.Write([]byte("content"))
	h.Close()

	ro := NewReadonlyProvider(base)

	rh, err := ro.Open("/file.txt", os.O_RDONLY, 0)
	require.NoError(t, err)
	defer rh.Close()

	buf := make([]byte, 100)
	n, _ := rh.Read(buf)
	assert.Equal(t, "content", string(buf[:n]))
}

func TestReadonlyProvider_WriteBlocked(t *testing.T) {
	base := NewMemoryProvider()
	ro := NewReadonlyProvider(base)

	_, err := ro.Create("/newfile.txt", 0644)
	require.Error(t, err, "Create should fail on readonly provider")

	err = ro.Mkdir("/newdir", 0755)
	require.Error(t, err, "Mkdir should fail on readonly provider")

	err = ro.Remove("/anything")
	require.Error(t, err, "Remove should fail on readonly provider")

	err = ro.Rename("/old", "/new")
	require.Error(t, err, "Rename should fail on readonly provider")
}

func TestReadonlyProvider_Readonly(t *testing.T) {
	base := NewMemoryProvider()
	ro := NewReadonlyProvider(base)

	assert.True(t, ro.Readonly())
}

func TestReadonlyProvider_Stat(t *testing.T) {
	base := NewMemoryProvider()
	base.Mkdir("/dir", 0755)

	ro := NewReadonlyProvider(base)

	info, err := ro.Stat("/dir")
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestReadonlyProvider_ReadDir(t *testing.T) {
	base := NewMemoryProvider()
	base.Mkdir("/dir", 0755)
	h, _ := base.Create("/dir/a.txt", 0644)
	h.Close()

	ro := NewReadonlyProvider(base)

	entries, err := ro.ReadDir("/dir")
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}
