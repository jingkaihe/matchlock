//go:build darwin

package diagnose

import (
	"fmt"
	"os"
	"runtime"
	"syscall"

	"github.com/jingkaihe/matchlock/pkg/sandbox"
)

func Run() Result {
	checks := []Check{
		checkDarwinArchitecture(),
		checkDarwinHypervisorSupport(),
		checkKernelArtifact(),
		checkGuestArtifact("guest-init", sandbox.DefaultGuestInitPath()),
	}
	return finalize(checks)
}

func checkDarwinArchitecture() Check {
	if runtime.GOARCH != "arm64" {
		return Fail("architecture", fmt.Sprintf("Unsupported architecture: %s.", runtime.GOARCH), "Matchlock on macOS requires Apple Silicon (arm64).")
	}
	return Pass("architecture", "Apple Silicon (arm64) detected.")
}

func checkDarwinHypervisorSupport() Check {
	out, err := syscall.Sysctl("kern.hv_support")
	if err != nil {
		return Fail("hypervisor", fmt.Sprintf("sysctl kern.hv_support: %v", err), "Run on Apple Silicon macOS with Virtualization.framework support.")
	}
	if out != "1" {
		return Fail("hypervisor", "Hypervisor support is disabled.", "Use a host that supports Virtualization.framework hypervisor acceleration.")
	}
	return Pass("hypervisor", "Hypervisor support is enabled.")
}

func checkKernelArtifact() Check {
	path := sandbox.DefaultKernelPath()
	if path == "" {
		return Fail("kernel", "Kernel path could not be resolved.", "Build or download the Matchlock kernel before running sandboxes.")
	}
	if _, err := os.Stat(path); err != nil {
		return Fail("kernel", fmt.Sprintf("Kernel not found at %s.", path), "Build or download the Matchlock kernel before running sandboxes.")
	}
	return Pass("kernel", fmt.Sprintf("Kernel available at %s.", path))
}

func checkGuestArtifact(name, path string) Check {
	if path == "" {
		return Fail(name, fmt.Sprintf("%s path could not be resolved.", name), fmt.Sprintf("Build %s and place it next to the matchlock binary or under ~/.cache/matchlock/.", name))
	}
	if _, err := os.Stat(path); err != nil {
		return Fail(name, fmt.Sprintf("%s not found at %s.", name, path), fmt.Sprintf("Build %s and place it next to the matchlock binary or under ~/.cache/matchlock/.", name))
	}
	return Pass(name, fmt.Sprintf("%s available at %s.", name, path))
}
