package state

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func TestIsUniqueViolation_Message(t *testing.T) {
	if !isUniqueViolation(errors.New("constraint failed: UNIQUE constraint failed: subnet_allocations.octet")) {
		t.Fatal("expected UNIQUE violation to be detected")
	}
	if isUniqueViolation(errors.New("no such table")) {
		t.Fatal("must not classify unrelated errors as UNIQUE violations")
	}
	if isUniqueViolation(nil) {
		t.Fatal("nil must not be a UNIQUE violation")
	}
}

// TestAllocateRetriesOnUniqueCollision seeds a specific octet then allocates
// twice: the second allocation must pick a different free octet rather than
// erroring on the UNIQUE constraint. This models two concurrent matchlock
// processes racing to allocate the same octet from a shared state.db.
func TestAllocateRetriesOnUniqueCollision(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "subnets") // scope the DB inside the temp dir
	a := NewSubnetAllocatorWithDir(dir)
	if err := a.ready(); err != nil {
		t.Fatal(err)
	}

	// First allocation claims a free octet (min = 100).
	first, err := a.Allocate("vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Octet == 0 {
		t.Fatal("expected a non-zero octet")
	}

	// Simulate another process holding a second octet by inserting it directly.
	octet2 := first.Octet + 1
	if octet2 > a.maxOctet {
		octet2 = a.minOctet
	}
	if _, err := a.db.Exec(
		`INSERT INTO subnet_allocations (vm_id, octet, gateway_ip, guest_ip, subnet, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"vm-other", octet2, fmt.Sprintf("192.168.%d.1", octet2),
		fmt.Sprintf("192.168.%d.2", octet2), fmt.Sprintf("192.168.%d.0/24", octet2), "now",
	); err != nil {
		t.Fatal(err)
	}

	// vm-2 must NOT get octet2, and must not error on the collision.
	second, err := a.Allocate("vm-2")
	if err != nil {
		t.Fatalf("expected successful retry, got %v", err)
	}
	if second.Octet == octet2 {
		t.Fatalf("vm-2 must not reuse the concurrently-held octet %d", octet2)
	}
	if second.Octet == first.Octet {
		t.Fatalf("vm-2 must not reuse vm-1's octet %d", first.Octet)
	}
}
