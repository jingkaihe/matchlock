//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBPFAttrLayout pins the union bpf_attr prefix layout the kernel expects.
// A field reorder or accidental padding would make bpf() read the wrong bytes;
// the live VM test catches that, but this keeps it a fast unit failure.
func TestBPFAttrLayout(t *testing.T) {
	assert.Equal(t, uintptr(8), unsafe.Sizeof(bpfInsn{}), "struct bpf_insn")
	assert.Equal(t, uintptr(72), unsafe.Sizeof(bpfProgLoadAttr{}), "BPF_PROG_LOAD attr prefix")
	assert.Equal(t, uintptr(0), unsafe.Offsetof(bpfProgLoadAttr{}.ProgType))
	assert.Equal(t, uintptr(4), unsafe.Offsetof(bpfProgLoadAttr{}.InsnCnt))
	assert.Equal(t, uintptr(8), unsafe.Offsetof(bpfProgLoadAttr{}.Insns))
	assert.Equal(t, uintptr(16), unsafe.Offsetof(bpfProgLoadAttr{}.License))
	assert.Equal(t, uintptr(24), unsafe.Offsetof(bpfProgLoadAttr{}.LogLevel))
	assert.Equal(t, uintptr(32), unsafe.Offsetof(bpfProgLoadAttr{}.LogBuf))
	assert.Equal(t, uintptr(68), unsafe.Offsetof(bpfProgLoadAttr{}.ExpectedAttachType))
	assert.Equal(t, uintptr(20), unsafe.Sizeof(bpfProgAttachAttr{}), "BPF_PROG_ATTACH attr prefix")
	assert.Equal(t, uintptr(0), unsafe.Offsetof(bpfProgAttachAttr{}.TargetFd))
	assert.Equal(t, uintptr(8), unsafe.Offsetof(bpfProgAttachAttr{}.AttachType))
	assert.Equal(t, uintptr(12), unsafe.Offsetof(bpfProgAttachAttr{}.AttachFlags))
}

// TestBuildSwapDeviceFilter pins the exact instruction array. The filter must
// default to allow (r0=1), test the BPF_DEVCG_DEV_BLOCK bit of ctx->access_type
// (offset 0) to ignore char devices, then compare ctx->major (offset 4) and
// ctx->minor (offset 8), and only when all three align set r0=0 (deny). Any
// mismatch jumps straight to the exit that returns the allow value.
func TestBuildSwapDeviceFilter(t *testing.T) {
	got := buildSwapDeviceFilter(254, 17)

	want := []bpfInsn{
		{Code: 0xb7, Regs: 0x00, Imm: 1},           // r0 = 1 (allow)
		{Code: 0x61, Regs: 0x12, Off: 0},           // r2 = *(u32 *)(r1 + 0)  (access_type)
		{Code: 0x57, Regs: 0x02, Imm: 1},           // r2 &= DEV_BLOCK
		{Code: 0x15, Regs: 0x02, Off: 5, Imm: 0},   // if r2 == 0 goto exit (char device -> allow)
		{Code: 0x61, Regs: 0x12, Off: 4},           // r2 = *(u32 *)(r1 + 4)  (major)
		{Code: 0x55, Regs: 0x02, Off: 3, Imm: 254}, // if r2 != major goto exit
		{Code: 0x61, Regs: 0x13, Off: 8},           // r3 = *(u32 *)(r1 + 8)  (minor)
		{Code: 0x55, Regs: 0x03, Off: 1, Imm: 17},  // if r3 != minor goto exit
		{Code: 0xb7, Regs: 0x00, Imm: 0},           // r0 = 0 (deny)
		{Code: 0x95, Regs: 0x00},                   // exit
	}
	require.Equal(t, want, got)
}

// TestBuildSwapDeviceFilterJumpsStayInBounds checks that all three conditional
// jumps land on the exit instruction (index 9) for every major/minor: a wrong
// offset would let the verifier reject the program or mis-route control.
func TestBuildSwapDeviceFilterJumpsStayInBounds(t *testing.T) {
	for _, tc := range []struct{ major, minor uint32 }{
		{0, 0}, {1, 3}, {254, 0}, {254, 16}, {8, 65535},
	} {
		filter := buildSwapDeviceFilter(tc.major, tc.minor)
		require.Len(t, filter, 10)
		for _, i := range []int{3, 5, 7} {
			target := i + 1 + int(filter[i].Off)
			assert.Equalf(t, 9, target, "jump at %d out of bounds for %d:%d", i, tc.major, tc.minor)
			assert.Equal(t, uint8(bpfOpJMPEXIT), filter[target].Code, "conditional jumps must land on EXIT")
		}
		assert.Equal(t, uint8(bpfOpJMPEXIT), filter[9].Code, "final instruction must be EXIT")
	}
}

// bpfCgroupDevCtx mirrors the kernel's struct bpf_cgroup_dev_ctx.
type bpfCgroupDevCtx struct {
	AccessType uint32
	Major      uint32
	Minor      uint32
}

// bpfInsnDst extracts the destination register from a bpfInsn.Regs nibble.
func bpfInsnDst(insn bpfInsn) uint8 {
	return insn.Regs & 0x0f
}

// evalSwapDeviceFilter interprets the instruction subset buildSwapDeviceFilter
// emits (MOV/AND immediate, LDXW from ctx, JEQ/JNE immediate, EXIT) and returns
// r0 (1 = allow, 0 = deny). It lets tests assert the security control's actual
// behavior over a ctx table instead of only pinning its shape.
func evalSwapDeviceFilter(insns []bpfInsn, ctx bpfCgroupDevCtx) uint32 {
	ctxBytes := make([]byte, 12)
	binary.LittleEndian.PutUint32(ctxBytes[0:4], ctx.AccessType)
	binary.LittleEndian.PutUint32(ctxBytes[4:8], ctx.Major)
	binary.LittleEndian.PutUint32(ctxBytes[8:12], ctx.Minor)

	var regs [11]uint32
	for pc := 0; pc >= 0 && pc < len(insns); {
		insn := insns[pc]
		dst := bpfInsnDst(insn)
		switch insn.Code {
		case bpfOpALU64MOV:
			regs[dst] = uint32(insn.Imm)
			pc++
		case bpfOpALU64AND:
			regs[dst] &= uint32(insn.Imm)
			pc++
		case bpfOpLDXW:
			regs[dst] = binary.LittleEndian.Uint32(ctxBytes[insn.Off : int(insn.Off)+4])
			pc++
		case bpfOpJMPJEQ:
			if regs[dst] == uint32(insn.Imm) {
				pc += 1 + int(insn.Off)
			} else {
				pc++
			}
		case bpfOpJMPJNE:
			if regs[dst] != uint32(insn.Imm) {
				pc += 1 + int(insn.Off)
			} else {
				pc++
			}
		case bpfOpJMPEXIT:
			return regs[0]
		default:
			panic(fmt.Sprintf("unsupported opcode %#x at pc %d", insn.Code, pc))
		}
	}
	panic("program ran off the end without EXIT")
}

// TestBuildSwapDeviceFilterBehavior is the security-property test: it proves the
// filter denies ONLY the swap BLOCK device. The key regression is that a CHAR
// device that shares the swap major:minor must be allowed — the old 7-insn
// program ignored ctx->access_type and denied it. The high-16 BPF_DEVCG_ACC_*
// bits must not matter: read, write and mknod (and any combination) all stay
// denied for the swap block device.
func TestBuildSwapDeviceFilterBehavior(t *testing.T) {
	const (
		accRead  = uint32(1) // BPF_DEVCG_ACC_READ
		accWrite = uint32(2) // BPF_DEVCG_ACC_WRITE
		accMknod = uint32(4) // BPF_DEVCG_ACC_MKNOD

		devBlock = uint32(0x1) // BPF_DEVCG_DEV_BLOCK
		devChar  = uint32(0x2) // BPF_DEVCG_DEV_CHAR
	)
	enc := func(acc, devType uint32) uint32 { return (acc << 16) | devType }

	filter := buildSwapDeviceFilter(254, 17)

	// (a) The swap BLOCK device is denied regardless of the access-mode bits.
	for _, acc := range []uint32{accRead, accWrite, accMknod, accRead | accWrite | accMknod, 0} {
		ctx := bpfCgroupDevCtx{AccessType: enc(acc, devBlock), Major: 254, Minor: 17}
		assert.Equalf(t, uint32(0), evalSwapDeviceFilter(filter, ctx),
			"swap BLOCK device 254:17 must be denied for access mode %#x", acc)
	}

	// (b)-(d) Everything that is not exactly the swap block device is allowed.
	for _, tc := range []struct {
		name string
		ctx  bpfCgroupDevCtx
	}{
		{"char device, same major:minor", bpfCgroupDevCtx{AccessType: enc(accRead, devChar), Major: 254, Minor: 17}},
		{"char device, same major:minor, all access bits", bpfCgroupDevCtx{AccessType: enc(accRead|accWrite|accMknod, devChar), Major: 254, Minor: 17}},
		{"char device with no low dev-type bits", bpfCgroupDevCtx{AccessType: enc(accRead, 0), Major: 254, Minor: 17}},
		{"block device, different minor", bpfCgroupDevCtx{AccessType: enc(accRead, devBlock), Major: 254, Minor: 0}},
		{"block device, different major", bpfCgroupDevCtx{AccessType: enc(accRead, devBlock), Major: 255, Minor: 17}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equalf(t, uint32(1), evalSwapDeviceFilter(filter, tc.ctx), "must be allowed: %+v", tc.ctx)
		})
	}
}

func TestBpfRegEncoding(t *testing.T) {
	assert.Equal(t, uint8(0x00), bpfReg(0, 0))
	assert.Equal(t, uint8(0x12), bpfReg(2, 1))
	assert.Equal(t, uint8(0x13), bpfReg(3, 1))
}

// TestSwapDevicePolicyRequired proves the policy is gated on swap-on and
// non-privileged. A missing policy on a swap-enabled sandbox would leave the
// root hole open; installing it for non-swap or privileged sandboxes would be
// needless or a behavioral regression.
func TestSwapDevicePolicyRequired(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *bootConfig
		want bool
	}{
		{name: "nil", cfg: nil, want: false},
		{name: "no-swap", cfg: &bootConfig{}, want: false},
		{name: "swap", cfg: &bootConfig{SwapDevice: "vdc"}, want: true},
		{name: "swap-privileged", cfg: &bootConfig{SwapDevice: "vdc", Privileged: true}, want: false},
		{name: "privileged-only", cfg: &bootConfig{Privileged: true}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, swapDevicePolicyRequired(tc.cfg))
		})
	}
}

// TestParseBootConfigPrivileged verifies the new bootConfig field is populated
// from the same kernel-cmdline field the sandbox launcher checks, so guest-init
// and the launcher agree on whether restrictions apply.
func TestParseBootConfigPrivileged(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{value: "matchlock.privileged=1", want: true},
		{value: "matchlock.privileged=true", want: true},
		{value: "matchlock.privileged=0", want: false},
		{value: "", want: false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cmdline := filepath.Join(t.TempDir(), "cmdline")
			require.NoError(t, os.WriteFile(cmdline, []byte("matchlock.dns=1.1.1.1 "+tc.value), 0644))

			cfg, err := parseBootConfig(cmdline)
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.Privileged)
		})
	}
}

// TestNewSwapDevicePolicyDerivesDevNumbers uses /dev/null (char device 1:3) as
// a stable stand-in for the swap node: the derivation is identical and needs no
// privileges.
func TestNewSwapDevicePolicyDerivesDevNumbers(t *testing.T) {
	if _, err := os.Stat("/dev/null"); err != nil {
		t.Skipf("/dev/null unavailable: %v", err)
	}
	policy, err := newSwapDevicePolicy("null")
	require.NoError(t, err)
	assert.Equal(t, uint32(1), policy.Major)
	assert.Equal(t, uint32(3), policy.Minor)
}

func TestNewSwapDevicePolicyRejectsMissingDevice(t *testing.T) {
	if _, err := os.Stat("/dev/vdzz"); err == nil {
		t.Skip("/dev/vdzz unexpectedly exists")
	}
	_, err := newSwapDevicePolicy("vdzz")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSwapDevicePolicy)
}

// TestInstallSwapDevicePolicyRejectsMissingDevice proves the install path fails
// before any syscall when the swap node is gone: a silent skip would leave the
// VM-wide store reachable.
func TestInstallSwapDevicePolicyRejectsMissingDevice(t *testing.T) {
	if _, err := os.Stat("/dev/vdzz"); err == nil {
		t.Skip("/dev/vdzz unexpectedly exists")
	}
	err := installSwapDevicePolicy("vdzz", cgroup2RootPath)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSwapDevicePolicy)
}

// TestAttachSwapDeviceProgramWrapsError exercises the attach error path with an
// invalid target fd; it must wrap the sentinel rather than panic.
func TestAttachSwapDeviceProgramWrapsError(t *testing.T) {
	err := attachSwapDeviceProgram(-1, -1)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSwapDevicePolicy)
}

func TestVerifierLogTrimsPaddingAndKeepsFirstLine(t *testing.T) {
	assert.Equal(t, "no verifier log", verifierLog(make([]byte, 8)))
	assert.Equal(t,
		"processed 7 insns (limit 1000000) max_states_per_insn 0",
		verifierLog([]byte("processed 7 insns (limit 1000000) max_states_per_insn 0\nmore\x00\x00")))
}

// TestStatRdevIsDevNumbers is a guard for the derivation: it confirms the
// /dev/null rdev decoded through syscall.Stat_t matches unix.Major/Minor, which
// is the same source newSwapDevicePolicy uses for the swap node.
func TestStatRdevIsDevNumbers(t *testing.T) {
	info, err := os.Stat("/dev/null")
	if err != nil {
		t.Skipf("/dev/null unavailable: %v", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	assert.Equal(t, uint32(1), uint32(st.Rdev>>8))   // major
	assert.Equal(t, uint32(3), uint32(st.Rdev&0xff)) // minor
}
