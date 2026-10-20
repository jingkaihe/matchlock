//go:build acceptance

package acceptance

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/state"
	"github.com/stretchr/testify/require"
)

// host_fs coherence acceptance tests.
//
// The guest FUSE daemon keys its nodes by a namespaced host (dev, ino) number
// without a generation and addresses the host through a cached path (see
// internal/guestruntime/fused/main.go). A host-side delete+recreate, atomic
// write-temp+rename, or rename therefore used to leave a cached node pointing
// at a dead path (open/readdir -> ENOENT) or, across file<->directory reuse, a
// stale type (EIO) until the guest kernel FORGET the node. The fused fix
// validates the cached path and type before reuse.
//
// These tests drive that end to end with real VMs:
//
//  1. start a detached VM over a host_fs volume,
//  2. prime the guest view with `matchlock exec` (LOOKUP caches the node),
//  3. mutate the host directory,
//  4. observe the guest view again and require it to converge within a bounded
//     window (EntryTimeout/AttrTimeout are 1s), never using drop_caches.
//
// The backing directory lives on a filesystem that reuses freed inode numbers
// (ext4/xfs) because the persistent delete+recreate case depends on the host
// handing a freed inode to the new object; t.TempDir() is tmpfs on the Linux
// host and allocates a fresh inode for every object.

const hostFSCoherenceGuestMount = "/workspace/m"

// hostFSCoherenceBacking returns a host directory on a reuse-capable
// filesystem (/var/tmp is ext4 on the Firecracker host). It falls back to
// os.TempDir() when /var/tmp is unavailable.
func hostFSCoherenceBacking(t *testing.T) string {
	t.Helper()
	for _, base := range []string{"/var/tmp", os.TempDir()} {
		dir, err := os.MkdirTemp(base, "matchlock-hostfs-coherence-*")
		if err != nil {
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return dir
	}
	require.FailNow(t, "no writable host backing directory (tried /var/tmp and os.TempDir())")
	return ""
}

func hostFSCoherenceGuestPath(name string) string {
	return path.Join(hostFSCoherenceGuestMount, name)
}

// hostFSCoherenceIno returns the raw host inode number for p.
func hostFSCoherenceIno(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Stat(p)
	require.NoErrorf(t, err, "stat host path %s", p)
	st, ok := fi.Sys().(*syscall.Stat_t)
	require.Truef(t, ok, "host stat for %s has no *syscall.Stat_t", p)
	return st.Ino
}

type hostFSCoherenceVM struct {
	t  *testing.T
	id string
}

// startHostFSCoherenceVM starts a detached Alpine VM whose /workspace/m is the
// host directory mounted as host_fs, and waits until `matchlock exec` works.
// The VM is always killed and removed in Cleanup.
func startHostFSCoherenceVM(t *testing.T, hostDir string) *hostFSCoherenceVM {
	t.Helper()
	return startHostFSCoherenceVMScript(t, hostDir, "sleep 600")
}

// startHostFSCoherenceVMScript is startHostFSCoherenceVM with a caller-supplied
// long-lived entrypoint, used by the append-held-fd case whose process must keep
// a file descriptor open across a host mutation. The VM is always killed and
// removed in Cleanup.
func startHostFSCoherenceVMScript(t *testing.T, hostDir, script string) *hostFSCoherenceVM {
	t.Helper()

	stdout, stderr, exitCode := runCLIWithTimeout(
		t,
		2*time.Minute,
		"run",
		"--image", "alpine:latest",
		"--workspace", "/workspace",
		"--no-network",
		"-d",
		"-v", hostDir+":"+hostFSCoherenceGuestMount+":host_fs",
		"--", "sh", "-c", script,
	)
	require.Equalf(t, 0, exitCode, "detached host_fs VM launch failed\nstdout: %s\nstderr: %s", stdout, stderr)
	vmID := strings.TrimSpace(stdout)
	require.Truef(t, strings.HasPrefix(vmID, "vm-"), "unexpected detached VM id %q (stderr: %s)", vmID, stderr)

	vm := &hostFSCoherenceVM{t: t, id: vmID}
	t.Cleanup(func() { hostFSCoherenceRemoveVM(t, vmID) })

	waitForDetachedVMExecReady(t, vmID)
	return vm
}

// hostFSCoherenceRemoveVM kills and removes the VM, then asserts `matchlock
// list` no longer shows it. kill/rm are best-effort so a cleanup failure never
// masks the test's real assertion.
func hostFSCoherenceRemoveVM(t *testing.T, vmID string) {
	t.Helper()
	_, _, _ = runCLI(t, "kill", vmID)

	// Under a NoNewPrivs sandbox `matchlock rm` fails to reconcile the
	// nftables rule ("netlink receive: operation not permitted") and leaves the
	// stopped row behind. state.Manager.Remove bypasses the reconciler (the
	// documented workaround) and still deletes the row and the VM directory.
	stateMgr := state.NewManager()

	deadline := time.Now().Add(30 * time.Second)
	for {
		_, _, _ = runCLI(t, "rm", vmID)
		if !hostFSCoherenceVMPresent(t, vmID) {
			return
		}
		if err := stateMgr.Remove(vmID); err != nil {
			t.Logf("state.Remove(%s): %v", vmID, err)
		} else if !hostFSCoherenceVMPresent(t, vmID) {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("VM %s is still listed by `matchlock list` after removal", vmID)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func hostFSCoherenceVMPresent(t *testing.T, vmID string) bool {
	t.Helper()
	listOut, _, _ := runCLI(t, "list")
	return strings.Contains(listOut, vmID)
}

func (vm *hostFSCoherenceVM) mustExec(timeout time.Duration, script string) {
	vm.t.Helper()
	stdout, stderr, code := runCLIWithTimeout(vm.t, timeout, "exec", vm.id, "--", "sh", "-c", script)
	require.Equalf(vm.t, 0, code, "guest exec failed (exit %d)\nscript: %s\nstdout: %s\nstderr: %s", code, script, stdout, stderr)
}

// primeFile caches the guest's view of a host file through a lookup (cat).
func (vm *hostFSCoherenceVM) primeFile(name string) {
	vm.t.Helper()
	vm.mustExec(30*time.Second, "cat '"+hostFSCoherenceGuestPath(name)+"' >/dev/null")
}

// waitGuestFileContent polls `cat <guestPath>` until it returns want
// (whitespace-trimmed) for up to timeout, absorbing the bounded 1s
// EntryTimeout/AttrTimeout window without ever using drop_caches.
func (vm *hostFSCoherenceVM) waitGuestFileContent(guestPath, want string, timeout time.Duration) {
	vm.t.Helper()
	deadline := time.Now().Add(timeout)
	var got, stderr string
	var code int
	for {
		var stdout string
		stdout, stderr, code = runCLIWithTimeout(vm.t, 20*time.Second, "exec", vm.id, "--", "sh", "-c", "cat '"+guestPath+"' 2>/dev/null")
		got = strings.TrimSpace(stdout)
		if code == 0 && got == want {
			return
		}
		if time.Now().After(deadline) {
			vm.t.Fatalf("guest view of %s never converged to %q; last=%q (exit %d, stderr: %s)",
				guestPath, want, got, code, strings.TrimSpace(stderr))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitGuestFileStat polls `stat -c %F <guestPath>` until it reports wantType
// for up to timeout, absorbing the bounded 1s entry/attr window.
func (vm *hostFSCoherenceVM) waitGuestFileStat(guestPath, wantType string, timeout time.Duration) {
	vm.t.Helper()
	deadline := time.Now().Add(timeout)
	var got, stderr string
	var code int
	for {
		var stdout string
		stdout, stderr, code = runCLIWithTimeout(vm.t, 20*time.Second, "exec", vm.id, "--", "sh", "-c", "stat -c '%F' '"+guestPath+"' 2>/dev/null")
		got = strings.TrimSpace(stdout)
		if code == 0 && got == wantType {
			return
		}
		if time.Now().After(deadline) {
			vm.t.Fatalf("guest stat of %s never reported %q; last=%q (exit %d, stderr: %s)",
				guestPath, wantType, got, code, strings.TrimSpace(stderr))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitGuestDirContains polls `ls -A <guestPath>` until every wanted entry is
// listed, for up to timeout, absorbing the bounded 1s entry window. It is the
// directory counterpart of waitGuestFileStat/Content and fails with the last
// listing when the guest never observes the entries.
func (vm *hostFSCoherenceVM) waitGuestDirContains(guestPath string, want []string, timeout time.Duration) {
	vm.t.Helper()
	deadline := time.Now().Add(timeout)
	var last, stderr string
	var code int
	for {
		var stdout string
		stdout, stderr, code = runCLIWithTimeout(vm.t, 20*time.Second, "exec", vm.id, "--", "sh", "-c", "ls -A '"+guestPath+"' 2>/dev/null")
		last = strings.TrimSpace(stdout)
		if code == 0 && containsAllLines(last, want) {
			return
		}
		if time.Now().After(deadline) {
			vm.t.Fatalf("guest listing of %s never showed %v; last=%q (exit %d, stderr: %s)",
				guestPath, want, last, code, strings.TrimSpace(stderr))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// containsAllLines reports whether every wanted name is an exact line of output
// (ls -A prints one name per line).
func containsAllLines(output string, want []string) bool {
	have := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		have[strings.TrimSpace(line)] = true
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}

// waitHostFileContains polls a host file until its content contains want. It is
// used by the append-held-fd case, where a long-lived guest process reports what
// its held descriptor sees by writing to a file inside the host_fs mount.
func waitHostFileContains(t *testing.T, p, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for {
		data, err := os.ReadFile(p)
		if err == nil {
			last = string(data)
			if strings.Contains(last, want) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("host file %s never contained %q; last=%q (err: %v)", p, want, last, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// appendHostFile appends data to a host file atomically for the reader (one
// O_APPEND write), so the guest observes the whole appended line at once.
func appendHostFile(t *testing.T, p, data string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0644)
	require.NoErrorf(t, err, "open %s for append", p)
	_, err = f.WriteString(data)
	require.NoErrorf(t, err, "append to %s", p)
	require.NoErrorf(t, f.Close(), "close %s", p)
}

// TestHostFSCoherenceDeleteRecreate covers case (a2): the guest has stat'ed the
// victim inode; the host removes the victim and immediately creates a new file,
// which ext4 hands the freed inode number. Pre-fix the new name resolved to the
// cached node and every open/read went to the dead victim path (ENOENT) until
// the kernel FORGET the node. After the fix the cached path is replaced and the
// new object is readable within the bounded window.
func TestHostFSCoherenceDeleteRecreate(t *testing.T) {
	hostDir := hostFSCoherenceBacking(t)
	const (
		victimName      = "victim.txt"
		replacementName = "replacement.txt"
	)
	victimPath := filepath.Join(hostDir, victimName)
	replacementPath := filepath.Join(hostDir, replacementName)

	require.NoError(t, os.WriteFile(victimPath, []byte("old-content\n"), 0644))
	vm := startHostFSCoherenceVM(t, hostDir)

	// Try twice to make the host reuse the freed inode; if it does not, the
	// test still checks guest correctness, just without the reuse trigger.
	reused := false
	for attempt := 1; attempt <= 2 && !reused; attempt++ {
		if _, err := os.Stat(victimPath); err != nil {
			require.NoError(t, os.WriteFile(victimPath, []byte("old-content\n"), 0644))
		}
		vm.primeFile(victimName)
		victimIno := hostFSCoherenceIno(t, victimPath)

		require.NoError(t, os.Remove(victimPath))
		require.NoError(t, os.WriteFile(replacementPath, []byte("new-content\n"), 0644))
		newIno := hostFSCoherenceIno(t, replacementPath)
		if newIno == victimIno {
			reused = true
			t.Logf("host reused inode %d for %s (attempt %d)", victimIno, replacementName, attempt)
			break
		}
		t.Logf("attempt %d: no host inode reuse (victim=%d replacement=%d)", attempt, victimIno, newIno)
		if attempt == 1 {
			// Re-prime for one more chance at reuse.
			require.NoError(t, os.Remove(replacementPath))
		}
	}
	if !reused {
		t.Logf("host filesystem did not reuse the freed inode; the stale-path guard is weaker on this filesystem")
	}

	// Assert past the 1s entry timeout: pre-fix the failure was persistent, not
	// a transient window.
	time.Sleep(1200 * time.Millisecond)
	vm.waitGuestFileStat(hostFSCoherenceGuestPath(replacementName), "regular file", 3*time.Second)
	vm.waitGuestFileContent(hostFSCoherenceGuestPath(replacementName), "new-content", 3*time.Second)
}

// TestHostFSCoherenceAtomicReplaceUnseen covers case (b1): the host writes a
// temp file and renames it over the target before the guest ever looked up the
// temp name. The guest must read the new content and see a regular file.
func TestHostFSCoherenceAtomicReplaceUnseen(t *testing.T) {
	hostDir := hostFSCoherenceBacking(t)
	const (
		targetName = "b.txt"
		tmpName    = ".b.tmp"
	)
	targetPath := filepath.Join(hostDir, targetName)
	tmpPath := filepath.Join(hostDir, tmpName)

	require.NoError(t, os.WriteFile(targetPath, []byte("v1\n"), 0644))
	oldIno := hostFSCoherenceIno(t, targetPath)

	vm := startHostFSCoherenceVM(t, hostDir)
	vm.primeFile(targetName)

	// Atomic replace: write-temp + rename. The guest never saw tmpName.
	require.NoError(t, os.WriteFile(tmpPath, []byte("v2\n"), 0644))
	tmpIno := hostFSCoherenceIno(t, tmpPath)
	require.NoError(t, os.Rename(tmpPath, targetPath))
	newIno := hostFSCoherenceIno(t, targetPath)
	require.Equalf(t, tmpIno, newIno, "rename did not move the temp inode onto %s", targetName)
	t.Logf("%s inode %d -> %d (temp inode %d)", targetName, oldIno, newIno, tmpIno)

	time.Sleep(1200 * time.Millisecond)
	vm.waitGuestFileStat(hostFSCoherenceGuestPath(targetName), "regular file", 3*time.Second)
	vm.waitGuestFileContent(hostFSCoherenceGuestPath(targetName), "v2", 3*time.Second)
}

// TestHostFSCoherenceAtomicReplaceTmpLookedUp covers case (b2): as (b1), but the
// guest looked up the temp name first (a normal `ls -la`/READDIRPLUS walk does),
// so the temp inode is cached under the now-dead temp path. Pre-fix the guest
// kept addressing the renamed file through the temp path and `cat` failed with
// ENOENT from the first entry-timeout revalidation onward until drop_caches.
// The test observes only after the 1s entry timeout and again after a later
// attr refresh to prove the fix persists.
func TestHostFSCoherenceAtomicReplaceTmpLookedUp(t *testing.T) {
	hostDir := hostFSCoherenceBacking(t)
	const (
		targetName = "b2.txt"
		tmpName    = ".b2.tmp"
	)
	targetPath := filepath.Join(hostDir, targetName)
	tmpPath := filepath.Join(hostDir, tmpName)

	require.NoError(t, os.WriteFile(targetPath, []byte("v1\n"), 0644))
	require.NoError(t, os.WriteFile(tmpPath, []byte("v2\n"), 0644))
	tmpIno := hostFSCoherenceIno(t, tmpPath)

	vm := startHostFSCoherenceVM(t, hostDir)
	// Prime the target and the temp name; `ls -la` drives READDIRPLUS over the
	// directory like any normal guest walk.
	vm.primeFile(targetName)
	vm.primeFile(tmpName)
	vm.mustExec(30*time.Second, "ls -la '"+hostFSCoherenceGuestMount+"' >/dev/null")

	require.NoError(t, os.Rename(tmpPath, targetPath))
	newIno := hostFSCoherenceIno(t, targetPath)
	require.Equalf(t, tmpIno, newIno, "rename did not move the looked-up temp inode onto %s", targetName)
	t.Logf("%s now has the looked-up temp inode %d", targetName, newIno)

	// Past the 1s entry timeout the kernel re-lookups the target name; pre-fix
	// that is when the stale temp path took over and read failed.
	time.Sleep(1300 * time.Millisecond)
	vm.waitGuestFileStat(hostFSCoherenceGuestPath(targetName), "regular file", 3*time.Second)
	vm.waitGuestFileContent(hostFSCoherenceGuestPath(targetName), "v2", 3*time.Second)

	// The old failure persisted until drop_caches; correctness must survive a
	// later attr refresh cycle too.
	time.Sleep(1500 * time.Millisecond)
	vm.waitGuestFileContent(hostFSCoherenceGuestPath(targetName), "v2", 2*time.Second)
}

// TestHostFSCoherenceSameNameRecreate covers case (c3): the host deletes a file
// and recreates it under the same name with the same type, possibly reusing the
// freed inode. The guest must read the new content and still see a regular
// file; the host inode reuse is recorded for the regression guard.
func TestHostFSCoherenceSameNameRecreate(t *testing.T) {
	hostDir := hostFSCoherenceBacking(t)
	const name = "c3.txt"
	p := filepath.Join(hostDir, name)
	require.NoError(t, os.WriteFile(p, []byte("v1\n"), 0644))

	vm := startHostFSCoherenceVM(t, hostDir)
	vm.primeFile(name)
	oldIno := hostFSCoherenceIno(t, p)

	require.NoError(t, os.Remove(p))
	require.NoError(t, os.WriteFile(p, []byte("v2\n"), 0644))
	newIno := hostFSCoherenceIno(t, p)
	t.Logf("%s recreated: inode %d -> %d reused=%v", name, oldIno, newIno, oldIno == newIno)

	time.Sleep(1200 * time.Millisecond)
	vm.waitGuestFileStat(hostFSCoherenceGuestPath(name), "regular file", 3*time.Second)
	vm.waitGuestFileContent(hostFSCoherenceGuestPath(name), "v2", 3*time.Second)
}

// TestHostFSCoherenceFileToDirectory covers case (c1): the guest has cached a
// file; the host removes it and creates a directory of the same name. When ext4
// hands the directory the freed file inode, the fork's ino-keyed registry used
// to answer the directory LOOKUP with the cached regular-file type ("directory
// reported as regular file"), so `cat` returned EIO and even `ls` failed,
// persisting until FORGET. After the fix the type mismatch drops the stale node
// and a fresh directory node is created; the guest must report a directory and
// list/read it within the bounded window.
func TestHostFSCoherenceFileToDirectory(t *testing.T) {
	hostDir := hostFSCoherenceBacking(t)
	const name = "c1"
	p := filepath.Join(hostDir, name)

	vm := startHostFSCoherenceVM(t, hostDir)

	// Best-effort same-inode variant: a fresh inode does not exercise the
	// wrong-type coalescing bug, so retry once after re-priming when the host
	// does not reuse the freed number.
	reused := false
	for attempt := 1; attempt <= 2 && !reused; attempt++ {
		if attempt > 1 {
			// Let the guest's cached dentry for the previous object age out
			// before re-priming it as a file.
			time.Sleep(1200 * time.Millisecond)
		}
		require.NoError(t, os.RemoveAll(p))
		require.NoError(t, os.WriteFile(p, []byte("old-file\n"), 0644))
		vm.primeFile(name)
		oldIno := hostFSCoherenceIno(t, p)

		require.NoError(t, os.Remove(p))
		require.NoError(t, os.Mkdir(p, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(p, "inner"), []byte("dir-content\n"), 0644))
		newIno := hostFSCoherenceIno(t, p)
		if newIno == oldIno {
			reused = true
			t.Logf("host reused inode %d for directory %s (attempt %d)", oldIno, name, attempt)
			break
		}
		t.Logf("attempt %d: no host inode reuse (file=%d directory=%d)", attempt, oldIno, newIno)
	}
	if !reused {
		t.Logf("host filesystem did not reuse the freed inode across file->directory")
	}

	// Past the 1s entry timeout: pre-fix the wrong type persisted until FORGET.
	time.Sleep(1200 * time.Millisecond)
	vm.waitGuestFileStat(hostFSCoherenceGuestPath(name), "directory", 3*time.Second)
	vm.waitGuestDirContains(hostFSCoherenceGuestPath(name), []string{"inner"}, 3*time.Second)
	vm.waitGuestFileContent(path.Join(hostFSCoherenceGuestPath(name), "inner"), "dir-content", 3*time.Second)
}

// TestHostFSCoherenceDirectoryToFile covers case (c2): the guest has cached a
// directory; the host removes it (with its child) and creates a file of the
// same name. With the freed directory inode reused for the file, the pre-fix
// registry answered with the cached directory type, so READDIRPLUS returned EIO
// persistently ("file reported as directory"). After the fix the guest must
// report a regular file and read it within the bounded window.
func TestHostFSCoherenceDirectoryToFile(t *testing.T) {
	hostDir := hostFSCoherenceBacking(t)
	const name = "c2"
	p := filepath.Join(hostDir, name)

	vm := startHostFSCoherenceVM(t, hostDir)

	reused := false
	for attempt := 1; attempt <= 2 && !reused; attempt++ {
		if attempt > 1 {
			time.Sleep(1200 * time.Millisecond)
		}
		require.NoError(t, os.RemoveAll(p))
		require.NoError(t, os.Mkdir(p, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(p, "inner"), []byte("inner\n"), 0644))
		// Prime the cached directory node through a listing.
		vm.waitGuestDirContains(hostFSCoherenceGuestPath(name), []string{"inner"}, 3*time.Second)
		oldIno := hostFSCoherenceIno(t, p)

		require.NoError(t, os.RemoveAll(p))
		require.NoError(t, os.WriteFile(p, []byte("new-file\n"), 0644))
		newIno := hostFSCoherenceIno(t, p)
		if newIno == oldIno {
			reused = true
			t.Logf("host reused inode %d for file %s (attempt %d)", oldIno, name, attempt)
			break
		}
		t.Logf("attempt %d: no host inode reuse (directory=%d file=%d)", attempt, oldIno, newIno)
	}
	if !reused {
		t.Logf("host filesystem did not reuse the freed inode across directory->file")
	}

	time.Sleep(1200 * time.Millisecond)
	vm.waitGuestFileStat(hostFSCoherenceGuestPath(name), "regular file", 3*time.Second)
	vm.waitGuestFileContent(hostFSCoherenceGuestPath(name), "new-file", 3*time.Second)
}

// TestHostFSCoherenceDirectoryRenameCachedChild covers case (d): the guest has
// read d1/inner (caching both nodes); the host renames d1 to d2. Pre-fix the
// cached d1 node kept its dead path when the d2 LOOKUP coalesced onto it, so
// `ls d2` was an empty directory and d2/inner was ENOENT until FORGET. After the
// fix the cached path is validated and replaced, so d2 lists and reads
// correctly, and keeps doing so through a later attr refresh.
func TestHostFSCoherenceDirectoryRenameCachedChild(t *testing.T) {
	hostDir := hostFSCoherenceBacking(t)
	const (
		oldName = "d1"
		newName = "d2"
	)
	oldPath := filepath.Join(hostDir, oldName)
	newPath := filepath.Join(hostDir, newName)
	require.NoError(t, os.Mkdir(oldPath, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(oldPath, "inner"), []byte("inner-content\n"), 0644))

	vm := startHostFSCoherenceVM(t, hostDir)
	vm.primeFile(oldName + "/inner")
	vm.waitGuestDirContains(hostFSCoherenceGuestPath(oldName), []string{"inner"}, 3*time.Second)

	require.NoError(t, os.Rename(oldPath, newPath))

	time.Sleep(1200 * time.Millisecond)
	vm.waitGuestDirContains(hostFSCoherenceGuestPath(newName), []string{"inner"}, 3*time.Second)
	vm.waitGuestFileContent(path.Join(hostFSCoherenceGuestPath(newName), "inner"), "inner-content", 3*time.Second)

	// The old failure persisted until drop_caches; correctness must survive a
	// later attr refresh cycle too.
	time.Sleep(1500 * time.Millisecond)
	vm.waitGuestDirContains(hostFSCoherenceGuestPath(newName), []string{"inner"}, 2*time.Second)
}

// TestHostFSCoherenceAppendHeldOpen covers case (e): the guest holds a file
// descriptor open while the host appends to the file. A fresh open must see the
// new data promptly; the descriptor opened before the append sees it after the
// documented bounded 1s AttrTimeout refresh but not from stale cached i_size.
// The long-lived guest entrypoint owns the held descriptor and reports what it
// reads into a file inside the host_fs mount, which the test reads on the host.
func TestHostFSCoherenceAppendHeldOpen(t *testing.T) {
	hostDir := hostFSCoherenceBacking(t)
	const name = "e.txt"
	hostPath := filepath.Join(hostDir, name)
	heldLog := filepath.Join(hostDir, "e-held.log")
	require.NoError(t, os.WriteFile(hostPath, []byte("line1\n"), 0644))

	// fd 3 holds the file open across the append. `dd <&3` reads from the
	// descriptor's current offset: the first drain leaves it at EOF, so only
	// post-append bytes are reported into e-held.log. The guest's fresh `cat`
	// (below, via a separate exec) is an independent open.
	const script = `
exec 3</workspace/m/e.txt || exit 91
exec 4>>/workspace/m/e-held.log || exit 92
dd bs=1M <&3 >&4 2>/dev/null
echo READY >&4
i=0
while [ $i -lt 200 ]; do
  dd bs=1M <&3 >&4 2>/dev/null
  if grep -q line2 /workspace/m/e-held.log 2>/dev/null; then break; fi
  i=$((i+1))
  sleep 0.1
done
sleep 600
`
	vm := startHostFSCoherenceVMScript(t, hostDir, script)

	// Wait until the held descriptor exists and has drained the initial line.
	waitHostFileContains(t, heldLog, "line1", 15*time.Second)
	waitHostFileContains(t, heldLog, "READY", 5*time.Second)

	appendHostFile(t, hostPath, "line2\n")

	// The descriptor held open across the append reads against its cached
	// i_size and sees the appended data only after the bounded 1s AttrTimeout
	// refreshes the attributes (CAP_AUTO_INVAL_DATA then invalidates the page
	// cache). Measure that from the append and assert it converges.
	heldStart := time.Now()
	waitHostFileContains(t, heldLog, "line2", 5*time.Second)
	t.Logf("held fd saw the appended line after %s", time.Since(heldStart))

	// A fresh open addresses the current host path through a new file handle
	// and must also see the new data, with no drop_caches.
	freshStart := time.Now()
	vm.waitGuestFileContent(hostFSCoherenceGuestPath(name), "line1\nline2", 3*time.Second)
	t.Logf("fresh open saw the appended line after %s", time.Since(freshStart))
}

// TestHostFSCoherenceReaddirAfterHostAdd covers case (f): after the guest lists
// a directory, the host adds files; the next guest listing (readdir is always
// served from the host) must show the new entries without waiting for the entry
// timeout.
func TestHostFSCoherenceReaddirAfterHostAdd(t *testing.T) {
	hostDir := hostFSCoherenceBacking(t)
	const dirName = "fdir"
	dirPath := filepath.Join(hostDir, dirName)
	require.NoError(t, os.Mkdir(dirPath, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dirPath, "existing"), []byte("x\n"), 0644))

	vm := startHostFSCoherenceVM(t, hostDir)
	vm.waitGuestDirContains(hostFSCoherenceGuestPath(dirName), []string{"existing"}, 3*time.Second)

	require.NoError(t, os.WriteFile(filepath.Join(dirPath, "f1"), []byte("1\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dirPath, "f2"), []byte("2\n"), 0644))

	vm.waitGuestDirContains(hostFSCoherenceGuestPath(dirName), []string{"existing", "f1", "f2"}, 3*time.Second)
}
