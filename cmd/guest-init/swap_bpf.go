//go:build linux

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"github.com/jingkaihe/matchlock/internal/errx"
	"golang.org/x/sys/unix"
)

// Swap is VM-wide: the backing store can hold pages swapped out from PID 1, the
// privileged guest agent, and every exec session. A non-root workload is blocked
// by the root:root 0600 node that restrictSwapDevice applies, but a uid-0
// workload is the node *owner*, so the DAC owner bits grant access before any
// capability check, and it can also recreate a node from the major:minor
// exposed in /sys/block/<dev>/dev. The only mechanism that is evaluated before
// and independently of file ownership, and that covers both open and mknod, is
// a cgroup v2 device-controller BPF policy.
//
// guest-init (PID 1) loads a BPF_PROG_TYPE_CGROUP_DEVICE program that denies the
// swap device's major:minor and attaches it to the cgroup2 root. The workload
// inherits the policy from the root cgroup, and because it cannot move above
// that root, it cannot escape the deny. The program denies *all* access modes
// (read, write and mknod) for exactly that major:minor block device — it tests
// the BPF_DEVCG_DEV_BLOCK bit in ctx->access_type so a character device that
// happens to share the same major:minor is not caught — which stops
// open-for-read, open-for-write and mknod alike.
//
// This file is linux-only (the whole cmd/guest-init package is), so the cgroup2
// specific syscalls are safe to reference unconditionally.

// cgroup2RootPath is the mount point prepareBaseFilesystems mounts as cgroup2
// and the attach target for the swap-device policy.
const cgroup2RootPath = "/sys/fs/cgroup"

// eBPF instruction opcodes (uapi/linux/bpf_common.h, uapi/linux/bpf.h).
const (
	bpfOpLDXW     = 0x61 // BPF_LDX | BPF_W | BPF_MEM
	bpfOpALU64MOV = 0xb7 // BPF_ALU64 | BPF_MOV | BPF_K
	bpfOpALU64AND = 0x57 // BPF_ALU64 | BPF_AND | BPF_K
	bpfOpJMPJEQ   = 0x15 // BPF_JMP | BPF_JEQ | BPF_K
	bpfOpJMPJNE   = 0x55 // BPF_JMP | BPF_JNE | BPF_K
	bpfOpJMPEXIT  = 0x95 // BPF_JMP | BPF_EXIT
)

// Offsets into struct bpf_cgroup_dev_ctx { __u32 access_type; __u32 major;
// __u32 minor; }. The filter tests the device TYPE bit inside access_type and
// then matches major and minor, so only the swap BLOCK device is denied. The
// high-16 BPF_DEVCG_ACC_* bits are deliberately not inspected: read, write and
// mknod must all be denied for the swap block device.
const (
	bpfCgroupDevCtxAccessType = 0
	bpfCgroupDevCtxMajor      = 4
	bpfCgroupDevCtxMinor      = 8
)

// bpfDevcgDevBlock is the BPF_DEVCG_DEV_BLOCK bit in the low 16 bits of
// access_type (uapi/linux/bpf.h; x/sys/unix exposes it as
// unix.BPF_DEVCG_DEV_BLOCK = 0x1). access_type is encoded as
// (BPF_DEVCG_ACC_* << 16) | BPF_DEVCG_DEV_*, so testing this bit distinguishes
// a block device from a character device.
const bpfDevcgDevBlock = 0x1

// bpfInsn mirrors the kernel's struct bpf_insn. Dst is the low nibble of Regs
// and Src the high nibble, matching dst_reg:4/src_reg:4.
type bpfInsn struct {
	Code uint8
	Regs uint8
	Off  int16
	Imm  int32
}

// bpfReg encodes a destination/source register pair for bpfInsn.Regs.
func bpfReg(dst, src uint8) uint8 {
	return (dst & 0x0f) | (src << 4)
}

// buildSwapDeviceFilter returns the cgroup-device BPF program that denies the
// swap BLOCK device at exactly major:minor and allows everything else,
// including a character device that happens to share those numbers. It is a
// pure function so the exact instruction encoding can be unit-tested without
// loading BPF.
//
// Program (10 instructions):
//
//	r0 = 1                              ; default: allow
//	r2 = *(u32 *)(r1 + 0)               ; ctx->access_type
//	r2 &= 1                             ; BPF_DEVCG_DEV_BLOCK bit
//	if r2 == 0 goto exit                ; char device -> allow
//	r2 = *(u32 *)(r1 + 4)               ; ctx->major
//	if r2 != major goto exit            ; non-swap major -> allow
//	r3 = *(u32 *)(r1 + 8)               ; ctx->minor
//	if r3 != minor goto exit            ; non-swap minor -> allow
//	r0 = 0                              ; swap block device -> deny every access
//	exit
func buildSwapDeviceFilter(major, minor uint32) []bpfInsn {
	return []bpfInsn{
		{Code: bpfOpALU64MOV, Regs: bpfReg(0, 0), Imm: 1},
		{Code: bpfOpLDXW, Regs: bpfReg(2, 1), Off: bpfCgroupDevCtxAccessType},
		{Code: bpfOpALU64AND, Regs: bpfReg(2, 0), Imm: bpfDevcgDevBlock},
		{Code: bpfOpJMPJEQ, Regs: bpfReg(2, 0), Off: 5, Imm: 0},
		{Code: bpfOpLDXW, Regs: bpfReg(2, 1), Off: bpfCgroupDevCtxMajor},
		{Code: bpfOpJMPJNE, Regs: bpfReg(2, 0), Off: 3, Imm: int32(major)},
		{Code: bpfOpLDXW, Regs: bpfReg(3, 1), Off: bpfCgroupDevCtxMinor},
		{Code: bpfOpJMPJNE, Regs: bpfReg(3, 0), Off: 1, Imm: int32(minor)},
		{Code: bpfOpALU64MOV, Regs: bpfReg(0, 0), Imm: 0},
		{Code: bpfOpJMPEXIT},
	}
}

// swapDevicePolicy is the resolved major:minor of the active swap device.
type swapDevicePolicy struct {
	Major uint32
	Minor uint32
}

// swapDevicePolicyRequired reports whether the cgroup device policy must be
// installed for a boot config: exactly when a swap device was requested and the
// sandbox is not privileged. Privileged mode deliberately opts out of all
// in-guest restrictions, so it is not a policy failure.
func swapDevicePolicyRequired(cfg *bootConfig) bool {
	return cfg != nil && cfg.SwapDevice != "" && !cfg.Privileged
}

// newSwapDevicePolicy stats /dev/<dev> and derives its device numbers. The node
// is created by devtmpfs before guest-init runs, so it must exist here; failing
// loudly is better than silently leaving the store reachable.
func newSwapDevicePolicy(dev string) (swapDevicePolicy, error) {
	path := filepath.Join("/dev", dev)
	info, err := os.Stat(path)
	if err != nil {
		return swapDevicePolicy{}, errx.With(ErrSwapDevicePolicy, " stat %s: %w", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return swapDevicePolicy{}, errx.With(ErrSwapDevicePolicy, " stat %s: unexpected file info", path)
	}
	return swapDevicePolicy{
		Major: unix.Major(uint64(st.Rdev)),
		Minor: unix.Minor(uint64(st.Rdev)),
	}, nil
}

// installSwapDevicePolicy resolves the swap device and attaches the deny policy
// to the cgroup2 root. Any failure is fatal at boot: silently continuing would
// expose the VM-wide swap store to a root workload.
func installSwapDevicePolicy(dev, cgroupRoot string) error {
	policy, err := newSwapDevicePolicy(dev)
	if err != nil {
		return err
	}

	cgroupFd, err := unix.Open(cgroupRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return errx.With(ErrSwapDevicePolicy, " open cgroup2 %s: %w", cgroupRoot, err)
	}
	defer unix.Close(cgroupFd)

	progFd, err := loadSwapDeviceProgram(policy)
	if err != nil {
		return err
	}
	defer unix.Close(progFd)

	return attachSwapDeviceProgram(cgroupFd, progFd)
}

// loadSwapDeviceProgram loads the swap-deny program with verifier logging so a
// failed load reports the exact verifier reason.
func loadSwapDeviceProgram(policy swapDevicePolicy) (int, error) {
	insns := buildSwapDeviceFilter(policy.Major, policy.Minor)
	logBuf := make([]byte, 64*1024)

	licensePtr, err := unix.BytePtrFromString("GPL")
	if err != nil {
		return -1, errx.With(ErrSwapDevicePolicy, " license: %w", err)
	}

	attr := bpfProgLoadAttr{
		ProgType:           unix.BPF_PROG_TYPE_CGROUP_DEVICE,
		InsnCnt:            uint32(len(insns)),
		Insns:              uint64(uintptr(unsafe.Pointer(&insns[0]))),
		License:            uint64(uintptr(unsafe.Pointer(licensePtr))),
		LogLevel:           1,
		LogSize:            uint32(len(logBuf)),
		LogBuf:             uint64(uintptr(unsafe.Pointer(&logBuf[0]))),
		ExpectedAttachType: unix.BPF_CGROUP_DEVICE,
	}

	fd, _, errno := unix.Syscall(unix.SYS_BPF, uintptr(unix.BPF_PROG_LOAD), uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr))
	runtime.KeepAlive(insns)
	runtime.KeepAlive(licensePtr)
	runtime.KeepAlive(logBuf)
	if errno != 0 {
		return -1, errx.With(ErrSwapDevicePolicy, " load program: %w (%s)", errno, verifierLog(logBuf))
	}
	return int(fd), nil
}

// attachSwapDeviceProgram attaches progFd to the cgroup2 root with
// attach_flags 0, so the policy is non-overridable by any descendant cgroup.
func attachSwapDeviceProgram(cgroupFd, progFd int) error {
	attr := bpfProgAttachAttr{
		TargetFd:    uint32(cgroupFd),
		AttachBpfFd: uint32(progFd),
		AttachType:  unix.BPF_CGROUP_DEVICE,
		AttachFlags: 0,
	}
	_, _, errno := unix.Syscall(unix.SYS_BPF, uintptr(unix.BPF_PROG_ATTACH), uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr))
	runtime.KeepAlive(&attr)
	if errno != 0 {
		return errx.With(ErrSwapDevicePolicy, " attach program: %w", errno)
	}
	return nil
}

// verifierLog trims the NUL padding from a verifier log buffer and shortens it
// to the first non-empty line so boot errors stay readable.
func verifierLog(logBuf []byte) string {
	text := strings.TrimSpace(strings.TrimRight(string(logBuf), "\x00"))
	if text == "" {
		return "no verifier log"
	}
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	return text
}

// bpfProgLoadAttr is the prefix of union bpf_attr used by BPF_PROG_LOAD. The
// kernel accepts a shorter attribute as long as every byte is zero after the
// last field the command understands; this struct ends at expected_attach_type.
type bpfProgLoadAttr struct {
	ProgType           uint32
	InsnCnt            uint32
	Insns              uint64
	License            uint64
	LogLevel           uint32
	LogSize            uint32
	LogBuf             uint64
	KernVersion        uint32
	ProgFlags          uint32
	ProgName           [16]byte
	ProgIfIndex        uint32
	ExpectedAttachType uint32
}

// bpfProgAttachAttr is the prefix of union bpf_attr used by BPF_PROG_ATTACH.
type bpfProgAttachAttr struct {
	TargetFd     uint32
	AttachBpfFd  uint32
	AttachType   uint32
	AttachFlags  uint32
	ReplaceBpfFd uint32
}
