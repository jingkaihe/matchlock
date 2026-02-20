package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"time"
	"sync"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/vm"
	"github.com/jingkaihe/matchlock/pkg/vsock"
)

const guestLoopbackAddress = "127.0.0.1"

type listenerSpec struct {
	listener   net.Listener
	remotePort int
}

// PortForwardManager manages host listeners that proxy traffic into a sandbox.
type PortForwardManager struct {
	cancel    context.CancelFunc
	listeners []listenerSpec
	bindings  []api.PortForwardBinding
	dialGuest func(remotePort int) (net.Conn, error)
	wg        sync.WaitGroup
}

// Bindings returns the active listener bindings.
func (m *PortForwardManager) Bindings() []api.PortForwardBinding {
	if m == nil {
		return nil
	}
	out := make([]api.PortForwardBinding, len(m.bindings))
	copy(out, m.bindings)
	return out
}

// Close stops all listeners and waits for active proxy loops to exit.
func (m *PortForwardManager) Close() error {
	if m == nil {
		return nil
	}
	if m.cancel != nil {
		m.cancel()
	}

	var errs []error
	for _, l := range m.listeners {
		if err := l.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	m.wg.Wait()

	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// StartPortForwards starts local listeners and proxies connections to the guest.
func (s *Sandbox) StartPortForwards(ctx context.Context, addresses []string, forwards []api.PortForward) (*PortForwardManager, error) {
	if len(forwards) == 0 {
		return nil, nil
	}
	if _, ok := s.machine.(vm.VsockDialer); !ok {
		return nil, ErrNoVsockDialer
	}

	if len(addresses) == 0 {
		addresses = []string{"127.0.0.1"}
	}

	pfCtx, cancel := context.WithCancel(ctx)
	manager := &PortForwardManager{
		cancel:    cancel,
		dialGuest: s.openGuestPort,
	}
	used := make(map[string]struct{})

	for _, pf := range forwards {
		for _, addr := range addresses {
			listenAddr := net.JoinHostPort(addr, strconv.Itoa(pf.LocalPort))
			if _, ok := used[listenAddr]; ok {
				_ = manager.Close()
				return nil, errx.With(ErrPortForwardBind, ": duplicate listen address %s", listenAddr)
			}
			used[listenAddr] = struct{}{}

			ln, err := net.Listen("tcp", listenAddr)
			if err != nil {
				_ = manager.Close()
				return nil, errx.With(ErrPortForwardBind, " %s: %w", listenAddr, err)
			}

			manager.listeners = append(manager.listeners, listenerSpec{
				listener:   ln,
				remotePort: pf.RemotePort,
			})

			tcpAddr, ok := ln.Addr().(*net.TCPAddr)
			if !ok {
				_ = manager.Close()
				return nil, errx.With(ErrPortForwardBind, ": unexpected listener address type %T", ln.Addr())
			}
			manager.bindings = append(manager.bindings, api.PortForwardBinding{
				Address:    tcpAddr.IP.String(),
				LocalPort:  tcpAddr.Port,
				RemotePort: pf.RemotePort,
			})
		}
	}

	for _, l := range manager.listeners {
		manager.wg.Add(1)
		go func(spec listenerSpec) {
			defer manager.wg.Done()
			manager.serveListener(pfCtx, spec)
		}(l)
	}

	return manager, nil
}

func (m *PortForwardManager) serveListener(ctx context.Context, spec listenerSpec) {
	for {
		conn, err := spec.listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}

		m.wg.Add(1)
		go func(c net.Conn, remotePort int) {
			defer m.wg.Done()
			m.proxyConn(ctx, c, remotePort)
		}(conn, spec.remotePort)
	}
}

func (m *PortForwardManager) proxyConn(ctx context.Context, clientConn net.Conn, remotePort int) {
	defer clientConn.Close()

	// Enable TCP keepalive on the host-side connection so idle WebSocket
	// connections are not dropped by the OS or intermediate proxies.
	if tc, ok := clientConn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(15 * time.Second)
	}

	dialCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	guestConn, err := m.dialGuest(remotePort)
	if err != nil {
		return
	}
	defer guestConn.Close()

	stop := context.AfterFunc(dialCtx, func() {
		_ = clientConn.Close()
		_ = guestConn.Close()
	})
	defer stop()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(guestConn, clientConn)
		// Do not propagate half-close onto guestConn. On Linux/Firecracker the
		// host-side UDS transport does not preserve TCP half-close semantics
		// reliably, which can cut off guest->host response data.
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(clientConn, guestConn)
		done <- struct{}{}
	}()

	select {
	case <-ctx.Done():
	case <-done:
		<-done
	}
}

func (s *Sandbox) openGuestPort(remotePort int) (net.Conn, error) {
	if remotePort < 1 || remotePort > 65535 {
		return nil, errx.With(ErrPortForwardInit, ": invalid remote port %d", remotePort)
	}

	dialer, ok := s.machine.(vm.VsockDialer)
	if !ok {
		return nil, ErrNoVsockDialer
	}

	conn, err := dialer.DialVsock(vsock.ServicePortExec)
	if err != nil {
		return nil, errx.Wrap(ErrPortForwardDial, err)
	}
	if err := vsock.OpenPortForward(conn, guestLoopbackAddress, uint16(remotePort)); err != nil {
		_ = conn.Close()
		return nil, errx.Wrap(ErrPortForwardInit, err)
	}
	return conn, nil
}

// PortForwardViaRelay proxies one local TCP connection through the run-process
// relay socket into the guest remote port.
func PortForwardViaRelay(ctx context.Context, relaySocketPath string, remotePort int, localConn net.Conn) error {
	relayConn, err := net.Dial("unix", relaySocketPath)
	if err != nil {
		return errx.With(ErrRelayConnect, " %s: %w", relaySocketPath, err)
	}
	defer relayConn.Close()

	reqData, err := json.Marshal(relayPortForwardRequest{RemotePort: remotePort})
	if err != nil {
		return errx.Wrap(ErrRelaySend, err)
	}
	if err := sendRelayMsg(relayConn, relayMsgPortForward, reqData); err != nil {
		return errx.Wrap(ErrRelaySend, err)
	}

	proxyCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(proxyCtx, func() {
		_ = relayConn.Close()
		_ = localConn.Close()
	})
	defer stop()

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, readErr := localConn.Read(buf)
			if n > 0 {
				if err := sendRelayMsg(relayConn, relayMsgStdin, buf[:n]); err != nil {
					cancel()
					return
				}
			}
			if readErr != nil {
				_ = sendRelayMsg(relayConn, relayMsgStdin, nil)
				if !errors.Is(readErr, io.EOF) {
					cancel()
				}
				return
			}
		}
	}()

	var remoteErr string
	for {
		msgType, data, err := readRelayMsg(relayConn)
		if err != nil {
			if proxyCtx.Err() != nil {
				return nil
			}
			return errx.Wrap(ErrRelayRead, err)
		}

		switch msgType {
		case relayMsgStdout:
			if _, err := localConn.Write(data); err != nil {
				return nil
			}
		case relayMsgStderr:
			remoteErr += string(data)
		case relayMsgExit:
			if len(data) >= 4 && binaryBigEndianUint32(data) != 0 {
				if remoteErr == "" {
					remoteErr = "port-forward failed"
				}
				return errx.With(ErrRelayProxy, ": %s", remoteErr)
			}
			return nil
		default:
			return errx.With(ErrRelayUnexpected, " message type %d", msgType)
		}
	}
}

func binaryBigEndianUint32(data []byte) uint32 {
	return uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
}
