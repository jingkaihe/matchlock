//go:build linux

package lifecycle

import (
	"errors"
	"net"
	"strings"
	"syscall"

	"github.com/google/nftables"
	"github.com/jingkaihe/matchlock/internal/errx"
	sandboxnet "github.com/jingkaihe/matchlock/pkg/net"
	linuxvm "github.com/jingkaihe/matchlock/pkg/vm/linux"
)

func (r *Reconciler) reconcilePlatform(rec *Record, store *Store, report *ReconcileReport) error {
	var errs []error
	var tapErrs []error

	for _, tap := range tapNameCandidates(rec.VMID, rec.Resources.TAPName) {
		if _, err := net.InterfaceByName(tap); err != nil {
			continue
		}
		if err := linuxvm.DeleteInterface(tap); err != nil {
			report.addFailed("tap_delete:"+tap, err)
			wrapped := errx.Wrap(ErrReconcileTap, err)
			errs = append(errs, wrapped)
			tapErrs = append(tapErrs, wrapped)
			continue
		}
		report.addCleaned("tap_delete:" + tap)
	}
	_ = store.MarkCleanup("tap_delete", errors.Join(tapErrs...))

	tableCandidates := make([]string, 0, 4)
	addTable := func(name string) {
		if name == "" {
			return
		}
		for _, existing := range tableCandidates {
			if existing == name {
				return
			}
		}
		tableCandidates = append(tableCandidates, name)
	}
	addTable(rec.Resources.FirewallTable)
	addTable(rec.Resources.NATTable)
	for _, tap := range tapNameCandidates(rec.VMID, rec.Resources.TAPName) {
		addTable("matchlock_" + tap)
		addTable("matchlock_nat_" + tap)
	}

	var tableErrs []error
	for _, table := range tableCandidates {
		if err := deleteNFTTable(table); err != nil {
			report.addFailed("nft_table_delete:"+table, err)
			wrapped := errx.With(ErrReconcileTable, " %s: %w", table, err)
			errs = append(errs, wrapped)
			tableErrs = append(tableErrs, wrapped)
			continue
		}
		report.addCleaned("nft_table_delete:" + table)
	}
	_ = store.MarkCleanup("nftables_cleanup", errors.Join(tableErrs...))

	// Clean DOCKER-USER accept rules for taps that no longer exist. These are
	// installed in the shared filter/DOCKER-USER chain (not this VM's private
	// table), so a crash before Cleanup leaves them behind; deleting only rules
	// tagged with matchlock's marker and whose tap is gone avoids touching
	// Docker's own rules or a live sandbox's.
	err := r.reconcileDockerUserForwardRules(tapNameCandidates(rec.VMID, rec.Resources.TAPName), report)
	if err != nil {
		errs = append(errs, err)
	}
	_ = store.MarkCleanup("forward_rule_cleanup", err)

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// reconcileDockerUserForwardRules removes DOCKER-USER accept rules that
// matchlock installed for a TAP that no longer exists. The rules are tagged
// with UserData == sandboxnet.TapAcceptMarker(tap), so only matchlock-owned
// rules for dead taps are removed (never Docker's own RETURN, nor a live
// sandbox's rules). It returns an error only when the chain cannot be inspected
// or a deletion/flush fails; a positively absent chain is a clean no-op. A
// rule is deleted only if its marker's tap is one of the given candidate taps
// (in scope) AND the tap is absent from the host's existing-interface snapshot,
// so reconciling one VM never touches another VM's or Docker's own rules.
func (r *Reconciler) reconcileDockerUserForwardRules(taps []string, report *ReconcileReport) error {
	src := dockerUserSource
	if src == nil {
		// Default to the real nftables source when a test hasn't injected one.
		src = newNftDockerUserSource()
	}
	liv := defaultLiveInterfaceSet
	if liv == nil {
		liv = liveInterfaceSet
	}

	// Snapshot the live interfaces once. A tap is definitively absent iff it is
	// not in this snapshot; if we cannot obtain a snapshot, we cannot confirm
	// absence, so we belay deletion entirely (do nothing).
	liveSet, err := liv()
	if err != nil {
		report.addFailed("forward_rule_inspect", err)
		return errx.Wrap(ErrReconcileRule, err)
	}
	isAlive := func(tap string) bool { return liveSet[tap] }
	inScope := func(tap string) bool {
		for _, c := range taps {
			if c == tap {
				return true
			}
		}
		return false
	}

	allRules, absent, err := src.list()
	if err != nil {
		report.addFailed("forward_rule_inspect", err)
		return errx.Wrap(ErrReconcileRule, err)
	}
	if absent {
		return nil
	}

	toDelete := selectDeadAcceptRules(isAlive, inScope, allRules)
	if len(toDelete) == 0 {
		return nil
	}

	for _, rule := range toDelete {
		if err := src.delete(rule); err != nil {
			report.addFailed("forward_rule_delete:"+string(rule.UserData), err)
			return errx.With(ErrReconcileRule, " %s: %w", string(rule.UserData), err)
		}
	}
	if err := src.flush(); err != nil {
		report.addFailed("forward_rule_flush", err)
		return errx.Wrap(ErrReconcileRule, err)
	}
	// Only after a successful flush are the deletions real; report them now.
	for _, rule := range toDelete {
		report.addCleaned("forward_rule_delete:" + string(rule.UserData))
	}
	return nil
}

// liveInterfaceSet returns the set of currently-up interface names, keyed by
// name. A tap name not in this set is treated as absent (dead); a tap in the set
// is treated as alive (a still-running VM's rule is never deleted).
func liveInterfaceSet() (map[string]bool, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(ifaces))
	for _, iface := range ifaces {
		set[iface.Name] = true
	}
	return set, nil
}

// defaultLiveInterfaceSet and dockerUserSource are package-level seams so the
// reconcile logic can be unit-tested without root and without a real nftables
// connection. On Linux they default to the net/nftables implementations; tests
// override them with fakes. They are linux-only (declared here with the //go:build
// linux tag) so the platform-neutral reconcile.go does not import nftables.
var (
	defaultLiveInterfaceSet = liveInterfaceSet
	dockerUserSource        interface {
		list() ([]*nftables.Rule, bool, error)
		delete(rule *nftables.Rule) error
		flush() error
	} = nil
)

// selectDeadAcceptRules returns the subset of DOCKER-USER rules to delete:
// matchlock-tap-accept:<tap> markers whose tap is in scope (one of the given
// candidate taps) and absent from the live-interface snapshot. It is a pure
// function (no I/O) so the delete decision is unit-testable without root. Rules
// for live taps, out-of-scope taps, and non-matchlock rules are never selected.
func selectDeadAcceptRules(isAlive func(string) bool, inScope func(string) bool, rules []*nftables.Rule) []*nftables.Rule {
	prefix := sandboxnet.ForwardAcceptMarker + ":"
	var out []*nftables.Rule
	for _, rule := range rules {
		marker := string(rule.UserData)
		if !strings.HasPrefix(marker, prefix) {
			continue
		}
		tap := strings.TrimPrefix(marker, prefix)
		if tap == "" || !inScope(tap) || isAlive(tap) {
			continue
		}
		out = append(out, rule)
	}
	return out
}

// nftDockerUserSource is the production dockerUserSource backed by nftables.
// list returns the DOCKER-USER rules (or absent=true when the chain is
// positively absent); delete/flush operate on the bound connection.
type nftDockerUserSource struct{ conn *nftables.Conn }

func newNftDockerUserSource() *nftDockerUserSource { return &nftDockerUserSource{} }

func (n *nftDockerUserSource) list() ([]*nftables.Rule, bool, error) {
	conn, err := nftables.New()
	if err != nil {
		return nil, false, err
	}
	n.conn = conn

	table, chain, err := dockerUserChain(conn)
	if err != nil {
		return nil, false, err
	}
	if table == nil || chain == nil {
		return nil, true, nil
	}

	rules, err := conn.GetRules(table, chain)
	if err != nil {
		return nil, false, err
	}
	return rules, false, nil
}

func (n *nftDockerUserSource) delete(rule *nftables.Rule) error {
	if n.conn == nil {
		return nil
	}
	return n.conn.DelRule(rule)
}

func (n *nftDockerUserSource) flush() error {
	if n.conn == nil {
		return nil
	}
	return n.conn.Flush()
}

// dockerUserChain returns the host's ip filter DOCKER-USER table/chain. It
// returns (nil, nil, nil) only when the chain is positively absent (no ip filter
// table, or no DOCKER-USER chain within it) — a clean no-op. Any genuine
// inspection error (netlink failure, permission) is propagated so the caller does
// not mistake an inability to read the ruleset for an empty one.
//
// It enumerates chains rather than calling ListChain on the fixed name, because
// ListChain returns an error for a missing chain (netlink replies with 0
// messages), which must not be treated as a failure — a host without Docker has
// no DOCKER-USER chain and that is a normal clean no-op.
func dockerUserChain(conn *nftables.Conn) (*nftables.Table, *nftables.Chain, error) {
	tables, err := conn.ListTables()
	if err != nil {
		return nil, nil, err
	}
	var filter *nftables.Table
	for _, t := range tables {
		if t.Family == nftables.TableFamilyIPv4 && t.Name == "filter" {
			filter = t
			break
		}
	}
	// No ip filter table -> no DOCKER-USER chain.
	if filter == nil {
		return nil, nil, nil
	}

	chains, err := conn.ListChains()
	if err != nil {
		return nil, nil, err
	}
	for _, c := range chains {
		if c.Name == "DOCKER-USER" &&
			c.Table != nil &&
			c.Table.Name == "filter" &&
			c.Table.Family == nftables.TableFamilyIPv4 {
			return filter, c, nil
		}
	}
	// ip filter table exists but no IPv4 DOCKER-USER chain -> clean no-op.
	return nil, nil, nil
}

func deleteNFTTable(tableName string) error {
	conn, err := nftables.New()
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			return nil
		}
		return err
	}
	tables, err := conn.ListTables()
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			return nil
		}
		return err
	}

	found := false
	for _, t := range tables {
		if t.Name == tableName {
			conn.DelTable(t)
			found = true
		}
	}
	if !found {
		return nil
	}
	if err := conn.Flush(); err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			return nil
		}
		return err
	}
	return nil
}
