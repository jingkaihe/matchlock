package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"

	"github.com/spf13/cobra"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/sandbox"
	"github.com/jingkaihe/matchlock/pkg/state"
)

var portForwardCmd = &cobra.Command{
	Use:   "port-forward <id> [LOCAL_PORT:]REMOTE_PORT [...[LOCAL_PORT_N:]REMOTE_PORT_N]",
	Short: "Forward local ports to ports in a running sandbox",
	Long: `Forward one or more local ports to ports in a running sandbox.

Examples:
  matchlock port-forward vm-abc123 8080
  matchlock port-forward vm-abc123 18080:8080 18443:8443
  matchlock port-forward --address 0.0.0.0 vm-abc123 8080:8080`,
	Args: cobra.MinimumNArgs(2),
	RunE: runPortForward,
}

func init() {
	portForwardCmd.Flags().StringSlice("address", []string{"127.0.0.1"}, "Address to bind local listeners on the host (can be repeated)")
	rootCmd.AddCommand(portForwardCmd)
}

func runPortForward(cmd *cobra.Command, args []string) error {
	vmID := args[0]
	forwardSpecs := args[1:]

	forwards, err := api.ParsePortForwards(forwardSpecs)
	if err != nil {
		return errx.Wrap(ErrInvalidPortForward, err)
	}

	addresses, _ := cmd.Flags().GetStringSlice("address")
	addresses, err = normalizePortForwardAddresses(addresses)
	if err != nil {
		return errx.Wrap(ErrInvalidPortForwardAddr, err)
	}

	mgr := state.NewManager()
	vmState, err := mgr.Get(vmID)
	if err != nil {
		return errx.With(ErrVMNotFound, " %s: %w", vmID, err)
	}
	if vmState.Status != "running" {
		return fmt.Errorf("VM %s is not running (status: %s)", vmID, vmState.Status)
	}

	execSocketPath := mgr.ExecSocketPath(vmID)
	if _, err := os.Stat(execSocketPath); err != nil {
		return fmt.Errorf("exec socket not found for %s (was it started with --rm=false?)", vmID)
	}

	type listenerBinding struct {
		listener net.Listener
		binding  api.PortForwardBinding
	}

	var listeners []listenerBinding
	closeListeners := func() {
		for _, l := range listeners {
			_ = l.listener.Close()
		}
	}

	seen := make(map[string]struct{})
	for _, pf := range forwards {
		for _, addr := range addresses {
			listenAddr := net.JoinHostPort(addr, strconv.Itoa(pf.LocalPort))
			if _, ok := seen[listenAddr]; ok {
				closeListeners()
				return errx.With(ErrPortForwardListen, ": duplicate listen address %s", listenAddr)
			}
			seen[listenAddr] = struct{}{}

			ln, err := net.Listen("tcp", listenAddr)
			if err != nil {
				closeListeners()
				return errx.With(ErrPortForwardListen, " %s: %w", listenAddr, err)
			}

			tcpAddr, ok := ln.Addr().(*net.TCPAddr)
			if !ok {
				closeListeners()
				return errx.With(ErrPortForwardListen, ": unexpected listener address type %T", ln.Addr())
			}

			listeners = append(listeners, listenerBinding{
				listener: ln,
				binding: api.PortForwardBinding{
					Address:    tcpAddr.IP.String(),
					LocalPort:  tcpAddr.Port,
					RemotePort: pf.RemotePort,
				},
			})
		}
	}
	defer closeListeners()

	for _, l := range listeners {
		fmt.Fprintf(os.Stderr, "Forwarding %s:%d -> sandbox:%d\n", l.binding.Address, l.binding.LocalPort, l.binding.RemotePort)
	}

	ctx, cancel := contextWithSignal(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	for _, l := range listeners {
		wg.Add(1)
		go func(lb listenerBinding) {
			defer wg.Done()
			for {
				conn, err := lb.listener.Accept()
				if err != nil {
					if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
						return
					}
					continue
				}

				go func(c net.Conn, remotePort int) {
					defer c.Close()
					if err := sandbox.PortForwardViaRelay(ctx, execSocketPath, remotePort, c); err != nil && ctx.Err() == nil {
						fmt.Fprintf(os.Stderr, "port-forward connection error: %v\n", err)
					}
				}(conn, lb.binding.RemotePort)
			}
		}(l)
	}

	<-ctx.Done()
	closeListeners()
	wg.Wait()
	return nil
}
