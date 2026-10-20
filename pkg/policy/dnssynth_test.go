package policy

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"
)

// policySyntheticDNS is a process-local DNS server used only in tests. It answers
// A (and, when an AAAA table is configured, AAAA) queries from a configurable
// table and returns no answers for any other qtype. Each name maps to an ordered
// list of answer "stages"; the answer returned for the N-th A query of that name
// is stage[(N-1) % len(stages)]. This can emulate a mixed public/private answer
// set or a DNS rebinding change across lookups.
//
// Installation replaces net.DefaultResolver (PreferGo + custom Dial) in-process
// only; it does not touch /etc/hosts or the host DNS configuration.
type policySyntheticDNS struct {
	mu      sync.Mutex
	ln      *net.UDPConn
	stages  map[string][][]string
	aqCount map[string]int

	// stagesAAAA is the optional IPv6 counterpart of stages (populated by
	// newPolicySyntheticDNSWithAAAA), so a test can model a host reachable only
	// over IPv6 or a dual-stack host.
	stagesAAAA map[string][][]string
	aq6Count   map[string]int

	prevResolver *net.Resolver
}

func newPolicySyntheticDNS(t *testing.T, table map[string][][]string) *policySyntheticDNS {
	t.Helper()
	ln, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("newPolicySyntheticDNS: %v", err)
	}
	s := &policySyntheticDNS{
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

// newPolicySyntheticDNSWithAAAA is newPolicySyntheticDNS with an extra AAAA
// table, so a name listed there resolves to an IPv6 address set (and a name
// absent from the A table resolves to nothing over IPv4). A name absent from the
// AAAA table keeps answering AAAA with an empty answer, so existing A-only tests
// are unaffected.
func newPolicySyntheticDNSWithAAAA(t *testing.T, a, aaaa map[string][][]string) *policySyntheticDNS {
	t.Helper()

	s := newPolicySyntheticDNS(t, a)
	s.mu.Lock()
	s.stagesAAAA = aaaa
	s.mu.Unlock()
	return s
}

// aAAAACount returns how many AAAA queries have been received for name.
func (s *policySyntheticDNS) aAAAACount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aq6Count[name]
}

func (s *policySyntheticDNS) install() {
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

func (s *policySyntheticDNS) uninstall() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prevResolver != nil {
		net.DefaultResolver = s.prevResolver
		s.prevResolver = nil
	}
}

func (s *policySyntheticDNS) aCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aqCount[name]
}

func (s *policySyntheticDNS) serve() {
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

func (s *policySyntheticDNS) respond(q []byte) []byte {
	name, qtype := parseQuery(q)
	if qtype != 1 && qtype != 28 {
		return buildEmptyAnswer(q)
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

func parseQuery(q []byte) (string, uint16) {
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
	return name, binary.BigEndian.Uint16(q[nameEnd : nameEnd+2])
}

func buildEmptyAnswer(q []byte) []byte {
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
	resp = append(resp, 0x81, 0x80)
	resp = append(resp, 0, 1)
	resp = append(resp, 0, 0)
	resp = append(resp, 0, 0, 0, 0)
	resp = append(resp, q[12:answerPtr]...)
	return resp
}

// buildAddressAnswer encodes one DNS answer per address, with the record length
// taken from the address itself so A (4-byte) and AAAA (16-byte) answers share
// the encoder. qtype is the question's type; the caller is responsible for
// handing in addresses of the matching family.
func buildAddressAnswer(q []byte, qtype uint16, answers []net.IP) []byte {
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
	var qclass uint16
	if nameEnd+4 <= len(q) {
		qclass = binary.BigEndian.Uint16(q[nameEnd+2 : nameEnd+4])
	}

	resp := make([]byte, 0, len(q)+16*len(answers))
	resp = append(resp, q[0:2]...)
	resp = append(resp, 0x81, 0x80)
	resp = append(resp, 0, 1)
	resp = append(resp, 0, byte(len(answers)))
	resp = append(resp, 0, 0, 0, 0)
	resp = append(resp, q[12:answerPtr]...)
	for _, ip := range answers {
		addr := ip
		if len(addr) != 4 && len(addr) != 16 {
			continue
		}
		resp = append(resp, 0xC0, 0x0C)
		resp = append(resp, 0, byte(qtype))
		resp = append(resp, 0, byte(qclass))
		resp = append(resp, 0, 0, 0, 60)
		resp = append(resp, 0, byte(len(addr)))
		resp = append(resp, addr...)
	}
	return resp
}
