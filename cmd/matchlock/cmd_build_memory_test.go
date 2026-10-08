package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveBuildMemory covers the default-cap contract for the BuildKit VM:
// the default ("all available") must never hand the guest the host's entire RAM
// on a very large machine, while an explicit --build-memory is honored (but not
// above host RAM).
func TestResolveBuildMemory(t *testing.T) {
	const host1TiB = 1414453 // MB, the 1.35 TiB vaimetal host
	const host8GiB = 8192
	const host32GiB = 32768
	const host16GiB = 16384

	tests := []struct {
		name      string
		changed   bool
		memoryMB  int
		hostMB    int
		want      int
		wantError bool
	}{
		{name: "default on huge host is capped", changed: false, memoryMB: 0, hostMB: host1TiB, want: defaultMaxBuildMemoryMB},
		{name: "explicit zero on huge host is capped", changed: true, memoryMB: 0, hostMB: host1TiB, want: defaultMaxBuildMemoryMB},
		{name: "default on small host uses host", changed: false, memoryMB: 0, hostMB: host8GiB, want: host8GiB},
		{name: "default at cap uses cap", changed: false, memoryMB: 0, hostMB: host32GiB, want: defaultMaxBuildMemoryMB},
		{name: "default on 16GiB host uses host", changed: false, memoryMB: 0, hostMB: host16GiB, want: host16GiB},
		{name: "unknown host falls back to cap", changed: false, memoryMB: 0, hostMB: 0, want: defaultMaxBuildMemoryMB},
		{name: "explicit below cap is honored", changed: true, memoryMB: 4096, hostMB: host1TiB, want: 4096},
		{name: "explicit above cap but below host is honored", changed: true, memoryMB: 65536, hostMB: host1TiB, want: 65536},
		{name: "explicit equal to host is honored", changed: true, memoryMB: host8GiB, hostMB: host8GiB, want: host8GiB},
		{name: "explicit above host errors", changed: true, memoryMB: host8GiB + 1, hostMB: host8GiB, wantError: true},
		{name: "negative errors", changed: false, memoryMB: -1, hostMB: host8GiB, wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveBuildMemory(tt.changed, tt.memoryMB, tt.hostMB)
			if tt.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
