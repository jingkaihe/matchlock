package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExecHelpDocumentsIsolation locks the user-facing documentation for the
// per-exec isolation semantics (change 3, DECIDED B: keep the behaviour,
// document it). The guest implementation is
// internal/guestruntime/agent/sandbox_proc.go (CLONE_NEWPID|CLONE_NEWNS), so
// the help text and README must describe the observable contract: an isolated
// session, per-session PID namespace, background processes that die with the
// exec, per-session PIDs, and the long-lived `matchlock exec -i` workaround.
func TestExecHelpDocumentsIsolation(t *testing.T) {
	long := execCmd.Long
	require.NotEmpty(t, long, "execCmd.Long must document exec isolation")

	helpPhrases := []string{
		"isolated session",
		"pid and mount namespace",
		"/proc remounted",
		"capabilities",
		"seccomp",
		"background processes",
		"killed",
		"pids are per session",
		"matchlock exec -i <vm> -- sh",
	}
	for _, phrase := range helpPhrases {
		assert.Contains(t, strings.ToLower(long), phrase, "execCmd.Long must document %q", phrase)
	}

	readmeBytes, err := os.ReadFile("../../README.md")
	require.NoError(t, err, "read README.md relative to cmd/matchlock")
	readme := string(readmeBytes)
	lowerReadme := strings.ToLower(readme)

	readmePhrases := []string{
		"## exec sessions are isolated",
		"isolated session",
		"pid and mount namespace",
		"background processes",
		"per session",
		"matchlock exec -i vm-abc12345 -- sh",
	}
	for _, phrase := range readmePhrases {
		assert.Contains(t, lowerReadme, phrase, "README.md must document %q", phrase)
	}

	// The isolation section should sit near the long-lived sandboxes example:
	// after it, and within a bounded distance so the two stay discoverable
	// together.
	longLived := strings.Index(readme, "# Long-lived sandboxes")
	isolation := strings.Index(readme, "## Exec sessions are isolated")
	require.NotEqual(t, -1, longLived, "README.md must keep the long-lived sandboxes example")
	require.NotEqual(t, -1, isolation, "README.md must keep the exec isolation section")
	assert.Greater(t, isolation, longLived, "exec isolation section should follow the long-lived sandboxes example")
	assert.Less(t, isolation-longLived, 2000, "exec isolation section should stay near the long-lived sandboxes example")
}
