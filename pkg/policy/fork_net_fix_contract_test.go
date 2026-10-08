package policy

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// forkNetFixContractDefaultPath is where the FORK-NET-FIX-0923 gate-and-contract
// story publishes the machine-readable contract for the allow_private NAME-entry
// fix and the darwin skip work. It lives outside the repository by design, so a
// checkout that has not run the VM gate (or CI) skips rather than fails. The path
// can be overridden with MATCHLOCK_FORK_NET_FIX_CONTRACT.
const forkNetFixContractDefaultPath = "/home/kaladin/matchlock-work/fork-net-fix-0923-contract.json"

// forkNetFixContract mirrors the published artifact's shape. Only the parts this
// test asserts on are typed; the matrix rows stay raw so the consumer does not
// have to track every column the gate evidence carries.
type forkNetFixContract struct {
	Task             string `json:"task"`
	ResolutionDesign struct {
		TTLSeconds          int             `json:"ttl_seconds"`
		LookupTimeoutSecond int             `json:"lookup_timeout_seconds"`
		NameEntrySemantics  string          `json:"name_entry_semantics"`
		AddHostsMapping     string          `json:"add_hosts_mapping"`
		RebindingGuard      json.RawMessage `json:"rebinding_guard"`
		PortScope           struct {
			Rule                       string `json:"rule"`
			BareEntryMatchesAnyPort    bool   `json:"bare_entry_matches_any_port"`
			PortEntryRestrictedToPort  bool   `json:"port_entry_restricted_to_port"`
			MismatchedPortEntryRefuses bool   `json:"mismatched_port_entry_refuses"`
		} `json:"port_scope"`
	} `json:"resolution_design"`
	Matrix []struct {
		Defect  string `json:"defect"`
		Outcome string `json:"outcome"`
	} `json:"matrix"`
	Gates struct {
		Unit struct {
			Command string `json:"command"`
			Result  string `json:"result"`
		} `json:"unit"`
		Lint struct {
			Command string `json:"command"`
			Result  string `json:"result"`
		} `json:"lint"`
		Errx struct {
			Command string `json:"command"`
			Result  string `json:"result"`
		} `json:"errx"`
		Darwin struct {
			Command string `json:"command"`
			Result  string `json:"result"`
		} `json:"darwin"`
		Acceptance struct {
			Command string `json:"command"`
			Result  string `json:"result"`
		} `json:"acceptance"`
	} `json:"gates"`
	Limitations []string `json:"limitations"`
}

// TestForkNetFixContractArtifact validates the published FORK-NET-FIX-0923
// contract artifact: it must be valid JSON carrying the required keys, and its
// documented resolution design (TTL, lookup timeout and port scope) must not
// drift from the engine constants that implement it. The contract is the
// deliverable of the gate-and-contract story, so this test is the in-repo
// consumer of it — the file is absent in a tree that never ran the VM gate, in
// which case the test skips.
func TestForkNetFixContractArtifact(t *testing.T) {
	path := os.Getenv("MATCHLOCK_FORK_NET_FIX_CONTRACT")
	if path == "" {
		path = forkNetFixContractDefaultPath
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("fork-net-fix contract %s not published in this tree: %v", path, err)
	}

	var contract forkNetFixContract
	require.NoError(t, json.Unmarshal(data, &contract), "contract must be valid JSON")

	require.NotEmpty(t, contract.Task, "contract.task must be set")

	design := contract.ResolutionDesign
	require.NotEmpty(t, design.NameEntrySemantics, "contract.resolution_design.name_entry_semantics must be set")
	require.NotEmpty(t, design.AddHostsMapping, "contract.resolution_design.add_hosts_mapping must be set")
	require.NotEmpty(t, design.RebindingGuard, "contract.resolution_design.rebinding_guard must be set")
	require.NotEmpty(t, design.PortScope.Rule, "contract.resolution_design.port_scope.rule must be set")

	// The published TTL and lookup bound must not drift from the engine.
	assert.Equal(t, int(allowPrivateNameTTL.Seconds()), design.TTLSeconds,
		"contract.resolution_design.ttl_seconds must match the engine's allowPrivateNameTTL")
	assert.Equal(t, int(allowPrivateNameLookupTimeout.Seconds()), design.LookupTimeoutSecond,
		"contract.resolution_design.lookup_timeout_seconds must match the engine's allowPrivateNameLookupTimeout")

	// Port scope is part of the operator contract: a bare entry covers every
	// port, a port-suffixed entry covers exactly its port.
	assert.True(t, design.PortScope.BareEntryMatchesAnyPort,
		"contract.resolution_design.port_scope.bare_entry_matches_any_port must be true")
	assert.True(t, design.PortScope.PortEntryRestrictedToPort,
		"contract.resolution_design.port_scope.port_entry_restricted_to_port must be true")
	assert.True(t, design.PortScope.MismatchedPortEntryRefuses,
		"contract.resolution_design.port_scope.mismatched_port_entry_refuses must be true")

	require.NotEmpty(t, contract.Matrix, "contract.matrix must be set")
	defects := make(map[string]bool, 2)
	for _, row := range contract.Matrix {
		require.NotEmpty(t, row.Outcome, "every contract.matrix row must carry an outcome")
		defects[row.Defect] = true
	}
	assert.True(t, defects["defect_1_name_entry"],
		"contract.matrix must cover the allow_private NAME-entry defect")
	assert.True(t, defects["defect_2_darwin_skip"],
		"contract.matrix must cover the darwin IPv6 skip defect")

	for name, gate := range map[string]struct {
		Command string `json:"command"`
		Result  string `json:"result"`
	}{
		"unit":       {contract.Gates.Unit.Command, contract.Gates.Unit.Result},
		"lint":       {contract.Gates.Lint.Command, contract.Gates.Lint.Result},
		"errx":       {contract.Gates.Errx.Command, contract.Gates.Errx.Result},
		"darwin":     {contract.Gates.Darwin.Command, contract.Gates.Darwin.Result},
		"acceptance": {contract.Gates.Acceptance.Command, contract.Gates.Acceptance.Result},
	} {
		assert.NotEmpty(t, gate.Command, "contract.gates.%s.command must be set", name)
		assert.NotEmpty(t, gate.Result, "contract.gates.%s.result must be set", name)
	}

	require.NotEmpty(t, contract.Limitations, "contract.limitations must be set")
}
