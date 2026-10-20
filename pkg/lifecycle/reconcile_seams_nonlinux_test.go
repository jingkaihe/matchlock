//go:build !linux

package lifecycle

import "testing"

// injectReconcileSeams is a no-op on platforms where the DOCKER-USER sweep does
// not exist. The Linux variant (in a //go:build linux file) stubs the seam so
// tests that drive ReconcileVM do not depend on a real nftables connection.
func injectReconcileSeams(*testing.T) {}
