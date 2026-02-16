//go:build linux

package guestagent

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildSeccompFilter(t *testing.T) {
	filter := buildSeccompFilter()

	// 3 header instructions + N blocked syscalls + 2 return instructions
	blocked, _ := blockedSyscalls()
	expectedLen := 3 + len(blocked) + 2
	assert.Len(t, filter, expectedLen)

	// First instruction: load architecture (offset 4 in seccomp_data)
	assert.Equal(t, uint16(bpfLD|bpfW|bpfABS), filter[0].Code, "first instruction should load arch")
	assert.Equal(t, uint32(4), filter[0].K, "first instruction offset should be 4")

	// Second instruction: check architecture
	assert.Equal(t, uint16(bpfJMP|bpfJEQ|bpfK), filter[1].Code, "second instruction should be arch check jump")

	// Third instruction: load syscall number (offset 0)
	assert.Equal(t, uint16(bpfLD|bpfW|bpfABS), filter[2].Code, "third instruction should load syscall nr")
	assert.Equal(t, uint32(0), filter[2].K, "third instruction offset should be 0")

	// Second-to-last: ALLOW
	allowIdx := len(filter) - 2
	assert.Equal(t, uint16(bpfRET|bpfK), filter[allowIdx].Code, "second-to-last should be RET")
	assert.Equal(t, uint32(seccompRetAllow), filter[allowIdx].K, "second-to-last should be ALLOW")

	// Last: ERRNO(EPERM)
	lastIdx := len(filter) - 1
	assert.Equal(t, uint16(bpfRET|bpfK), filter[lastIdx].Code, "last should be RET")
	assert.Equal(t, uint32(seccompRetErrno|errnoEPERM), filter[lastIdx].K, "last should be ERRNO(EPERM)")
}

func TestBlockedSyscalls(t *testing.T) {
	blocked, auditArch := blockedSyscalls()

	require.Len(t, blocked, 5)

	switch runtime.GOARCH {
	case "amd64":
		assert.Equal(t, uint32(auditArchX86_64), auditArch)
		expected := []uint32{
			sysProcessVMReadvAmd64,
			sysProcessVMWritevAmd64,
			sysPtraceAmd64,
			sysKexecLoadAmd64,
			sysKexecFileLoadAmd64,
		}
		for i, nr := range expected {
			assert.Equal(t, nr, blocked[i], "blocked[%d]", i)
		}
	case "arm64":
		assert.Equal(t, uint32(auditArchAARCH64), auditArch)
	}
}

func TestWipeBytes(t *testing.T) {
	data := []byte("secret-api-key-value")
	wipeBytes(data)
	for i, b := range data {
		assert.Equal(t, byte(0), b, "byte %d not wiped", i)
	}
}

func TestWipeMap(t *testing.T) {
	m := map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-api03-xxx",
		"OTHER_KEY":         "other-value",
	}
	wipeMap(m)
	assert.Empty(t, m)
}

func TestIsSandboxLauncher(t *testing.T) {
	assert.False(t, isSandboxLauncher(), "should not be sandbox launcher by default")

	t.Setenv(sandboxLauncherEnvKey, "1")
	assert.True(t, isSandboxLauncher(), "should be sandbox launcher when env var is set")
}
