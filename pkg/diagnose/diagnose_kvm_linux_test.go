//go:build linux

package diagnose

import (
	"strings"
	"syscall"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/kvm"
)

func TestKVMAccelerationCheckMapsAllStatuses(t *testing.T) {
	cases := []struct {
		name       string
		res        kvm.Result
		wantStatus Status
		wantFix    string
	}{
		{"available", kvm.Result{Status: kvm.StatusAvailable}, StatusPass, ""},
		{"not-present", kvm.Result{Status: kvm.StatusNotPresent, Op: "open", Err: syscall.ENOENT}, StatusFail, "Enable CPU virtualization"},
		{"permission-denied", kvm.Result{Status: kvm.StatusPermissionDenied, Op: "open", Err: syscall.EACCES}, StatusFail, "kvm group"},
		{"incompatible", kvm.Result{Status: kvm.StatusIncompatible, Op: "get_api_version"}, StatusFail, "nested virtualization"},
		{"unusable", kvm.Result{Status: kvm.StatusUnusable, Op: "create_vm", Err: syscall.ENODEV}, StatusFail, "nested virtualization"},
		{"unexpected", kvm.Result{Status: kvm.StatusUnexpected, Op: "create_vm", Err: syscall.ENOMEM}, StatusWarn, "KVM probe failed unexpectedly"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			chk := kvmAccelerationCheck(c.res)
			if chk.Status != c.wantStatus {
				t.Fatalf("status=%v want %v (msg=%q)", chk.Status, c.wantStatus, chk.Message)
			}
			if c.wantFix == "" {
				return
			}
			if !strings.Contains(chk.Fix, c.wantFix) {
				t.Fatalf("fix=%q does not contain %q", chk.Fix, c.wantFix)
			}
		})
	}
}
