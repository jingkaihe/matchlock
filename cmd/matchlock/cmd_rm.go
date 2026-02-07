package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/jingkaihe/matchlock/pkg/state"
)

var rmCmd = &cobra.Command{
	Use:     "rm <id>",
	Aliases: []string{"remove"},
	Short:   "Remove a stopped sandbox",
	RunE:    runRemove,
}

func init() {
	rmCmd.Flags().Bool("stopped", false, "Remove all stopped VMs")
	viper.BindPFlag("rm.stopped", rmCmd.Flags().Lookup("stopped"))

	rootCmd.AddCommand(rmCmd)
}

func runRemove(cmd *cobra.Command, args []string) error {
	stopped, _ := cmd.Flags().GetBool("stopped")
	mgr := state.NewManager()

	if stopped {
		states, _ := mgr.List()
		for _, s := range states {
			if s.Status != "running" {
				if err := mgr.Remove(s.ID); err != nil {
					fmt.Fprintf(os.Stderr, "Failed to remove %s: %v\n", s.ID, err)
				} else {
					fmt.Printf("Removed %s\n", s.ID)
				}
			}
		}
		return nil
	}

	if len(args) == 0 {
		return fmt.Errorf("VM ID required (or use --stopped)")
	}

	if err := mgr.Remove(args[0]); err != nil {
		return err
	}
	fmt.Printf("Removed %s\n", args[0])
	return nil
}
