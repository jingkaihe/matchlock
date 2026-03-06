//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHasCPUVirtualizationFlag(t *testing.T) {
	assert.True(t, hasCPUVirtualizationFlag([]byte("flags\t: fpu vmx sse")))
	assert.True(t, hasCPUVirtualizationFlag([]byte("flags\t: fpu svm sse")))
	assert.False(t, hasCPUVirtualizationFlag([]byte("flags\t: fpu sse sse2")))
}

func TestCheckUserGroupPassesWhenGroupPresent(t *testing.T) {
	original := execCommand
	execCommand = fakeExecCommand(t, map[string]fakeExecResponse{
		"groups tester": {stdout: "tester wheel kvm"},
	})
	t.Cleanup(func() { execCommand = original })

	t.Setenv("SUDO_USER", "")
	t.Setenv("USER", "tester")

	check := checkUserGroup("kvm", "fix")
	assert.Equal(t, diagnoseStatusPass, check.Status)
	assert.Contains(t, check.Message, "tester")
}

func TestCheckUserGroupFailsWhenGroupMissing(t *testing.T) {
	original := execCommand
	execCommand = fakeExecCommand(t, map[string]fakeExecResponse{
		"groups tester": {stdout: "tester wheel"},
	})
	t.Cleanup(func() { execCommand = original })

	t.Setenv("SUDO_USER", "")
	t.Setenv("USER", "tester")

	check := checkUserGroup("kvm", "fix")
	assert.Equal(t, diagnoseStatusFail, check.Status)
	assert.Equal(t, "fix", check.Fix)
}

func TestCheckUserGroupFailsWhenInspectionFails(t *testing.T) {
	original := execCommand
	execCommand = fakeExecCommand(t, map[string]fakeExecResponse{
		"groups tester": {stderr: "groups: not found", exitCode: 1},
	})
	t.Cleanup(func() { execCommand = original })

	t.Setenv("SUDO_USER", "")
	t.Setenv("USER", "tester")

	check := checkUserGroup("kvm", "fix")
	assert.Equal(t, diagnoseStatusFail, check.Status)
	assert.Contains(t, check.Message, "Could not inspect groups")
}

func TestCheckNFTablesAvailablePasses(t *testing.T) {
	original := execCommand
	execCommand = fakeExecCommand(t, map[string]fakeExecResponse{
		"modprobe -n -v nf_tables": {stdout: "insmod /lib/modules/nf_tables.ko"},
	})
	t.Cleanup(func() { execCommand = original })

	check := checkNFTablesAvailable()
	assert.Equal(t, diagnoseStatusPass, check.Status)
}

func TestCheckNFTablesAvailableFails(t *testing.T) {
	original := execCommand
	execCommand = fakeExecCommand(t, map[string]fakeExecResponse{
		"modprobe -n -v nf_tables": {stderr: "modprobe: FATAL: Module nf_tables not found", exitCode: 1},
	})
	t.Cleanup(func() { execCommand = original })

	check := checkNFTablesAvailable()
	assert.Equal(t, diagnoseStatusFail, check.Status)
	assert.Contains(t, check.Message, "nf_tables")
}

func TestCheckMatchlockCapabilitiesFailsWhenGetcapInspectionFails(t *testing.T) {
	original := execCommand
	execCommand = fakeExecCommand(t, map[string]fakeExecResponse{
		"getcap /proc/self/exe": {stderr: "getcap: command not found", exitCode: 1},
	})
	t.Cleanup(func() { execCommand = original })

	check := checkMatchlockCapabilities()
	assert.Equal(t, diagnoseStatusFail, check.Status)
	assert.Contains(t, check.Message, "Could not inspect file capabilities")
}

func TestRunSetupStepFailsWithoutBestEffort(t *testing.T) {
	err := runSetupStep("network setup", false, func() error {
		return errors.New("boom")
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSetupStep)
	assert.Contains(t, err.Error(), "network setup")
}

func TestRunSetupStepBestEffortSuppressesError(t *testing.T) {
	err := runSetupStep("network setup", true, func() error {
		return errors.New("boom")
	})
	assert.NoError(t, err)
}

type fakeExecResponse struct {
	stdout   string
	stderr   string
	exitCode int
}

func fakeExecCommand(t *testing.T, responses map[string]fakeExecResponse) commandRunner {
	t.Helper()

	helper := filepath.Join(t.TempDir(), "fake-exec.sh")
	script := "#!/bin/sh\nset -eu\nkey=\"$1\"\nshift\nfor arg in \"$@\"; do\n  key=\"$key $arg\"\ndone\n"
	for key, response := range responses {
		script += "if [ \"$key\" = '" + shellSingleQuote(key) + "' ]; then\n"
		if response.stdout != "" {
			script += "  printf '%s' '" + shellSingleQuote(response.stdout) + "'\n"
		}
		if response.stderr != "" {
			script += "  printf '%s' '" + shellSingleQuote(response.stderr) + "' 1>&2\n"
		}
		script += "  exit " + strconv.Itoa(response.exitCode) + "\n"
		script += "fi\n"
	}
	script += "echo \"unexpected command: $key\" 1>&2\nexit 99\n"
	require.NoError(t, os.WriteFile(helper, []byte(script), 0755))

	return func(name string, args ...string) *exec.Cmd {
		cmdArgs := append([]string{helper, name}, args...)
		return exec.Command("sh", cmdArgs...)
	}
}

func shellSingleQuote(s string) string {
	return strings.ReplaceAll(s, "'", "'\"'\"'")
}
