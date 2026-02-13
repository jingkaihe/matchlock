package api

import (
	"testing"

	shellquote "github.com/kballard/go-shellquote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShellQuoteArgsSimple(t *testing.T) {
	got := ShellQuoteArgs([]string{"echo", "hello"})
	assert.Equal(t, "echo hello", got)
}

func TestShellQuoteArgsWithSpaces(t *testing.T) {
	got := ShellQuoteArgs([]string{"echo", "hello world"})
	assertRoundTrips(t, got, []string{"echo", "hello world"})
}

func TestShellQuoteArgsEmpty(t *testing.T) {
	got := ShellQuoteArgs(nil)
	assert.Equal(t, "", got)
}

func TestShellQuoteArgsSingleArg(t *testing.T) {
	got := ShellQuoteArgs([]string{"ls"})
	assert.Equal(t, "ls", got)
}

func TestShellQuoteArgsSpecialChars(t *testing.T) {
	assertRoundTrips(t, ShellQuoteArgs([]string{"echo", "$HOME"}), []string{"echo", "$HOME"})
}

func TestShellQuoteArgsSingleQuoteInArg(t *testing.T) {
	assertRoundTrips(t, ShellQuoteArgs([]string{"echo", "it's"}), []string{"echo", "it's"})
}

func TestShellQuoteArgsGlob(t *testing.T) {
	assertRoundTrips(t, ShellQuoteArgs([]string{"ls", "*.go"}), []string{"ls", "*.go"})
}

func TestShellQuoteArgsMixed(t *testing.T) {
	assertRoundTrips(t, ShellQuoteArgs([]string{"sh", "-c", "echo hello && ls -la"}), []string{"sh", "-c", "echo hello && ls -la"})
}

func TestShellQuoteArgsEmptyArg(t *testing.T) {
	assertRoundTrips(t, ShellQuoteArgs([]string{"echo", ""}), []string{"echo", ""})
}

func TestShellQuoteArgsBackslash(t *testing.T) {
	assertRoundTrips(t, ShellQuoteArgs([]string{"echo", `a\b`}), []string{"echo", `a\b`})
}

// assertRoundTrips verifies that the quoted string splits back into the original args.
func assertRoundTrips(t *testing.T, quoted string, want []string) {
	t.Helper()
	got, err := shellquote.Split(quoted)
	require.NoError(t, err, "Split(%q)", quoted)
	assert.Equal(t, want, got, "Split(%q)", quoted)
}
