package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cotpCR builds a TPKT-wrapped COTP connection request like gos7 sends.
func cotpCR(local, remote uint16) []byte {
	return []byte{
		3, 0, 0, 22, // TPKT
		17, 0xE0, 0, 0, 0, 1, 0, // COTP CR header
		0xC0, 1, 0x0A, // TPDU size
		0xC1, 2, byte(local >> 8), byte(local),
		0xC2, 2, byte(remote >> 8), byte(remote),
	}
}

func TestCOTPRemoteTSAP(t *testing.T) {
	if got, ok := cotpRemoteTSAP(cotpCR(0x0100, 0x0301)); !ok || got != 0x0301 {
		t.Fatalf("got 0x%04X %v, want 0x0301", got, ok)
	}
	cc := cotpCR(0x0100, 0x0301)
	cc[5] = 0xD0 // connection confirm, not a request
	if _, ok := cotpRemoteTSAP(cc); ok {
		t.Fatal("CC packet accepted as CR")
	}
	if _, ok := cotpRemoteTSAP(cotpCR(0x0100, 0x0301)[:18]); ok {
		t.Fatal("truncated CR accepted")
	}
}

func TestSmartProfileFlags(t *testing.T) {
	p, err := newSmartProfile(8192, "0x0301")
	if err != nil || !p.CheckTSAP || p.TSAP != 0x0301 {
		t.Fatalf("got %+v, %v", p, err)
	}
	if p, _ = newSmartProfile(8192, "any"); p.CheckTSAP {
		t.Fatal("any should disable the TSAP check")
	}
	if _, err = newSmartProfile(0, "0x0301"); err == nil {
		t.Fatal("vsize 0 accepted")
	}
	if _, err = newSmartProfile(8192, "zz"); err == nil {
		t.Fatal("bad TSAP accepted")
	}
}

func loadCSV(t *testing.T, text string) Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg.csv")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

const smartCSV = `[server],1,s7smart,127.0.0.1|0|1|102,0,1000
[state-trigger],1,1|3000,dbx,0,1,1,,0,
[state-dosing],1,1,Q,1,1,1,,0,色浆,1|1130,dbw,ml,1,U16
[point],1,游离碱,g/L,1,1|0,dbw,100,U16,1|1002,1|1004,1|1006,1
[alarm],1,1|2013,dbx,0,1,EC过高
`

func TestSmartLayout(t *testing.T) {
	cfg := loadCSV(t, smartCSV)
	if cfg.Server.Protocol != "s7smart" {
		t.Fatalf("protocol %q", cfg.Server.Protocol)
	}
	p, _ := newSmartProfile(4096, "0x0301")
	sim := newSimulation(cfg, SampleData{}, "", 0)
	if err := sim.applySmartLayout(p); err != nil {
		t.Fatal(err)
	}
	if len(sim.dbAreas) != 1 || len(sim.dbAreas[1]) != 4096 || len(sim.qArea) != smartIOSize {
		t.Fatalf("layout: dbs=%v V=%d Q=%d", sim.dbNumbers(), len(sim.dbAreas[1]), len(sim.qArea))
	}

	// The full Q area exists even when the config uses no Q bits.
	noQ := loadCSV(t, "[server],1,s7smart,127.0.0.1|0|1|102,0,1000\n[point],1,a,g/L,1,1|0,dbw,1,U16,1|10,1|12,1|14,1\n")
	sim = newSimulation(noQ, SampleData{}, "", 0)
	if err := sim.applySmartLayout(p); err != nil || len(sim.qArea) != smartIOSize {
		t.Fatalf("no-Q config: Q=%d err=%v", len(sim.qArea), err)
	}

	// Too small V memory.
	p, _ = newSmartProfile(2000, "0x0301")
	if err := newSimulation(cfg, SampleData{}, "", 0).applySmartLayout(p); err == nil || !strings.Contains(err.Error(), "smart-vsize") {
		t.Fatalf("want V size error, got %v", err)
	}

	// DB other than DB1, from the config and from sample frames.
	other := loadCSV(t, strings.Replace(smartCSV, "1|2013", "30|13", 1))
	sample := SampleData{Frames: []Frame{{States: map[string]int{"31:0:0": 1}}}}
	p, _ = newSmartProfile(8192, "0x0301")
	err := newSimulation(other, sample, "", 0).applySmartLayout(p)
	if err == nil || !strings.Contains(err.Error(), "DB [30 31]") || !strings.Contains(err.Error(), "31:0:0") {
		t.Fatalf("want DB error, got %v", err)
	}
}

func TestSmartTSAPProxy(t *testing.T) {
	// A fake "snap7" backend that echoes whatever it receives.
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 64)
				n, _ := c.Read(buf)
				c.Write(buf[:n])
				c.Close()
			}()
		}
	}()
	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &tsapProxy{ln: front, target: backend.Addr().String(), tsap: 0x0301, conns: map[net.Conn]struct{}{}}
	p.wg.Add(1)
	go p.serve()
	defer p.close()

	try := func(remote uint16) (int, error) {
		c, err := net.Dial("tcp", front.Addr().String())
		if err != nil {
			return 0, err
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(2 * time.Second))
		c.Write(cotpCR(0x0100, remote))
		buf := make([]byte, 64)
		return c.Read(buf)
	}
	if n, err := try(0x0301); err != nil || n != 22 {
		t.Fatalf("0x0301: n=%d err=%v, want the CR forwarded", n, err)
	}
	if n, err := try(0x0102); err == nil || n != 0 {
		t.Fatalf("0x0102: n=%d err=%v, want the connection closed", n, err)
	}
}
