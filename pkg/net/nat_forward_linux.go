//go:build linux

package net

import (
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
)

// forwardAcceptMarker tags the nftables rules installed by installForwardAccept
// so they can be removed unambiguously on Cleanup without touching pre-existing
// DOCKER-USER content.
const forwardAcceptMarker = "matchlock-tap-accept"

// ForwardAcceptMarker is the exported prefix for the DOCKER-USER accept rules
// matchlock installs for a sandbox TAP. The full marker is
// ForwardAcceptMarker + ":" + tap. It is exported so the lifecycle reconciler
// can find and remove orphaned rules for taps that no longer exist.
const ForwardAcceptMarker = forwardAcceptMarker

// tapAcceptMarker returns the per-TAP marker used to tag the rules for a given
// interface. Encoding the TAP keeps concurrent sandboxes' rules distinguishable,
// so one sandbox's cleanup never deletes another's.
func tapAcceptMarker(tap string) string {
	return forwardAcceptMarker + ":" + tap
}

// TapAcceptMarker returns the exported per-TAP marker (ForwardAcceptMarker + ":" + tap).
func TapAcceptMarker(tap string) string {
	return tapAcceptMarker(tap)
}

// installForwardAccept inserts bidirectional accept rules for the sandbox TAP
// into the host's ip filter DOCKER-USER chain. The host's FORWARD base chain
// jumps to DOCKER-USER before its policy drop, so accept rules here prevent the
// host from dropping the sandbox's forwarded guest traffic. Rules are inserted
// at the head of the chain (before Docker's return) and tagged with a UserData
// marker so they can be removed unambiguously on Cleanup.
//
// Returns nil when the host has no DOCKER-USER chain (a non-Docker host); this
// is not fatal and the sandbox still runs.
func (n *NFTablesNAT) installForwardAccept() error {
	if n.conn == nil {
		conn, err := nftables.New()
		if err != nil {
			return err
		}
		n.conn = conn
	}

	table, chain := n.dockerChain()
	if table == nil || chain == nil {
		return nil
	}

	for _, key := range []expr.MetaKey{expr.MetaKeyIIFNAME, expr.MetaKeyOIFNAME} {
		n.conn.InsertRule(&nftables.Rule{
			Table: table,
			Chain: chain,
			Exprs: []expr.Any{
				&expr.Meta{Key: key, Register: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(n.tapInterface)},
				&expr.Verdict{Kind: expr.VerdictAccept},
			},
			UserData: []byte(tapAcceptMarker(n.tapInterface)),
		})
	}
	return n.conn.Flush()
}

// removeForwardAccept deletes the TAP accept rules previously installed by
// installForwardAccept, identified by their UserData marker.
func (n *NFTablesNAT) removeForwardAccept() {
	if n.conn == nil {
		conn, err := nftables.New()
		if err != nil {
			return
		}
		n.conn = conn
	}

	table, chain := n.dockerChain()
	if table == nil || chain == nil {
		return
	}

	rules, err := n.conn.GetRules(table, chain)
	if err != nil {
		return
	}
	for _, r := range rules {
		if string(r.UserData) == tapAcceptMarker(n.tapInterface) {
			_ = n.conn.DelRule(r)
		}
	}
	_ = n.conn.Flush()
}

// dockerChain returns the host's ip filter DOCKER-USER chain, or nil if the host
// has no such table/chain.
func (n *NFTablesNAT) dockerChain() (*nftables.Table, *nftables.Chain) {
	tables, err := n.conn.ListTables()
	if err != nil {
		return nil, nil
	}
	for _, t := range tables {
		if t.Family == nftables.TableFamilyIPv4 && t.Name == "filter" {
			chain, err := n.conn.ListChain(t, "DOCKER-USER")
			if err != nil || chain == nil {
				return nil, nil
			}
			return t, chain
		}
	}
	return nil, nil
}
