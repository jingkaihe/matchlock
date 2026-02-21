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
	content := "console=hvc0 matchlock.workspace=/workspace/project matchlock.dns=1.1.1.1,8.8.8.8 matchlock.mtu=1200 matchlock.disk.vdb=/var/lib/buildkit matchlock.add_host.0=api.internal,10.0.0.10"
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
	require.Len(t, cfg.AddHosts, 1)
	assert.Equal(t, hostIPMapping{Host: "api.internal", IP: "10.0.0.10"}, cfg.AddHosts[0])
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

func TestParseBootConfigRejectsInvalidAddHost(t *testing.T) {
	dir := t.TempDir()
	cmdline := filepath.Join(dir, "cmdline")
	require.NoError(t, os.WriteFile(cmdline, []byte("matchlock.dns=1.1.1.1 matchlock.add_host.0=invalid"), 0644))

	cfg, err := parseBootConfig(cmdline)
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.ErrorIs(t, err, ErrInvalidAddHost)
}

func TestParseBootConfigNoNetwork(t *testing.T) {
	dir := t.TempDir()
	cmdline := filepath.Join(dir, "cmdline")
	require.NoError(t, os.WriteFile(cmdline, []byte("matchlock.dns=1.1.1.1 matchlock.no_network=1"), 0644))

	cfg, err := parseBootConfig(cmdline)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.True(t, cfg.NoNetwork)
}

func TestParseBootConfigOverlayRoot(t *testing.T) {
	dir := t.TempDir()
	cmdline := filepath.Join(dir, "cmdline")
	content := "matchlock.dns=1.1.1.1 matchlock.overlay=1 matchlock.overlay.lower=vdb,vdc matchlock.overlay.lowerfs=erofs,erofs matchlock.overlay.upper=vdd"
	require.NoError(t, os.WriteFile(cmdline, []byte(content), 0644))

	cfg, err := parseBootConfig(cmdline)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.True(t, cfg.Overlay.Enabled)
	assert.Equal(t, []string{"vdb", "vdc"}, cfg.Overlay.LowerDevices)
	assert.Equal(t, []string{"erofs", "erofs"}, cfg.Overlay.LowerFSTypes)
	assert.Equal(t, "vdd", cfg.Overlay.UpperDevice)
}

func TestParseBootConfigOverlayRequiresBothDevices(t *testing.T) {
	dir := t.TempDir()
	cmdline := filepath.Join(dir, "cmdline")
	content := "matchlock.dns=1.1.1.1 matchlock.overlay=1 matchlock.overlay.lower=vdb"
	require.NoError(t, os.WriteFile(cmdline, []byte(content), 0644))

	cfg, err := parseBootConfig(cmdline)
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.ErrorIs(t, err, ErrInvalidOverlayCfg)
}

func TestParseBootConfigOverlayLowerFSMismatch(t *testing.T) {
	dir := t.TempDir()
	cmdline := filepath.Join(dir, "cmdline")
	content := "matchlock.dns=1.1.1.1 matchlock.overlay=1 matchlock.overlay.lower=vdb,vdc matchlock.overlay.lowerfs=erofs matchlock.overlay.upper=vdd"
	require.NoError(t, os.WriteFile(cmdline, []byte(content), 0644))

	cfg, err := parseBootConfig(cmdline)
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.ErrorIs(t, err, ErrInvalidOverlayCfg)
}

func TestParseAddHostField(t *testing.T) {
	mapping, err := parseAddHostField("api.internal,10.0.0.10")
	require.NoError(t, err)
	assert.Equal(t, hostIPMapping{Host: "api.internal", IP: "10.0.0.10"}, mapping)
}

func TestParseAddHostFieldRejectsInvalidIP(t *testing.T) {
	_, err := parseAddHostField("api.internal,not-an-ip")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidAddHost)
}

func TestRenderEtcHosts(t *testing.T) {
	got := renderEtcHosts("override.internal", nil)

	assert.Contains(t, got, "127.0.0.1 localhost localhost.localdomain override.internal\n")
	assert.Contains(t, got, "::1 localhost ip6-localhost ip6-loopback\n")
	assert.Contains(t, got, "ff02::1 ip6-allnodes\n")
	assert.Contains(t, got, "ff02::2 ip6-allrouters\n")
}

func TestRenderEtcHostsIncludesAddHosts(t *testing.T) {
	got := renderEtcHosts("override.internal", []hostIPMapping{{Host: "api.internal", IP: "10.0.0.10"}, {Host: "db.internal", IP: "2001:db8::10"}})

	assert.Contains(t, got, "10.0.0.10 api.internal\n")
	assert.Contains(t, got, "2001:db8::10 db.internal\n")
}

func TestWriteEtcHostsCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")

	err := writeEtcHosts(path, "vm-12345678", []hostIPMapping{{Host: "api.internal", IP: "10.0.0.10"}})
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, renderEtcHosts("vm-12345678", []hostIPMapping{{Host: "api.internal", IP: "10.0.0.10"}}), string(data))
}
