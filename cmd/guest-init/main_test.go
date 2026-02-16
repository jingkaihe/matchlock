//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseBootConfig(t *testing.T) {
	dir := t.TempDir()
	cmdline := filepath.Join(dir, "cmdline")
	content := "console=hvc0 matchlock.workspace=/workspace/project matchlock.dns=1.1.1.1,8.8.8.8 matchlock.mtu=1200 matchlock.disk.vdb=/var/lib/buildkit"
	require.NoError(t, os.WriteFile(cmdline, []byte(content), 0644))

	cfg, err := parseBootConfig(cmdline)
	require.NoError(t, err)
	require.NotNil(t, cfg)

	assert.Equal(t, "/workspace/project", cfg.Workspace)
	assert.Equal(t, []string{"1.1.1.1", "8.8.8.8"}, cfg.DNSServers)
	assert.Equal(t, 1200, cfg.MTU)
	require.Len(t, cfg.Disks, 1)
	assert.Equal(t, "vdb", cfg.Disks[0].Device)
	assert.Equal(t, "/var/lib/buildkit", cfg.Disks[0].Path)
}

func TestParseBootConfigDefaultsWorkspace(t *testing.T) {
	dir := t.TempDir()
	cmdline := filepath.Join(dir, "cmdline")
	require.NoError(t, os.WriteFile(cmdline, []byte("matchlock.dns=9.9.9.9"), 0644))

	cfg, err := parseBootConfig(cmdline)
	require.NoError(t, err)
	assert.Equal(t, defaultWorkspace, cfg.Workspace)
	assert.Equal(t, []string{"9.9.9.9"}, cfg.DNSServers)
	assert.Equal(t, defaultNetworkMTU, cfg.MTU)
}

func TestParseBootConfigRequiresDNS(t *testing.T) {
	dir := t.TempDir()
	cmdline := filepath.Join(dir, "cmdline")
	require.NoError(t, os.WriteFile(cmdline, []byte("matchlock.workspace=/workspace"), 0644))

	cfg, err := parseBootConfig(cmdline)
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.ErrorIs(t, err, ErrMissingDNS)
}

func TestParseBootConfigRejectsInvalidMTU(t *testing.T) {
	dir := t.TempDir()
	cmdline := filepath.Join(dir, "cmdline")
	require.NoError(t, os.WriteFile(cmdline, []byte("matchlock.dns=1.1.1.1 matchlock.mtu=not-a-number"), 0644))

	cfg, err := parseBootConfig(cmdline)
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.ErrorIs(t, err, ErrInvalidMTU)
}
