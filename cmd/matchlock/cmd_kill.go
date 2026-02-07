package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/jingkaihe/matchlock/pkg/state"
)

var killCmd = &cobra.Command{
	Use:   "kill <id>",
	Short: "Kill a running sandbox",
	RunE:  runKill,
}

func init() {
	killCmd.Flags().Bool("all", false, "Kill all running VMs")
	viper.BindPFlag("kill.all", killCmd.Flags().Lookup("all"))

	rootCmd.AddCommand(killCmd)
}

func runKill(cmd *cobra.Command, args []string) error {
	all, _ := cmd.Flags().GetBool("all")
	mgr := state.NewManager()

	if all {
		states, _ := mgr.List()
		for _, s := range states {
			if s.Status == "running" {
				if err := mgr.Kill(s.ID); err != nil {
					fmt.Fprintf(os.Stderr, "Failed to kill %s: %v\n", s.ID, err)
				} else {
					fmt.Printf("Killed %s\n", s.ID)
				}
			}
		}
		return nil
	}

	if len(args) == 0 {
		return fmt.Errorf("VM ID required (or use --all)")
	}

	if err := mgr.Kill(args[0]); err != nil {
		return err
	}
	fmt.Printf("Killed %s\n", args[0])
	return nil
}
