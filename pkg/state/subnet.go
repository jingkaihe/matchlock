package state

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jingkaihe/matchlock/internal/errx"
	"modernc.org/sqlite"
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
	// IPv6 unique-local addressing for the same VM, derived from Octet so the
	// v6 /64 is as unique as the v4 /24 (see ipv6ForOctet).
	GatewayIPv6 string `json:"gateway_ipv6"` // Host TAP IPv6 (e.g., fd00:100::1)
	GuestIPv6   string `json:"guest_ipv6"`   // Guest IPv6 (e.g., fd00:100::2)
	Subnet6     string `json:"subnet6"`      // IPv6 CIDR (e.g., fd00:100::/64)
	VMID        string `json:"vm_id"`
}

// ipv6ForOctet derives the per-VM IPv6 unique-local (ULA) addressing from the
// same third octet that selects the IPv4 /24, so a single lease yields a
// collision-free v6 /64 next to it. The octet is rendered as the second 16-bit
// group of an fd00::/8 prefix: octet 100 -> fd00:100::/64, gateway fd00:100::1,
// guest fd00:100::2. All callers (Allocate, Get, backfill) go through this
// helper so the mapping can never drift.
func ipv6ForOctet(octet int) (gatewayIPv6, guestIPv6, subnet6 string) {
	return fmt.Sprintf("fd00:%d::1", octet),
		fmt.Sprintf("fd00:%d::2", octet),
		fmt.Sprintf("fd00:%d::/64", octet)
}

// fillIPv6 derives any missing IPv6 field from Octet. Rows written before the
// v6 columns existed (or with NULL columns) keep working instead of surfacing
// an empty address to the caller.
func (s *SubnetInfo) fillIPv6() {
	if s.GatewayIPv6 != "" && s.GuestIPv6 != "" && s.Subnet6 != "" {
		return
	}
	gatewayIPv6, guestIPv6, subnet6 := ipv6ForOctet(s.Octet)
	if s.GatewayIPv6 == "" {
		s.GatewayIPv6 = gatewayIPv6
	}
	if s.GuestIPv6 == "" {
		s.GuestIPv6 = guestIPv6
	}
	if s.Subnet6 == "" {
		s.Subnet6 = subnet6
	}
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
	return a.allocate(vmID, nil)
}

// allocate is the internal seam used by Allocate. If hook is non-nil it is called
// after the free-octet scan selects a candidate and before the candidate is
// inserted. Tests use it to deterministically force a genuine UNIQUE collision so
// the retry-on-UNIQUE-violation branch is provably exercised. In production hook
// is always nil, so behavior is unchanged.
func (a *SubnetAllocator) allocate(vmID string, hook func(octet int)) (*SubnetInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.ready(); err != nil {
		return nil, err
	}

	if existing, err := a.Get(vmID); err == nil {
		return existing, nil
	}

	// Retry to tolerate cross-process races: another matchlock process (e.g. a
	// concurrent sandbox under SDK --parallel) may insert the same octet between
	// our scan and INSERT. The process-local mutex does not serialize across
	// processes sharing the state.db, so on a UNIQUE violation we re-scan and
	// pick a different free octet. Bounded to avoid a livelock.
	const maxAttempts = 8
	for attempt := 0; attempt < maxAttempts; attempt++ {
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

		// Test seam: let a test serialize two allocators onto the same candidate
		// octet (genuine UNIQUE collision) before either inserts it.
		if hook != nil {
			hook(octet)
		}

		gatewayIPv6, guestIPv6, subnet6 := ipv6ForOctet(octet)
		info := &SubnetInfo{
			Octet:       octet,
			GatewayIP:   fmt.Sprintf("192.168.%d.1", octet),
			GuestIP:     fmt.Sprintf("192.168.%d.2", octet),
			Subnet:      fmt.Sprintf("192.168.%d.0/24", octet),
			GatewayIPv6: gatewayIPv6,
			GuestIPv6:   guestIPv6,
			Subnet6:     subnet6,
			VMID:        vmID,
		}

		_, err = a.db.Exec(
			`INSERT INTO subnet_allocations (vm_id, octet, gateway_ip, guest_ip, subnet, gateway_ip6, guest_ip6, subnet6, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			info.VMID,
			info.Octet,
			info.GatewayIP,
			info.GuestIP,
			info.Subnet,
			info.GatewayIPv6,
			info.GuestIPv6,
			info.Subnet6,
			time.Now().UTC().Format(time.RFC3339Nano),
		)
		if err == nil {
			return info, nil
		}
		// UNIQUE constraint failed on octet -> another process beat us; re-scan.
		if !isUniqueViolation(err) {
			return nil, errx.Wrap(ErrSaveSubnetAllocation, err)
		}
		// Same vm_id re-inserted concurrently -> return the existing row.
		if existing, gerr := a.Get(vmID); gerr == nil {
			return existing, nil
		}
	}
	return nil, errx.With(ErrNoAvailableSubnets, " (could not allocate a free octet after %d attempts)", maxAttempts)
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

	row := a.db.QueryRow(
		`SELECT octet, gateway_ip, guest_ip, subnet, gateway_ip6, guest_ip6, subnet6, vm_id
		 FROM subnet_allocations WHERE vm_id = ?`, vmID)
	var (
		info                        SubnetInfo
		gatewayIPv6, guestIPv6, sub6 sql.NullString
	)
	if err := row.Scan(
		&info.Octet, &info.GatewayIP, &info.GuestIP, &info.Subnet,
		&gatewayIPv6, &guestIPv6, &sub6, &info.VMID,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("subnet allocation not found for %s", vmID)
		}
		return nil, errx.Wrap(ErrSaveSubnetAllocation, err)
	}
	// Legacy rows (written before the v6 columns existed, or with a NULL
	// column) leave these empty; derive them from the octet so an in-flight
	// state.db keeps working.
	info.GatewayIPv6 = gatewayIPv6.String
	info.GuestIPv6 = guestIPv6.String
	info.Subnet6 = sub6.String
	info.fillIPv6()
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

// isUniqueViolation reports whether err is a SQLite UNIQUE constraint
// violation (SQLITE_CONSTRAINT_UNIQUE = 2067). modernc.org/sqlite surfaces it
// as a *sqlite.Error whose Code() returns 2067; we also fall back to substring
// matching for robustness across driver versions and wrapped errors.
func isUniqueViolation(err error) bool {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) && sqliteErr.Code() == 2067 {
		return true
	}
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// AllocationPath is retained for lifecycle/debug compatibility.
func (a *SubnetAllocator) AllocationPath(vmID string) string {
	return filepath.Join(a.baseDir, vmID+".json")
}
