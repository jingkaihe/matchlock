//go:build linux

package sandbox

import (
	"strings"
	"syscall"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/kvm"
)

func qemuOK() (bool, string) { return true, "" }

func TestSelectBackendKind(t *testing.T) {
	cases := []struct {
		name      string
		res       kvm.Result
		qemu      qemuPrereq
		want      backendKind
		wantErr   bool
		wantError string
	}{
		{"available->firecracker", kvm.Result{Status: kvm.StatusAvailable}, qemuOK, backendFirecracker, false, ""},
		{"not-present->qemu", kvm.Result{Status: kvm.StatusNotPresent, Op: "open", Err: syscall.ENOENT}, qemuOK, backendQEMU, false, ""},
		{"unusable->qemu", kvm.Result{Status: kvm.StatusUnusable, Op: "create_vm", Err: syscall.ENODEV}, qemuOK, backendQEMU, false, ""},
		{"incompatible->qemu", kvm.Result{Status: kvm.StatusIncompatible, Op: "get_api_version"}, qemuOK, backendQEMU, false, ""},
		{"permission->qemu", kvm.Result{Status: kvm.StatusPermissionDenied, Op: "open", Err: syscall.EACCES}, qemuOK, backendQEMU, false, ""},
		{"qemu-blocked->error", kvm.Result{Status: kvm.StatusNotPresent, Op: "open", Err: syscall.ENOENT}, func() (bool, string) { return false, "no qemu" }, 0, true, "qemu fallback unavailable"},
		{"unexpected->error", kvm.Result{Status: kvm.StatusUnexpected, Op: "create_vm", Err: syscall.ENOMEM}, qemuOK, 0, true, "refusing to guess"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := selectBackendKind(c.res, c.qemu)
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, c.wantErr)
			}
			if err == nil {
				if got != c.want {
					t.Fatalf("kind=%v want %v", got, c.want)
				}
				return
			}
			if !strings.Contains(err.Error(), c.wantError) {
				t.Fatalf("err=%q missing %q", err.Error(), c.wantError)
			}
		})
	}
}

func TestBackendKindString(t *testing.T) {
	if backendFirecracker.String() != "firecracker" {
		t.Fatalf("firecracker=%q", backendFirecracker.String())
	}
	if backendQEMU.String() != "qemu" {
		t.Fatalf("qemu=%q", backendQEMU.String())
	}
}

func TestValidateBackendConstraints(t *testing.T) {
	mkNet := func(noNetwork bool) *api.Config {
		n := &api.NetworkConfig{NoNetwork: noNetwork}
		return &api.Config{Network: n}
	}

	t.Run("firecracker-accepts-all", func(t *testing.T) {
		// Firecracker must accept network-enabled and VFS configs.
		if err := validateBackendConstraints(backendFirecracker, mkNet(false)); err != nil {
			t.Fatalf("firecracker rejected network config: %v", err)
		}
	})

	t.Run("qemu-accepts-no-network", func(t *testing.T) {
		if err := validateBackendConstraints(backendQEMU, mkNet(true)); err != nil {
			t.Fatalf("qemu rejected --no-network: %v", err)
		}
	})

	t.Run("qemu-accepts-networking", func(t *testing.T) {
		// QEMU now supports TAP networking; it must accept a network-enabled
		// config so the sandbox provisions NAT/proxy for the TAP.
		if err := validateBackendConstraints(backendQEMU, mkNet(false)); err != nil {
			t.Fatalf("qemu rejected networking: %v", err)
		}
	})
	t.Run("qemu-accepts-nil-network", func(t *testing.T) {
		// Network == nil means networking enabled by default; QEMU accepts it.
		if err := validateBackendConstraints(backendQEMU, &api.Config{}); err != nil {
			t.Fatalf("qemu rejected nil network: %v", err)
		}
	})

	t.Run("qemu-accepts-vfs", func(t *testing.T) {
		// QEMU now supports guest-side VFS (workspace/volumes) over a per-sandbox
		// vsock listener.
		c := mkNet(true)
		c.VFS = &api.VFSConfig{Mounts: map[string]api.MountConfig{"/ws": {}}}
		if err := validateBackendConstraints(backendQEMU, c); err != nil {
			t.Fatalf("qemu should accept vfs, got %v", err)
		}
	})

	t.Run("qemu-accepts-interception", func(t *testing.T) {
		// QEMU now supports the host-side transparent proxy / MITM / policy
		// engine, which binds to the TAP gateway both backends provide. A
		// network-enabled config with interception must be accepted.
		c := mkNet(false)
		c.Network.Intercept = true
		c.Network.AllowedHosts = []string{"example.com"}
		c.Network.Secrets = map[string]api.Secret{"API_KEY": {Value: "s3cr3t"}}
		if err := validateBackendConstraints(backendQEMU, c); err != nil {
			t.Fatalf("qemu should accept interception, got %v", err)
		}
	})
}
