//go:build linux

package guestfused

import "testing"

func TestParseVFSPortFromCmdline_Parses32Bit(t *testing.T) {
	// AF_VSOCK ports are 32-bit; a kernel-assigned port may exceed 65535.
	got, err := parseVFSPortFromCmdline("console=ttyS0 matchlock.vfs_port=4000000001")
	if err != nil {
		t.Fatal(err)
	}
	if got != 4000000001 {
		t.Fatalf("expected 4000000001, got %d", got)
	}
}

func TestParseVFSPortFromCmdline_Default(t *testing.T) {
	// No override -> the well-known default port, without error.
	got, err := parseVFSPortFromCmdline("console=ttyS0")
	if err != nil {
		t.Fatal(err)
	}
	if got != uint32(VsockPortVFS) {
		t.Fatalf("expected default %d, got %d", VsockPortVFS, got)
	}
}

func TestParseVFSPortFromCmdline_Malformed(t *testing.T) {
	for _, bad := range []string{"notanumber", "0", "-5", "4294967296", "99999999999999999999"} {
		if got, err := parseVFSPortFromCmdline("matchlock.vfs_port=" + bad); err == nil {
			t.Fatalf("expected error for %q, got port %d", bad, got)
		}
	}
}

func TestParseVFSPortFromCmdline_ReservedAny(t *testing.T) {
	if got, err := parseVFSPortFromCmdline("matchlock.vfs_port=4294967295"); err == nil {
		t.Fatalf("expected error for VMADDR_PORT_ANY, got port %d", got)
	}
}

func TestParseVFSPortFromCmdline_Duplicate(t *testing.T) {
	if got, err := parseVFSPortFromCmdline("matchlock.vfs_port=5007 matchlock.vfs_port=5008"); err == nil {
		t.Fatalf("expected duplicate-arg error, got port %d", got)
	}
}

func TestParseVFSPortFromCmdline_DuplicateEmptyFirst(t *testing.T) {
	// A duplicate (even one whose first instance is empty) must be rejected
	// rather than silently selecting the last value.
	if got, err := parseVFSPortFromCmdline("matchlock.vfs_port= matchlock.vfs_port=5007"); err == nil {
		t.Fatalf("expected duplicate-arg error, got port %d", got)
	}
}

func TestParseVFSPortFromCmdline_Empty(t *testing.T) {
	if got, err := parseVFSPortFromCmdline("matchlock.vfs_port="); err == nil {
		t.Fatalf("expected empty-arg error, got port %d", got)
	}
}
