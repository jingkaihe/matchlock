package vfs

import (
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryProvider_Basic(t *testing.T) {
	mp := NewMemoryProvider()

	require.NoError(t, mp.Mkdir("/test", 0755))

	info, err := mp.Stat("/test")
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	h, err := mp.Create("/test/file.txt", 0644)
	require.NoError(t, err)

	content := []byte("hello world")
	n, err := h.Write(content)
	require.NoError(t, err)
	assert.Equal(t, len(content), n)
	h.Close()

	h, err = mp.Open("/test/file.txt", os.O_RDONLY, 0)
	require.NoError(t, err)
	defer h.Close()

	buf := make([]byte, 100)
	n, err = h.Read(buf)
	if err != nil && err != io.EOF {
		require.NoError(t, err, "Read failed")
	}
	assert.Equal(t, "hello world", string(buf[:n]))
}

func TestMemoryProvider_ReadDir(t *testing.T) {
	mp := NewMemoryProvider()

	mp.Mkdir("/dir", 0755)
	h1, _ := mp.Create("/dir/a.txt", 0644)
	h1.Close()
	h2, _ := mp.Create("/dir/b.txt", 0644)
	h2.Close()
	mp.Mkdir("/dir/subdir", 0755)

	entries, err := mp.ReadDir("/dir")
	require.NoError(t, err)
	require.Len(t, entries, 3)

	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name()] = true
	}

	assert.True(t, names["a.txt"], "missing a.txt")
	assert.True(t, names["b.txt"], "missing b.txt")
	assert.True(t, names["subdir"], "missing subdir")
}

func TestMemoryProvider_Remove(t *testing.T) {
	mp := NewMemoryProvider()

	h, _ := mp.Create("/file.txt", 0644)
	h.Write([]byte("test"))
	h.Close()

	require.NoError(t, mp.Remove("/file.txt"))

	_, err := mp.Stat("/file.txt")
	require.Error(t, err)
}

func TestMemoryProvider_Rename(t *testing.T) {
	mp := NewMemoryProvider()

	mp.Mkdir("/src", 0755)
	h, _ := mp.Create("/src/file.txt", 0644)
	h.Write([]byte("content"))
	h.Close()

	mp.Mkdir("/dst", 0755)

	require.NoError(t, mp.Rename("/src/file.txt", "/dst/moved.txt"))

	_, err := mp.Stat("/src/file.txt")
	require.Error(t, err, "Old path should not exist")

	info, err := mp.Stat("/dst/moved.txt")
	require.NoError(t, err, "New path should exist")
	assert.Equal(t, int64(7), info.Size())
}

func TestMemoryProvider_Readonly(t *testing.T) {
	mp := NewMemoryProvider()
	assert.False(t, mp.Readonly())
}

func TestMemoryProvider_WriteFile_ReadFile(t *testing.T) {
	mp := NewMemoryProvider()

	content := []byte("file content")
	require.NoError(t, mp.WriteFile("/test.txt", content, 0644))

	read, err := mp.ReadFile("/test.txt")
	require.NoError(t, err)
	assert.Equal(t, string(content), string(read))
}

func TestMemoryProvider_MkdirAll(t *testing.T) {
	mp := NewMemoryProvider()

	require.NoError(t, mp.MkdirAll("/a/b/c/d", 0755))

	for _, path := range []string{"/a", "/a/b", "/a/b/c", "/a/b/c/d"} {
		info, err := mp.Stat(path)
		require.NoError(t, err, "Path %s should exist", path)
		assert.True(t, info.IsDir(), "Path %s should be directory", path)
	}
}

func TestMemoryProvider_Seek(t *testing.T) {
	mp := NewMemoryProvider()

	h, _ := mp.Create("/seek.txt", 0644)
	h.Write([]byte("0123456789"))
	h.Close()

	h, _ = mp.Open("/seek.txt", os.O_RDONLY, 0)
	defer h.Close()

	pos, err := h.Seek(5, io.SeekStart)
	require.NoError(t, err)
	assert.Equal(t, int64(5), pos)

	buf := make([]byte, 3)
	n, _ := h.Read(buf)
	assert.Equal(t, "567", string(buf[:n]))
}

func TestMemoryProvider_Truncate(t *testing.T) {
	mp := NewMemoryProvider()

	h, _ := mp.Create("/trunc.txt", 0644)
	h.Write([]byte("0123456789"))

	require.NoError(t, h.Truncate(5))

	info, _ := h.Stat()
	assert.Equal(t, int64(5), info.Size())
	h.Close()

	content, _ := mp.ReadFile("/trunc.txt")
	assert.Equal(t, "01234", string(content))
}
