package sandbox

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readSwapPageZero(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	header := make([]byte, swapPageSize)
	n, err := f.Read(header)
	require.NoError(t, err)
	require.Equal(t, swapPageSize, n, "short read of swap header")
	return header
}

func TestCreateSwapImage_SizeAndHeader(t *testing.T) {
	for _, sizeMB := range []int64{1, 4, 512} {
		sizeMB := sizeMB
		t.Run(fmt.Sprintf("%dMB", sizeMB), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "swap.raw")
			sizeBytes := sizeMB * 1024 * 1024

			require.NoError(t, createSwapImage(path, sizeMB))

			info, err := os.Stat(path)
			require.NoError(t, err)
			assert.EqualValues(t, sizeBytes, info.Size(), "image must be exactly sizeMB*1MiB")

			header := readSwapPageZero(t, path)

			for i, b := range header[:1024] {
				require.Zero(t, b, "bootbits byte %d must be zero", i)
			}
			assert.EqualValues(t, 1, binary.LittleEndian.Uint32(header[1024:1028]), "version")
			assert.EqualValues(t, sizeBytes/swapPageSize-1, binary.LittleEndian.Uint32(header[1028:1032]), "last_page")
			assert.EqualValues(t, 0, binary.LittleEndian.Uint32(header[1032:1036]), "nr_badpages")

			assert.Equal(t, "SWAPSPACE2", string(header[swapPageSize-10:swapPageSize]), "magic at offset %d", swapPageSize-10)
			assert.Equal(t, byte('S'), header[4086], "magic must start at exactly offset 4086")
		})
	}
}

func TestCreateSwapImage_RejectsNonPositiveSize(t *testing.T) {
	for _, sizeMB := range []int64{0, -1, -4096} {
		sizeMB := sizeMB
		t.Run(fmt.Sprintf("%dMB", sizeMB), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "swap.raw")

			err := createSwapImage(path, sizeMB)

			require.Error(t, err)
			assert.ErrorIs(t, err, ErrCreateRootfs)
			_, statErr := os.Stat(path)
			assert.True(t, os.IsNotExist(statErr), "no partial swap image may be left behind")
		})
	}
}

// TestWriteSwapImage_RejectsNonPageAlignedSize covers the alignment guard
// directly: sizeMB is whole megabytes (always 4K-aligned), so the byte-level
// writer is the seam where a misaligned size can be observed and rejected.
func TestWriteSwapImage_RejectsNonPageAlignedSize(t *testing.T) {
	for _, sizeBytes := range []int64{1, swapPageSize - 1, swapPageSize + 1, 3 * swapPageSize / 2} {
		sizeBytes := sizeBytes
		t.Run(fmt.Sprintf("%dbytes", sizeBytes), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "swap.raw")

			err := writeSwapImage(path, sizeBytes)

			require.Error(t, err)
			assert.ErrorIs(t, err, ErrCreateRootfs)
			_, statErr := os.Stat(path)
			assert.True(t, os.IsNotExist(statErr), "no partial swap image may be left behind")
		})
	}
}

func TestWriteSwapImage_RejectsNonPositiveSize(t *testing.T) {
	for _, sizeBytes := range []int64{0, -swapPageSize} {
		sizeBytes := sizeBytes
		t.Run(fmt.Sprintf("%dbytes", sizeBytes), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "swap.raw")

			err := writeSwapImage(path, sizeBytes)

			require.Error(t, err)
			assert.ErrorIs(t, err, ErrCreateRootfs)
			_, statErr := os.Stat(path)
			assert.True(t, os.IsNotExist(statErr), "no partial swap image may be left behind")
		})
	}
}
