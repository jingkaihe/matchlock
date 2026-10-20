package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/pkg/kernel"
)

func TestKernelRmRejectsAllWithVersion(t *testing.T) {
	cmd := kernelRmCmd
	cmd.Flags().Set("all", "true")
	t.Cleanup(func() {
		cmd.Flags().Set("all", "false")
	})

	err := runKernelRm(cmd, []string{"6.1.137"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--all cannot be combined")
}

func TestKernelRmRejectsRefWithVersion(t *testing.T) {
	cmd := kernelRmCmd
	cmd.Flags().Set("ref", "ghcr.io/example/kernel:6.1.137")
	t.Cleanup(func() {
		cmd.Flags().Set("ref", "")
	})

	err := runKernelRm(cmd, []string{"6.1.137"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--ref cannot be combined")
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	orig := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	require.NoError(t, w.Close())
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	return string(out)
}

// newKernelPullTestManager builds a manager rooted at a temp cache dir whose
// registry points at an unreachable address, so any accidental download
// attempt fails fast instead of touching the network.
func newKernelPullTestManager(t *testing.T) *kernel.Manager {
	t.Helper()
	return kernel.NewManager(
		kernel.WithCacheDir(t.TempDir()),
		kernel.WithRegistry("127.0.0.1:1"),
	)
}

// precreateKernel writes a placeholder kernel file at the manager's resolved
// path for the given version/arch.
func precreateKernel(t *testing.T, mgr *kernel.Manager, arch kernel.Architecture, version string) string {
	t.Helper()
	path := mgr.KernelPath(arch, version)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("kernel"), 0o644))
	return path
}

func TestKernelPullCommandRegistered(t *testing.T) {
	var pull *cobra.Command
	for _, c := range kernelCmd.Commands() {
		if c.Name() == "pull" {
			pull = c
			break
		}
	}
	require.NotNil(t, pull, "kernelCmd must register a pull subcommand")
	assert.True(t, strings.HasPrefix(pull.Use, "pull"), "pull Use = %q", pull.Use)

	flag := pull.Flags().Lookup("version")
	require.NotNil(t, flag, "pull must expose a --version flag")
	assert.Equal(t, "string", flag.Value.Type())
	assert.Equal(t, "", flag.DefValue)
}

func TestRunKernelPullPresentKernelNoOp(t *testing.T) {
	arch := kernel.CurrentArch()
	const version = "9.9.9"
	mgr := newKernelPullTestManager(t)
	wantPath := precreateKernel(t, mgr, arch, version)

	out := captureStdout(t, func() {
		require.NoError(t, runKernelPull(context.Background(), mgr, version))
	})

	wantLine := fmt.Sprintf("kernel %s (%s): %s\n", version, arch, wantPath)
	assert.Equal(t, wantLine, out)
	assert.Equal(t, 1, strings.Count(out, "\n"), "exactly one stdout line expected")
	assert.Regexp(t,
		regexp.MustCompile(`^kernel `+regexp.QuoteMeta(version)+` \(`+regexp.QuoteMeta(string(arch))+`\): `+regexp.QuoteMeta(wantPath)+`\n$`),
		out)
}

func TestRunKernelPullEmptyVersionUsesDefault(t *testing.T) {
	arch := kernel.CurrentArch()
	mgr := newKernelPullTestManager(t)
	wantPath := precreateKernel(t, mgr, arch, kernel.Version)

	out := captureStdout(t, func() {
		require.NoError(t, runKernelPull(context.Background(), mgr, ""))
	})

	assert.Contains(t, out, fmt.Sprintf("kernel %s (%s): %s", kernel.Version, arch, wantPath))
	assert.Equal(t, 1, strings.Count(out, "\n"))
}

func TestRunKernelPullVersionOverrideSelectsVersionPath(t *testing.T) {
	arch := kernel.CurrentArch()
	mgr := newKernelPullTestManager(t)
	wantPath := precreateKernel(t, mgr, arch, "9.9.9")

	out := captureStdout(t, func() {
		require.NoError(t, runKernelPull(context.Background(), mgr, "9.9.9"))
	})

	assert.Contains(t, out, filepath.Join("kernels", "9.9.9", arch.KernelFilename()))
	assert.Contains(t, out, wantPath)
	assert.NotContains(t, out, kernel.Version)
}

// captureStdoutCleanup redirects os.Stdout into a pipe and registers the
// restore in t.Cleanup, so the real stdout is reinstated even if an assertion
// fails before the test returns. The returned func drains and returns
// everything written to the pipe; it is safe to call more than once (a second
// call returns ""). Unlike captureStdout (which restores via defer), this
// variant satisfies tests that must restore os.Stdout from t.Cleanup.
func captureStdoutCleanup(t *testing.T) func() string {
	t.Helper()

	orig := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	closed := false
	read := func() string {
		if closed {
			return ""
		}
		closed = true
		require.NoError(t, w.Close())
		out, err := io.ReadAll(r)
		require.NoError(t, err)
		require.NoError(t, r.Close())
		return string(out)
	}

	t.Cleanup(func() {
		if !closed {
			closed = true
			_ = w.Close()
			_, _ = io.ReadAll(r)
			_ = r.Close()
		}
		os.Stdout = orig
	})

	return read
}

// TestKernelPullDispatchCachedSuccessUsesVersionPath drives the REAL cobra
// command tree for the success path: with a fresh temp HOME and a kernel
// already present under the resolved 9.9.9 cache path,
// `kernel pull --version 9.9.9` must succeed through the real RunE, print
// exactly the single documented success line, name the 9.9.9 cache path, and
// never fall back to the default version. The pre-created file makes
// EnsureKernel short-circuit on os.Stat, so no network I/O happens.
func TestKernelPullDispatchCachedSuccessUsesVersionPath(t *testing.T) {
	// HOME drives kernel.NewManager()'s default cache dir; empty SUDO_USER
	// stops getUserHome() from preferring /home on a root test runner.
	t.Setenv("SUDO_USER", "")
	home := t.TempDir()
	t.Setenv("HOME", home)

	arch := kernel.CurrentArch()
	const version = "9.9.9"
	wantPath := filepath.Join(home, ".cache", "matchlock", "kernels", version, arch.KernelFilename())
	require.NoError(t, os.MkdirAll(filepath.Dir(wantPath), 0o755))
	require.NoError(t, os.WriteFile(wantPath, []byte("kernel"), 0o644))

	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		require.NoError(t, kernelPullCmd.Flags().Set("version", ""))
	})

	rootCmd.SetArgs([]string{"kernel", "pull", "--version", version})

	read := captureStdoutCleanup(t)
	err := rootCmd.Execute()
	out := read()

	require.NoError(t, err, "cached kernel pull must succeed through dispatch")
	wantLine := fmt.Sprintf("kernel %s (%s): %s\n", version, arch, wantPath)
	assert.Equal(t, wantLine, out, "success path prints exactly the single documented line")
	assert.Contains(t, out, wantPath, "stdout must name the resolved 9.9.9 cache path")
	assert.NotContains(t, out, kernel.Version, "must not fall back to the default version")
	assert.Equal(t, 1, strings.Count(out, "\n"), "exactly one stdout line expected")
}

// TestKernelPullDispatchVersionFlowsToRegistryReference proves the --version
// value flows through the REAL dispatch into the registry reference used in the
// ErrPullKernel wrapping: injecting a failing seam and running
// `kernel pull --version 9.9.9` must return an error that satisfies
// errors.Is(err, ErrPullKernel) and names kernel.ImageReference("9.9.9").
func TestKernelPullDispatchVersionFlowsToRegistryReference(t *testing.T) {
	t.Setenv("SUDO_USER", "")
	home := t.TempDir()
	t.Setenv("HOME", home)

	orig := kernelEnsureFn
	t.Cleanup(func() { kernelEnsureFn = orig })

	kernelEnsureFn = func(_ *kernel.Manager, _ context.Context, _ kernel.Architecture, _ string) (string, error) {
		return "", errors.New("registry unavailable")
	}

	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		require.NoError(t, kernelPullCmd.Flags().Set("version", ""))
	})

	rootCmd.SetArgs([]string{"kernel", "pull", "--version", "9.9.9"})

	read := captureStdoutCleanup(t)
	err := rootCmd.Execute()
	out := read()

	require.Error(t, err, "dispatch must surface the pull failure")
	assert.ErrorIs(t, err, ErrPullKernel)
	assert.Contains(t, err.Error(), kernel.ImageReference("9.9.9"))
	assert.NotContains(t, out, "kernel ", "failed pull must not print the success line")
}

// TestKernelPullRunEUsesInjectableSeam proves the command RunE routes through
// runKernelPull and the package-level seam, without any real cache or network.
func TestKernelPullRunEUsesInjectableSeam(t *testing.T) {
	orig := kernelEnsureFn
	t.Cleanup(func() { kernelEnsureFn = orig })

	called := false
	kernelEnsureFn = func(_ *kernel.Manager, _ context.Context, arch kernel.Architecture, version string) (string, error) {
		called = true
		return "/injected/" + string(arch) + "/" + version + "/" + arch.KernelFilename(), nil
	}

	require.NoError(t, kernelPullCmd.Flags().Set("version", "9.9.9"))
	t.Cleanup(func() { kernelPullCmd.Flags().Set("version", "") })

	arch := kernel.CurrentArch()
	out := captureStdout(t, func() {
		require.NoError(t, kernelPullCmd.RunE(kernelPullCmd, nil))
	})

	assert.True(t, called, "seam must be invoked")
	assert.Contains(t, out, fmt.Sprintf("kernel 9.9.9 (%s): /injected/%s/9.9.9/%s",
		arch, arch, arch.KernelFilename()))
}

// TestRunKernelPullWrapsFailureWithRegistryReference proves a seam/download
// failure surfaces as ErrPullKernel and names the offending registry reference
// for the selected version.
func TestRunKernelPullWrapsFailureWithRegistryReference(t *testing.T) {
	orig := kernelEnsureFn
	t.Cleanup(func() { kernelEnsureFn = orig })

	kernelEnsureFn = func(_ *kernel.Manager, _ context.Context, _ kernel.Architecture, _ string) (string, error) {
		return "", errors.New("registry unavailable")
	}

	for _, tc := range []struct {
		name    string
		version string
		want    string
	}{
		{name: "explicit version", version: "9.9.9", want: "ghcr.io/jingkaihe/matchlock/kernel:9.9.9"},
		{name: "default version", version: "", want: kernel.ImageReference(kernel.Version)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runKernelPull(context.Background(), newKernelPullTestManager(t), tc.version)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrPullKernel)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestKernelPullRunEPropagatesFailure proves the cobra RunE returns the wrapped
// pull error so `matchlock kernel pull` exits non-zero.
func TestKernelPullRunEPropagatesFailure(t *testing.T) {
	orig := kernelEnsureFn
	t.Cleanup(func() { kernelEnsureFn = orig })

	kernelEnsureFn = func(_ *kernel.Manager, _ context.Context, _ kernel.Architecture, _ string) (string, error) {
		return "", errors.New("registry unavailable")
	}

	require.NoError(t, kernelPullCmd.Flags().Set("version", "9.9.9"))
	t.Cleanup(func() { kernelPullCmd.Flags().Set("version", "") })

	err := kernelPullCmd.RunE(kernelPullCmd, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPullKernel)
	assert.Contains(t, err.Error(), "ghcr.io/jingkaihe/matchlock/kernel:9.9.9")
}

// TestRunKernelPullBoundedCancellation proves a cancelled or expired parent
// context makes the pull return promptly with the context error instead of
// hanging on a stalled download.
func TestRunKernelPullBoundedCancellation(t *testing.T) {
	orig := kernelEnsureFn
	t.Cleanup(func() { kernelEnsureFn = orig })

	kernelEnsureFn = func(_ *kernel.Manager, ctx context.Context, _ kernel.Architecture, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}

	cases := []struct {
		name    string
		ctx     func() context.Context
		wantErr error
	}{
		{
			name: "cancelled",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			wantErr: context.Canceled,
		},
		{
			name: "expired",
			ctx: func() context.Context {
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				t.Cleanup(cancel)
				return ctx
			},
			wantErr: context.DeadlineExceeded,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			err := runKernelPull(tc.ctx(), newKernelPullTestManager(t), "9.9.9")
			elapsed := time.Since(start)

			require.Error(t, err)
			assert.ErrorIs(t, err, tc.wantErr)
			assert.ErrorIs(t, err, ErrPullKernel)
			assert.Less(t, elapsed, time.Second, "pull must return promptly, not hang")
		})
	}
}

// TestRunKernelPullAppliesFiveMinuteBound proves runKernelPull hands the seam a
// context carrying the 5-minute bound.
func TestRunKernelPullAppliesFiveMinuteBound(t *testing.T) {
	orig := kernelEnsureFn
	t.Cleanup(func() { kernelEnsureFn = orig })

	assert.Equal(t, 5*time.Minute, kernelPullTimeout)

	var (
		deadline    time.Time
		hasDeadline bool
	)
	kernelEnsureFn = func(_ *kernel.Manager, ctx context.Context, _ kernel.Architecture, _ string) (string, error) {
		deadline, hasDeadline = ctx.Deadline()
		return "/injected/kernel", nil
	}

	captureStdout(t, func() {
		require.NoError(t, runKernelPull(context.Background(), newKernelPullTestManager(t), "9.9.9"))
	})

	require.True(t, hasDeadline, "pull context must carry a deadline")
	remaining := time.Until(deadline)
	assert.Greater(t, remaining, 4*time.Minute)
	assert.LessOrEqual(t, remaining, kernelPullTimeout)
}

// TestKernelPullDispatchRejectsUnexpectedArg drives the REAL cobra command tree
// via rootCmd.SetArgs([]string{"kernel","pull","unexpected-arg"}) +
// rootCmd.Execute() so kernelPullCmd's Args validator actually runs. A direct
// RunE call would bypass the validator and could not prove rejection. Execute()
// returning the args-validation error IS the non-zero-exit proof because
// cmd/matchlock/main.go exits non-zero on any returned error.
func TestKernelPullDispatchRejectsUnexpectedArg(t *testing.T) {
	// Isolate the cache root: HOME drives kernel.NewManager()'s default cache
	// dir. Empty SUDO_USER stops getUserHome() from preferring /home when the
	// test binary runs as root.
	t.Setenv("SUDO_USER", "")
	home := t.TempDir()
	t.Setenv("HOME", home)

	orig := kernelEnsureFn
	t.Cleanup(func() { kernelEnsureFn = orig })

	calls := 0
	kernelEnsureFn = func(_ *kernel.Manager, _ context.Context, _ kernel.Architecture, _ string) (string, error) {
		calls++
		return "", errors.New("kernelEnsureFn must not be invoked for rejected args")
	}

	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		require.NoError(t, kernelPullCmd.Flags().Set("version", ""))
	})

	rootCmd.SetArgs([]string{"kernel", "pull", "unexpected-arg"})

	out := captureStdout(t, func() {
		err := rootCmd.Execute()
		require.Error(t, err, "kernel pull with an unexpected positional arg must fail")
		assert.Contains(t, err.Error(), "unexpected-arg", "error must name the offending argument")
		assert.Contains(t, err.Error(), "kernel pull", "error must name the kernel pull command")
	})

	// The seam counter proves no fetch was attempted; it says nothing about
	// cache interaction, so also prove kernel.NewManager() never ran by showing
	// the entire cache tree was never created under the fresh HOME.
	assert.Equal(t, 0, calls, "kernelEnsureFn must be invoked exactly 0 times")
	assert.NotContains(t, out, "kernel ", "rejection path must not print the success line")

	_, statErr := os.Stat(filepath.Join(home, ".cache", "matchlock"))
	require.Error(t, statErr, "RunE must never run: the cache dir must not be created")
	assert.True(t, os.IsNotExist(statErr), "cache dir must be absent, got: %v", statErr)
}
