//go:build acceptance

package acceptance

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/stretchr/testify/require"
)

const launchTimeout = 45 * time.Second

func matchlockConfig(t *testing.T) sdk.Config {
	t.Helper()
	cfg := sdk.DefaultConfig()
	if os.Getenv("MATCHLOCK_BIN") == "" {
		cfg.BinaryPath = "matchlock"
	}
	return cfg
}

func launchAlpine(t *testing.T) *sdk.Client {
	t.Helper()
	return launchWithBuilder(t, sdk.New("alpine:latest"))
}

func launchWithBuilder(t *testing.T, builder *sdk.SandboxBuilder) *sdk.Client {
	t.Helper()
	client, err := sdk.NewClient(matchlockConfig(t))
	require.NoError(t, err, "NewClient")

	t.Cleanup(func() {
		client.Close(0)
		client.Remove()
	})

	type launchResult struct {
		id  string
		err error
	}
	done := make(chan launchResult, 1)
	go func() {
		id, launchErr := client.Launch(builder)
		done <- launchResult{id: id, err: launchErr}
	}()

	select {
	case result := <-done:
		require.NoError(t, result.err, "Launch")
	case <-time.After(launchTimeout):
		_ = client.Close(0)
		_ = client.Remove()
		require.FailNowf(t, "Launch timed out", "image=%s timeout=%s", builder.Options().Image, launchTimeout)
	}

	return client
}

// launchAlpineWithNetwork creates a sandbox with network policy configured.
func launchAlpineWithNetwork(t *testing.T, builder *sdk.SandboxBuilder) *sdk.Client {
	t.Helper()
	return launchWithBuilder(t, builder)
}

func matchlockBin(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("MATCHLOCK_BIN"); bin != "" {
		return bin
	}
	return "matchlock"
}

func runCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	bin := matchlockBin(t)
	cmd := exec.Command(bin, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			require.NoError(t, err, "failed to run %s %v", bin, args)
		}
	}
	return stdout.String(), stderr.String(), exitCode
}

// runCLIWithTimeout runs the CLI with a timeout and returns stdout, stderr, exit code.
func runCLIWithTimeout(t *testing.T, timeout time.Duration, args ...string) (string, string, int) {
	return runCLIEnvWithTimeout(t, timeout, nil, args...)
}

// runCLIEnvWithTimeout runs the CLI with optional env and a timeout and returns stdout, stderr, exit code.
func runCLIEnvWithTimeout(t *testing.T, timeout time.Duration, env []string, args ...string) (string, string, int) {
	t.Helper()
	bin := matchlockBin(t)
	cmd := exec.Command(bin, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	require.NoError(t, cmd.Start(), "failed to start %s %v", bin, args)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			}
		}
		return stdout.String(), stderr.String(), exitCode
	case <-time.After(timeout):
		cmd.Process.Kill()
		require.Fail(t, "command timed out", "%s %v", bin, args)
		return "", "", -1
	}
}
