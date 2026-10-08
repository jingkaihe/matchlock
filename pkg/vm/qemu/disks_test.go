//go:build linux

package qemu

import (
	"testing"

	"github.com/jingkaihe/matchlock/pkg/vm"
	"github.com/stretchr/testify/assert"
)

func TestDiskKernelArgUIDGID(t *testing.T) {
	uid := uint32(1234)
	gid := uint32(1235)

	got := diskKernelArg(vm.DiskConfig{GuestMount: "/pgdata", OwnerUID: &uid, OwnerGID: &gid})
	assert.Equal(t, "/pgdata,uid=1234,gid=1235", got)
}

func TestDiskKernelArgReadOnly(t *testing.T) {
	got := diskKernelArg(vm.DiskConfig{GuestMount: "/foo", ReadOnly: true})
	assert.Equal(t, "/foo,ro", got)
}

func TestDiskKernelArgNoOwner(t *testing.T) {
	got := diskKernelArg(vm.DiskConfig{GuestMount: "/bar"})
	assert.Equal(t, "/bar", got)
}
