//go:build darwin

package net

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// ---------- helpers ----------

func socketpairFiles(b *testing.B) (*os.File, *os.File) {
	b.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		b.Fatal(err)
	}
	syscall.SetNonblock(fds[0], true)
	syscall.SetNonblock(fds[1], true)
	return os.NewFile(uintptr(fds[0]), "a"), os.NewFile(uintptr(fds[1]), "b")
}

func makeEthFrame(payloadSize int) []byte {
	frame := make([]byte, header.EthernetMinimumSize+payloadSize)
	eth := header.Ethernet(frame)
	eth.Encode(&header.EthernetFields{
		SrcAddr: tcpip.LinkAddress([]byte{0x02, 0, 0, 0, 0, 1}),
		DstAddr: tcpip.LinkAddress([]byte{0x02, 0, 0, 0, 0, 2}),
		Type:    header.IPv4ProtocolNumber,
	})
	return frame
}

func sizeLabel(n int) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%dMB", n/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%dKB", n/1024)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// ---------- Benchmark: per-packet allocation in read path ----------

// BenchmarkReadPathAlloc measures the cost of the per-packet make+copy
// that is required because gVisor buffer chunks outlive PacketBuffers.
func BenchmarkReadPathAlloc(b *testing.B) {
	for _, size := range []int{64, 512, 1514} {
		b.Run(sizeLabel(size), func(b *testing.B) {
			src := make([]byte, size)
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				data := make([]byte, size)
				copy(data, src)
				buf := buffer.MakeWithData(data)
				pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buf})
				pkt.DecRef()
			}
		})
	}
}

// ---------- Benchmark: write buffer pool ----------

// BenchmarkWriteBufPool measures write-path serialization with the pool
// vs a fresh allocation each time.
func BenchmarkWriteBufPool(b *testing.B) {
	frame := makeEthFrame(1460)

	b.Run("pool", func(b *testing.B) {
		b.SetBytes(int64(len(frame)))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			bp := writeBufPool.Get().(*[]byte)
			wb := (*bp)[:0]
			wb = append(wb, frame...)
			*bp = wb
			writeBufPool.Put(bp)
		}
	})

	b.Run("alloc", func(b *testing.B) {
		b.SetBytes(int64(len(frame)))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			wb := make([]byte, len(frame))
			copy(wb, frame)
			_ = wb
		}
	})
}

// ---------- Benchmark: socketpair raw write+read ----------

// BenchmarkSocketPairRoundtrip measures the raw syscall overhead of writing
// one Ethernet frame through the socketpair and reading it back.
func BenchmarkSocketPairRoundtrip(b *testing.B) {
	a, bFile := socketpairFiles(b)
	defer a.Close()
	defer bFile.Close()

	setSocketBufferSizes(a)
	setSocketBufferSizes(bFile)

	frame := makeEthFrame(1460)
	readBuf := make([]byte, 2048)

	b.SetBytes(int64(len(frame)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.Write(frame)
		bFile.Read(readBuf)
	}
}

// ---------- Benchmark: full gVisor stack TCP throughput ----------

// BenchmarkGvisorTCPThroughput creates a full gVisor NetworkStack over a
// socketpair and measures bidirectional TCP throughput through it.
func BenchmarkGvisorTCPThroughput(b *testing.B) {
	for _, payloadKB := range []int{1, 16, 64} {
		b.Run(sizeLabel(payloadKB*1024), func(b *testing.B) {
			benchTCPThroughput(b, payloadKB*1024)
		})
	}
}

func benchTCPThroughput(b *testing.B, chunkSize int) {
	b.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go io.Copy(c, c)
		}
	}()

	echoAddr := ln.Addr().(*net.TCPAddr)

	// Empty AllowedHosts = allow all traffic.
	pol := policy.NewEngine(&api.NetworkConfig{}, nil, nil)
	events := make(chan api.Event, 100)

	a, bFile := socketpairFiles(b)
	defer a.Close()
	defer bFile.Close()

	ns, err := NewNetworkStack(&Config{
		File:      a,
		GatewayIP: "192.168.100.1",
		GuestIP:   "192.168.100.2",
		MTU:       1500,
		Policy:    pol,
		Events:    events,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer ns.Close()

	guestConn, err := gonet.DialTCPWithBind(
		context.Background(),
		ns.stack,
		tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(net.ParseIP("192.168.100.2").To4())},
		tcpip.FullAddress{Addr: tcpip.AddrFromSlice(echoAddr.IP.To4()), Port: uint16(echoAddr.Port)},
		header.IPv4ProtocolNumber,
	)
	if err != nil {
		b.Skipf("cannot dial through gVisor stack (expected in passthrough mode): %v", err)
	}
	defer guestConn.Close()

	payload := make([]byte, chunkSize)
	readBuf := make([]byte, chunkSize)

	b.SetBytes(int64(chunkSize) * 2)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := guestConn.Write(payload); err != nil {
			b.Fatal(err)
		}
		if _, err := io.ReadFull(guestConn, readBuf); err != nil {
			b.Fatal(err)
		}
	}
}

// ---------- Benchmark: connection pool ----------

func BenchmarkConnPool(b *testing.B) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				io.Copy(io.Discard, c)
				c.Close()
			}()
		}
	}()

	addr := ln.Addr().String()

	b.Run("with_pool", func(b *testing.B) {
		pool := newUpstreamConnPool()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			pc := pool.get(addr)
			if pc == nil {
				c, err := net.DialTimeout("tcp", addr, 5*time.Second)
				if err != nil {
					b.Fatal(err)
				}
				pc = &pooledConn{conn: c}
			}
			pool.put(addr, pc)
		}
	})

	b.Run("no_pool", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			c, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				// Ephemeral port exhaustion is expected under sustained load.
				b.Skipf("port exhaustion after %d dials: %v", i, err)
			}
			// Set SO_LINGER=0 to force RST and immediate port release.
			if tc, ok := c.(*net.TCPConn); ok {
				tc.SetLinger(0)
			}
			c.Close()
		}
	})
}

// ---------- Benchmark: copyWithCancel relay ----------

func BenchmarkCopyRelay(b *testing.B) {
	for _, size := range []int{1024, 32 * 1024, 128 * 1024} {
		b.Run(sizeLabel(size), func(b *testing.B) {
			benchCopyRelay(b, size)
		})
	}
}

func benchCopyRelay(b *testing.B, chunkSize int) {
	clientR, clientW := net.Pipe()
	serverR, serverW := net.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go copyWithCancel(ctx, serverW, clientR)

	payload := make([]byte, chunkSize)
	readBuf := make([]byte, chunkSize)

	var wg sync.WaitGroup

	b.SetBytes(int64(chunkSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			io.ReadFull(serverR, readBuf)
		}()
		clientW.Write(payload)
		wg.Wait()
	}

	cancel()
	clientR.Close()
	clientW.Close()
	serverR.Close()
	serverW.Close()
}
