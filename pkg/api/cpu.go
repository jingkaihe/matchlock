package api

import "math"

// MaxFirecrackerVCPUs is the largest vCPU count the Firecracker VMM accepts.
// Firecracker rejects larger values at config load (InvalidVcpuCount), so any
// caller that would otherwise default to "all host CPUs" must cap here. It is
// defined in this cross-platform package so both the CLI and the Linux backend
// can reference a single authoritative value.
const MaxFirecrackerVCPUs = 32

// IsValidCPUCount reports whether cpus is finite and greater than zero.
func IsValidCPUCount(cpus float64) bool {
	return !math.IsNaN(cpus) && !math.IsInf(cpus, 0) && cpus > 0
}

// VCPUCount rounds a valid CPU value up to a whole vCPU count.
// The second return value is false when cpus is invalid or overflows int.
func VCPUCount(cpus float64) (int, bool) {
	if !IsValidCPUCount(cpus) {
		return 0, false
	}
	vcpus := math.Ceil(cpus)
	if vcpus > float64(math.MaxInt) {
		return 0, false
	}
	return int(vcpus), true
}
