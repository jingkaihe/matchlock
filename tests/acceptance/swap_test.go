//go:build acceptance

package acceptance

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Live acceptance coverage for `matchlock run --swap`.
//
// The tests boot real Firecracker/QEMU guests on the host. They use
// --no-network throughout: swap is a block-device feature that is independent
// of networking, and --no-network avoids TAP creation, so the suite also runs
// in a NoNewPrivs harness that cannot acquire CAP_NET_ADMIN. That is the same
// harness property the existing TestCLIRPCExec test documents.
const (
	swapAcceptanceImage = "igorhvr/bedlam-ubuntu"

	// swapBootTimeout bounds a single detached `matchlock run` plus the wait
	// for its exec relay socket to come up.
	swapBootTimeout = 3 * time.Minute

	// swapExecTimeout bounds a short one-shot inspection exec (cat /proc/swaps,
	// free). The pressure sampler deliberately uses the shorter
	// swapSampleTimeout so one slow cold exec on an emulated host cannot
	// consume the whole swap hold window.
	swapExecTimeout = 45 * time.Second

	// swapSampleTimeout bounds exactly one /proc/swaps pressure sample. The
	// pressure program holds its allocation for only 30s, so the sampler must
	// complete several polls inside that window even on a slow (TCG-emulated)
	// host; a 45s blocking sample could outlast the hold and report zero.
	swapSampleTimeout = 10 * time.Second

	// swapPressureExecTimeout is the pressure workload's own budget. The
	// sampling loop must never give up before this expires.
	swapPressureExecTimeout = 4 * time.Minute

	// swapPressureSampleInterval is the delay between pressure samples.
	swapPressureSampleInterval = time.Second

	// swapPressureSettleMargin is added to the pressure exec budget before the
	// sampling loop gives up, so the loop always outlives the workload it
	// supervises instead of failing early on a slow host.
	swapPressureSettleMargin = 30 * time.Second

	// swapPressureDrainTimeout bounds the final receive of the pressure result
	// after the sampling loop exits, closing the race where the workload
	// finished while a sample was in flight.
	swapPressureDrainTimeout = 15 * time.Second

	// swapPressureBytes is deliberately larger than the 256 MB guest RAM used
	// by the pressure test so the touched pages cannot all stay resident.
	swapPressureBytes = 384 * 1024 * 1024

	swapRequestedMB = 512
)

// swapPressureProgram allocates swapPressureBytes, touches one byte on every
// 4 KiB page (so every page is materialised, not just reserved), holds the
// allocation for 30s, and only then prints. Touching every page is what forces
// real page-out; a large but untouched or immediately-freed allocation could be
// satisfied from reclaimable page cache and would prove nothing.
const swapPressureProgram = `import time
n = 384 * 1024 * 1024
b = bytearray(n)
for i in range(0, n, 4096):
    b[i] = 1
time.sleep(30)
print("pressure-ok", b[0], b[n-4096])
`

type swapExecResult struct {
	stdout string
	stderr string
	code   int
}

// startSwapVM boots a detached, persistent sandbox with --no-network and
// returns its VM id after the exec relay is ready. Every VM it creates is
// removed by t.Cleanup, and only by its exact id.
func startSwapVM(t *testing.T, args ...string) string {
	t.Helper()

	runArgs := append([]string{
		"run",
		"-d",
		"--rm=false",
		"--no-network",
		"--image", swapAcceptanceImage,
	}, args...)

	stdout, stderr, code := runCLIWithTimeout(t, swapBootTimeout, runArgs...)
	require.Equalf(t, 0, code, "detached run failed (exit=%d): stdout=%s stderr=%s", code, stdout, stderr)

	id := strings.TrimSpace(stdout)
	require.Truef(t, strings.HasPrefix(id, "vm-"), "unexpected VM id %q (stderr: %s)", stdout, stderr)

	t.Cleanup(func() { cleanupSwapVM(t, id) })
	waitForSwapExecReady(t, id)
	return id
}

// waitForSwapExecReady polls `matchlock exec <id> true` until the guest agent
// answers. A detached `run` prints the id as soon as the VM state row exists,
// which can precede the exec relay by a couple of seconds.
func waitForSwapExecReady(t *testing.T, id string) {
	t.Helper()

	deadline := time.Now().Add(swapBootTimeout)
	for time.Now().Before(deadline) {
		if _, _, code := runCLIWithTimeout(t, 30*time.Second, "exec", id, "true"); code == 0 {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.FailNowf(t, "sandbox never became exec-ready", "vm=%s", id)
}

// cleanupSwapVM stops the sandbox and removes its state. `matchlock rm` is
// attempted first, but under a NoNewPrivs harness it cannot reconcile the
// nftables ruleset ("netlink receive: operation not permitted") and leaves the
// stopped row behind; state.Manager.Remove is the documented bypass.
func cleanupSwapVM(t *testing.T, id string) {
	t.Helper()
	if id == "" {
		return
	}

	_, _, _ = runCLI(t, "kill", id)

	stateMgr := state.NewManager()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := stateMgr.Remove(id); err == nil {
			return
		}
		if _, statErr := os.Stat(stateMgr.Dir(id)); os.IsNotExist(statErr) {
			return
		}
		if time.Now().After(deadline) {
			t.Logf("cleanup: giving up removing VM %s", id)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// swapHostImagePath returns the host path of the per-VM swap image. It is
// derived from the same state manager the CLI uses, so it follows HOME.
func swapHostImagePath(id string) string {
	return state.NewManager().Dir(id) + "/swap.raw"
}

// execSwapCommand runs `matchlock exec <id> -- <guestArgs...>` in its own
// process without any *testing.T helpers, so it is safe to call from a
// goroutine. It returns -1 on start failure or timeout.
func execSwapCommand(bin, id string, timeout time.Duration, guestArgs ...string) swapExecResult {
	return execSwapCommandAs(bin, id, "", timeout, guestArgs...)
}

// execSwapCommandAs is execSwapCommand with an optional `exec -u <user>` flag.
// user is empty for the default user. It is used by the root-denial test to run
// the same probe as uid 0 and as a non-root uid.
func execSwapCommandAs(bin, id, user string, timeout time.Duration, guestArgs ...string) swapExecResult {
	args := []string{"exec", id}
	if user != "" {
		args = append(args, "-u", user)
	}
	args = append(args, "--")
	args = append(args, guestArgs...)
	cmd := exec.Command(bin, args...)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return swapExecResult{stderr: err.Error(), code: -1}
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		code := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			} else {
				code = -1
			}
		}
		return swapExecResult{stdout: stdout.String(), stderr: stderr.String(), code: code}
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return swapExecResult{stdout: stdout.String(), stderr: stderr.String(), code: -1}
	}
}

// swapUsedKB reads the used-kB column of /proc/swaps (0 when there is no
// swap or the sample cannot be read). Swap is kernel-global, so any exec sees
// the swap PID 1 enabled at boot.
func swapUsedKB(bin, id string) int {
	return swapUsedKBWithTimeout(bin, id, swapExecTimeout)
}

// swapUsedKBWithTimeout is swapUsedKB with a caller-supplied per-sample timeout.
// The pressure loop passes swapSampleTimeout so a single slow sample cannot
// block past the short window in which the workload holds its allocation.
func swapUsedKBWithTimeout(bin, id string, timeout time.Duration) int {
	res := execSwapCommand(bin, id, timeout, "sh", "-c", "awk 'NR>1{print $4}' /proc/swaps")
	if res.code != 0 {
		return 0
	}
	fields := strings.Fields(strings.TrimSpace(res.stdout))
	if len(fields) == 0 {
		return 0
	}
	used, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0
	}
	return used
}

// swapTotalKB parses the total column of `free -k`'s "Swap:" line.
func swapTotalKB(bin, id string) int {
	res := execSwapCommand(bin, id, swapExecTimeout, "sh", "-c", "free -k | awk '/Swap:/{print $2}'")
	if res.code != 0 {
		return -1
	}
	fields := strings.Fields(strings.TrimSpace(res.stdout))
	if len(fields) == 0 {
		return -1
	}
	total, err := strconv.Atoi(fields[0])
	if err != nil {
		return -1
	}
	return total
}

// TestSwapVisibility proves the guest actually has the swap device PID 1
// enabled: /proc/swaps lists it and `free` reports the requested total.
func TestSwapVisibility(t *testing.T) {
	id := startSwapVM(t, "--memory", "512", "--swap", strconv.Itoa(swapRequestedMB), "--", "sleep", "600")

	stdout, stderr, code := runCLIWithTimeout(t, swapExecTimeout, "exec", id, "cat", "/proc/swaps")
	require.Equalf(t, 0, code, "cat /proc/swaps failed: stdout=%s stderr=%s", stdout, stderr)
	assert.Regexpf(t, `(?m)^/dev/vd[a-z]+\s+partition\s+\d+`, stdout,
		"/proc/swaps should list the swap block device: %s", stdout)

	// 512 MiB = 524288 KiB; the kernel reports 4 KiB less (one header page).
	total := swapTotalKB(matchlockBin(t), id)
	require.GreaterOrEqual(t, total, swapRequestedMB*1024-16, "free should report the requested swap total: %d", total)
	require.LessOrEqual(t, total, swapRequestedMB*1024, "free should report the requested swap total: %d", total)
}

// TestSwapNegativeWithoutSwap proves swap is opt-in: with no --swap the guest
// has no swap backing store at all.
func TestSwapNegativeWithoutSwap(t *testing.T) {
	id := startSwapVM(t, "--memory", "512", "--", "sleep", "600")

	stdout, stderr, code := runCLIWithTimeout(t, swapExecTimeout, "exec", id, "cat", "/proc/swaps")
	require.Equalf(t, 0, code, "cat /proc/swaps failed: stdout=%s stderr=%s", stdout, stderr)
	assert.NotContainsf(t, stdout, "/dev/", "/proc/swaps should have no entries without --swap: %s", stdout)

	total := swapTotalKB(matchlockBin(t), id)
	require.Equalf(t, 0, total, "free should report zero swap without --swap: %d", total)
}

// TestSwapPressure proves the swap device is genuinely usable, not merely
// visible: a workload that touches more memory than the guest has RAM survives
// because pages are swapped out, and the same workload without --swap fails.
func TestSwapPressure(t *testing.T) {
	bin := matchlockBin(t)

	t.Run("with-swap", func(t *testing.T) {
		id := startSwapVM(t, "--memory", "256", "--swap", strconv.Itoa(swapRequestedMB), "--", "sleep", "600")

		pressureDone := make(chan swapExecResult, 1)
		go func() {
			pressureDone <- execSwapCommand(bin, id, swapPressureExecTimeout, "python3", "-c", swapPressureProgram)
		}()

		// Sample /proc/swaps from a dedicated goroutine with a short per-sample
		// timeout. The workload holds its allocation for only 30s, so a single
		// blocking 45s sample could outlast the hold and leave maxUsedKB == 0
		// even though swap was genuinely used; short, repeated polls make the
		// observation robust to a slow (TCG-emulated) host.
		samples := make(chan int, 1)
		stopSampling := make(chan struct{})
		samplerDone := make(chan struct{})
		go func() {
			defer close(samplerDone)
			ticker := time.NewTicker(swapPressureSampleInterval)
			defer ticker.Stop()
			for {
				used := swapUsedKBWithTimeout(bin, id, swapSampleTimeout)
				select {
				case samples <- used:
				default:
				}
				select {
				case <-ticker.C:
				case <-stopSampling:
					return
				}
			}
		}()

		maxUsedKB := 0
		var result swapExecResult
		finished := false
		// The loop must outlive the workload's own budget. The old 3-minute
		// deadline watched a 4-minute workload, so a slow host could abort the
		// sampling loop while the workload was still legitimately running.
		deadline := time.Now().Add(swapPressureExecTimeout + swapPressureSettleMargin)
		for !finished && time.Now().Before(deadline) {
			select {
			case result = <-pressureDone:
				finished = true
			case used := <-samples:
				if used > maxUsedKB {
					maxUsedKB = used
				}
			case <-time.After(swapPressureSampleInterval):
				// Keep the loop responsive even if a sample is slow or absent.
			}
		}
		close(stopSampling)
		<-samplerDone

		// Close the race where the workload finished while a sample was in
		// flight: drain the result once more before failing.
		if !finished {
			select {
			case result = <-pressureDone:
				finished = true
			case <-time.After(swapPressureDrainTimeout):
			}
		}

		require.Truef(t, finished, "pressure workload did not finish within %s", swapPressureExecTimeout)
		require.Equalf(t, 0, result.code,
			"workload touching %d bytes should survive with 256 MB RAM + swap: stdout=%s stderr=%s",
			swapPressureBytes, result.stdout, result.stderr)
		assert.Containsf(t, result.stdout, "pressure-ok", "pressure workload output: %s", result.stdout)
		assert.Greaterf(t, maxUsedKB, 0,
			"swap must be used (used > 0) while the workload holds %d bytes in a 256 MB guest; peak used=%d kB",
			swapPressureBytes, maxUsedKB)
	})

	t.Run("without-swap", func(t *testing.T) {
		id := startSwapVM(t, "--memory", "256", "--", "sleep", "600")

		result := execSwapCommand(bin, id, 3*time.Minute, "python3", "-c", swapPressureProgram)
		require.NotEqualf(t, 0, result.code,
			"workload touching %d bytes in a 256 MB guest without swap should be OOM-killed or fail: stdout=%s stderr=%s",
			swapPressureBytes, result.stdout, result.stderr)
	})
}

// TestSwapTeardownRemovesImage proves the per-VM swap image does not leak: it
// exists while the sandbox runs and is gone after the sandbox is stopped and
// removed. The file teardown is driven by the sandbox Close path (triggered by
// `matchlock kill`, which stops the detached run process); `matchlock rm` then
// removes the VM state. Under a NoNewPrivs harness `rm` cannot reconcile the
// nftables ruleset, so its exit code is not asserted here.
func TestSwapTeardownRemovesImage(t *testing.T) {
	id := startSwapVM(t, "--memory", "512", "--swap", strconv.Itoa(swapRequestedMB), "--", "sleep", "600")

	swapPath := swapHostImagePath(id)
	require.FileExistsf(t, swapPath, "swap image should exist while the sandbox runs")

	_, _, _ = runCLI(t, "kill", id)
	require.Eventuallyf(t, func() bool {
		_, err := os.Stat(swapPath)
		return os.IsNotExist(err)
	}, 30*time.Second, 300*time.Millisecond, "swap image %s was not removed when the sandbox stopped", swapPath)

	_, _, _ = runCLI(t, "rm", id)
	_, err := os.Stat(swapPath)
	assert.Truef(t, os.IsNotExist(err), "swap image %s should be gone after matchlock rm: err=%v", swapPath, err)
}

// swapGuestDevice returns the guest path of the active swap device.
func swapGuestDevice(t *testing.T, bin, id string) string {
	t.Helper()
	res := execSwapCommand(bin, id, swapExecTimeout, "sh", "-c", "awk 'NR>1{print $1}' /proc/swaps")
	require.Equalf(t, 0, res.code, "read /proc/swaps: stdout=%s stderr=%s", res.stdout, res.stderr)
	dev := strings.TrimSpace(res.stdout)
	require.Truef(t, strings.HasPrefix(dev, "/dev/vd"), "unexpected swap device %q", dev)
	return dev
}

// assertSwapDenied requires a deny with EACCES/EPERM (not ENOENT), so a missing
// or detached policy cannot masquerade as a pass.
func assertSwapDenied(t *testing.T, stderr, what string) {
	t.Helper()
	lower := strings.ToLower(stderr)
	assert.Truef(t,
		strings.Contains(lower, "permission") || strings.Contains(lower, "not permitted"),
		"%s must be denied with EACCES/EPERM, got stderr=%q", what, stderr)
}

// TestSwapRootDenied proves the cgroup v2 device policy, not DAC ownership, is
// the swap-store boundary. root-in-workload owns the 0600 node and also holds
// CAP_MKNOD, so before the policy it could read the VM-wide store through the
// node or a freshly mknod'ed one. This asserts both paths now fail for uid 0,
// that a non-root uid stays blocked, that CAP_BPF is dropped, and that an
// unrelated device and active swap are unaffected.
func TestSwapRootDenied(t *testing.T) {
	bin := matchlockBin(t)
	id := startSwapVM(t, "--memory", "512", "--swap", strconv.Itoa(swapRequestedMB), "--", "sleep", "600")

	dev := swapGuestDevice(t, bin, id)
	deviceName := strings.TrimPrefix(dev, "/dev/")

	// /sys/block/<dev>/dev exposes the decoded major:minor; the policy denies
	// exactly this pair no matter how the node is named or recreated.
	sysfsRes := execSwapCommand(bin, id, swapExecTimeout, "cat", "/sys/block/"+deviceName+"/dev")
	require.Equalf(t, 0, sysfsRes.code, "read /sys/block/%s/dev: stdout=%s stderr=%s", deviceName, sysfsRes.stdout, sysfsRes.stderr)
	parts := strings.SplitN(strings.TrimSpace(sysfsRes.stdout), ":", 2)
	require.Len(t, parts, 2, "unexpected /sys/block %s/dev value %q", deviceName, sysfsRes.stdout)
	major, minor := parts[0], parts[1]

	// uid 0: opening the existing root:root 0600 node must now be denied even
	// though uid 0 is the owner.
	rootExisting := execSwapCommandAs(bin, id, "0", swapExecTimeout, "dd", "if="+dev, "of=/dev/null", "bs=4096", "count=1")
	require.NotEqualf(t, 0, rootExisting.code, "uid0 read of %s must fail", dev)
	assertSwapDenied(t, rootExisting.stderr, "uid0 open of existing swap node")

	// uid 0: recreate a node from the major:minor and read it. mknod must be
	// denied; if a future change ever let mknod through, the open of the
	// recreated node must still be denied.
	mknodRes := execSwapCommandAs(bin, id, "0", swapExecTimeout,
		"sh", "-c",
		"rm -f /tmp/swaprepro; mknod /tmp/swaprepro b "+major+" "+minor+" && dd if=/tmp/swaprepro of=/dev/null bs=4096 count=1")
	require.NotEqualf(t, 0, mknodRes.code, "uid0 mknod+read must fail: stdout=%s stderr=%s", mknodRes.stdout, mknodRes.stderr)
	assertSwapDenied(t, mknodRes.stderr, "uid0 mknod/open of recreated swap node")

	// non-root: the 0600 owner/match denial must remain in place.
	nonRoot := execSwapCommandAs(bin, id, "1000", swapExecTimeout, "dd", "if="+dev, "of=/dev/null", "bs=4096", "count=1")
	require.NotEqualf(t, 0, nonRoot.code, "uid1000 read of %s must fail", dev)
	assertSwapDenied(t, nonRoot.stderr, "uid1000 open of swap node")

	// The policy denies exactly the swap major:minor, so an unrelated device
	// must stay usable.
	ok := execSwapCommandAs(bin, id, "0", swapExecTimeout, "dd", "if=/dev/zero", "of=/dev/null", "bs=4096", "count=1")
	require.Equalf(t, 0, ok.code, "uid0 /dev/zero must stay usable: stdout=%s stderr=%s", ok.stdout, ok.stderr)

	// Active swap must be unaffected by a policy that denies userspace opens.
	assert.Greaterf(t, swapTotalKB(bin, id), 0, "swap must remain active after the policy is attached")

	// Tamper resistance: without CAP_BPF (and with CAP_SYS_ADMIN already
	// dropped) the workload cannot bpf(BPF_PROG_DETACH) the attached policy.
	bndRes := execSwapCommandAs(bin, id, "0", swapExecTimeout, "sh", "-c", "awk '/CapBnd/{print $2}' /proc/self/status")
	require.Equalf(t, 0, bndRes.code, "read CapBnd: stdout=%s stderr=%s", bndRes.stdout, bndRes.stderr)
	bnd, err := strconv.ParseUint(strings.TrimSpace(bndRes.stdout), 16, 64)
	require.NoErrorf(t, err, "parse CapBnd %q", bndRes.stdout)
	assert.Zerof(t, bnd&(uint64(1)<<39), "CAP_BPF (bit 39) must be dropped from the workload bounding set: %#x", bnd)
}
