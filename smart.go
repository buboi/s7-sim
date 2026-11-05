package main

// S7-200 SMART emulation.
//
// An S7-200 SMART CPU speaks the same S7 protocol as the 300/400/1200/1500,
// but differs in ways a collector must handle:
//   - it only accepts connection requests with remote TSAP 0x0301 (other COTP
//     CRs are refused with a disconnect);
//   - it negotiates a 240-byte PDU;
//   - its V memory is the only data block and is exposed as DB1;
//   - its I, Q and M areas are small (I/Q 128 bytes, M 32 bytes).

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultSmartVSize = 8192
	smartPDU          = 240
	smartIOSize       = 128 // I0.0..I127.7 and Q0.0..Q127.7
	smartMSize        = 32  // M0.0..M31.7
	smartVDB          = 1
)

type smartProfile struct {
	VSize     int
	TSAP      uint16
	CheckTSAP bool
	I, M      []byte
}

func newSmartProfile(vsize int, tsap string) (*smartProfile, error) {
	if vsize <= 0 || vsize > 65535 {
		return nil, fmt.Errorf("-smart-vsize must be 1..65535, got %d", vsize)
	}
	p := &smartProfile{VSize: vsize, I: make([]byte, smartIOSize), M: make([]byte, smartMSize)}
	tsap = strings.TrimSpace(tsap)
	if tsap != "" && !strings.EqualFold(tsap, "any") {
		v, err := strconv.ParseUint(tsap, 0, 16)
		if err != nil {
			return nil, fmt.Errorf("-smart-tsap %q: %v", tsap, err)
		}
		p.TSAP, p.CheckTSAP = uint16(v), true
	}
	return p, nil
}

func (p *smartProfile) tsapText() string {
	if !p.CheckTSAP {
		return "any"
	}
	return fmt.Sprintf("0x%04X", p.TSAP)
}

// applySmartLayout checks that the layout built from the config (and sample
// frames) fits an S7-200 SMART, then sizes the areas like the real CPU so
// out-of-range reads fail the same way they would on the PLC.
func (s *Simulation) applySmartLayout(p *smartProfile) error {
	var problems []string
	var other []int
	for db := range s.dbAreas {
		if db != smartVDB {
			other = append(other, db)
		}
	}
	if len(other) > 0 {
		sort.Ints(other)
		msg := fmt.Sprintf("uses DB %v, but V memory is the only data block (DB1)", other)
		if keys := s.sampleKeysOutsideV(); len(keys) > 0 {
			msg += fmt.Sprintf(" (sample frame keys: %s)", strings.Join(keys, ", "))
		}
		problems = append(problems, msg)
	}
	if need := len(s.dbAreas[smartVDB]); need > p.VSize {
		problems = append(problems, fmt.Sprintf("needs V memory up to byte %d, larger than -smart-vsize %d", need-1, p.VSize))
	}
	if need := len(s.qArea); need > smartIOSize {
		problems = append(problems, fmt.Sprintf("uses Q byte %d, but the Q area ends at Q%d", need-1, smartIOSize-1))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	s.ensureSize(smartVDB, p.VSize)
	s.ensureSizeArea("q", 0, smartIOSize)
	s.smart = p
	return nil
}

// sampleKeysOutsideV lists (up to 5) JSON frame state/word keys that address
// a DB other than DB1.
func (s *Simulation) sampleKeysOutsideV() []string {
	seen := map[string]bool{}
	var keys []string
	add := func(k string, area string, db int) {
		if area == "db" && db != smartVDB && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for _, f := range s.sample.Frames {
		for k := range f.States {
			if area, db, _, _, err := parseStateKey(k); err == nil {
				add(k, area, db)
			}
		}
		for k := range f.Words {
			if area, db, _, err := parseWordKey(k); err == nil {
				add(k, area, db)
			}
		}
	}
	sort.Strings(keys)
	if len(keys) > 5 {
		keys = append(keys[:5], "...")
	}
	return keys
}

// tsapProxy accepts S7 connections on the public address, checks the COTP
// connection request and forwards accepted connections to Snap7.
type tsapProxy struct {
	ln     net.Listener
	target string
	tsap   uint16
	wg     sync.WaitGroup
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

// startTSAPProxy listens on s.listen:s.port and returns the loopback address
// and port Snap7 must listen on.
func (s *Simulation) startTSAPProxy() (string, int, error) {
	inner, err := freeLoopbackPort()
	if err != nil {
		return "", 0, fmt.Errorf("pick internal port: %v", err)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(s.listen, strconv.Itoa(s.port)))
	if err != nil {
		return "", 0, err
	}
	p := &tsapProxy{
		ln:     ln,
		target: net.JoinHostPort("127.0.0.1", strconv.Itoa(inner)),
		tsap:   s.smart.TSAP,
		conns:  map[net.Conn]struct{}{},
	}
	s.proxy = p
	p.wg.Add(1)
	go p.serve()
	return "127.0.0.1", inner, nil
}

func (s *Simulation) stopProxy() {
	if s.proxy != nil {
		s.proxy.close()
		s.proxy = nil
	}
}

func freeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func (p *tsapProxy) serve() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			if !p.isClosed() {
				log.Printf("smart proxy accept: %v", err)
			}
			return
		}
		p.wg.Add(1)
		go p.handle(c)
	}
}

func (p *tsapProxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *tsapProxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
	c.Close()
}

func (p *tsapProxy) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

func (p *tsapProxy) close() {
	p.mu.Lock()
	p.closed = true
	for c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
	p.ln.Close()
	p.wg.Wait()
}

func (p *tsapProxy) handle(client net.Conn) {
	defer p.wg.Done()
	if !p.track(client) {
		client.Close()
		return
	}
	defer p.untrack(client)

	client.SetReadDeadline(time.Now().Add(10 * time.Second))
	cr, err := readTPKT(client)
	if err != nil {
		return
	}
	client.SetReadDeadline(time.Time{})
	remote, ok := cotpRemoteTSAP(cr)
	if !ok {
		log.Printf("smart: rejected %s: first packet is not a COTP connection request", client.RemoteAddr())
		return
	}
	if remote != p.tsap {
		log.Printf("smart: rejected %s: remote TSAP 0x%04X (an S7-200 SMART only accepts 0x%04X; use protocol=s7smart)",
			client.RemoteAddr(), remote, p.tsap)
		return
	}

	server, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		log.Printf("smart proxy: dial snap7: %v", err)
		return
	}
	if !p.track(server) {
		server.Close()
		return
	}
	defer p.untrack(server)
	if _, err := server.Write(cr); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		io.Copy(dst, src)
		dst.Close()
		src.Close()
		done <- struct{}{}
	}
	go pipe(server, client)
	go pipe(client, server)
	<-done
	<-done
}

// readTPKT reads one RFC1006 TPKT packet (4-byte header + payload).
func readTPKT(r io.Reader) ([]byte, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	if hdr[0] != 3 {
		return nil, fmt.Errorf("not a TPKT packet")
	}
	n := int(binary.BigEndian.Uint16(hdr[2:]))
	if n < 7 || n > 4096 {
		return nil, fmt.Errorf("bad TPKT length %d", n)
	}
	pkt := make([]byte, n)
	copy(pkt, hdr)
	if _, err := io.ReadFull(r, pkt[4:]); err != nil {
		return nil, err
	}
	return pkt, nil
}

// cotpRemoteTSAP returns the called (destination) TSAP of a COTP connection
// request wrapped in a TPKT packet.
func cotpRemoteTSAP(pkt []byte) (uint16, bool) {
	if len(pkt) < 11 || pkt[5]&0xF0 != 0xE0 { // CR
		return 0, false
	}
	end := min(5+int(pkt[4]), len(pkt)) // LI counts the bytes after itself
	for i := 11; i+2 <= end; {
		code, l := pkt[i], int(pkt[i+1])
		if i+2+l > end {
			break
		}
		if code == 0xC2 && l == 2 {
			return binary.BigEndian.Uint16(pkt[i+2:]), true
		}
		i += 2 + l
	}
	return 0, false
}
