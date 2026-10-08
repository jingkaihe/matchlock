//go:build linux

package lifecycle

import (
	"testing"

	"github.com/google/nftables"
)

// noopDockerUserSource reports the DOCKER-USER chain as positively absent so a
// reconcile invoked in an environment without nftables privilege performs no
// firewall mutation.
type noopDockerUserSource struct{}

func (noopDockerUserSource) list() ([]*nftables.Rule, bool, error) { return nil, true, nil }
func (noopDockerUserSource) delete(*nftables.Rule) error           { return nil }
func (noopDockerUserSource) flush() error                          { return nil }

// noopInterfaceLister reports no existing interfaces.
type noopInterfaceLister struct{}

func (noopInterfaceLister) existingInterfaces() (map[string]bool, error) { return nil, nil }

// injectReconcileSeams stubs the DOCKER-USER reconcile seam so tests that call
// ReconcileVM (and thus reconcilePlatform) do not depend on a real nftables
// connection. It registers a cleanup that restores the prior globals, so the
// mutable seam does not leak between tests. Call it at the start of any test
// that drives reconcilePlatform on a host without nftables privilege.
func injectReconcileSeams(t *testing.T) {
	t.Helper()
	prevSource := dockerUserSource
	prevLive := defaultLiveInterfaceSet
	dockerUserSource = noopDockerUserSource{}
	defaultLiveInterfaceSet = func() (map[string]bool, error) { return nil, nil }
	t.Cleanup(func() {
		dockerUserSource = prevSource
		defaultLiveInterfaceSet = prevLive
	})
}
