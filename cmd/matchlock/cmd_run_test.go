package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/state"
)

func TestParseDiskMountSpec(t *testing.T) {
	dir := t.TempDir()
	hostDisk := filepath.Join(dir, "cache.ext4")
	require.NoError(t, os.WriteFile(hostDisk, []byte("disk"), 0644))

	got, err := parseDiskMountSpec(hostDisk + ":/var/lib/buildkit")
	require.NoError(t, err)
	assert.Equal(t, hostDisk, got.HostPath)
	assert.Equal(t, "/var/lib/buildkit", got.GuestMount)
	assert.False(t, got.ReadOnly)
}

func TestParseDiskMountSpecUIDGID(t *testing.T) {
	dir := t.TempDir()
	hostDisk := filepath.Join(dir, "pgdata.ext4")
	require.NoError(t, os.WriteFile(hostDisk, []byte("disk"), 0644))

	got, err := parseDiskMountSpec(hostDisk + ":/var/lib/postgresql:uid=1000,gid=2000")
	require.NoError(t, err)
	require.NotNil(t, got.OwnerUID)
	require.NotNil(t, got.OwnerGID)
	assert.Equal(t, uint32(1000), *got.OwnerUID)
	assert.Equal(t, uint32(2000), *got.OwnerGID)
}

func TestParseDiskMountSpecRelativeHostPath(t *testing.T) {
	dir := t.TempDir()
	hostDisk := filepath.Join(dir, "data.ext4")
	require.NoError(t, os.WriteFile(hostDisk, []byte("disk"), 0644))

	oldWd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	got, err := parseDiskMountSpec("data.ext4:/mnt/data:ro")
	require.NoError(t, err)
	expectedHostPath, err := filepath.EvalSymlinks(hostDisk)
	require.NoError(t, err)
	actualHostPath, err := filepath.EvalSymlinks(got.HostPath)
	require.NoError(t, err)
	assert.Equal(t, expectedHostPath, actualHostPath)
	assert.Equal(t, "/mnt/data", got.GuestMount)
	assert.True(t, got.ReadOnly)
}

func TestParseDiskMountSpecRejectsInvalidGuestMount(t *testing.T) {
	dir := t.TempDir()
	hostDisk := filepath.Join(dir, "cache.ext4")
	require.NoError(t, os.WriteFile(hostDisk, []byte("disk"), 0644))

	_, err := parseDiskMountSpec(hostDisk + ":relative/path")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid guest mount path")
}

func TestParseDiskMountSpecRejectsUnknownOption(t *testing.T) {
	dir := t.TempDir()
	hostDisk := filepath.Join(dir, "cache.ext4")
	require.NoError(t, os.WriteFile(hostDisk, []byte("disk"), 0644))

	_, err := parseDiskMountSpec(hostDisk + ":/var/lib/buildkit:wat")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown disk option")
}

func TestParseDiskMountSpecRejectsReadonlyOwner(t *testing.T) {
	dir := t.TempDir()
	hostDisk := filepath.Join(dir, "cache.ext4")
	require.NoError(t, os.WriteFile(hostDisk, []byte("disk"), 0644))

	_, err := parseDiskMountSpec(hostDisk + ":/var/lib/buildkit:ro,uid=1000")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "writable")
}

func TestParseDiskMountSpecRejectsInvalidOwnerID(t *testing.T) {
	dir := t.TempDir()
	hostDisk := filepath.Join(dir, "cache.ext4")
	require.NoError(t, os.WriteFile(hostDisk, []byte("disk"), 0644))

	_, err := parseDiskMountSpec(hostDisk + ":/var/lib/buildkit:uid=abc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsigned 32-bit integer")
}

func TestParseDiskMountSpecNamedVolume(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	volumeDir := filepath.Join(home, ".cache", "matchlock", "volumes")
	require.NoError(t, os.MkdirAll(volumeDir, 0755))
	volumePath := filepath.Join(volumeDir, "cache.ext4")
	require.NoError(t, os.WriteFile(volumePath, []byte("disk"), 0644))

	got, err := parseDiskMountSpec("@cache:/var/lib/buildkit:ro")
	require.NoError(t, err)
	assert.Equal(t, volumePath, got.HostPath)
	assert.Equal(t, "/var/lib/buildkit", got.GuestMount)
	assert.True(t, got.ReadOnly)
}

func TestParseDiskMountSpecNamedVolumeOwnerIDs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	volumeDir := filepath.Join(home, ".cache", "matchlock", "volumes")
	require.NoError(t, os.MkdirAll(volumeDir, 0755))
	volumePath := filepath.Join(volumeDir, "pgdata.ext4")
	require.NoError(t, os.WriteFile(volumePath, []byte("disk"), 0644))

	got, err := parseDiskMountSpec("@pgdata:/var/lib/postgresql:uid=999,gid=999")
	require.NoError(t, err)
	assert.Equal(t, volumePath, got.HostPath)
	require.NotNil(t, got.OwnerUID)
	require.NotNil(t, got.OwnerGID)
	assert.Equal(t, uint32(999), *got.OwnerUID)
	assert.Equal(t, uint32(999), *got.OwnerGID)
}

func TestParseDiskMountSpecNamedVolumeMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	_, err := parseDiskMountSpec("@missing:/var/lib/buildkit")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "named volume not found")
}

func TestDiskMountShadowedByWorkspace(t *testing.T) {
	assert.True(t, diskMountShadowedByWorkspace("/workspace/cache", "/workspace"))
	assert.True(t, diskMountShadowedByWorkspace("/workspace", "/workspace"))
}

func TestDiskMountShadowedByWorkspaceOutsideWorkspace(t *testing.T) {
	assert.False(t, diskMountShadowedByWorkspace("/var/lib/buildkit", "/workspace"))
	assert.False(t, diskMountShadowedByWorkspace("/workspace-cache", "/workspace"))
	assert.False(t, diskMountShadowedByWorkspace("/var/lib/buildkit", ""))
}

func TestValidateDetachFlags(t *testing.T) {
	assert.NoError(t, validateDetachFlags(false, false, false))
	assert.NoError(t, validateDetachFlags(true, false, false))

	err := validateDetachFlags(true, true, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--detach cannot be combined")

	err = validateDetachFlags(true, false, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--detach cannot be combined")

	err = validateDetachFlags(true, true, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--detach cannot be combined")
}

func TestDetachedChildArgs(t *testing.T) {
	assert.Equal(
		t,
		[]string{"run", "--image", "alpine:latest", "-d", "--detach=false", "--rm=false"},
		detachedChildArgs([]string{"run", "--image", "alpine:latest", "-d"}),
	)

	assert.Equal(
		t,
		[]string{"run", "--image", "alpine:latest", "-d", "--detach=false", "--rm=false", "--", "sh", "-c", "echo hi"},
		detachedChildArgs([]string{"run", "--image", "alpine:latest", "-d", "--", "sh", "-c", "echo hi"}),
	)

	assert.Equal(
		t,
		[]string{"run", "-dit", "--detach=false", "--rm=false"},
		detachedChildArgs([]string{"run", "-dit"}),
	)

	assert.Equal(
		t,
		[]string{"run", "-itd", "--detach=false", "--rm=false"},
		detachedChildArgs([]string{"run", "-itd"}),
	)

	assert.Equal(
		t,
		[]string{"run", "--image", "alpine:latest", "-w/tmp/data", "-eMODE=prod", "--detach=false", "--rm=false"},
		detachedChildArgs([]string{"run", "--image", "alpine:latest", "-w/tmp/data", "-eMODE=prod"}),
	)

	assert.Equal(
		t,
		[]string{"run", "--image", "alpine:latest", "--kernel", "file:///tmp/vmlinux", "--detach=false", "--rm=false"},
		detachedChildArgs([]string{"run", "--image", "alpine:latest", "--kernel", "file:///tmp/vmlinux"}),
	)
}

func TestWaitDetachedVMIDFindsRunningState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	mgr := state.NewManager()
	vmID := "vm-detached-find"
	require.NoError(t, mgr.Register(vmID, map[string]any{"image": "alpine:latest"}))
	t.Cleanup(func() {
		_ = mgr.Unregister(vmID)
		_ = mgr.Remove(vmID)
	})

	got, err := waitDetachedVMID(os.Getpid(), nil, time.Second)
	require.NoError(t, err)
	assert.Equal(t, vmID, got)
}

func TestWaitDetachedVMIDReturnsErrorWhenProcessIsNotRunning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	_, err := waitDetachedVMID(0, nil, time.Second)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrFindDetachedVM)
}

func TestWaitDetachedVMIDReturnsErrorWhenChildExitsEarly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	exitErr := exec.Command("/bin/sh", "-c", "exit 7").Run()
	require.Error(t, exitErr)

	childExited := make(chan detachedResult, 1)
	childExited <- detachedResult{exitErr: exitErr, stderr: "boom: detached child failed"}

	start := time.Now()
	_, err := waitDetachedVMID(os.Getpid(), childExited, 30*time.Second)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDetachedRunExited)
	assert.Contains(t, err.Error(), "boom: detached child failed")
	assert.Contains(t, err.Error(), "exit status 7")
	assert.Less(t, elapsed, 5*time.Second, "early child exit must fail fast, not hang")
}

func TestWaitDetachedVMIDReturnsErrorWhenChildExitsCleanWithoutRegistering(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	childExited := make(chan detachedResult, 1)
	childExited <- detachedResult{stderr: "pull access denied for nope"}

	start := time.Now()
	_, err := waitDetachedVMID(os.Getpid(), childExited, 30*time.Second)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDetachedRunExited)
	assert.Contains(t, err.Error(), "pull access denied for nope")
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestWaitDetachedVMIDTimesOut(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Use a real, separate process so the timeout path's best-effort kill does
	// not terminate the test binary.
	child := exec.Command("/bin/sh", "-c", "sleep 30")
	require.NoError(t, child.Start())
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()

	start := time.Now()
	_, err := waitDetachedVMID(child.Process.Pid, nil, 200*time.Millisecond)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDetachedStartupTimeout)
	assert.Less(t, time.Since(start), 5*time.Second, "bounded wait must not hang")
}

func TestStartDetachedChildSurfacesEarlyExit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	start := time.Now()
	err := startDetachedChild("/bin/sh", []string{"-c", "echo 'stub boom' >&2; exit 7"}, 30*time.Second)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDetachedRunExited)
	assert.Contains(t, err.Error(), "stub boom")
	assert.Contains(t, err.Error(), "exit status 7")
	assert.Less(t, elapsed, 5*time.Second, "detached startup must fail fast when the child dies early")
}

func TestReadFileTailKeepsLastBytes(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tail-*")
	require.NoError(t, err)
	defer f.Close()

	_, err = f.WriteString("abcdefghij")
	require.NoError(t, err)

	assert.Equal(t, "fghij", readFileTail(f, 5))
	assert.Equal(t, "abcdefghij", readFileTail(f, 100))
	assert.Equal(t, "", readFileTail(f, 0))
	assert.Equal(t, "", readFileTail(nil, 5))
}

func TestParseRunSecretsWithPlaceholderOverride(t *testing.T) {
	secrets, err := parseRunSecrets(
		[]string{"GH_TOKEN=gho_real_token@github.com"},
		[]string{"GH_TOKEN=gho_sandbox_placeholder"},
		"",
	)
	require.NoError(t, err)
	require.Contains(t, secrets, "GH_TOKEN")
	assert.Equal(t, "gho_real_token", secrets["GH_TOKEN"].Value)
	assert.Equal(t, "gho_sandbox_placeholder", secrets["GH_TOKEN"].Placeholder)
	assert.Equal(t, []string{"github.com"}, secrets["GH_TOKEN"].Hosts)
}

func TestSecretFlagPreservesCommaSeparatedHosts(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().StringArray("secret", nil, "Secret (NAME=VALUE@host1,host2 or NAME@host1,host2)")

	err := cmd.ParseFlags([]string{"--secret", "GH_TOKEN@github.com,api.github.com"})
	require.NoError(t, err)

	secrets, err := cmd.Flags().GetStringArray("secret")
	require.NoError(t, err)
	assert.Equal(t, []string{"GH_TOKEN@github.com,api.github.com"}, secrets)
}

func TestSecretPlaceholderFlagPreservesLiteralValue(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().StringArray("secret-placeholder", nil, "Secret placeholder override (NAME=PLACEHOLDER; can be repeated)")

	err := cmd.ParseFlags([]string{"--secret-placeholder", "GH_TOKEN=gho,sandbox,placeholder"})
	require.NoError(t, err)

	placeholders, err := cmd.Flags().GetStringArray("secret-placeholder")
	require.NoError(t, err)
	assert.Equal(t, []string{"GH_TOKEN=gho,sandbox,placeholder"}, placeholders)
}

func TestParseRunSecretsLoadsSecretFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	payload := map[string]map[string]any{
		"GH_TOKEN": {
			"value":       "gho_real_token",
			"placeholder": "gho_sandbox_placeholder",
			"hosts":       []string{"github.com", "api.github.com"},
		},
	}
	data, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0644))

	secrets, err := parseRunSecrets(nil, nil, path)
	require.NoError(t, err)
	require.Contains(t, secrets, "GH_TOKEN")
	assert.Equal(t, "gho_real_token", secrets["GH_TOKEN"].Value)
	assert.Equal(t, "gho_sandbox_placeholder", secrets["GH_TOKEN"].Placeholder)
	assert.Equal(t, []string{"github.com", "api.github.com"}, secrets["GH_TOKEN"].Hosts)
}

func TestParseRunSecretsRejectsUnknownPlaceholderReference(t *testing.T) {
	_, err := parseRunSecrets(nil, []string{"GH_TOKEN=gho_sandbox_placeholder"}, "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidSecret)
	assert.Contains(t, err.Error(), "unknown secret")
}

func TestParseRunSecretsRejectsOverlappingPlaceholderValues(t *testing.T) {
	_, err := parseRunSecrets(
		[]string{
			"A=real_a@example.com",
			"B=real_b@example.com",
		},
		[]string{
			"A=foo",
			"B=foobar",
		},
		"",
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidSecret)
	assert.Contains(t, err.Error(), "overlap")
	assert.Contains(t, err.Error(), `"A"`)
	assert.Contains(t, err.Error(), `"B"`)
}

func TestParseRunSecretsRejectsPlaceholderOverlapWithGeneratedFormat(t *testing.T) {
	_, err := parseRunSecrets(
		[]string{
			"A=real_a@example.com",
			"B=real_b@example.com",
		},
		[]string{
			"A=SECRET",
		},
		"",
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidSecret)
	assert.Contains(t, err.Error(), "overlap")
	assert.Contains(t, err.Error(), `"A"`)
	assert.Contains(t, err.Error(), `"B"`)
}

func TestLoadSecretsFileRejectsEmptyHostEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"GH_TOKEN":{"value":"gho_real_token","hosts":["github.com",""]}}`), 0644))

	_, err := loadSecretsFile(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty host entry")
}

func TestLoadSecretsFileTrimsHosts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	require.NoError(
		t,
		os.WriteFile(
			path,
			[]byte(`{"GH_TOKEN":{"value":"gho_real_token","placeholder":" gho_sandbox_placeholder ","hosts":[" github.com "," api.github.com "]}}`),
			0644,
		),
	)

	secrets, err := loadSecretsFile(path)
	require.NoError(t, err)
	require.Contains(t, secrets, "GH_TOKEN")
	assert.Equal(t, "gho_sandbox_placeholder", secrets["GH_TOKEN"].Placeholder)
	assert.Equal(t, []string{"github.com", "api.github.com"}, secrets["GH_TOKEN"].Hosts)
}

func TestRunAllowPrivateFlagRegistered(t *testing.T) {
	flag := runCmd.Flags().Lookup("allow-private")
	require.NotNil(t, flag, "--allow-private must be registered on the run command")
	assert.Equal(t, "stringArray", flag.Value.Type(), "--allow-private must be a repeatable string array")
	assert.Equal(t, "[]", flag.DefValue, "--allow-private must default to empty")
	assert.Contains(t, flag.Usage, ":port", "--allow-private usage must document the optional :port suffix")
}

func TestAllowPrivateFlagIsRepeatable(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().StringArray("allow-private", nil, "Allow an otherwise-blocked private destination")

	err := cmd.ParseFlags([]string{
		"--allow-private", "192.168.107.74:8888",
		"--allow-private", "200:1234::1",
		"--allow-private", "[fd00::1]:443",
	})
	require.NoError(t, err)

	got, err := cmd.Flags().GetStringArray("allow-private")
	require.NoError(t, err)
	assert.Equal(t, []string{"192.168.107.74:8888", "200:1234::1", "[fd00::1]:443"}, got)
}

func TestBuildRunNetworkConfigCarriesAllowPrivate(t *testing.T) {
	cfg := buildRunNetworkConfig(
		[]string{"api.openai.com"},
		nil,
		[]string{"192.168.107.74:8888", "200:1234::1"},
		false,
		true,
		nil,
		[]string{"8.8.8.8"},
		"demo",
		1400,
	)
	require.NotNil(t, cfg)
	assert.Equal(t, []string{"192.168.107.74:8888", "200:1234::1"}, cfg.AllowPrivate)
	// The CLI always keeps the private block on; --allow-private is the only
	// escape hatch and must never turn the block off.
	assert.True(t, cfg.BlockPrivateIPs)
	assert.Equal(t, []string{"api.openai.com"}, cfg.AllowedHosts)
	assert.True(t, cfg.Intercept)
	assert.Equal(t, 1400, cfg.MTU)
}

func TestRunNoNetworkRejectsAllowPrivate(t *testing.T) {
	cmd := &cobra.Command{RunE: runRun}
	cmd.Flags().Float64("cpus", 1, "Number of CPUs")
	cmd.Flags().Int("mtu", api.DefaultNetworkMTU, "Network MTU for guest interface")
	cmd.Flags().Bool("no-network", false, "Create sandbox with no network interfaces")
	cmd.Flags().StringArray("allow-private", nil, "Allow an otherwise-blocked private destination")

	require.NoError(t, cmd.ParseFlags([]string{
		"--no-network",
		"--allow-private", "192.168.107.74:8888",
	}))

	err := runRun(cmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--no-network cannot be combined with --allow-private")
}

func TestRunHelpMentionsAllowPrivate(t *testing.T) {
	assert.Contains(t, runCmd.Long, "--allow-private")
	assert.Contains(t, runCmd.Long, ":port")
	// Cobra renders registered flags into the help/usage output.
	assert.Contains(t, runCmd.UsageString(), "--allow-private")
}
