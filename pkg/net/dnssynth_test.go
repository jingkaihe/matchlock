//go:build linux

package net

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// syntheticDNS is a process-local DNS server used only in tests. It answers A
// queries (and returns no answers for any other qtype) from a configurable
// table. For each name it holds an ordered list of "stages"; the answer returned
// for the N-th A query of that name is stage[(N-1) % len(stages)]. This lets a
// test simulate a mixed public/private answer set (a single stage with both) or
// a DNS rebinding change between a policy check and a dial (one stage returning
// a public address, a later stage returning a private one).
//
// It is installed by replacing net.DefaultResolver (PreferGo + custom Dial), a
// process-local change that does NOT touch /etc/hosts or the host DNS config.
type syntheticDNS struct {
	mu      sync.Mutex
	ln      *net.UDPConn
	stages  map[string][][]string // name -> ordered A answer stages
	aqCount map[string]int        // per-name A query count

	// stagesAAAA is the optional IPv6 counterpart of stages (populated by
	// newSyntheticDNSWithAAAA), so a test can model a dual-stack host.
	stagesAAAA map[string][][]string
	aq6Count   map[string]int

	prevResolver *net.Resolver
}

type syntheticDNSConfig map[string][][]string

func newSyntheticDNS(t *testing.T, table syntheticDNSConfig) *syntheticDNS {
	t.Helper()
	ln, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	require.NoError(t, err)
	s := &syntheticDNS{
		ln:       ln,
		stages:   table,
		aqCount:  make(map[string]int),
		aq6Count: make(map[string]int),
	}
	go s.serve()
	t.Cleanup(func() {
		s.uninstall()
		_ = ln.Close()
	})
	return s
}

// install routes all in-process hostname resolution through this server. It
// saves the previous resolver so uninstall can restore it.
func (s *syntheticDNS) install() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prevResolver != nil {
		return
	}
	s.prevResolver = net.DefaultResolver
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return net.Dial("udp", s.ln.LocalAddr().String())
		},
	}
}

func (s *syntheticDNS) uninstall() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prevResolver != nil {
		net.DefaultResolver = s.prevResolver
		s.prevResolver = nil
	}
}

// aCount returns how many A queries have been received for name.
func (s *syntheticDNS) aCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aqCount[name]
}

// newSyntheticDNSWithAAAA is newSyntheticDNS with an extra AAAA table, so a name
// listed in both resolves to a mixed (dual-stack) address set. Names absent from
// the AAAA table keep answering AAAA with an empty answer.
func newSyntheticDNSWithAAAA(t *testing.T, a syntheticDNSConfig, aaaa syntheticDNSConfig) *syntheticDNS {
	t.Helper()

	s := newSyntheticDNS(t, a)
	s.stagesAAAA = aaaa
	return s
}

// aAAAACount returns how many AAAA queries have been received for name.
func (s *syntheticDNS) aAAAACount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aq6Count[name]
}

func (s *syntheticDNS) serve() {
	buf := make([]byte, 512)
	for {
		n, addr, err := s.ln.ReadFromUDP(buf)
		if err != nil {
			return
		}
		resp := s.respond(buf[:n])
		if resp != nil {
			_, _ = s.ln.WriteToUDP(resp, addr)
		}
	}
}

func (s *syntheticDNS) respond(q []byte) []byte {
	name, qtype := parseDNSQuery(q)
	if qtype != 1 && qtype != 28 {
		return buildEmptyDNSAnswer(q) // Only A/AAAA are modelled; others get an empty answer.
	}

	s.mu.Lock()
	var stage []string
	if qtype == 1 {
		s.aqCount[name]++
		if list := s.stages[name]; len(list) > 0 {
			stage = list[(s.aqCount[name]-1)%len(list)]
		}
	} else {
		s.aq6Count[name]++
		if list := s.stagesAAAA[name]; len(list) > 0 {
			stage = list[(s.aq6Count[name]-1)%len(list)]
		}
	}
	s.mu.Unlock()

	answers := make([]net.IP, 0, len(stage))
	for _, raw := range stage {
		ip := net.ParseIP(raw)
		if ip == nil {
			continue
		}
		if qtype == 1 {
			ip = ip.To4()
		} else {
			ip = ip.To16()
		}
		if ip == nil {
			continue
		}
		answers = append(answers, ip)
	}

	return buildAddressAnswer(q, qtype, answers)
}

// parseDNSQuery extracts the question name (with trailing dot) and qtype.
func parseDNSQuery(q []byte) (string, uint16) {
	if len(q) < 12 {
		return "", 0
	}
	nameEnd := 12
	var name string
	for {
		if nameEnd >= len(q) {
			return "", 0
		}
		l := int(q[nameEnd])
		if l == 0 {
			nameEnd++
			break
		}
		if nameEnd+1+l > len(q) {
			return "", 0
		}
		name += string(q[nameEnd+1:nameEnd+1+l]) + "."
		nameEnd += l + 1
	}
	if nameEnd+4 > len(q) {
		return name, 0
	}
	qtype := binary.BigEndian.Uint16(q[nameEnd : nameEnd+2])
	return name, qtype
}

func buildEmptyDNSAnswer(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	nameEnd := 12
	for {
		if nameEnd >= len(q) {
			return nil
		}
		if q[nameEnd] == 0 {
			nameEnd++
			break
		}
		nameEnd += int(q[nameEnd]) + 1
	}
	answerPtr := nameEnd + 4
	if answerPtr > len(q) {
		return nil
	}
	resp := make([]byte, 0, len(q)+12)
	resp = append(resp, q[0:2]...)
	resp = append(resp, 0x81, 0x80)         // response, recursion desired+available
	resp = append(resp, 0, 1)               // qdcount
	resp = append(resp, 0, 0)               // ancount
	resp = append(resp, 0, 0, 0, 0)         // nscount, arcount
	resp = append(resp, q[12:answerPtr]...) // question
	return resp
}

// buildAddressAnswer encodes one DNS answer per address. The record length is
// taken from the address itself, so A (4 bytes) and AAAA (16 bytes) answers
// share the encoder.
func buildAddressAnswer(q []byte, qtype uint16, answers []net.IP) []byte {
	if len(q) < 12 {
		return nil
	}
	// Recompute the question section length to slice it back out.
	nameEnd := 12
	for {
		if nameEnd >= len(q) {
			return nil
		}
		if q[nameEnd] == 0 {
			nameEnd++
			break
		}
		nameEnd += int(q[nameEnd]) + 1
	}
	answerPtr := nameEnd + 4
	if answerPtr > len(q) {
		return nil
	}
	// qclass
	var qclass uint16
	if nameEnd+4 <= len(q) {
		qclass = binary.BigEndian.Uint16(q[nameEnd+2 : nameEnd+4])
	}

	resp := make([]byte, 0, len(q)+16*len(answers))
	resp = append(resp, q[0:2]...)
	resp = append(resp, 0x81, 0x80)
	resp = append(resp, 0, 1)                  // qdcount
	resp = append(resp, 0, byte(len(answers))) // ancount
	resp = append(resp, 0, 0, 0, 0)            // nscount, arcount
	resp = append(resp, q[12:answerPtr]...)    // question
	for _, ip := range answers {
		addr := ip.To4()
		if addr == nil {
			addr = ip.To16()
		}
		resp = append(resp, 0xC0, 0x0C) // compressed name pointer
		resp = append(resp, 0, byte(qtype))
		resp = append(resp, 0, byte(qclass))
		resp = append(resp, 0, 0, 0, 60)        // ttl 60s
		resp = append(resp, 0, byte(len(addr))) // rdlength
		resp = append(resp, addr...)
	}
	return resp
}
