package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var volumeCmd = &cobra.Command{
	Use:   "volume",
	Short: "Manage named raw disk volumes",
}

var volumeCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a named ext4 volume",
	Args:  cobra.ExactArgs(1),
	RunE:  runVolumeCreate,
}

var volumeLsCmd = &cobra.Command{
	Use:     "ls",
	Aliases: []string{"list"},
	Short:   "List named volumes",
	RunE:    runVolumeLs,
}

func init() {
	volumeCreateCmd.Flags().Int("size", defaultNamedVolumeSizeMB, "Volume size in MB")

	volumeCmd.AddCommand(volumeCreateCmd)
	volumeCmd.AddCommand(volumeLsCmd)
	rootCmd.AddCommand(volumeCmd)
}

func runVolumeCreate(cmd *cobra.Command, args []string) error {
	name := args[0]
	sizeMB, _ := cmd.Flags().GetInt("size")

	path, err := createNamedVolume(name, sizeMB)
	if err != nil {
		return err
	}

	fmt.Printf("Created volume %s (%d MB)\n", name, sizeMB)
	fmt.Printf("Path: %s\n", path)
	return nil
}

func runVolumeLs(cmd *cobra.Command, args []string) error {
	vols, err := listNamedVolumes()
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSIZE\tPATH")
	for _, v := range vols {
		fmt.Fprintf(w, "%s\t%s\t%s\n", v.Name, humanizeMB(v.SizeBytes), v.Path)
	}
	w.Flush()
	return nil
}
