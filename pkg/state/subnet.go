package state

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// SubnetAllocator manages unique /24 subnet allocation for VMs
// Uses 192.168.X.0/24 where X ranges from 100-254.
type SubnetAllocator struct {
	mu       sync.Mutex
	baseDir  string
	minOctet int
	maxOctet int
	db       *sql.DB
	initErr  error
}

type SubnetInfo struct {
	Octet     int    `json:"octet"`      // Third octet (e.g., 100 for 192.168.100.0/24)
	GatewayIP string `json:"gateway_ip"` // Host TAP IP (e.g., 192.168.100.1)
	GuestIP   string `json:"guest_ip"`   // Guest IP (e.g., 192.168.100.2)
	Subnet    string `json:"subnet"`     // CIDR notation (e.g., 192.168.100.0/24)
	VMID      string `json:"vm_id"`
}

func NewSubnetAllocator() *SubnetAllocator {
	home, _ := os.UserHomeDir()
	baseDir := filepath.Join(home, ".matchlock", "subnets")
	return NewSubnetAllocatorWithDir(baseDir)
}

func NewSubnetAllocatorWithDir(baseDir string) *SubnetAllocator {
	db, err := openStateDB(baseDir)
	return &SubnetAllocator{
		baseDir:  baseDir,
		minOctet: 100,
		maxOctet: 254,
		db:       db,
		initErr:  err,
	}
}

func (a *SubnetAllocator) ready() error {
	if a.initErr != nil {
		return errx.Wrap(ErrStateDBInit, a.initErr)
	}
	if a.db == nil {
		return ErrStateDBInit
	}
	return nil
}

// Allocate assigns a unique subnet to a VM.
func (a *SubnetAllocator) Allocate(vmID string) (*SubnetInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.ready(); err != nil {
		return nil, err
	}

	if existing, err := a.Get(vmID); err == nil {
		return existing, nil
	}

	used, err := a.usedOctets()
	if err != nil {
		return nil, err
	}

	var octet int
	for o := a.minOctet; o <= a.maxOctet; o++ {
		if !used[o] {
			octet = o
			break
		}
	}
	if octet == 0 {
		return nil, errx.With(ErrNoAvailableSubnets, " (all %d-%d in use)", a.minOctet, a.maxOctet)
	}

	info := &SubnetInfo{
		Octet:     octet,
		GatewayIP: fmt.Sprintf("192.168.%d.1", octet),
		GuestIP:   fmt.Sprintf("192.168.%d.2", octet),
		Subnet:    fmt.Sprintf("192.168.%d.0/24", octet),
		VMID:      vmID,
	}

	_, err = a.db.Exec(
		`INSERT INTO subnet_allocations (vm_id, octet, gateway_ip, guest_ip, subnet, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		info.VMID,
		info.Octet,
		info.GatewayIP,
		info.GuestIP,
		info.Subnet,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, errx.Wrap(ErrSaveSubnetAllocation, err)
	}
	return info, nil
}

func (a *SubnetAllocator) usedOctets() (map[int]bool, error) {
	rows, err := a.db.Query(`SELECT octet FROM subnet_allocations`)
	if err != nil {
		return nil, errx.Wrap(ErrSaveSubnetAllocation, err)
	}
	defer rows.Close()

	used := make(map[int]bool)
	for rows.Next() {
		var octet int
		if err := rows.Scan(&octet); err != nil {
			return nil, errx.Wrap(ErrSaveSubnetAllocation, err)
		}
		used[octet] = true
	}
	if err := rows.Err(); err != nil {
		return nil, errx.Wrap(ErrSaveSubnetAllocation, err)
	}
	return used, nil
}

// Release frees a subnet allocation.
func (a *SubnetAllocator) Release(vmID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.ready(); err != nil {
		return err
	}

	if _, err := a.db.Exec(`DELETE FROM subnet_allocations WHERE vm_id = ?`, vmID); err != nil {
		return errx.Wrap(ErrSaveSubnetAllocation, err)
	}
	return nil
}

// Get retrieves subnet info for a VM.
func (a *SubnetAllocator) Get(vmID string) (*SubnetInfo, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}

	row := a.db.QueryRow(`SELECT octet, gateway_ip, guest_ip, subnet, vm_id FROM subnet_allocations WHERE vm_id = ?`, vmID)
	var info SubnetInfo
	if err := row.Scan(&info.Octet, &info.GatewayIP, &info.GuestIP, &info.Subnet, &info.VMID); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("subnet allocation not found for %s", vmID)
		}
		return nil, errx.Wrap(ErrSaveSubnetAllocation, err)
	}
	return &info, nil
}

// Cleanup removes all stale subnet allocations (VMs that no longer exist).
func (a *SubnetAllocator) Cleanup(mgr *Manager) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.ready(); err != nil {
		return err
	}

	rows, err := a.db.Query(`SELECT vm_id FROM subnet_allocations`)
	if err != nil {
		return errx.Wrap(ErrSaveSubnetAllocation, err)
	}
	defer rows.Close()

	var stale []string
	for rows.Next() {
		var vmID string
		if err := rows.Scan(&vmID); err != nil {
			return errx.Wrap(ErrSaveSubnetAllocation, err)
		}
		if _, err := mgr.Get(vmID); err != nil {
			stale = append(stale, vmID)
		}
	}
	if err := rows.Err(); err != nil {
		return errx.Wrap(ErrSaveSubnetAllocation, err)
	}
	for _, vmID := range stale {
		if _, err := a.db.Exec(`DELETE FROM subnet_allocations WHERE vm_id = ?`, vmID); err != nil {
			return errx.Wrap(ErrSaveSubnetAllocation, err)
		}
	}
	return nil
}

// AllocationPath is retained for lifecycle/debug compatibility.
func (a *SubnetAllocator) AllocationPath(vmID string) string {
	return filepath.Join(a.baseDir, vmID+".json")
}
