package vfs

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOverlayProvider_ReadFromLower(t *testing.T) {
	lower := NewMemoryProvider()
	upper := NewMemoryProvider()

	h, _ := lower.Create("/file.txt", 0644)
	h.Write([]byte("lower content"))
	h.Close()

	overlay := NewOverlayProvider(upper, lower)

	h, err := overlay.Open("/file.txt", os.O_RDONLY, 0)
	require.NoError(t, err)
	defer h.Close()

	buf := make([]byte, 100)
	n, _ := h.Read(buf)
	assert.Equal(t, "lower content", string(buf[:n]))
}

func TestOverlayProvider_WriteToUpper(t *testing.T) {
	lower := NewMemoryProvider()
	upper := NewMemoryProvider()

	overlay := NewOverlayProvider(upper, lower)

	h, err := overlay.Create("/new.txt", 0644)
	require.NoError(t, err)
	h.Write([]byte("upper content"))
	h.Close()

	_, err = upper.Stat("/new.txt")
	require.NoError(t, err, "File should exist in upper layer")

	_, err = lower.Stat("/new.txt")
	require.Error(t, err, "File should not exist in lower layer")
}

func TestOverlayProvider_UpperShadowsLower(t *testing.T) {
	lower := NewMemoryProvider()
	upper := NewMemoryProvider()

	h1, _ := lower.Create("/shadow.txt", 0644)
	h1.Write([]byte("lower"))
	h1.Close()

	h2, _ := upper.Create("/shadow.txt", 0644)
	h2.Write([]byte("upper"))
	h2.Close()

	overlay := NewOverlayProvider(upper, lower)

	h, _ := overlay.Open("/shadow.txt", os.O_RDONLY, 0)
	defer h.Close()

	buf := make([]byte, 100)
	n, _ := h.Read(buf)
	assert.Equal(t, "upper", string(buf[:n]))
}

func TestOverlayProvider_ReadDirMerged(t *testing.T) {
	lower := NewMemoryProvider()
	upper := NewMemoryProvider()

	lower.Mkdir("/dir", 0755)
	h1, _ := lower.Create("/dir/lower.txt", 0644)
	h1.Close()

	upper.Mkdir("/dir", 0755)
	h2, _ := upper.Create("/dir/upper.txt", 0644)
	h2.Close()

	overlay := NewOverlayProvider(upper, lower)

	entries, err := overlay.ReadDir("/dir")
	require.NoError(t, err)

	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name()] = true
	}

	assert.True(t, names["lower.txt"], "should see lower.txt")
	assert.True(t, names["upper.txt"], "should see upper.txt")
}

func TestOverlayProvider_Mkdir(t *testing.T) {
	lower := NewMemoryProvider()
	upper := NewMemoryProvider()

	overlay := NewOverlayProvider(upper, lower)

	require.NoError(t, overlay.Mkdir("/newdir", 0755))

	_, err := upper.Stat("/newdir")
	require.NoError(t, err, "Directory should exist in upper layer")
}

func TestOverlayProvider_NotReadonly(t *testing.T) {
	lower := NewMemoryProvider()
	upper := NewMemoryProvider()
	overlay := NewOverlayProvider(upper, lower)

	assert.False(t, overlay.Readonly())
}
