//go:build linux

package guestagent

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// Syscall numbers for x86_64 and arm64
const (
	sysProcessVMReadvAmd64  = 310
	sysProcessVMWritevAmd64 = 311
	sysPtraceAmd64          = 101
	sysKexecLoadAmd64       = 246
	sysKexecFileLoadAmd64   = 320

	sysProcessVMReadvArm64  = 270
	sysProcessVMWritevArm64 = 271
	sysPtraceArm64          = 117
	sysKexecLoadArm64       = 104
	sysKexecFileLoadArm64   = 294
)

const (
	prSetSeccomp      = 22
	seccompModeFilter = 2
	prSetNoNewPrivs   = 38

	bpfLD  = 0x00
	bpfJMP = 0x05
	bpfRET = 0x06
	bpfW   = 0x00
	bpfABS = 0x20
	bpfJEQ = 0x10
	bpfK   = 0x00

	seccompRetAllow = 0x7fff0000
	seccompRetErrno = 0x00050000
	errnoEPERM      = 1

	auditArchX86_64  = 0xc000003e
	auditArchAARCH64 = 0xc00000b7

	// prCAP* constants for capability manipulation
	prCapBSetDrop = 24

	// Only drop capabilities that enable the attack vector
	capSysPtrace = 19 // process_vm_readv/writev, ptrace
	capSysAdmin  = 21 // mount namespace escape, bpf, etc.
	capSysModule = 16 // kernel module loading
	capSysRawio  = 17 // raw I/O port access
	capSysBoot   = 22 // kexec_load, reboot
)

type sockFprog struct {
	Len    uint16
	_      [6]byte
	Filter *sockFilter
}

type sockFilter struct {
	Code uint16
	Jt   uint8
	Jf   uint8
	K    uint32
}

func bpfStmt(code uint16, k uint32) sockFilter {
	return sockFilter{Code: code, K: k}
}

func bpfJump(code uint16, k uint32, jt, jf uint8) sockFilter {
	return sockFilter{Code: code, Jt: jt, Jf: jf, K: k}
}

func blockedSyscalls() ([]uint32, uint32) {
	switch runtime.GOARCH {
	case "arm64":
		return []uint32{
			sysProcessVMReadvArm64,
			sysProcessVMWritevArm64,
			sysPtraceArm64,
			sysKexecLoadArm64,
			sysKexecFileLoadArm64,
		}, auditArchAARCH64
	default:
		return []uint32{
			sysProcessVMReadvAmd64,
			sysProcessVMWritevAmd64,
			sysPtraceAmd64,
			sysKexecLoadAmd64,
			sysKexecFileLoadAmd64,
		}, auditArchX86_64
	}
}

// buildSeccompFilter creates a BPF program that blocks dangerous syscalls
// (process_vm_readv, process_vm_writev, ptrace, kexec_load, kexec_file_load)
// with EPERM. All other syscalls are allowed.
func buildSeccompFilter() []sockFilter {
	blocked, auditArch := blockedSyscalls()

	filter := []sockFilter{
		bpfStmt(bpfLD|bpfW|bpfABS, 4),
		bpfJump(bpfJMP|bpfJEQ|bpfK, auditArch, 0, uint8(len(blocked)+1)),
		bpfStmt(bpfLD|bpfW|bpfABS, 0),
	}

	for i, nr := range blocked {
		remaining := uint8(len(blocked) - i - 1)
		filter = append(filter, bpfJump(bpfJMP|bpfJEQ|bpfK, nr, remaining+1, 0))
	}

	filter = append(filter, bpfStmt(bpfRET|bpfK, seccompRetAllow))
	filter = append(filter, bpfStmt(bpfRET|bpfK, seccompRetErrno|errnoEPERM))

	return filter
}

// sandboxLauncherEnvKey is the environment variable used to signal that this
// process is a re-execed sandbox launcher (not the main agent).
const sandboxLauncherEnvKey = "__MATCHLOCK_SANDBOX_LAUNCHER"

// isPrivilegedMode checks /proc/cmdline for matchlock.privileged=1.
// When privileged, the sandbox launcher skips cap drops, seccomp, and no_new_privs.
func isPrivilegedMode() bool {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return false
	}
	for _, field := range strings.Fields(string(data)) {
		if field == "matchlock.privileged=1" {
			return true
		}
	}
	return false
}

// isSandboxLauncher returns true if this process was re-execed as a sandbox launcher.
func isSandboxLauncher() bool {
	return os.Getenv(sandboxLauncherEnvKey) == "1"
}

// runSandboxLauncher is the entrypoint for the re-execed process.
// It drops capabilities, installs seccomp, then execs the real command.
func runSandboxLauncher() {
	// Remove our marker so child doesn't inherit it
	os.Unsetenv(sandboxLauncherEnvKey)

	// Remount /proc for our new PID namespace so the workload only sees
	// its own processes, not the guest-agent in the parent namespace.
	syscall.Unmount("/proc", syscall.MNT_DETACH)
	syscall.Mount("proc", "/proc", "proc", 0, "")

	privileged := isPrivilegedMode()

	if !privileged {
		// Drop specific dangerous capabilities from the bounding set
		for _, cap := range []uintptr{capSysPtrace, capSysAdmin, capSysModule, capSysRawio, capSysBoot} {
			syscall.RawSyscall(syscall.SYS_PRCTL, prCapBSetDrop, cap, 0)
		}

		// Set no_new_privs (required for seccomp and prevents privilege escalation)
		if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); errno != 0 {
			fmt.Fprintf(os.Stderr, "matchlock: failed to set no_new_privs: %v\n", errno)
			os.Exit(127)
		}

		// Install seccomp filter
		filter := buildSeccompFilter()
		prog := sockFprog{
			Len:    uint16(len(filter)),
			Filter: &filter[0],
		}
		if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetSeccomp, seccompModeFilter, uintptr(unsafe.Pointer(&prog))); errno != 0 {
			fmt.Fprintf(os.Stderr, "matchlock: failed to install seccomp filter: %v\n", errno)
			os.Exit(127)
		}
	}

	// The command to exec is passed via MATCHLOCK_CMD env var,
	// and individual args via MATCHLOCK_ARG_0, MATCHLOCK_ARG_1, etc.
	command := os.Getenv("MATCHLOCK_CMD")
	os.Unsetenv("MATCHLOCK_CMD")

	var args []string
	for i := 0; ; i++ {
		key := fmt.Sprintf("MATCHLOCK_ARG_%d", i)
		val, ok := os.LookupEnv(key)
		if !ok {
			break
		}
		args = append(args, val)
		os.Unsetenv(key)
	}

	if command == "" {
		fmt.Fprintf(os.Stderr, "matchlock: no command specified\n")
		os.Exit(127)
	}

	// Find the binary
	binary, err := exec.LookPath(command)
	if err != nil {
		fmt.Fprintf(os.Stderr, "matchlock: %v\n", err)
		os.Exit(127)
	}

	argv := append([]string{command}, args...)

	// Switch user if requested via MATCHLOCK_USER env var
	userSpec := os.Getenv("MATCHLOCK_USER")
	os.Unsetenv("MATCHLOCK_USER")
	if userSpec != "" {
		uid, gid, homeDir, err := resolveUser(userSpec)
		if err != nil {
			fmt.Fprintf(os.Stderr, "matchlock: resolve user %q: %v\n", userSpec, err)
			os.Exit(127)
		}
		if err := syscall.Setgroups([]int{gid}); err != nil {
			fmt.Fprintf(os.Stderr, "matchlock: setgroups([%d]): %v\n", gid, err)
			os.Exit(127)
		}
		if err := syscall.Setgid(gid); err != nil {
			fmt.Fprintf(os.Stderr, "matchlock: setgid(%d): %v\n", gid, err)
			os.Exit(127)
		}
		if err := syscall.Setuid(uid); err != nil {
			fmt.Fprintf(os.Stderr, "matchlock: setuid(%d): %v\n", uid, err)
			os.Exit(127)
		}
		if homeDir != "" {
			os.Setenv("HOME", homeDir)
		}
	}

	// Filter out our internal env vars from the environment
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "MATCHLOCK_CMD=") || strings.HasPrefix(e, "MATCHLOCK_ARG_") || strings.HasPrefix(e, sandboxLauncherEnvKey+"=") || strings.HasPrefix(e, "MATCHLOCK_USER=") {
			continue
		}
		env = append(env, e)
	}

	// Exec the real command (replaces this process)
	err = syscall.Exec(binary, argv, env)
	if err != nil {
		fmt.Fprintf(os.Stderr, "matchlock: exec %s: %v\n", command, err)
		os.Exit(127)
	}
}

// applySandboxSysProcAttr configures the child process with:
// - PID namespace isolation (CLONE_NEWPID) so workload can't see the agent
// - Pdeathsig to kill child if agent dies
//
// The command is wrapped: instead of running `sh -c <cmd>` directly,
// we re-exec the guest-agent binary with sandboxLauncherEnvKey set.
// The launcher then drops caps, installs seccomp, and execs the real command.
func applySandboxSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
		Pdeathsig:  syscall.SIGKILL,
	}
}

// applySandboxSysProcAttrBatch is like applySandboxSysProcAttr but also sets
// Setpgid so the child gets its own process group. This enables process-group
// kill (-pid) for cancellation. Must NOT be used with PTY exec because
// pty.Start sets Setsid which conflicts with Setpgid.
func applySandboxSysProcAttrBatch(cmd *exec.Cmd) {
	applySandboxSysProcAttr(cmd)
	cmd.SysProcAttr.Setpgid = true
}

// wrapCommandForSandbox rewrites the exec.Cmd to use the re-exec launcher pattern.
// The original command is passed via environment variables, and the binary is
// replaced with /proc/self/exe (the guest-agent itself).
func wrapCommandForSandbox(cmd *exec.Cmd) {
	origArgs := cmd.Args // e.g. ["sh", "-c", "python3 script.py"]

	// Set the binary to re-exec ourselves
	cmd.Path = "/proc/self/exe"
	cmd.Args = []string{"guest-agent"}

	// Pass the original command via env vars
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, sandboxLauncherEnvKey+"=1")

	if len(origArgs) > 0 {
		cmd.Env = append(cmd.Env, "MATCHLOCK_CMD="+origArgs[0])
		for i, arg := range origArgs[1:] {
			cmd.Env = append(cmd.Env, fmt.Sprintf("MATCHLOCK_ARG_%d=%s", i, arg))
		}
	}
}

// resolveUser resolves a user spec ("uid", "uid:gid", or "username") to numeric
// UID, GID, and home directory by parsing /etc/passwd and /etc/group.
func resolveUser(spec string) (uid, gid int, homeDir string, err error) {
	return resolveUserFrom(spec, "/etc/passwd", "/etc/group")
}

func resolveUserFrom(spec, passwdPath, groupPath string) (uid, gid int, homeDir string, err error) {
	if parts := strings.SplitN(spec, ":", 2); len(parts) == 2 {
		uid, err = resolveUIDFrom(parts[0], passwdPath)
		if err != nil {
			return 0, 0, "", errx.With(ErrResolveUID, " %q: %w", parts[0], err)
		}
		gid, err = resolveGIDFrom(parts[1], groupPath)
		if err != nil {
			return 0, 0, "", errx.With(ErrResolveGID, " %q: %w", parts[1], err)
		}
		_, _, homeDir = lookupPasswdByUIDFrom(uid, passwdPath)
		return uid, gid, homeDir, nil
	}

	if n, err := strconv.Atoi(spec); err == nil {
		gid, _, homeDir = lookupPasswdByUIDFrom(n, passwdPath)
		return n, gid, homeDir, nil
	}

	uid, gid, homeDir, ok := lookupPasswdByNameFrom(spec, passwdPath)
	if !ok {
		return 0, 0, "", errx.With(ErrUserNotFound, ": %q", spec)
	}
	return uid, gid, homeDir, nil
}

func resolveUIDFrom(s, passwdPath string) (int, error) {
	if n, err := strconv.Atoi(s); err == nil {
		return n, nil
	}
	uid, _, _, ok := lookupPasswdByNameFrom(s, passwdPath)
	if !ok {
		return 0, errx.With(ErrUserNotFound, ": %q", s)
	}
	return uid, nil
}

func resolveGIDFrom(s, groupPath string) (int, error) {
	if n, err := strconv.Atoi(s); err == nil {
		return n, nil
	}
	f, err := os.Open(groupPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		fields := strings.SplitN(line, ":", 4)
		if len(fields) >= 3 && fields[0] == s {
			gid, err := strconv.Atoi(fields[2])
			if err == nil {
				return gid, nil
			}
		}
	}
	return 0, errx.With(ErrGroupNotFound, ": %q", s)
}

func lookupPasswdByNameFrom(name, passwdPath string) (int, int, string, bool) {
	f, err := os.Open(passwdPath)
	if err != nil {
		return 0, 0, "", false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		fields := strings.SplitN(line, ":", 7)
		if len(fields) >= 6 && fields[0] == name {
			uid, _ := strconv.Atoi(fields[2])
			gid, _ := strconv.Atoi(fields[3])
			return uid, gid, fields[5], true
		}
	}
	return 0, 0, "", false
}

func lookupPasswdByUIDFrom(uid int, passwdPath string) (gid int, shell string, homeDir string) {
	f, err := os.Open(passwdPath)
	if err != nil {
		return uid, "", ""
	}
	defer f.Close()
	uidStr := strconv.Itoa(uid)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		fields := strings.SplitN(line, ":", 7)
		if len(fields) >= 6 && fields[2] == uidStr {
			gid, _ := strconv.Atoi(fields[3])
			shell := ""
			if len(fields) >= 7 {
				shell = fields[6]
			}
			return gid, shell, fields[5]
		}
	}
	return uid, "", ""
}

// wipeBytes zeros out a byte slice to remove sensitive data from memory.
func wipeBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// wipeMap removes all entries from a map.
func wipeMap(m map[string]string) {
	for k := range m {
		delete(m, k)
	}
}
