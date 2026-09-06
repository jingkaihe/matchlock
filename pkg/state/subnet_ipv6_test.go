package state

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// TestSubnetInfoIPv6JSONFields locks the wire shape of the new addressing: the
// fields are part of the JSON contract consumed by tooling, so a rename would be
// a silent breakage.
func TestSubnetInfoIPv6JSONFields(t *testing.T) {
	gatewayIPv6, guestIPv6, subnet6 := ipv6ForOctet(100)
	raw, err := json.Marshal(SubnetInfo{
		Octet:       100,
		GatewayIP:   "192.168.100.1",
		GuestIP:     "192.168.100.2",
		Subnet:      "192.168.100.0/24",
		GatewayIPv6: gatewayIPv6,
		GuestIPv6:   guestIPv6,
		Subnet6:     subnet6,
		VMID:        "vm-json",
	})
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Equal(t, "fd00:100::1", decoded["gateway_ipv6"])
	require.Equal(t, "fd00:100::2", decoded["guest_ipv6"])
	require.Equal(t, "fd00:100::/64", decoded["subnet6"])
}

// TestIPv6ForOctetDerivation pins the octet -> ULA mapping for the whole lease
// range and proves every derived value is a usable IPv6 address/CIDR.
func TestIPv6ForOctetDerivation(t *testing.T) {
	cases := []struct {
		octet   int
		gateway string
		guest   string
		subnet6 string
	}{
		{octet: 100, gateway: "fd00:100::1", guest: "fd00:100::2", subnet6: "fd00:100::/64"},
		{octet: 137, gateway: "fd00:137::1", guest: "fd00:137::2", subnet6: "fd00:137::/64"},
		{octet: 254, gateway: "fd00:254::1", guest: "fd00:254::2", subnet6: "fd00:254::/64"},
	}

	seen := map[string]int{}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("octet-%d", tc.octet), func(t *testing.T) {
			gateway, guest, subnet6 := ipv6ForOctet(tc.octet)
			require.Equal(t, tc.gateway, gateway)
			require.Equal(t, tc.guest, guest)
			require.Equal(t, tc.subnet6, subnet6)

			ip := net.ParseIP(gateway)
			require.NotNil(t, ip, "gateway %q must parse as an IPv6 address", gateway)
			require.Nil(t, ip.To4(), "gateway %q must not be an IPv4 address", gateway)
			require.True(t, ip.IsPrivate(), "gateway %q must be a unique-local v6 address", gateway)
			require.NotNil(t, net.ParseIP(guest), "guest %q must parse as an IPv6 address", guest)

			_, network, err := net.ParseCIDR(subnet6)
			require.NoError(t, err, "subnet6 %q must be valid CIDR", subnet6)
			require.Equal(t, 64, ones(network))
			require.True(t, network.Contains(net.ParseIP(guest)), "guest must live inside %s", subnet6)
			require.True(t, network.Contains(net.ParseIP(gateway)), "gateway must live inside %s", subnet6)

			seen[subnet6]++
		})
	}
	require.Len(t, seen, len(cases), "each octet must map to a distinct /64")
}

func ones(n *net.IPNet) int {
	o, _ := n.Mask.Size()
	return o
}

// TestAllocateDerivesIPv6FromOctet covers the production path: a lease returns
// the v6 trio derived from the octet it selected, and the value survives the
// database round-trip through Get.
func TestAllocateDerivesIPv6FromOctet(t *testing.T) {
	dir := allocBaseDir(t)
	a := NewSubnetAllocatorWithDir(dir)
	require.NoError(t, a.ready())
	defer a.db.Close()

	info, err := a.Allocate("vm-v6-derive")
	require.NoError(t, err)
	require.NotZero(t, info.Octet)

	require.Equal(t, fmt.Sprintf("fd00:%d::1", info.Octet), info.GatewayIPv6)
	require.Equal(t, fmt.Sprintf("fd00:%d::2", info.Octet), info.GuestIPv6)
	require.Equal(t, fmt.Sprintf("fd00:%d::/64", info.Octet), info.Subnet6)
	// The IPv4 lease must be untouched by the v6 work.
	require.Equal(t, fmt.Sprintf("192.168.%d.1", info.Octet), info.GatewayIP)
	require.Equal(t, fmt.Sprintf("192.168.%d.2", info.Octet), info.GuestIP)
	require.Equal(t, fmt.Sprintf("192.168.%d.0/24", info.Octet), info.Subnet)

	got, err := a.Get("vm-v6-derive")
	require.NoError(t, err)
	require.Equal(t, info, got, "Get must return the same v6 addressing Allocate handed out")

	// Re-allocating the same VMID is idempotent and keeps the same /64.
	again, err := a.Allocate("vm-v6-derive")
	require.NoError(t, err)
	require.Equal(t, info, again)
}

// TestAllocateIPv6UniqueAcrossVMs drives two allocators (own handle, own mutex,
// shared state.db) concurrently: the IPv4 octet uniqueness must carry the v6
// /64s apart, so no two VMs ever share a gateway or guest address.
func TestAllocateIPv6UniqueAcrossVMs(t *testing.T) {
	dir := allocBaseDir(t)
	const n = 4

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = make([]*SubnetInfo, n)
		errs    = make([]error, n)
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			a := NewSubnetAllocatorWithDir(dir)
			if err := a.ready(); err != nil {
				errs[idx] = err
				return
			}
			defer a.db.Close()
			info, err := a.Allocate(fmt.Sprintf("vm-v6-%d", idx))
			if err != nil {
				errs[idx] = err
				return
			}
			mu.Lock()
			results[idx] = info
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	subnets6 := map[string]string{}
	gateways6 := map[string]string{}
	guests6 := map[string]string{}
	for i, err := range errs {
		require.NoError(t, err, "alloc %d", i)
		require.NotNil(t, results[i])
		info := results[i]
		require.Equal(t, fmt.Sprintf("fd00:%d::/64", info.Octet), info.Subnet6)
		require.Equal(t, fmt.Sprintf("fd00:%d::1", info.Octet), info.GatewayIPv6)
		require.Equal(t, fmt.Sprintf("fd00:%d::2", info.Octet), info.GuestIPv6)

		require.NotContains(t, subnets6, info.Subnet6, "duplicate v6 /64: %s and %s", subnets6[info.Subnet6], info.VMID)
		require.NotContains(t, gateways6, info.GatewayIPv6, "duplicate v6 gateway: %s and %s", gateways6[info.GatewayIPv6], info.VMID)
		require.NotContains(t, guests6, info.GuestIPv6, "duplicate v6 guest: %s and %s", guests6[info.GuestIPv6], info.VMID)
		subnets6[info.Subnet6] = info.VMID
		gateways6[info.GatewayIPv6] = info.VMID
		guests6[info.GuestIPv6] = info.VMID
	}
	require.Len(t, subnets6, n)
}

// TestSubnetIPv6MigrationIdempotent reopens the same state.db three times. The
// column-adding migration must apply exactly once (the second and third Open hit
// the version ledger and must not re-run ALTER TABLE, which would fail with
// "duplicate column name").
func TestSubnetIPv6MigrationIdempotent(t *testing.T) {
	dir := allocBaseDir(t)

	a := NewSubnetAllocatorWithDir(dir)
	require.NoError(t, a.ready())
	first, err := a.Allocate("vm-migrate-1")
	require.NoError(t, err)
	require.NoError(t, a.db.Close())

	for _, attempt := range []string{"second", "third"} {
		b := NewSubnetAllocatorWithDir(dir)
		require.NoError(t, b.ready(), "%s open of the same state.db must succeed", attempt)

		cols := subnetAllocationColumns(t, b.db)
		for _, col := range []string{"gateway_ip6", "guest_ip6", "subnet6"} {
			require.True(t, cols[col], "%s open must see column %s", attempt, col)
		}

		var rows int
		require.NoError(t, b.db.QueryRow(
			`SELECT COUNT(*) FROM schema_migrations WHERE module = ? AND version = 3`, stateModule,
		).Scan(&rows))
		require.Equal(t, 1, rows, "migration version 3 must be recorded exactly once")

		// The allocation from the first open is still readable.
		got, err := b.Get("vm-migrate-1")
		require.NoError(t, err)
		require.Equal(t, first.Subnet6, got.Subnet6)
		require.NoError(t, b.db.Close())
	}
}

// TestGetBackfillsLegacyIPv6Row builds a state.db exactly as the pre-IPv6 code
// left it (migrations 1-2 applied, an allocation row with no v6 columns) and
// proves Get derives the v6 trio from the octet instead of erroring or returning
// empty addresses.
func TestGetBackfillsLegacyIPv6Row(t *testing.T) {
	dir := allocBaseDir(t)
	writeLegacyStateDB(t, dir)

	a := NewSubnetAllocatorWithDir(dir)
	require.NoError(t, a.ready())
	defer a.db.Close()

	info, err := a.Get("vm-legacy")
	require.NoError(t, err, "a legacy row must not make Get fail")
	require.Equal(t, 137, info.Octet)
	require.Equal(t, "192.168.137.0/24", info.Subnet)
	require.Equal(t, "fd00:137::/64", info.Subnet6)
	require.Equal(t, "fd00:137::1", info.GatewayIPv6)
	require.Equal(t, "fd00:137::2", info.GuestIPv6)

	// The row itself was left alone: the backfill is a read-time derivation.
	var stored string
	require.NoError(t, a.db.QueryRow(
		`SELECT subnet6 FROM subnet_allocations WHERE vm_id = ?`, "vm-legacy",
	).Scan(&stored))
	require.Empty(t, stored, "Get must derive, not rewrite, the legacy row")

	// A new allocation on the upgraded legacy db still respects the lease held
	// by the legacy row.
	fresh, err := a.Allocate("vm-after-upgrade")
	require.NoError(t, err)
	require.NotEqual(t, 137, fresh.Octet)
	require.Equal(t, fmt.Sprintf("fd00:%d::/64", fresh.Octet), fresh.Subnet6)
}

// TestFillIPv6PartialRow proves a row with only some v6 columns populated is
// completed from the octet rather than mixing derived and stored values.
func TestFillIPv6PartialRow(t *testing.T) {
	info := &SubnetInfo{Octet: 200, GatewayIPv6: "fd00:200::9"}
	info.fillIPv6()
	require.Equal(t, "fd00:200::9", info.GatewayIPv6, "an explicit value must be preserved")
	require.Equal(t, "fd00:200::2", info.GuestIPv6)
	require.Equal(t, "fd00:200::/64", info.Subnet6)
}

func subnetAllocationColumns(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(subnet_allocations)`)
	require.NoError(t, err)
	defer rows.Close()

	cols := map[string]bool{}
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		require.NoError(t, rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk))
		cols[name] = true
	}
	require.NoError(t, rows.Err())
	return cols
}

// writeLegacyStateDB materialises the pre-IPv6 database: the schema produced by
// migrations 1 and 2 (reusing their real SQL so the fixture cannot drift), the
// matching ledger rows, and one allocation that predates the v6 columns.
func writeLegacyStateDB(t *testing.T, dir string) {
	t.Helper()
	path := stateDBPath(dir)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
  module TEXT NOT NULL,
  version INTEGER NOT NULL,
  name TEXT NOT NULL,
  applied_at TEXT NOT NULL,
  PRIMARY KEY (module, version)
)`)
	require.NoError(t, err)

	for _, m := range stateMigrations() {
		if m.Version > 2 {
			continue
		}
		_, err = db.Exec(m.SQL)
		require.NoError(t, err, "legacy migration %d", m.Version)
		_, err = db.Exec(
			`INSERT INTO schema_migrations (module, version, name, applied_at) VALUES (?, ?, ?, ?)`,
			stateModule, m.Version, m.Name, "2020-01-01T00:00:00Z",
		)
		require.NoError(t, err)
	}

	_, err = db.Exec(
		`INSERT INTO subnet_allocations (vm_id, octet, gateway_ip, guest_ip, subnet, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"vm-legacy", 137, "192.168.137.1", "192.168.137.2", "192.168.137.0/24", "2020-01-01T00:00:00Z",
	)
	require.NoError(t, err)
}
