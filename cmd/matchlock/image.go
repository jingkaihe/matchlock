package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/jingkaihe/matchlock/pkg/image"
)

var imageCmd = &cobra.Command{
	Use:   "image",
	Short: "Manage images",
}

var imageLsCmd = &cobra.Command{
	Use:     "ls",
	Aliases: []string{"list"},
	Short:   "List all images (local + cached registry)",
	RunE:    runImageLs,
}

var imageRmCmd = &cobra.Command{
	Use:     "rm <tag>",
	Aliases: []string{"remove"},
	Short:   "Remove a local image",
	Args:    cobra.ExactArgs(1),
	RunE:    runImageRm,
}

var imageImportCmd = &cobra.Command{
	Use:   "import <tag>",
	Short: "Import an image from a Docker/OCI tarball via stdin",
	Long: `Import an image from a Docker/OCI tarball read from stdin.

The tarball format is the same as "docker save" output.`,
	Example: `  docker save myapp:latest | matchlock image import myapp:latest
  matchlock image import myapp:latest < image.tar`,
	Args: cobra.ExactArgs(1),
	RunE: runImageImport,
}

func init() {
	imageCmd.AddCommand(imageLsCmd)
	imageCmd.AddCommand(imageRmCmd)
	imageCmd.AddCommand(imageImportCmd)
	rootCmd.AddCommand(imageCmd)
}

func runImageLs(cmd *cobra.Command, args []string) error {
	store := image.NewStore("")
	localImages, err := store.List()
	if err != nil {
		return err
	}

	registryImages, err := image.ListRegistryCache("")
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TAG\tSOURCE\tSIZE\tCREATED")

	for _, img := range localImages {
		source := img.Meta.Source
		if source == "" {
			source = "local"
		}
		fmt.Fprintf(w, "%s\t%s\t%.1f MB\t%s\n",
			img.Tag,
			source,
			float64(img.Meta.Size)/(1024*1024),
			img.Meta.CreatedAt.Format(time.DateTime),
		)
	}

	for _, img := range registryImages {
		fmt.Fprintf(w, "%s\t%s\t%.1f MB\t%s\n",
			img.Tag,
			"registry",
			float64(img.Meta.Size)/(1024*1024),
			img.Meta.CreatedAt.Format(time.DateTime),
		)
	}

	w.Flush()
	return nil
}

func runImageRm(cmd *cobra.Command, args []string) error {
	store := image.NewStore("")
	if err := store.Remove(args[0]); err != nil {
		return err
	}
	fmt.Printf("Removed %s\n", args[0])
	return nil
}

func runImageImport(cmd *cobra.Command, args []string) error {
	tag := args[0]

	builder := image.NewBuilder(&image.BuildOptions{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	fmt.Fprintf(os.Stderr, "Importing %s from stdin...\n", tag)
	result, err := builder.Import(ctx, os.Stdin, tag)
	if err != nil {
		return err
	}

	fmt.Printf("Imported: %s\n", tag)
	fmt.Printf("Rootfs: %s\n", result.RootfsPath)
	fmt.Printf("Size: %.1f MB\n", float64(result.Size)/(1024*1024))
	return nil
}
