//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/sandbox"
)

type diagnoseStatus string

const (
	diagnoseStatusPass diagnoseStatus = "pass"
	diagnoseStatusFail diagnoseStatus = "fail"
	diagnoseStatusWarn diagnoseStatus = "warn"
)

type diagnoseCheck struct {
	Name    string         `json:"name"`
	Status  diagnoseStatus `json:"status"`
	Message string         `json:"message"`
	Fix     string         `json:"fix,omitempty"`
}

type diagnoseResult struct {
	OK     bool            `json:"ok"`
	Checks []diagnoseCheck `json:"checks"`
}

var diagnoseCmd = &cobra.Command{
	Use:   "diagnose",
	Short: "Diagnose host requirements for running matchlock",
	RunE:  runDiagnose,
}

func init() {
	diagnoseCmd.Flags().Bool("json", false, "Output machine-readable JSON")
	rootCmd.AddCommand(diagnoseCmd)
}

func runDiagnose(cmd *cobra.Command, args []string) error {
	jsonOutput, _ := cmd.Flags().GetBool("json")
	result := runDarwinDiagnose()

	if jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			return err
		}
	} else {
		for _, check := range result.Checks {
			icon := "✓"
			switch check.Status {
			case diagnoseStatusFail:
				icon = "✗"
			case diagnoseStatusWarn:
				icon = "⚠"
			}
			fmt.Printf("%s %s: %s\n", icon, check.Name, check.Message)
			if check.Fix != "" {
				fmt.Printf("  fix: %s\n", check.Fix)
			}
		}
	}

	if !result.OK {
		return errx.With(ErrDiagnoseFailed, ": one or more checks failed")
	}
	return nil
}

func runDarwinDiagnose() diagnoseResult {
	checks := []diagnoseCheck{
		checkDarwinArchitecture(),
		checkDarwinHypervisorSupport(),
		checkKernelArtifact(),
		checkGuestArtifact("guest-init", sandbox.DefaultGuestInitPath()),
		checkCodesignAvailability(),
	}

	ok := true
	for _, check := range checks {
		if check.Status == diagnoseStatusFail {
			ok = false
			break
		}
	}

	return diagnoseResult{OK: ok, Checks: checks}
}

func checkDarwinArchitecture() diagnoseCheck {
	if runtime.GOARCH != "arm64" {
		return diagnoseCheck{
			Name:    "architecture",
			Status:  diagnoseStatusFail,
			Message: fmt.Sprintf("Unsupported architecture: %s.", runtime.GOARCH),
			Fix:     "Matchlock on macOS requires Apple Silicon (arm64).",
		}
	}

	return diagnoseCheck{
		Name:    "architecture",
		Status:  diagnoseStatusPass,
		Message: "Apple Silicon (arm64) detected.",
	}
}

func checkDarwinHypervisorSupport() diagnoseCheck {
	out, err := exec.Command("sysctl", "-n", "kern.hv_support").Output()
	if err != nil {
		return diagnoseCheck{
			Name:    "hypervisor",
			Status:  diagnoseStatusFail,
			Message: fmt.Sprintf("Could not query Hypervisor support: %v", err),
			Fix:     "Run on Apple Silicon macOS with Virtualization.framework support.",
		}
	}

	if strings.TrimSpace(string(out)) != "1" {
		return diagnoseCheck{
			Name:    "hypervisor",
			Status:  diagnoseStatusFail,
			Message: "Hypervisor support is disabled.",
			Fix:     "Use a host that supports Virtualization.framework hypervisor acceleration.",
		}
	}

	return diagnoseCheck{
		Name:    "hypervisor",
		Status:  diagnoseStatusPass,
		Message: "Hypervisor support is enabled.",
	}
}

func checkKernelArtifact() diagnoseCheck {
	path := sandbox.DefaultKernelPath()
	if path == "" {
		return diagnoseCheck{
			Name:    "kernel",
			Status:  diagnoseStatusFail,
			Message: "Kernel path could not be resolved.",
			Fix:     "Build or download the Matchlock kernel before running sandboxes.",
		}
	}
	if _, err := os.Stat(path); err != nil {
		return diagnoseCheck{
			Name:    "kernel",
			Status:  diagnoseStatusFail,
			Message: fmt.Sprintf("Kernel not found at %s.", path),
			Fix:     "Build or download the Matchlock kernel before running sandboxes.",
		}
	}

	return diagnoseCheck{
		Name:    "kernel",
		Status:  diagnoseStatusPass,
		Message: fmt.Sprintf("Kernel available at %s.", path),
	}
}

func checkGuestArtifact(name, path string) diagnoseCheck {
	if path == "" {
		return diagnoseCheck{
			Name:    name,
			Status:  diagnoseStatusFail,
			Message: fmt.Sprintf("%s path could not be resolved.", name),
			Fix:     fmt.Sprintf("Build %s and place it next to the matchlock binary or under ~/.cache/matchlock/.", name),
		}
	}
	if _, err := os.Stat(path); err != nil {
		return diagnoseCheck{
			Name:    name,
			Status:  diagnoseStatusFail,
			Message: fmt.Sprintf("%s not found at %s.", name, path),
			Fix:     fmt.Sprintf("Build %s and place it next to the matchlock binary or under ~/.cache/matchlock/.", name),
		}
	}

	return diagnoseCheck{
		Name:    name,
		Status:  diagnoseStatusPass,
		Message: fmt.Sprintf("%s available at %s.", name, path),
	}
}

func checkCodesignAvailability() diagnoseCheck {
	path, err := os.Executable()
	if err != nil {
		return diagnoseCheck{
			Name:    "codesign",
			Status:  diagnoseStatusWarn,
			Message: fmt.Sprintf("Could not resolve current binary for codesign inspection: %v", err),
			Fix:     "Ensure the matchlock binary is codesigned with the virtualization entitlement if startup fails.",
		}
	}

	out, err := exec.Command("codesign", "-dv", path).CombinedOutput()
	if err != nil {
		return diagnoseCheck{
			Name:    "codesign",
			Status:  diagnoseStatusWarn,
			Message: fmt.Sprintf("Could not inspect codesign state for %s: %v", path, err),
			Fix:     "Codesign the binary with the virtualization entitlement if Virtualization.framework rejects startup.",
		}
	}

	message := "Binary is codesigned."
	if strings.Contains(string(out), "not signed") {
		return diagnoseCheck{
			Name:    "codesign",
			Status:  diagnoseStatusWarn,
			Message: "Binary is not codesigned.",
			Fix:     "Codesign the binary with the virtualization entitlement if Virtualization.framework rejects startup.",
		}
	}

	return diagnoseCheck{
		Name:    "codesign",
		Status:  diagnoseStatusPass,
		Message: message,
	}
}
