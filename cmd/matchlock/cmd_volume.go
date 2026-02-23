package main

import (
	"encoding/json"
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

var volumeRmCmd = &cobra.Command{
	Use:     "rm <name>",
	Aliases: []string{"remove"},
	Short:   "Remove a named volume",
	Args:    cobra.ExactArgs(1),
	RunE:    runVolumeRm,
}

func init() {
	volumeCreateCmd.Flags().Int("size", defaultNamedVolumeSizeMB, "Volume size in MB")
	volumeCreateCmd.Flags().Bool("json", false, "Output machine-readable JSON")
	volumeLsCmd.Flags().Bool("json", false, "Output machine-readable JSON")

	volumeCmd.AddCommand(volumeCreateCmd)
	volumeCmd.AddCommand(volumeLsCmd)
	volumeCmd.AddCommand(volumeRmCmd)
	rootCmd.AddCommand(volumeCmd)
}

func runVolumeCreate(cmd *cobra.Command, args []string) error {
	name := args[0]
	sizeMB, _ := cmd.Flags().GetInt("size")
	jsonOutput, _ := cmd.Flags().GetBool("json")

	path, err := createNamedVolume(name, sizeMB)
	if err != nil {
		return err
	}

	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(struct {
			Name string `json:"name"`
			Size string `json:"size"`
			Path string `json:"path"`
		}{
			Name: name,
			Size: fmt.Sprintf("%.1f MB", float64(sizeMB)),
			Path: path,
		})
	}

	fmt.Printf("Created volume %s (%d MB)\n", name, sizeMB)
	return nil
}

func runVolumeLs(cmd *cobra.Command, args []string) error {
	jsonOutput, _ := cmd.Flags().GetBool("json")
	vols, err := listNamedVolumes()
	if err != nil {
		return err
	}

	if jsonOutput {
		output := make([]struct {
			Name string `json:"name"`
			Size string `json:"size"`
			Path string `json:"path"`
		}, 0, len(vols))
		for _, v := range vols {
			output = append(output, struct {
				Name string `json:"name"`
				Size string `json:"size"`
				Path string `json:"path"`
			}{
				Name: v.Name,
				Size: humanizeMB(v.SizeBytes),
				Path: v.Path,
			})
		}
		return json.NewEncoder(os.Stdout).Encode(output)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSIZE")
	for _, v := range vols {
		fmt.Fprintf(w, "%s\t%s\n", v.Name, humanizeMB(v.SizeBytes))
	}
	w.Flush()
	return nil
}

func runVolumeRm(cmd *cobra.Command, args []string) error {
	name := args[0]

	if err := removeNamedVolume(name); err != nil {
		return err
	}

	fmt.Printf("Removed volume %s\n", name)
	return nil
}
