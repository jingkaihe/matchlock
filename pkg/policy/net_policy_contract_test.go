package policy

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// netPolicyContractDefaultPath is where the MATCHLOCK-NET-POLICY gate-and-contract
// story publishes the machine-readable contract. It lives outside the repository
// by design, so a checkout that has not run the gate (or CI) skips rather than
// fails. The path can be overridden with MATCHLOCK_NET_POLICY_CONTRACT.
const netPolicyContractDefaultPath = "/home/kaladin/matchlock-work/matchlock-net-policy-contract.json"

// TestNetPolicyContractArtifact validates the published net-policy contract
// artifact: it must be valid JSON carrying the required keys, and its
// private_ranges list must match the engine's compiled default range list. The
// contract is the deliverable of the gate-and-contract story, so this test is
// the in-repo consumer of it.
func TestNetPolicyContractArtifact(t *testing.T) {
	path := os.Getenv("MATCHLOCK_NET_POLICY_CONTRACT")
	if path == "" {
		path = netPolicyContractDefaultPath
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("net-policy contract %s not published in this tree: %v", path, err)
	}

	var contract struct {
		Task               string          `json:"task"`
		PrivateRanges      []string        `json:"private_ranges"`
		ExemptionSemantics json.RawMessage `json:"exemption_semantics"`
		Matrix             json.RawMessage `json:"matrix"`
		Gates              json.RawMessage `json:"gates"`
	}
	require.NoError(t, json.Unmarshal(data, &contract), "contract must be valid JSON")

	require.NotEmpty(t, contract.Task, "contract.task must be set")
	require.NotEmpty(t, contract.PrivateRanges, "contract.private_ranges must be set")
	require.NotEmpty(t, contract.ExemptionSemantics, "contract.exemption_semantics must be set")
	require.NotEmpty(t, contract.Matrix, "contract.matrix must be set")
	require.NotEmpty(t, contract.Gates, "contract.gates must be set")

	// The published list must not drift from the engine.
	assert.ElementsMatch(t, privateRanges, contract.PrivateRanges,
		"contract.private_ranges must match the engine's compiled privateRanges")
}
