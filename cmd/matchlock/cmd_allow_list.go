package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/sandbox"
	"github.com/jingkaihe/matchlock/pkg/state"
)

var allowListCmd = &cobra.Command{
	Use:   "allow-list",
	Short: "Manage runtime network allow-list for a running sandbox",
}

var allowListAddCmd = &cobra.Command{
	Use:   "add <id> <host1,host2...>",
	Short: "Add hosts to a running sandbox allow-list",
	Args:  cobra.MinimumNArgs(2),
	RunE:  runAllowListAdd,
}

var allowListDeleteCmd = &cobra.Command{
	Use:   "delete <id> <host1,host2...>",
	Short: "Delete hosts from a running sandbox allow-list",
	Args:  cobra.MinimumNArgs(2),
	RunE:  runAllowListDelete,
}

func init() {
	allowListCmd.AddCommand(allowListAddCmd)
	allowListCmd.AddCommand(allowListDeleteCmd)
	rootCmd.AddCommand(allowListCmd)
}

func runAllowListAdd(cmd *cobra.Command, args []string) error {
	vmID := args[0]
	hosts, err := parseAllowListHosts(args[1:])
	if err != nil {
		return err
	}

	execSocketPath, err := resolveAllowListExecSocket(vmID)
	if err != nil {
		return err
	}

	ctx, cancel := contextWithSignal(context.Background())
	defer cancel()

	result, err := sandbox.AllowListAddViaRelay(ctx, execSocketPath, hosts)
	if err != nil {
		return errx.Wrap(ErrAllowListUpdate, err)
	}

	if len(result.Added) == 0 {
		fmt.Printf("No new hosts were added to %s\n", vmID)
	} else {
		fmt.Printf("Added to %s: %s\n", vmID, strings.Join(result.Added, ","))
	}
	fmt.Printf("Current allow-list: %s\n", formatAllowList(result.AllowedHosts))
	return nil
}

func runAllowListDelete(cmd *cobra.Command, args []string) error {
	vmID := args[0]
	hosts, err := parseAllowListHosts(args[1:])
	if err != nil {
		return err
	}

	execSocketPath, err := resolveAllowListExecSocket(vmID)
	if err != nil {
		return err
	}

	ctx, cancel := contextWithSignal(context.Background())
	defer cancel()

	result, err := sandbox.AllowListDeleteViaRelay(ctx, execSocketPath, hosts)
	if err != nil {
		return errx.Wrap(ErrAllowListUpdate, err)
	}

	if len(result.Removed) == 0 {
		fmt.Printf("No hosts were removed from %s\n", vmID)
	} else {
		fmt.Printf("Removed from %s: %s\n", vmID, strings.Join(result.Removed, ","))
	}
	fmt.Printf("Current allow-list: %s\n", formatAllowList(result.AllowedHosts))
	return nil
}

func parseAllowListHosts(parts []string) ([]string, error) {
	hosts := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))

	for _, part := range parts {
		for _, token := range strings.Split(part, ",") {
			host := strings.TrimSpace(token)
			if host == "" {
				continue
			}
			if _, ok := seen[host]; ok {
				continue
			}
			hosts = append(hosts, host)
			seen[host] = struct{}{}
		}
	}

	if len(hosts) == 0 {
		return nil, errx.With(ErrInvalidAllowListHost, ": expected one or more hosts (comma-separated)")
	}
	return hosts, nil
}

func resolveAllowListExecSocket(vmID string) (string, error) {
	mgr := state.NewManager()
	vmState, err := mgr.Get(vmID)
	if err != nil {
		return "", errx.With(ErrVMNotFound, " %s: %w", vmID, err)
	}
	if vmState.Status != "running" {
		return "", fmt.Errorf("VM %s is not running (status: %s)", vmID, vmState.Status)
	}

	execSocketPath := mgr.ExecSocketPath(vmID)
	if _, err := os.Stat(execSocketPath); err != nil {
		return "", fmt.Errorf("exec socket not found for %s (was it started with --rm=false?)", vmID)
	}
	return execSocketPath, nil
}

func formatAllowList(hosts []string) string {
	if len(hosts) == 0 {
		return "(empty: all hosts allowed)"
	}
	return strings.Join(hosts, ",")
}
