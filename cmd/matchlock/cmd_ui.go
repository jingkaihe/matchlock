package main

import "github.com/spf13/cobra"

var uiCmd = &cobra.Command{
	Use:   "ui",
	Short: "UI moved to standalone binary",
	Long:  "The embedded UI has moved to the standalone matchlock-ui binary.",
	RunE: func(cmd *cobra.Command, args []string) error {
		return &exitCodeError{code: 1}
	},
}

func init() {
	rootCmd.AddCommand(uiCmd)
}
