package kernel

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fragmentDir is relative to this package directory (pkg/kernel), matching the
// repo convention used by other package tests (e.g. cmd/matchlock reads
// ../../README.md).
const fragmentDir = "../../guest/kernel"

var (
	swapEnabledLine   = regexp.MustCompile(`(?m)^CONFIG_SWAP=y$`)
	arm64Page4KLine   = regexp.MustCompile(`(?m)^CONFIG_ARM64_4K_PAGES=y$`)
	arm64PageOtherOn  = regexp.MustCompile(`(?m)^CONFIG_ARM64_(16K|64K)_PAGES=y$`)
	swapDisabledLines = []string{
		"CONFIG_SWAP=n",
		"# CONFIG_SWAP is not set",
	}
)

// readFragment loads a vendored kernel config fragment and fails the test with
// a path-aware message when it is missing.
func readFragment(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join(fragmentDir, name)
	data, err := os.ReadFile(path)
	require.NoErrorf(t, err, "read kernel fragment %s (resolved from %s)", name, path)
	return string(data)
}

// TestKernelFragmentsEnableSwap locks the CONFIG_SWAP=y pin for every vendored
// guest kernel fragment. Without it the swap feature depends on olddefconfig
// inference instead of an explicit build input.
func TestKernelFragmentsEnableSwap(t *testing.T) {
	fragments := []string{"x86_64.config", "arm64.config", "qemu-x86_64.config"}

	for _, name := range fragments {
		t.Run(name, func(t *testing.T) {
			content := readFragment(t, name)

			assert.Regexpf(t, swapEnabledLine, content, "%s must pin CONFIG_SWAP=y", name)
			for _, disabled := range swapDisabledLines {
				assert.NotContainsf(t, content, disabled, "%s must not disable swap via %q", name, disabled)
			}
		})
	}
}

// TestKernelFragmentArm64Pins4KPages locks the arm64 page-size pin that keeps
// createSwapImage's 4096-byte header layout valid. A 16K/64K arm64 kernel would
// silently corrupt last_page and the SWAPSPACE2 magic offset.
func TestKernelFragmentArm64Pins4KPages(t *testing.T) {
	content := readFragment(t, "arm64.config")

	assert.Regexp(t, arm64Page4KLine, content, "arm64.config must pin CONFIG_ARM64_4K_PAGES=y")
	assert.NotRegexpf(t, arm64PageOtherOn, content, "arm64.config must not enable 16K/64K pages")
}
