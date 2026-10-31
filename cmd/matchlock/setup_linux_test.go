//go:build linux

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFirecrackerVersionSupported(t *testing.T) {
	for version, want := range map[string]bool{
		"v1.10.1": false,
		"v1.13.9": false,
		"v1.14.0": true,
		"v1.17.0": true,
		"v2.0.0":  true,
		"":        false,
		"unknown": false,
	} {
		assert.Equal(t, want, firecrackerVersionSupported(version), version)
	}
}

func TestFirecrackerVersionMatchesPackaging(t *testing.T) {
	require.True(t, firecrackerVersionSupported(firecrackerVersion))
	data, err := os.ReadFile("../../.goreleaser.yaml")
	require.NoError(t, err)
	pins := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "fetch-firecracker.sh") {
			pins++
			assert.Contains(t, line, "fetch-firecracker.sh "+firecrackerVersion+" ", "packaged Firecracker must match setup")
		}
	}
	assert.Equal(t, 4, pins, "expected firecracker and jailer pins for amd64 and arm64")
}
