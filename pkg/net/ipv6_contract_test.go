//go:build linux

package net

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/pkg/state"
)

// ipv6ContractDefaultPath is where the MATCHLOCK-IPV6 deliverable story publishes
// the machine-readable IPv6 contract. It lives outside the repository by design,
// so a checkout that has not run the gate (or CI) skips rather than fails. The
// path can be overridden with MATCHLOCK_IPV6_CONTRACT.
//
// This file is linux-only because it is the consumer of the Linux interception
// path: the ip6 rule model it checks against (buildIPv6Plan) and the nftables
// table naming it pins both live in linux-tagged files of this package. The
// contract itself documents that macOS keeps its existing IPv4-only behaviour.
const ipv6ContractDefaultPath = "/home/kaladin/matchlock-work/matchlock-ipv6-contract.json"

// ipv6Contract is the subset of the published contract this consumer asserts on:
// the required top-level keys plus the addressing and rule-model sections, which
// are checked against the code that produces them.
type ipv6Contract struct {
	Task            string          `json:"task"`
	Branch          string          `json:"branch"`
	IntegrationBase json.RawMessage `json:"integration_base"`
	Addressing      struct {
		PrefixLen        int    `json:"prefix_len"`
		Subnet6Template  string `json:"subnet6_template"`
		Gateway6Template string `json:"gateway_ipv6_template"`
		Guest6Template   string `json:"guest_ipv6_template"`
		OctetRange       struct {
			Min int `json:"min"`
			Max int `json:"max"`
		} `json:"octet_range"`
		Example struct {
			Octet       int    `json:"octet"`
			Subnet6     string `json:"subnet6"`
			GatewayIPv6 string `json:"gateway_ipv6"`
			GuestIPv6   string `json:"guest_ipv6"`
		} `json:"example"`
	} `json:"addressing"`
	IP6RuleModel struct {
		TableNameTemplate string `json:"table_name_template"`
		Sample            struct {
			Tap     string `json:"tap"`
			TableV4 string `json:"table_v4"`
			TableV6 string `json:"table_v6"`
		} `json:"sample"`
		Rules []struct {
			Chain  string `json:"chain"`
			Kind   string `json:"kind"`
			Match  string `json:"match"`
			Action string `json:"action"`
		} `json:"rules"`
		NoGatewayShape []struct {
			Chain string `json:"chain"`
			Kind  string `json:"kind"`
		} `json:"no_gateway_shape"`
		FailClosed json.RawMessage `json:"fail_closed"`
	} `json:"ip6_rule_model"`
	ProxyChanges json.RawMessage `json:"proxy_changes"`
	DNS          json.RawMessage `json:"dns"`
	GuestConfig  json.RawMessage `json:"guest_config"`
	TestMatrix   json.RawMessage `json:"test_matrix"`
	Gates        json.RawMessage `json:"gates"`
}

// TestIPv6ContractArtifact validates the published IPv6 contract artifact: it
// must be valid JSON carrying the required keys, and its addressing and rule
// model must agree with the code that implements them — the per-VM ULA lease the
// state allocator hands out and the ordered ip6 rule plan the firewall installs.
// The contract is the deliverable of the final story, so this test is the
// in-repo consumer of it: a contract that drifts from the code fails here.
func TestIPv6ContractArtifact(t *testing.T) {
	path := os.Getenv("MATCHLOCK_IPV6_CONTRACT")
	if path == "" {
		path = ipv6ContractDefaultPath
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("IPv6 contract %s not published in this tree: %v", path, err)
	}

	var contract ipv6Contract
	require.NoError(t, json.Unmarshal(data, &contract), "contract must be valid JSON")

	require.NotEmpty(t, contract.Task, "contract.task must be set")
	require.NotEmpty(t, contract.Branch, "contract.branch must be set")
	require.NotEmpty(t, contract.IntegrationBase, "contract.integration_base must be set")
	require.NotEmpty(t, contract.IP6RuleModel.FailClosed, "contract.ip6_rule_model.fail_closed must be set")
	require.NotEmpty(t, contract.ProxyChanges, "contract.proxy_changes must be set")
	require.NotEmpty(t, contract.DNS, "contract.dns must be set")
	require.NotEmpty(t, contract.GuestConfig, "contract.guest_config must be set")
	require.NotEmpty(t, contract.TestMatrix, "contract.test_matrix must be set")
	require.NotEmpty(t, contract.Gates, "contract.gates must be set")

	checkIPv6ContractAddressing(t, contract)
	checkIPv6ContractRuleModel(t, contract)
}

// checkIPv6ContractAddressing pins the documented ULA scheme to the code: the
// example the contract publishes must be exactly what the subnet allocator
// derives for that octet, and the first lease of a fresh allocator must be that
// same octet.
func checkIPv6ContractAddressing(t *testing.T, contract ipv6Contract) {
	t.Helper()

	addr := contract.Addressing
	require.Equal(t, 64, addr.PrefixLen, "contract.addressing.prefix_len must be the per-VM /64")
	require.Positive(t, addr.Example.Octet, "contract.addressing.example.octet must be set")
	require.LessOrEqual(t, addr.OctetRange.Min, addr.Example.Octet,
		"the example octet must be inside the documented lease range")
	require.GreaterOrEqual(t, addr.OctetRange.Max, addr.Example.Octet,
		"the example octet must be inside the documented lease range")

	// The published templates must be the ones the code implements.
	derived := map[string]string{
		"subnet6":      fmt.Sprintf(addr.Subnet6Template, addr.Example.Octet),
		"gateway_ipv6": fmt.Sprintf(addr.Gateway6Template, addr.Example.Octet),
		"guest_ipv6":   fmt.Sprintf(addr.Guest6Template, addr.Example.Octet),
	}
	for name, tmpl := range map[string]string{
		"subnet6_template":      addr.Subnet6Template,
		"gateway_ipv6_template": addr.Gateway6Template,
		"guest_ipv6_template":   addr.Guest6Template,
	} {
		require.Contains(t, tmpl, "%d", "contract.addressing.%s must carry the octet placeholder", name)
	}
	assert.Equal(t, addr.Example.Subnet6, derived["subnet6"], "example.subnet6 must match the subnet6 template")
	assert.Equal(t, addr.Example.GatewayIPv6, derived["gateway_ipv6"],
		"example.gateway_ipv6 must match the gateway template")
	assert.Equal(t, addr.Example.GuestIPv6, derived["guest_ipv6"], "example.guest_ipv6 must match the guest template")

	// The contract must agree with the real allocator, which is the single source
	// of truth for per-VM addressing.
	allocator := state.NewSubnetAllocatorWithDir(filepath.Join(t.TempDir(), "subnets"))
	info, err := allocator.Allocate("vm-ipv6contract")
	require.NoError(t, err, "the state allocator must hand out a lease")

	assert.Equal(t, addr.Example.Octet, info.Octet,
		"the first lease of a fresh allocator must be the octet the contract uses as its example")
	assert.Equal(t, addr.Example.Subnet6, info.Subnet6,
		"the contract's example /64 must be the /64 the allocator derives for that octet")
	assert.Equal(t, addr.Example.GatewayIPv6, info.GatewayIPv6,
		"the contract's example gateway must be the gateway the allocator derives for that octet")
	assert.Equal(t, addr.Example.GuestIPv6, info.GuestIPv6,
		"the contract's example guest address must be the one the allocator derives for that octet")
	assert.Equal(t, fmt.Sprintf("fd00:%d::/64", info.Octet), info.Subnet6,
		"the allocator must derive the /64 from the lease octet")
}

// checkIPv6ContractRuleModel pins the documented ip6 rules to the plan the
// firewall actually installs: same table naming, same ordered (chain, kind)
// sequence for a fully configured rules object, and the same drops-only shape
// when no gateway is configured (the fail-closed case).
func checkIPv6ContractRuleModel(t *testing.T, contract ipv6Contract) {
	t.Helper()

	model := contract.IP6RuleModel
	sampleTap := model.Sample.Tap
	require.NotEmpty(t, sampleTap, "contract.ip6_rule_model.sample.tap must be set")
	assert.Equal(t, FirewallTableV6Name(sampleTap), model.Sample.TableV6,
		"contract sample table_v6 must be the name the firewall installs")
	assert.Equal(t, FirewallTableName(sampleTap), model.Sample.TableV4,
		"contract sample table_v4 must be the name the firewall installs")
	assert.Equal(t, "matchlock6_<tap>", model.TableNameTemplate,
		"contract table_name_template must document the ip6 table naming")

	// Fully configured interception: the plan the rules object produces for a
	// gateway plus every listener port.
	rules := NewNFTablesRules(sampleTap, "192.168.100.1", 15001, 15002, 15003, []string{"8.8.8.8"})
	rules.SetDNSForwarderPort(15004)
	rules.SetGatewayIPv6("fd00:100::1")

	plan := compactPlan(rules.buildIPv6Plan())
	require.NotEmpty(t, plan, "a configured rules object must install rules")
	require.NotEmpty(t, model.Rules, "contract.ip6_rule_model.rules must list the installed rules")

	documented := make([]string, 0, len(model.Rules))
	for _, rule := range model.Rules {
		require.NotEmpty(t, rule.Chain, "every documented rule needs a chain")
		require.NotEmpty(t, rule.Kind, "every documented rule needs a kind")
		require.NotEmpty(t, rule.Match, "every documented rule needs a match")
		require.NotEmpty(t, rule.Action, "every documented rule needs an action")
		documented = append(documented, rule.Chain+"|"+rule.Kind)
	}
	assert.Equal(t, documented, plan,
		"the documented ip6 rules must match the ordered plan the firewall installs")

	// The contract's fail-closed claim: every redirect precedes every residual
	// drop, so traffic the redirects did not cover hits a drop.
	lastRedirect := -1
	firstDrop := -1
	for i, entry := range plan {
		kind := ipv6RuleKind(strings.SplitN(entry, "|", 2)[1])
		if kind.redirects() {
			lastRedirect = i
		}
		if kind.drops() && firstDrop < 0 {
			firstDrop = i
		}
	}
	require.NotEqual(t, -1, lastRedirect, "the plan must carry redirects when a gateway is configured")
	require.NotEqual(t, -1, firstDrop, "the plan must always carry the residual drops")
	assert.Less(t, lastRedirect, firstDrop,
		"every redirect must precede the residual drops, so a non-redirected packet is dropped")

	// No gateway: the rules keep the previous all-dropped shape.
	bare := NewNFTablesRules(sampleTap, "192.168.100.1", 15001, 15002, 15003, []string{"8.8.8.8"})
	bare.SetDNSForwarderPort(15004)
	barePlan := compactPlan(bare.buildIPv6Plan())

	bareDocumented := make([]string, 0, len(model.NoGatewayShape))
	for _, rule := range model.NoGatewayShape {
		bareDocumented = append(bareDocumented, rule.Chain+"|"+rule.Kind)
	}
	require.NotEmpty(t, bareDocumented, "contract.ip6_rule_model.no_gateway_shape must be documented")
	assert.Equal(t, bareDocumented, barePlan,
		"without a gateway the ip6 table must be exactly the documented drops-only shape")
	for _, entry := range barePlan {
		kind := ipv6RuleKind(strings.SplitN(entry, "|", 2)[1])
		assert.False(t, kind.redirects(), "a gateway-less ip6 table must not redirect anything")
		assert.True(t, kind.drops(), "a gateway-less ip6 table must only drop")
	}
}

// compactPlan renders a rule plan as "chain|kind" entries in plan order.
func compactPlan(plan []plannedIPv6Rule) []string {
	out := make([]string, 0, len(plan))
	for _, rule := range plan {
		out = append(out, rule.chain+"|"+string(rule.kind))
	}
	return out
}
