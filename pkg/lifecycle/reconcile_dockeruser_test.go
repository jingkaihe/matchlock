//go:build linux

package lifecycle

import (
	"errors"
	"testing"

	"github.com/google/nftables"
	"github.com/jingkaihe/matchlock/pkg/net"
	"github.com/stretchr/testify/require"
)

func acceptRule(marker string) *nftables.Rule {
	return &nftables.Rule{UserData: []byte(marker)}
}

func scopeOf(taps ...string) func(string) bool {
	set := make(map[string]bool, len(taps))
	for _, t := range taps {
		set[t] = true
	}
	return func(tap string) bool { return set[tap] }
}

func aliveOf(live ...string) func(string) bool {
	set := make(map[string]bool, len(live))
	for _, t := range live {
		set[t] = true
	}
	return func(tap string) bool { return set[tap] }
}

func TestSelectDeadAcceptRules_DeadTapInScopeSelected(t *testing.T) {
	rules := []*nftables.Rule{
		acceptRule(net.TapAcceptMarker("vm-dead1")), // dead + in scope -> delete
		acceptRule(net.TapAcceptMarker("vm-live")),  // alive + in scope -> keep
	}
	out := selectDeadAcceptRules(aliveOf("vm-live"), scopeOf("vm-dead1", "vm-live"), rules)
	require.Len(t, out, 1)
	require.Equal(t, net.TapAcceptMarker("vm-dead1"), string(out[0].UserData))
}

func TestSelectDeadAcceptRules_OutOfScopeNeverDeleted(t *testing.T) {
	// Reconciling VM A ("vm-a") must never delete VM B's rule, even if VM B's
	// tap is dead. Only rules in scope (this VM's candidates) are candidates.
	rules := []*nftables.Rule{
		acceptRule(net.TapAcceptMarker("vm-b-dead")), // another VM's dead tap
		acceptRule(net.TapAcceptMarker("vm-a-dead")), // this VM's dead tap -> delete
	}
	out := selectDeadAcceptRules(aliveOf(), scopeOf("vm-a-dead", "vm-a-live"), rules)
	require.Len(t, out, 1)
	require.Equal(t, net.TapAcceptMarker("vm-a-dead"), string(out[0].UserData))
}

func TestSelectDeadAcceptRules_LiveTapNeverDeleted(t *testing.T) {
	// A tap present in the live-interface snapshot is a still-running sandbox's
	// rule and must never be deleted, regardless of scope.
	rules := []*nftables.Rule{
		acceptRule(net.TapAcceptMarker("vm-running")), // alive + in scope -> keep
		acceptRule(net.TapAcceptMarker("vm-dead")),    // dead + in scope -> delete
	}
	out := selectDeadAcceptRules(aliveOf("vm-running"), scopeOf("vm-running", "vm-dead"), rules)
	require.Len(t, out, 1)
	require.Equal(t, net.TapAcceptMarker("vm-dead"), string(out[0].UserData))
}

func TestSelectDeadAcceptRules_NonMatchlockIgnored(t *testing.T) {
	rules := []*nftables.Rule{
		{UserData: []byte("docker-return")},        // docker's own rule -> keep
		{UserData: []byte("some-other-rule")},       // unrelated -> keep
		acceptRule(net.TapAcceptMarker("dead")),     // dead + in scope -> delete
	}
	out := selectDeadAcceptRules(aliveOf(), scopeOf("dead"), rules)
	require.Len(t, out, 1)
	require.Equal(t, net.TapAcceptMarker("dead"), string(out[0].UserData))
}

func TestSelectDeadAcceptRules_EmptyAndMisnamedIgnored(t *testing.T) {
	rules := []*nftables.Rule{
		{UserData: []byte("")},                         // empty marker -> keep
		acceptRule("matchlock-tap-accept:"),            // empty tap -> keep
		acceptRule("matchlock-tap-accept:vm-gone"),     // dead + in scope -> delete
	}
	out := selectDeadAcceptRules(aliveOf(), scopeOf("vm-gone"), rules)
	require.Len(t, out, 1)
	require.Equal(t, "matchlock-tap-accept:vm-gone", string(out[0].UserData))
}

// fakeDockerUserSource is a dockerUserSource that returns a fixed rule set and
// records which rules were deleted and flushed, for orchestration tests.
type fakeDockerUserSource struct {
	rules   []*nftables.Rule
	deleted []string
	flushed bool
}

func (f *fakeDockerUserSource) list() ([]*nftables.Rule, bool, error) { return f.rules, false, nil }
func (f *fakeDockerUserSource) delete(r *nftables.Rule) error {
	f.deleted = append(f.deleted, string(r.UserData))
	return nil
}
func (f *fakeDockerUserSource) flush() error {
	f.flushed = true
	return nil
}

func TestReconcileDockerUserForwardRules_DeletesDeadTap(t *testing.T) {
	src := &fakeDockerUserSource{rules: []*nftables.Rule{
		acceptRule(net.TapAcceptMarker("vm-dead1")), // dead + in scope -> delete
	}}
	prevLive := defaultLiveInterfaceSet
	prevSrc := dockerUserSource
	defaultLiveInterfaceSet = func() (map[string]bool, error) { return nil, nil }
	dockerUserSource = src
	t.Cleanup(func() {
		defaultLiveInterfaceSet = prevLive
		dockerUserSource = prevSrc
	})

	r := &Reconciler{}
	report := &ReconcileReport{}
	err := r.reconcileDockerUserForwardRules([]string{"vm-dead1"}, report)
	require.NoError(t, err)
	require.Equal(t, []string{net.TapAcceptMarker("vm-dead1")}, src.deleted)
	require.True(t, src.flushed)
	require.Contains(t, report.Cleaned, "forward_rule_delete:"+net.TapAcceptMarker("vm-dead1"))
}

func TestReconcileDockerUserForwardRules_LiveTapNotDeleted(t *testing.T) {
	src := &fakeDockerUserSource{rules: []*nftables.Rule{
		acceptRule(net.TapAcceptMarker("vm-live")), // alive -> keep
	}}
	prevLive := defaultLiveInterfaceSet
	prevSrc := dockerUserSource
	defaultLiveInterfaceSet = func() (map[string]bool, error) { return map[string]bool{"vm-live": true}, nil }
	dockerUserSource = src
	t.Cleanup(func() {
		defaultLiveInterfaceSet = prevLive
		dockerUserSource = prevSrc
	})

	r := &Reconciler{}
	report := &ReconcileReport{}
	err := r.reconcileDockerUserForwardRules([]string{"vm-live"}, report)
	require.NoError(t, err)
	require.Len(t, src.deleted, 0)
	require.False(t, src.flushed)
	require.Empty(t, report.Cleaned)
}

// errorDockerUserSource is a dockerUserSource whose list()/delete()/flush() fail
// with a fixed error, to exercise the reconcile error contract.
type errorDockerUserSource struct {
	listErr   error
	flushErr  error
	ruleCount int
}

func (e *errorDockerUserSource) list() ([]*nftables.Rule, bool, error) {
	if e.listErr != nil {
		return nil, false, e.listErr
	}
	rules := make([]*nftables.Rule, e.ruleCount)
	for i := range rules {
		rules[i] = acceptRule(net.TapAcceptMarker("vm-gone"))
	}
	return rules, false, nil
}
func (e *errorDockerUserSource) delete(*nftables.Rule) error { return nil }
func (e *errorDockerUserSource) flush() error {
	if e.flushErr != nil {
		return e.flushErr
	}
	return nil
}

func TestReconcileDockerUserForwardRules_InspectionErrorReported(t *testing.T) {
	src := &errorDockerUserSource{listErr: errors.New("netlink down")}
	prevLive := defaultLiveInterfaceSet
	prevSrc := dockerUserSource
	defaultLiveInterfaceSet = func() (map[string]bool, error) { return nil, nil }
	dockerUserSource = src
	t.Cleanup(func() {
		defaultLiveInterfaceSet = prevLive
		dockerUserSource = prevSrc
	})

	r := &Reconciler{}
	report := &ReconcileReport{}
	err := r.reconcileDockerUserForwardRules([]string{"vm-gone"}, report)
	require.Error(t, err)
	require.Contains(t, report.Failed, "forward_rule_inspect")
	require.Empty(t, report.Cleaned)
}

func TestReconcileDockerUserForwardRules_FlushErrorReported(t *testing.T) {
	src := &errorDockerUserSource{flushErr: errors.New("flush failed"), ruleCount: 1}
	prevLive := defaultLiveInterfaceSet
	prevSrc := dockerUserSource
	defaultLiveInterfaceSet = func() (map[string]bool, error) { return nil, nil }
	dockerUserSource = src
	t.Cleanup(func() {
		defaultLiveInterfaceSet = prevLive
		dockerUserSource = prevSrc
	})

	r := &Reconciler{}
	report := &ReconcileReport{}
	err := r.reconcileDockerUserForwardRules([]string{"vm-gone"}, report)
	require.Error(t, err)
	require.Contains(t, report.Failed, "forward_rule_flush")
	// A failed flush means the deletions were not committed, so nothing is
	// reported as cleaned.
	require.Empty(t, report.Cleaned)
}

func TestReconcileDockerUserForwardRules_AbsentChainIsCleanNoop(t *testing.T) {
	// A host with no DOCKER-USER chain (non-Docker host) must be a clean no-op,
	// not a spurious failure. Previously ListChain-on-fixed-name returned an
	// error for a missing chain, so this surfaced as a failed cleanup.
	src := &absentDockerUserSource{}
	prevLive := defaultLiveInterfaceSet
	prevSrc := dockerUserSource
	defaultLiveInterfaceSet = func() (map[string]bool, error) { return nil, nil }
	dockerUserSource = src
	t.Cleanup(func() {
		defaultLiveInterfaceSet = prevLive
		dockerUserSource = prevSrc
	})

	r := &Reconciler{}
	report := &ReconcileReport{}
	err := r.reconcileDockerUserForwardRules([]string{"vm-gone"}, report)
	require.NoError(t, err)
	require.Empty(t, report.Cleaned)
	require.Empty(t, report.Failed)
}

// absentDockerUserSource reports the DOCKER-USER chain as positively absent.
type absentDockerUserSource struct{}

func (absentDockerUserSource) list() ([]*nftables.Rule, bool, error) { return nil, true, nil }
func (absentDockerUserSource) delete(*nftables.Rule) error           { return nil }
func (absentDockerUserSource) flush() error                          { return nil }
