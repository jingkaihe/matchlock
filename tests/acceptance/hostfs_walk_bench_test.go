//go:build acceptance

package acceptance

import (
	"debug/elf"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// host_fs walk benchmark guard.
//
// The host_fs coherence fix (US-001) validates the cached path and file type
// before reusing a guest node. That validation must stay off the hot path: on a
// normal warm metadata walk every looked-up path equals the cached path and
// every file type matches, so the fix should be within noise of the pre-fix
// guest-init. The fix is entirely guest-side (internal/guestruntime/fused), so
// the host CLI/VFS server is identical for both variants; the only difference
// is the injected guest-init.
//
// This test builds the baseline guest-init from the integration base commit and
// runs `git status --porcelain` over a ~5000-file git working tree mounted via
// host_fs, once per guest-init variant. Each variant gets one cold run (to warm
// the guest dentry/attr caches) followed by three warm runs; the best warm run
// is compared. A fixed build more than 10% slower than the baseline fails the
// test. `--no-optional-locks` keeps `git status` a pure metadata walk (it never
// writes `.git/index.lock`), so the host tree is not mutated.
//
// The baseline commit is materialised with `git archive` and built with the
// pinned guest toolchain flags; git/go unavailability skips the test with a
// clear message rather than silently passing.

const (
	walkBenchBaselineCommit = "a3add95"
	walkBenchImage          = "igorhvr/bedlam-ubuntu"
	walkBenchFileCount      = 5000
	walkBenchWarmRuns       = 3
	walkBenchMaxSlowdown    = 1.10
)

// walkBenchRepoRoot locates the module root by walking up from the test source
// file (falling back to the process working directory) until go.mod is found.
func walkBenchRepoRoot(t *testing.T) string {
	t.Helper()

	starts := make([]string, 0, 2)
	if _, file, _, ok := runtime.Caller(0); ok {
		starts = append(starts, filepath.Dir(file))
	}
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}

	for _, start := range starts {
		dir := start
		for {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	require.FailNow(t, "walk benchmark: cannot locate repo root (go.mod)")
	return ""
}

// buildWalkBenchBaselineGuestInit materialises the integration base commit
// (a3add95) with `git archive` and builds its guest-init with the pinned guest
// toolchain flags. The build directory is removed in Cleanup.
func buildWalkBenchBaselineGuestInit(t *testing.T, repoRoot string) string {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("walk benchmark skipped: git not available: %v", err)
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("walk benchmark skipped: go toolchain not available: %v", err)
	}

	base, err := os.MkdirTemp("", "matchlock-walk-bench-base-*")
	require.NoError(t, err, "create baseline build dir")
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	archivePath := filepath.Join(base, "src.tar")
	archiveFile, err := os.Create(archivePath)
	require.NoErrorf(t, err, "create %s", archivePath)
	archive := exec.Command("git", "archive", "--format=tar", walkBenchBaselineCommit)
	archive.Dir = repoRoot
	archive.Stdout = archiveFile
	archiveErr := archive.Run()
	closeErr := archiveFile.Close()
	if archiveErr != nil {
		t.Skipf("walk benchmark skipped: cannot materialise baseline commit %s: %v", walkBenchBaselineCommit, archiveErr)
	}
	require.NoError(t, closeErr, "close baseline archive")

	extract := exec.Command("tar", "-xf", archivePath, "-C", base)
	if out, err := extract.CombinedOutput(); err != nil {
		t.Skipf("walk benchmark skipped: cannot extract baseline commit %s: %v\n%s", walkBenchBaselineCommit, err, out)
	}

	guestInit := filepath.Join(base, "guest-init")
	build := exec.Command("go", "build", "-trimpath", "-o", guestInit, "./cmd/guest-init")
	build.Dir = base
	// The toolchain flags must match the guest the host actually boots. The
	// CLI injects an arm64 guest-init on Apple Silicon and an amd64 one on
	// linux/amd64, so the baseline guest-init has to use the same guest arch
	// (runtime.GOARCH) or the baseline VM cannot boot it.
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("walk benchmark: baseline guest-init build from %s failed: %v\n%s", walkBenchBaselineCommit, err, out)
	}
	require.Greaterf(t, walkBenchFileSize(t, guestInit), int64(0), "baseline guest-init is empty")
	t.Logf("walk benchmark: baseline guest-init %s (%d bytes, commit %s)", guestInit, walkBenchFileSize(t, guestInit), walkBenchBaselineCommit)
	return guestInit
}

// TestWalkBenchBaselineGuestInitArch pins the defect this guard hit on Apple
// Silicon: buildWalkBenchBaselineGuestInit must build the baseline guest-init
// for the guest arch the host boots (runtime.GOARCH), not a hardcoded amd64.
// Parsing the ELF header needs no VM, so a regression fails here even on a host
// where the benchmark itself has to skip.
func TestWalkBenchBaselineGuestInitArch(t *testing.T) {
	repoRoot := walkBenchRepoRoot(t)
	guestInit := buildWalkBenchBaselineGuestInit(t, repoRoot)

	f, err := elf.Open(guestInit)
	require.NoErrorf(t, err, "walk benchmark: open baseline guest-init ELF %s", guestInit)
	t.Cleanup(func() { _ = f.Close() })

	var wantMachine elf.Machine
	switch runtime.GOARCH {
	case "amd64":
		wantMachine = elf.EM_X86_64 // 62
	case "arm64":
		wantMachine = elf.EM_AARCH64 // 183
	default:
		t.Skipf("walk benchmark: unsupported host arch %s", runtime.GOARCH)
	}

	require.Equalf(t, wantMachine, f.Machine,
		"walk benchmark: baseline guest-init ELF machine = %d (%s), want %d (%s) for runtime.GOARCH=%s",
		f.Machine, f.Machine, wantMachine, wantMachine, runtime.GOARCH)
}

// walkBenchFixedGuestInit resolves the guest-init that the uncustomised CLI
// injects (the binary next to MATCHLOCK_BIN, or the cached one), so the test can
// prove the two VMs really ran different binaries.
func walkBenchFixedGuestInit(t *testing.T) string {
	t.Helper()

	bin := matchlockBin(t)
	if resolved, err := exec.LookPath(bin); err == nil {
		bin = resolved
	}
	if abs, err := filepath.Abs(bin); err == nil {
		bin = abs
	}

	candidates := []string{
		filepath.Join(filepath.Dir(bin), "guest-init"),
		filepath.Join(os.Getenv("HOME"), ".cache", "matchlock", "guest-init"),
	}
	for _, candidate := range candidates {
		if fi, err := os.Stat(candidate); err == nil && fi.Size() > 0 {
			t.Logf("walk benchmark: fixed guest-init %s (%d bytes)", candidate, fi.Size())
			return candidate
		}
	}
	t.Fatalf("walk benchmark: fixed guest-init not found next to %s (candidates: %v)", bin, candidates)
	return ""
}

// createWalkBenchGitTree creates a clean git working tree of fileCount small
// files so `git status --porcelain` has a realistic metadata walk to perform,
// then warms the host page cache so both guest variants start from the same
// host state.
func createWalkBenchGitTree(t *testing.T, dir string, fileCount int) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0755))

	run := func(name string, args ...string) {
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("walk benchmark: %s %v failed: %v\n%s", name, args, err, out)
		}
	}

	run("git", "init", "-q")
	run("git", "config", "user.email", "walk-bench@matchlock.local")
	run("git", "config", "user.name", "Matchlock Walk Benchmark")

	for i := 0; i < fileCount; i++ {
		sub := filepath.Join(dir, fmt.Sprintf("d%03d", i%100))
		require.NoError(t, os.MkdirAll(sub, 0755))
		p := filepath.Join(sub, fmt.Sprintf("f%05d.txt", i))
		require.NoError(t, os.WriteFile(p, []byte(fmt.Sprintf("walk-bench file %d\n", i)), 0644))
	}
	run("git", "add", "-A")
	run("git", "commit", "-q", "-m", "walk-bench tree")

	// Warm the host page cache so the cold guest run is warm on the host for
	// both variants.
	warm := exec.Command("git", "status", "--porcelain")
	warm.Dir = dir
	_, _ = warm.Output()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "read walk-bench tree")
	t.Logf("walk benchmark: git tree ready at %s (%d top-level entries, ~%d files)", dir, len(entries), fileCount)
}

func walkBenchFileSize(t *testing.T, p string) int64 {
	t.Helper()
	fi, err := os.Stat(p)
	require.NoErrorf(t, err, "stat %s", p)
	return fi.Size()
}

// startWalkBenchVM launches a detached VM with the host tree mounted at
// /workspace/m as host_fs and waits for `matchlock exec` readiness. env is
// appended to the CLI environment (used to select the baseline guest-init). The
// VM is always killed and removed in Cleanup.
func startWalkBenchVM(t *testing.T, hostDir, image string, env []string) *hostFSCoherenceVM {
	t.Helper()

	stdout, stderr, exitCode := runCLIEnvWithTimeout(
		t,
		3*time.Minute,
		env,
		"run",
		"--image", image,
		"--cpus", "2",
		"--memory", "1024",
		"--workspace", "/workspace",
		"--no-network",
		"--timeout", "1800",
		"-d",
		"-v", hostDir+":"+hostFSCoherenceGuestMount+":host_fs",
		"--", "sh", "-c", "sleep 1500",
	)
	require.Equalf(t, 0, exitCode, "detached walk-bench VM launch failed\nstdout: %s\nstderr: %s", stdout, stderr)
	vmID := strings.TrimSpace(stdout)
	require.Truef(t, strings.HasPrefix(vmID, "vm-"), "unexpected detached VM id %q (stderr: %s)", vmID, stderr)

	vm := &hostFSCoherenceVM{t: t, id: vmID}
	t.Cleanup(func() { hostFSCoherenceRemoveVM(t, vmID) })

	waitForDetachedVMExecReady(t, vmID)
	return vm
}

// guestExecFusedSize returns the size of the guest-fused binary the VM booted
// with. guest-init injects the same binary as /opt/matchlock/guest-fused, so
// the size proves which guest-init variant the VM is running.
func (vm *hostFSCoherenceVM) guestExecFusedSize(t *testing.T) int64 {
	t.Helper()
	stdout, stderr, code := runCLIWithTimeout(
		t,
		30*time.Second,
		"exec", vm.id, "--", "stat", "-c", "%s", "/opt/matchlock/guest-fused",
	)
	require.Equalf(t, 0, code, "stat /opt/matchlock/guest-fused failed: %s", strings.TrimSpace(stderr))
	size, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	require.NoErrorf(t, err, "parse guest-fused size %q", stdout)
	return size
}

// walkBenchTimings is the result of one variant's `git status --porcelain`
// measurement: one cold run plus walkBenchWarmRuns warm runs.
type walkBenchTimings struct {
	cold time.Duration
	warm []time.Duration
	best time.Duration
}

// measureWalk runs one cold and three warm `git status --porcelain` runs inside
// the guest and parses the per-run elapsed times. Timing happens inside the
// guest (bash $EPOCHREALTIME) so `matchlock exec` overhead is not counted. The
// command runs to completion; a timeout or a failed run fails the test rather
// than silently skipping.
func (vm *hostFSCoherenceVM) measureWalk(t *testing.T) walkBenchTimings {
	t.Helper()

	script := `
set -e
git config --global --add safe.directory '*' 2>/dev/null || true
cd /workspace/m
s=$EPOCHREALTIME
git --no-optional-locks status --porcelain >/dev/null
e=$EPOCHREALTIME
awk -v s="$s" -v e="$e" 'BEGIN{printf "WALK_COLD %.6f\n", e-s}'
for i in 1 2 3; do
  s=$EPOCHREALTIME
  git --no-optional-locks status --porcelain >/dev/null
  e=$EPOCHREALTIME
  awk -v s="$s" -v e="$e" -v i="$i" 'BEGIN{printf "WALK_RUN %d %.6f\n", i, e-s}'
done
echo WALK_DONE
`

	stdout, stderr, code := runCLIWithTimeout(t, 10*time.Minute, "exec", vm.id, "--", "bash", "-c", script)
	require.Equalf(t, 0, code, "walk benchmark guest exec failed (exit %d)\nstdout: %s\nstderr: %s", code, stdout, strings.TrimSpace(stderr))
	require.Containsf(t, stdout, "WALK_DONE", "walk benchmark did not complete:\nstdout: %s\nstderr: %s", stdout, stderr)

	result := walkBenchTimings{}
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "WALK_COLD":
			require.Len(t, fields, 2)
			result.cold = walkBenchParseSeconds(t, fields[1])
		case "WALK_RUN":
			require.Len(t, fields, 3)
			result.warm = append(result.warm, walkBenchParseSeconds(t, fields[2]))
		}
	}
	require.Lenf(t, result.warm, walkBenchWarmRuns, "expected %d warm runs, parsed %d from:\n%s", walkBenchWarmRuns, len(result.warm), stdout)

	result.best = result.warm[0]
	for _, d := range result.warm[1:] {
		if d < result.best {
			result.best = d
		}
	}
	return result
}

func walkBenchParseSeconds(t *testing.T, s string) time.Duration {
	t.Helper()
	seconds, err := strconv.ParseFloat(s, 64)
	require.NoErrorf(t, err, "parse timing %q", s)
	return time.Duration(seconds * float64(time.Second))
}

// TestHostFSWalkBenchmark is the walk benchmark guard from the host_fs
// coherence task: the fixed guest-init must not make `git status --porcelain`
// over a ~5000-file host_fs tree more than 10% slower than the pre-fix
// baseline.
func TestHostFSWalkBenchmark(t *testing.T) {
	repoRoot := walkBenchRepoRoot(t)
	baselineGuestInit := buildWalkBenchBaselineGuestInit(t, repoRoot)
	fixedGuestInit := walkBenchFixedGuestInit(t)

	hostDir := hostFSCoherenceBacking(t)
	// The repo root is the mount root (/workspace/m) so `cd /workspace/m`
	// inside the guest lands in the git working tree.
	createWalkBenchGitTree(t, hostDir, walkBenchFileCount)

	baselineSize := walkBenchFileSize(t, baselineGuestInit)
	fixedSize := walkBenchFileSize(t, fixedGuestInit)
	require.NotEqualf(t, baselineSize, fixedSize, "baseline and fixed guest-init are the same size; the differential would be meaningless")

	// Run the two variants sequentially over the same host tree. The baseline
	// VM is removed before the fixed VM boots so only one heavy VM is live.
	baselineVM := startWalkBenchVM(t, hostDir, walkBenchImage, []string{"MATCHLOCK_GUEST_INIT=" + baselineGuestInit})
	require.Equalf(t, baselineSize, baselineVM.guestExecFusedSize(t),
		"baseline VM did not boot the baseline guest-init (expected %d bytes)", baselineSize)
	baseline := baselineVM.measureWalk(t)
	hostFSCoherenceRemoveVM(t, baselineVM.id)

	// Point the fixed VM explicitly at the resolved fixed guest-init so an
	// ambient MATCHLOCK_GUEST_INIT cannot make the differential meaningless.
	fixedVM := startWalkBenchVM(t, hostDir, walkBenchImage, []string{"MATCHLOCK_GUEST_INIT=" + fixedGuestInit})
	require.Equalf(t, fixedSize, fixedVM.guestExecFusedSize(t),
		"fixed VM did not boot the fixed guest-init (expected %d bytes)", fixedSize)
	fixed := fixedVM.measureWalk(t)

	t.Logf("walk benchmark baseline: cold=%s warm=%v best=%s", baseline.cold, baseline.warm, baseline.best)
	t.Logf("walk benchmark fixed:    cold=%s warm=%v best=%s", fixed.cold, fixed.warm, fixed.best)

	ratio := float64(fixed.best) / float64(baseline.best)
	limit := time.Duration(float64(baseline.best) * walkBenchMaxSlowdown)
	t.Logf("walk benchmark: fixed best=%s baseline best=%s ratio=%.3f limit=%s (%.0f%%)",
		fixed.best, baseline.best, ratio, limit, walkBenchMaxSlowdown*100)

	require.Greaterf(t, baseline.best, time.Duration(0), "baseline walk timing was zero")
	require.LessOrEqualf(t, fixed.best, limit,
		"fixed guest-init walk is more than %.0f%% slower than baseline: fixed=%s baseline=%s ratio=%.3f",
		(walkBenchMaxSlowdown-1)*100, fixed.best, baseline.best, ratio)
}
