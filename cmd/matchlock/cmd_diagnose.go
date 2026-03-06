package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/diagnose"
)

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
	result := diagnose.Run()

	if jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			return err
		}
	} else {
		for _, check := range result.Checks {
			icon := "✓"
			switch check.Status {
			case diagnose.StatusFail:
				icon = "✗"
			case diagnose.StatusWarn:
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
