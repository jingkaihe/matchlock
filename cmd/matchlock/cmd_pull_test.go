package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/pkg/image"
)

// newPullTestCmd returns a standalone pull command so the wiring tests never
// mutate the package-level pullCmd's flag state.
func newPullTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "pull", RunE: runPull}
	cmd.Flags().Bool("force", false, "")
	cmd.Flags().StringP("tag", "t", "", "")
	return cmd
}

// unsetenvForTest unsets key for the duration of the test and restores the
// previous state on cleanup. t.Setenv cannot express "variable absent".
func unsetenvForTest(t *testing.T, key string) {
	t.Helper()
	orig, had := os.LookupEnv(key)
	require.NoError(t, os.Unsetenv(key))
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, orig)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

// withPullBuildSeam swaps the build seam for the duration of the test so the
// pull wiring is exercised without any network I/O.
func withPullBuildSeam(t *testing.T, fn func(*image.Builder, context.Context, string) (*image.BuildResult, error)) {
	t.Helper()
	orig := pullBuildFn
	pullBuildFn = fn
	t.Cleanup(func() { pullBuildFn = orig })
}

// isolatePullHome points HOME at a temp dir so builder construction never
// touches the real image store.
func isolatePullHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestResolvePullTimeoutDefaultUnset(t *testing.T) {
	unsetenvForTest(t, pullTimeoutEnv)

	got, err := resolvePullTimeout()
	require.NoError(t, err)
	// Assert the literal external contract (12h), not just the production constant.
	assert.Equal(t, 12*time.Hour, got, "unset override must resolve to the 12h default")
}

func TestResolvePullTimeoutOverride(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"minutes", "30m", 30 * time.Minute},
		{"hours", "24h", 24 * time.Hour},
		{"composite", "1h30m", 90 * time.Minute},
		{"bare-seconds", "90s", 90 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(pullTimeoutEnv, tc.raw)

			got, err := resolvePullTimeout()
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestResolvePullTimeoutInvalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"garbage", "banana"},
		{"missing-unit", "10"},
		{"spelled-out", "10 minutes"},
		{"leading-space", " 30m"},
		{"empty-string", ""},
		{"overflow", "100000000000000h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(pullTimeoutEnv, tc.raw)

			got, err := resolvePullTimeout()
			assert.Equal(t, time.Duration(0), got)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidPullTimeout)
			// Guard the literal external contract spelling, not the constant.
			assert.Contains(t, err.Error(), "MATCHLOCK_PULL_TIMEOUT")
		})
	}
}

func TestResolvePullTimeoutNonPositive(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"zero", "0"},
		{"zero-duration", "0s"},
		{"negative", "-5m"},
		{"negative-nanos", "-1ns"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(pullTimeoutEnv, tc.raw)

			got, err := resolvePullTimeout()
			assert.Equal(t, time.Duration(0), got)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidPullTimeout)
			assert.Contains(t, err.Error(), "MATCHLOCK_PULL_TIMEOUT")
			assert.Contains(t, err.Error(), "positive")
		})
	}
}

// TestPullRunEInvalidTimeoutFailsFastBeforeBuild proves an invalid override
// aborts before any image work: the build seam is never reached and no image
// store is created.
func TestPullRunEInvalidTimeoutFailsFastBeforeBuild(t *testing.T) {
	home := isolatePullHome(t)
	t.Setenv(pullTimeoutEnv, "banana")

	calls := 0
	withPullBuildSeam(t, func(*image.Builder, context.Context, string) (*image.BuildResult, error) {
		calls++
		return nil, errors.New("build must not run when the override is invalid")
	})

	err := runPull(newPullTestCmd(), []string{"alpine:latest"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidPullTimeout)
	assert.Zero(t, calls, "an invalid override must fail before any image work")

	storeDir := filepath.Join(home, ".cache", "matchlock", "images")
	_, statErr := os.Stat(storeDir)
	assert.True(t, os.IsNotExist(statErr), "no image store should be created when the override is invalid")

	// Positive control: builder construction DOES create the store dir, so
	// the absence asserted above is meaningful.
	_ = image.NewBuilder(&image.BuildOptions{})
	_, statErr = os.Stat(storeDir)
	assert.NoError(t, statErr)
}

// TestPullRunEAppliesResolvedTimeout proves the resolved deadline (default or
// override) is carried on the context handed to the builder.
func TestPullRunEAppliesResolvedTimeout(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		env  string
		want time.Duration
	}{
		{name: "default-unset", set: false, want: 12 * time.Hour},
		{name: "override-30m", set: true, env: "30m", want: 30 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolatePullHome(t)
			if tc.set {
				t.Setenv(pullTimeoutEnv, tc.env)
			} else {
				unsetenvForTest(t, pullTimeoutEnv)
			}

			var (
				deadline    time.Time
				hasDeadline bool
			)
			withPullBuildSeam(t, func(_ *image.Builder, ctx context.Context, _ string) (*image.BuildResult, error) {
				deadline, hasDeadline = ctx.Deadline()
				return &image.BuildResult{Digest: "sha256:test", Size: 2 * 1024 * 1024}, nil
			})

			out := captureStdout(t, func() {
				require.NoError(t, runPull(newPullTestCmd(), []string{"alpine:latest"}))
			})

			require.True(t, hasDeadline, "pull build context must carry the resolved deadline")
			remaining := time.Until(deadline)
			assert.LessOrEqual(t, remaining, tc.want)
			assert.Greater(t, remaining, tc.want-5*time.Minute, "deadline must reflect the resolved timeout")
			assert.Contains(t, out, "Digest: sha256:test")
			assert.Contains(t, out, "Size: 2.0 MB")
		})
	}
}

// TestPullRunECancelledParentAbortsPull proves cancelling the caller context
// while the pull is in flight aborts it promptly instead of waiting out the
// deadline. Cancellation is triggered from inside the seam (after runPull has
// derived its contexts), and the seam's own wait is independently bounded so
// a propagation regression fails a clean assertion instead of hanging.
func TestPullRunECancelledParentAbortsPull(t *testing.T) {
	isolatePullHome(t)
	t.Setenv(pullTimeoutEnv, "10m")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	withPullBuildSeam(t, func(_ *image.Builder, buildCtx context.Context, _ string) (*image.BuildResult, error) {
		cancel()
		select {
		case <-buildCtx.Done():
			return nil, buildCtx.Err()
		case <-time.After(10 * time.Second):
			return nil, errors.New("seam: pull context was never cancelled")
		}
	})

	cmd := newPullTestCmd()
	cmd.SetContext(ctx)

	err := runPull(cmd, []string{"alpine:latest"})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestPullRunESigintCancelsPull proves Ctrl-C reaches the pull: a real SIGINT
// sent to this process (from inside the seam, i.e. after runPull registered
// its handler) cancels the build context and the pull returns instead of
// waiting out its deadline. A test-local SIGINT subscription stays registered
// as a safety net so a regression dropping the pull's signal wiring cannot
// terminate the whole test binary via the default disposition.
func TestPullRunESigintCancelsPull(t *testing.T) {
	isolatePullHome(t)
	t.Setenv(pullTimeoutEnv, "10m")

	safety := make(chan os.Signal, 1)
	signal.Notify(safety, syscall.SIGINT)
	t.Cleanup(func() { signal.Stop(safety) })

	withPullBuildSeam(t, func(_ *image.Builder, buildCtx context.Context, _ string) (*image.BuildResult, error) {
		require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT))
		select {
		case <-buildCtx.Done():
			return nil, buildCtx.Err()
		case <-time.After(10 * time.Second):
			return nil, errors.New("seam: pull context was never cancelled by SIGINT")
		}
	})

	err := runPull(newPullTestCmd(), []string{"alpine:latest"})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestPullCmdHelpDocumentsTimeoutOverride pins the user-visible contract of
// `matchlock pull --help`: the 12h default and the override variable.
func TestPullCmdHelpDocumentsTimeoutOverride(t *testing.T) {
	out := captureStdout(t, func() {
		require.NoError(t, pullCmd.Help())
	})
	assert.Contains(t, out, "12h")
	assert.Contains(t, out, "MATCHLOCK_PULL_TIMEOUT")
	assert.Contains(t, out, "positive")
}
