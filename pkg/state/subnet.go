package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// SubnetAllocator manages unique /24 subnet allocation for VMs
// Uses 192.168.X.0/24 where X ranges from 100-254
type SubnetAllocator struct {
	mu       sync.Mutex
	baseDir  string
	minOctet int
	maxOctet int
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
	os.MkdirAll(baseDir, 0755)

	return &SubnetAllocator{
		baseDir:  baseDir,
		minOctet: 100,
		maxOctet: 254,
	}
}

// Allocate assigns a unique subnet to a VM
func (a *SubnetAllocator) Allocate(vmID string) (*SubnetInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Find an available octet
	used := make(map[int]bool)
	entries, _ := os.ReadDir(a.baseDir)
	for _, entry := range entries {
		if !entry.IsDir() {
			data, err := os.ReadFile(filepath.Join(a.baseDir, entry.Name()))
			if err != nil {
				continue
			}
			var info SubnetInfo
			if json.Unmarshal(data, &info) == nil {
				used[info.Octet] = true
			}
		}
	}

	// Find first available octet
	var octet int
	for o := a.minOctet; o <= a.maxOctet; o++ {
		if !used[o] {
			octet = o
			break
		}
	}

	if octet == 0 {
		return nil, fmt.Errorf("no available subnets (all %d-%d in use)", a.minOctet, a.maxOctet)
	}

	info := &SubnetInfo{
		Octet:     octet,
		GatewayIP: fmt.Sprintf("192.168.%d.1", octet),
		GuestIP:   fmt.Sprintf("192.168.%d.2", octet),
		Subnet:    fmt.Sprintf("192.168.%d.0/24", octet),
		VMID:      vmID,
	}

	// Save allocation
	data, _ := json.Marshal(info)
	if err := os.WriteFile(filepath.Join(a.baseDir, vmID+".json"), data, 0644); err != nil {
		return nil, fmt.Errorf("failed to save subnet allocation: %w", err)
	}

	return info, nil
}

// Release frees a subnet allocation
func (a *SubnetAllocator) Release(vmID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	path := filepath.Join(a.baseDir, vmID+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Get retrieves subnet info for a VM
func (a *SubnetAllocator) Get(vmID string) (*SubnetInfo, error) {
	path := filepath.Join(a.baseDir, vmID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var info SubnetInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// Cleanup removes all stale subnet allocations (VMs that no longer exist)
func (a *SubnetAllocator) Cleanup(mgr *Manager) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	entries, err := os.ReadDir(a.baseDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		vmID := entry.Name()
		if len(vmID) > 5 && vmID[len(vmID)-5:] == ".json" {
			vmID = vmID[:len(vmID)-5]
		}

		// Check if VM still exists
		if _, err := mgr.Get(vmID); err != nil {
			os.Remove(filepath.Join(a.baseDir, entry.Name()))
		}
	}

	return nil
}
