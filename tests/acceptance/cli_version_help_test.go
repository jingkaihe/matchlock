//go:build acceptance

package acceptance

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIVersion(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "version")
	require.Equal(t, 0, exitCode)
	assert.True(t, strings.HasPrefix(stdout, "matchlock "), "stdout = %q, want prefix 'matchlock '", stdout)
	assert.Contains(t, stdout, "commit:")
}

func TestCLIVersionFlag(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "--version")
	require.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "matchlock")
}

func TestCLIHelp(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "--help")
	require.Equal(t, 0, exitCode)
	for _, sub := range []string{"run", "exec", "build", "pull", "list", "get", "kill", "rm", "prune", "port-forward", "rpc", "version"} {
		assert.Containsf(t, stdout, sub, "help output should mention %q subcommand", sub)
	}
}

func TestCLIRunHelp(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "run", "--help")
	require.Equal(t, 0, exitCode)
	for _, flag := range []string{"--image", "--cpus", "--memory", "--timeout", "--disk-size", "--allow-host", "--secret", "--publish", "--address", "--rm"} {
		assert.Containsf(t, stdout, flag, "run --help should mention %q flag", flag)
	}
}
