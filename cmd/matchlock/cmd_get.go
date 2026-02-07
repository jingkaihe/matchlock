package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jingkaihe/matchlock/pkg/state"
)

var getCmd = &cobra.Command{
	Use:   "get <id>",
	Short: "Get details of a sandbox",
	Args:  cobra.ExactArgs(1),
	RunE:  runGet,
}

func init() {
	rootCmd.AddCommand(getCmd)
}

func runGet(cmd *cobra.Command, args []string) error {
	mgr := state.NewManager()
	s, err := mgr.Get(args[0])
	if err != nil {
		return err
	}

	output, _ := json.MarshalIndent(s, "", "  ")
	fmt.Println(string(output))
	return nil
}
