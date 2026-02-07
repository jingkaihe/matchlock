package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jingkaihe/matchlock/pkg/version"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("matchlock %s (commit: %s, built: %s)\n", version.Version, version.GitCommit, version.BuildTime)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
