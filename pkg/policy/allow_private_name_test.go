package policy

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests exercise the allow_private NAME entry resolver introduced for the
// passthrough defect: a NAME entry must cover the addresses the name resolves
// to, resolved host-side into a TTL-bounded address set, so a later decision on
// a bare destination IP can match it without a DNS lookup per connection.
//
// They use the package's process-local synthetic resolver (dnssynth_test.go),
// which replaces net.DefaultResolver, so they must NOT run with t.Parallel().

// TestNameEntryAddresses_ResolvesDNSAnswer covers acceptance criterion 1: a NAME
// entry resolves to the address set its DNS answer provides, visible through the
// unexported engine accessor.
func TestNameEntryAddresses_ResolvesDNSAnswer(t *testing.T) {
	t.Run("A answer", func(t *testing.T) {
		dns := newPolicySyntheticDNS(t, map[string][][]string{
			"priv-name.test.": {{"10.1.2.3", "10.1.2.4"}},
		})
		dns.install()
		defer dns.uninstall()

		engine := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"priv-name.test:8888"},
		})

		addresses := engine.nameEntryAddresses("priv-name.test")
		require.Len(t, addresses, 2)
		assert.Equal(t, "10.1.2.3", addresses[0].String())
		assert.Equal(t, "10.1.2.4", addresses[1].String())
		assert.Equal(t, 1, dns.aCount("priv-name.test."),
			"resolving the NAME entry must issue exactly one lookup")
	})

	t.Run("AAAA answer and name normalization", func(t *testing.T) {
		dns := newPolicySyntheticDNSWithAAAA(t,
			nil,
			map[string][][]string{"priv-v6.test.": {{"fd00:5::9"}}},
		)
		dns.install()
		defer dns.uninstall()

		engine := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"[priv-v6.test]:8888"},
		})

		// The accessor is case-insensitive and tolerates a trailing root dot,
		// like a DNS answer key would.
		addresses := engine.nameEntryAddresses("PRIV-V6.TEST.")
		require.Len(t, addresses, 1)
		assert.Equal(t, "fd00:5::9", addresses[0].String())
	})
}

// TestNameEntryAddresses_CachedWithinTTL covers acceptance criterion 2: every
// lookup inside the TTL is served from the cache, so a name that is consulted
// per connection still issues at most one DNS query per TTL.
func TestNameEntryAddresses_CachedWithinTTL(t *testing.T) {
	dns := newPolicySyntheticDNS(t, map[string][][]string{
		"cached.test.": {{"10.7.7.7"}},
	})
	dns.install()
	defer dns.uninstall()

	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"cached.test:8888"},
	})

	for i := 0; i < 3; i++ {
		addresses := engine.nameEntryAddresses("cached.test")
		require.Len(t, addresses, 1)
		assert.Equal(t, "10.7.7.7", addresses[0].String())
	}

	// The public decision entry points warm the cache off-lock. The destination
	// is a public literal, so the decision itself resolves nothing and every
	// query counted below comes from the NAME entry resolver.
	require.True(t, engine.IsHostAllowedPort("203.0.113.50", 8888))
	require.True(t, engine.IsHostAllowed("203.0.113.50:8888"))

	assert.Equal(t, 1, dns.aCount("cached.test."),
		"repeated lookups inside the TTL must issue exactly one DNS query")
}

// TestNameEntryAddresses_RefreshAfterTTL covers acceptance criterion 3: once the
// TTL elapses the name is resolved again and the NEW answer set is the one used.
func TestNameEntryAddresses_RefreshAfterTTL(t *testing.T) {
	dns := newPolicySyntheticDNS(t, map[string][][]string{
		"staged.test.": {
			{"10.4.4.4"}, // first resolution
			{"10.4.4.5"}, // after the TTL elapsed
		},
	})
	dns.install()
	defer dns.uninstall()

	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"staged.test:8888"},
	})

	current := time.Now()
	engine.now = func() time.Time { return current }

	addresses := engine.nameEntryAddresses("staged.test")
	require.Len(t, addresses, 1)
	assert.Equal(t, "10.4.4.4", addresses[0].String())
	assert.Equal(t, 1, dns.aCount("staged.test."))

	// Still inside the TTL: the cached answer is served, no new query.
	current = current.Add(engine.nameTTLValue() - time.Second)
	addresses = engine.nameEntryAddresses("staged.test")
	require.Len(t, addresses, 1)
	assert.Equal(t, "10.4.4.4", addresses[0].String(),
		"an entry inside the TTL must not be re-resolved")
	assert.Equal(t, 1, dns.aCount("staged.test."))

	// Past the TTL: re-resolution happens and the new answer set is used.
	current = current.Add(2 * time.Second)
	addresses = engine.nameEntryAddresses("staged.test")
	require.Len(t, addresses, 1)
	assert.Equal(t, "10.4.4.5", addresses[0].String(),
		"an expired entry must be re-resolved and the new answer set used")
	assert.Equal(t, 2, dns.aCount("staged.test."))

	// The public decision path refreshes an expired entry too, off-lock.
	current = current.Add(engine.nameTTLValue() + time.Second)
	require.True(t, engine.IsHostAllowedPort("203.0.113.50", 8888))
	assert.Equal(t, 3, dns.aCount("staged.test."),
		"the public decision path must refresh an expired NAME entry")
	cached, ok := engine.nameEntryAddressesCached("staged.test")
	require.True(t, ok)
	require.Len(t, cached, 1)
	assert.Equal(t, "10.4.4.4", cached[0].String(),
		"the refreshed answer set must be the one the decision path cached")
}

// TestNameEntryAddresses_UnresolvableNameCachedEmptyAndWarnedOnce covers
// acceptance criterion 4: an unresolvable NAME entry is cached as an empty
// address set, never matches, and is reported exactly once.
func TestNameEntryAddresses_UnresolvableNameCachedEmptyAndWarnedOnce(t *testing.T) {
	dns := newPolicySyntheticDNS(t, map[string][][]string{
		"resolvable.test.": {{"10.1.1.1"}},
	})
	dns.install()
	defer dns.uninstall()

	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"missing.test:8888"},
	})

	var (
		warnMu sync.Mutex
		warns  []string
	)
	engine.nameWarn = func(name string) {
		warnMu.Lock()
		defer warnMu.Unlock()
		warns = append(warns, name)
	}

	current := time.Now()
	engine.now = func() time.Time { return current }

	for i := 0; i < 3; i++ {
		assert.Empty(t, engine.nameEntryAddresses("missing.test"),
			"an unresolvable name must resolve to an empty address set")
	}

	// The empty set is cached: the decision path neither re-resolves it nor
	// panics on it (a public-literal destination keeps the decision itself out
	// of the DNS path, so every counted query is the resolver's).
	before := dns.aCount("missing.test.")
	require.True(t, engine.IsHostAllowedPort("203.0.113.51", 8888))
	assert.Equal(t, before, dns.aCount("missing.test."),
		"a cached empty answer must not be re-resolved inside the TTL")

	warnMu.Lock()
	require.Len(t, warns, 1, "an unresolvable NAME entry must be logged exactly once")
	assert.Equal(t, "missing.test", warns[0])
	warnMu.Unlock()

	// Past the TTL a re-resolution happens, the set stays empty and the warning
	// is still emitted only once.
	current = current.Add(engine.nameTTLValue() + time.Second)
	assert.Empty(t, engine.nameEntryAddresses("missing.test"))
	assert.Greater(t, dns.aCount("missing.test."), before,
		"an expired entry must be re-resolved even when it resolves to nothing")

	warnMu.Lock()
	assert.Len(t, warns, 1, "a name that stays unresolvable must not warn again")
	warnMu.Unlock()
}

// TestNameEntryAddresses_AddHostsMappingNeedsNoDNS covers acceptance criterion 5:
// a static network.add_hosts mapping is authoritative and resolves a NAME entry
// with zero DNS queries.
func TestNameEntryAddresses_AddHostsMappingNeedsNoDNS(t *testing.T) {
	dns := newPolicySyntheticDNS(t, map[string][][]string{
		// A deliberately different answer: any DNS use would be observable both
		// in the address set and in aCount.
		"fixture-name.test.": {{"10.1.1.1"}},
	})
	dns.install()
	defer dns.uninstall()

	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"fixture-name.test:8888"},
		AddHosts:        []api.HostIPMapping{{Host: "fixture-name.test", IP: "10.9.9.9"}},
	})

	addresses := engine.nameEntryAddresses("fixture-name.test")
	require.Len(t, addresses, 1)
	assert.Equal(t, "10.9.9.9", addresses[0].String(),
		"the add_hosts mapping must be authoritative for policy evaluation")
	assert.Equal(t, 0, dns.aCount("fixture-name.test."),
		"an add_hosts mapping must resolve the name with zero DNS queries")

	// The mapping is normalized (case and a trailing root dot) exactly like the
	// entry it is matched against.
	normalized := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"fixture-name.test:8888"},
		AddHosts:        []api.HostIPMapping{{Host: " FIXTURE-NAME.TEST. ", IP: "10.9.9.10"}},
	})
	addresses = normalized.nameEntryAddresses("fixture-name.test")
	require.Len(t, addresses, 1)
	assert.Equal(t, "10.9.9.10", addresses[0].String())
	assert.Equal(t, 0, dns.aCount("fixture-name.test."))
}

// TestNameEntryAddresses_PortScopedEntryStillResolves documents that the
// resolver is name-scoped: the resolved address set is available for a
// port-scoped entry, and the port scope is applied when the decision matches a
// destination (see allowPrivateAddress).
func TestNameEntryAddresses_PortScopedEntryStillResolves(t *testing.T) {
	dns := newPolicySyntheticDNS(t, map[string][][]string{
		"scoped.test.": {{"10.3.3.3"}},
	})
	dns.install()
	defer dns.uninstall()

	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"scoped.test:9443"},
	})

	addresses := engine.nameEntryAddresses("scoped.test")
	require.Len(t, addresses, 1)
	assert.Equal(t, "10.3.3.3", addresses[0].String())

	cached, ok := engine.nameEntryAddressesCached("scoped.test")
	require.True(t, ok)
	require.Len(t, cached, 1)
	assert.Equal(t, "10.3.3.3", cached[0].String())
}

// TestNameEntryAddresses_NoDNSWhenPrivateBlockOff qualifies that the resolver is
// only consulted when the private block can actually use it.
func TestNameEntryAddresses_NoDNSWhenPrivateBlockOff(t *testing.T) {
	dns := newPolicySyntheticDNS(t, map[string][][]string{
		"priv-name.test.": {{"10.1.2.3"}},
	})
	dns.install()
	defer dns.uninstall()

	engine := NewEngine(&api.NetworkConfig{
		AllowPrivate: []string{"priv-name.test:8888"},
	})

	require.True(t, engine.IsHostAllowedPort("203.0.113.52", 8888))
	assert.Equal(t, 0, dns.aCount("priv-name.test."),
		"no DNS work is needed while BlockPrivateIPs is off")

	// No exception list at all: the resolver must not touch the lock or DNS.
	plain := NewEngine(&api.NetworkConfig{BlockPrivateIPs: true})
	require.True(t, plain.IsHostAllowedPort("203.0.113.53", 8888))
	assert.Equal(t, 0, dns.aCount("priv-name.test."))
}

// TestNameEntryAddresses_NoRecursiveLock is the regression test for the deadlock
// trap: the resolver is reachable from the private-block decision, which already
// holds e.mu for reading, so it must never take e.mu itself. A concurrent writer
// (AddAllowedHosts) would otherwise deadlock the decision path.
func TestNameEntryAddresses_NoRecursiveLock(t *testing.T) {
	dns := newPolicySyntheticDNS(t, map[string][][]string{
		"locked.test.": {{"10.2.2.2"}},
	})
	dns.install()
	defer dns.uninstall()

	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"locked.test:8888"},
	})

	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			engine.AddAllowedHosts(fmt.Sprintf("writer-%d.test", i))
		}
	}()

	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 100; j++ {
				engine.IsHostAllowedPort("10.2.2.2", 8888)
				engine.nameEntryAddresses("locked.test")
			}
		}()
	}

	readersDone := make(chan struct{})
	go func() {
		readers.Wait()
		close(readersDone)
	}()

	select {
	case <-readersDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the NAME entry resolver deadlocked against an e.mu writer")
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the e.mu writer never completed")
	}
}
