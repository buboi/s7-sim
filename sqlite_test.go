package main

import (
	"database/sql"
	"encoding/binary"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testCSV = `[server],1,s7,127.0.0.1|0|1|102,0,1000
[state-trigger],1,31|0,dbx,0,1,1,,0,
[state-dosing],1,1,Q,1,1,1,,0,色浆,20|130,dbw,ml,1,U16
[point],1,游离碱,g/L,1,10|0,dbw,100,U16,20|2,20|4,20|6,1,,,,,,20|60,20|62
[point],2,EC,uS/cm,1,10|18,dbw,1,U16,20|20,20|22,20|24,1
[alarm],1,30|13,dbx,0,1,硅烷槽EC值过高
`

func writeTestConfig(t *testing.T) Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg.csv")
	if err := os.WriteFile(p, []byte(testCSV), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// writeTestDB builds a DB with the collector's schema.
func writeTestDB(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rec.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE dynamic_data_1 (id INTEGER PRIMARY KEY AUTOINCREMENT, timestamp TEXT NOT NULL, "1-dbw-10|0" REAL, "1-dbw-99|0" REAL)`,
		`CREATE TABLE dynamic_data_2 (id INTEGER PRIMARY KEY AUTOINCREMENT, timestamp TEXT NOT NULL, "2-dbw-10:18" REAL)`,
		`CREATE TABLE settings_history (timestamp TEXT NOT NULL, data_address TEXT NOT NULL, cv REAL, ha REAL, la REAL, hc REAL, lc REAL, PRIMARY KEY (timestamp, data_address))`,
		`CREATE TABLE alarms (id INTEGER PRIMARY KEY AUTOINCREMENT, timestamp TEXT NOT NULL, address TEXT NOT NULL, bitfield INTEGER, message TEXT)`,
		`CREATE TABLE dosing_records (id INTEGER PRIMARY KEY AUTOINCREMENT, timestamp TEXT NOT NULL, tank_id INTEGER NOT NULL, address TEXT NOT NULL, dosage REAL NOT NULL, bitfield INTEGER, name TEXT, unit TEXT)`,
		// Rows deliberately inserted out of order across tables.
		`INSERT INTO dynamic_data_1 (timestamp, "1-dbw-10|0", "1-dbw-99|0") VALUES ('2026-07-16T17:00:00.000+08:00', 2.4, 1), ('2026-07-16T17:00:04.000+08:00', NULL, 1), ('2026-07-16T17:00:08.000+08:00', 1.1, 1)`,
		`INSERT INTO dynamic_data_2 (timestamp, "2-dbw-10:18") VALUES ('2026-07-16T17:00:04.000+08:00', 950)`,
		`INSERT INTO settings_history (timestamp, data_address, cv, ha, la, hc, lc) VALUES ('2026-07-16T17:00:00.000+08:00', '1-dbw-10|0', 2.4, 3.5, 1.0, 3.0, 1.5)`,
		`INSERT INTO alarms (timestamp, address, bitfield, message) VALUES ('2026-07-16T17:00:02.000+08:00', 'dbx-30|13', 0, ''), ('2026-07-16T17:00:02.000+08:00', 'dbx-30|13', 0, ''), ('2026-07-16T17:00:05.000+08:00', 'dbx-30|13', 0, '')`,
		`INSERT INTO dosing_records (timestamp, tank_id, address, dosage, bitfield, name) VALUES ('2026-07-16T17:00:04.000+08:00', 1, 'Q-1', 30, 1, '色浆')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return p
}

func TestParseKeys(t *testing.T) {
	cases := []struct{ in, tank, key string }{
		{"1-dbw-10|0", "1", "dbw-10|0"},
		{"1-DBW-10:0", "1", "dbw-10|0"},
		{"12-dbd-900|20", "12", "dbd-900|20"},
	}
	for _, c := range cases {
		tank, key, err := parseColumnKey(c.in)
		if err != nil || key != c.key || strconv.Itoa(tank) != c.tank {
			t.Errorf("parseColumnKey(%q) = %d %q %v", c.in, tank, key, err)
		}
	}
	if k, err := parseAddressKey("Q-1"); err != nil || k != "q-1" {
		t.Errorf("parseAddressKey(Q-1) = %q %v", k, err)
	}
	if _, _, err := parseColumnKey("garbage"); err == nil {
		t.Error("expected error for unparsable column")
	}
}

func TestConfigQAndDosingLinks(t *testing.T) {
	cfg := writeTestConfig(t)
	var dose DosingDef
	for _, d := range cfg.Dosing {
		dose = d
	}
	if dose.Trigger == nil || dose.Trigger.Area != "q" || dose.Trigger.ByteOffset != 1 || dose.Trigger.Bit != 1 {
		t.Fatalf("dosing trigger = %+v", dose.Trigger)
	}
	if _, ok := cfg.doseByName["1:色浆"]; !ok {
		t.Fatal("dosing not indexed by tank:name")
	}
	var trig StateDef
	for _, st := range cfg.States {
		if st.Kind == "trigger" {
			trig = st
		}
	}
	if trig.TankID != 1 || trig.DB != 31 {
		t.Fatalf("trigger = %+v", trig)
	}
}

func TestMapping(t *testing.T) {
	cfg := writeTestConfig(t)
	m := newDBMapping(&cfg, &dbMapFile{})
	if p, ok := m.point("1-dbw-10|0"); !ok || p.def.Key != "1:游离碱" {
		t.Fatalf("point map = %+v %v", p, ok)
	}
	if p, ok := m.point("2-dbw-10:18"); !ok || p.def.Key != "2:EC" {
		t.Fatalf("colon form not normalised: %+v %v", p, ok)
	}
	if _, ok := m.point("1-dbw-99|0"); ok {
		t.Fatal("expected unmapped column")
	}
	if a, ok := m.alarm("dbx-30|13", sql.NullInt64{Valid: true}, ""); !ok || a.Message != "硅烷槽EC值过高" {
		t.Fatalf("alarm by address = %+v %v", a, ok)
	}
	if _, ok := m.alarm("dbx-77|0", sql.NullInt64{Valid: true}, "硅烷槽EC值过高"); !ok {
		t.Fatal("alarm fallback by message failed")
	}
	if d, ok := m.dosing(1, "Q-1", sql.NullInt64{Int64: 1, Valid: true}, "renamed"); !ok || d.Name != "色浆" {
		t.Fatalf("dosing fallback by address = %+v %v", d, ok)
	}
	over := &dbMapFile{Points: map[string]string{"1-dbw-99|0": "2:EC"}, Scale: map[string]float64{"1-dbw-99|0": 2}}
	m2 := newDBMapping(&cfg, over)
	if p, ok := m2.point("1-dbw-99|0"); !ok || p.def.Key != "2:EC" || p.scale != 2 || p.how != "map" {
		t.Fatalf("override = %+v %v", p, ok)
	}
}

type emitted struct {
	at time.Duration
	f  Frame
}

func runSource(t *testing.T, opt replayOptions) []emitted {
	t.Helper()
	cfg := writeTestConfig(t)
	path := writeTestDB(t)
	src, err := openSQLiteSource(path, "", &cfg, opt, false)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	var out []emitted
	var at time.Duration
	for i := 0; i < 100; i++ {
		f, wait, err := src.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, emitted{at, f})
		at += wait
	}
	t.Fatal("source did not terminate")
	return nil
}

func defaultTestOpt() replayOptions {
	return replayOptions{Realtime: true, Speed: 1, Interval: time.Second, MaxGap: 10 * time.Second,
		Pulse: true, PulseLead: time.Second, AlarmHold: 5 * time.Second}
}

func TestReplayTimeline(t *testing.T) {
	out := runSource(t, defaultTestOpt())
	find := func(pred func(Frame) bool) []time.Duration {
		var ts []time.Duration
		for _, e := range out {
			if pred(e.f) {
				ts = append(ts, e.at)
			}
		}
		return ts
	}
	if len(out) == 0 || out[0].at != 0 || out[0].f.States["31:0:0"] != 0 || out[0].f.States["Q:1:1"] != 0 {
		t.Fatalf("first frame should reset trigger/dosing bits: %+v", out[0])
	}
	// Source t0=17:00:00 -> v=2s. Trigger high at 1s with values, low at 2s.
	high := find(func(f Frame) bool { v, ok := f.States["31:0:0"]; return ok && v == 1 })
	low := find(func(f Frame) bool { v, ok := f.States["31:0:0"]; return ok && v == 0 })
	want := []time.Duration{time.Second, 5 * time.Second, 9 * time.Second}
	if !equalDur(high, want) {
		t.Errorf("trigger high at %v, want %v", high, want)
	}
	if !equalDur(low, []time.Duration{0, 2 * time.Second, 6 * time.Second, 10 * time.Second}) {
		t.Errorf("trigger low at %v", low)
	}
	// Values are present in the high frame; the NULL row keeps the old value (no point written).
	for _, e := range out {
		if e.at == time.Second {
			if e.f.Points["1:游离碱"] != 2.4 || e.f.CV["1:游离碱"] != 2.4 || e.f.LA["1:游离碱"] != 1.0 || e.f.HA["1:游离碱"] != 3.5 ||
				e.f.LC["1:游离碱"] != 1.5 || e.f.HC["1:游离碱"] != 3.0 {
				t.Errorf("frame at 1s = %+v", e.f)
			}
		}
		if e.at == 5*time.Second {
			if _, ok := e.f.Points["1:游离碱"]; ok {
				t.Errorf("NULL value should not be written: %+v", e.f.Points)
			}
		}
	}
	// Tank 2 has no trigger: value written at the step time (17:00:04 -> 6s).
	if ts := find(func(f Frame) bool { _, ok := f.Points["2:EC"]; return ok }); !equalDur(ts, []time.Duration{6 * time.Second}) {
		t.Errorf("tank 2 value at %v", ts)
	}
	// Dosing: amount + Q high at 5s, Q low at 6s.
	if ts := find(func(f Frame) bool { return f.Dosing["1:色浆"] == 30 && f.States["Q:1:1"] == 1 }); !equalDur(ts, []time.Duration{5 * time.Second}) {
		t.Errorf("dose high at %v", ts)
	}
	// Alarms: raised at 4s (17:00:02, deduplicated) and 7s (17:00:05);
	// the first is cleared before the second raise (midpoint 5.5s), the second after 5s hold.
	on := find(func(f Frame) bool { v, ok := f.States["30:13:0"]; return ok && v == 1 })
	off := find(func(f Frame) bool { v, ok := f.States["30:13:0"]; return ok && v == 0 })
	if !equalDur(on, []time.Duration{4 * time.Second, 7 * time.Second}) {
		t.Errorf("alarm on at %v", on)
	}
	if !equalDur(off, []time.Duration{5500 * time.Millisecond, 12 * time.Second}) {
		t.Errorf("alarm off at %v", off)
	}
	for _, e := range out {
		if !e.f.KeepAlarms {
			t.Fatal("sqlite frames must keep alarms")
		}
	}
}

func TestReplayFixedAndWindow(t *testing.T) {
	opt := defaultTestOpt()
	opt.Realtime = false
	opt.Interval = 3 * time.Second
	opt.Pulse = false
	opt.From, _ = parseUserTime("2026-07-16T17:00:03+08:00")
	out := runSource(t, opt)
	var pts []time.Duration
	for _, e := range out {
		if _, ok := e.f.Points["1:游离碱"]; ok {
			pts = append(pts, e.at)
		}
		if len(e.f.States) > 0 && e.f.States["31:0:0"] == 1 {
			t.Fatal("no trigger edges expected with pulse=false")
		}
	}
	// Steps after 17:00:03, one per interval from the first emitted frame:
	// 04 (+0s), 05 alarm (+3s), 08 (+6s); only 08 has a non-NULL tank-1 value.
	if !equalDur(pts, []time.Duration{6 * time.Second}) {
		t.Errorf("tank-1 values at %v", pts)
	}
}

func TestReplayLoop(t *testing.T) {
	cfg := writeTestConfig(t)
	opt := defaultTestOpt()
	opt.Loop = true
	src, err := openSQLiteSource(writeTestDB(t), "", &cfg, opt, false)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	n := 0
	for i := 0; i < 60; i++ {
		f, _, err := src.Next()
		if err != nil {
			t.Fatal(err)
		}
		if f.Points["1:游离碱"] == 2.4 {
			n++
		}
	}
	if n < 2 {
		t.Fatalf("expected the first row to be replayed again after looping, got %d", n)
	}
}

func TestInspect(t *testing.T) {
	cfg := writeTestConfig(t)
	path := writeTestDB(t)
	db, err := openReplayDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema, err := discoverSchema(db)
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if err := inspectDB(&sb, db, path, schema, newDBMapping(&cfg, &dbMapFile{}), defaultTestOpt()); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{"1-dbw-10|0", "-> 1:游离碱", "(unmapped: no [point]", "alarms", "mapped=1", "trigger Q:1:1"} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect output missing %q:\n%s", want, out)
		}
	}
}

func TestNotCollectorDB(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.db")
	db, _ := sql.Open("sqlite", p)
	db.Exec("CREATE TABLE foo (a)")
	db.Close()
	cfg := writeTestConfig(t)
	if _, err := openSQLiteSource(p, "", &cfg, defaultTestOpt(), false); err == nil || !strings.Contains(err.Error(), "not a collector DB") {
		t.Fatalf("err = %v", err)
	}
}

func equalDur(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestControlLimitsConfigAndWrite(t *testing.T) {
	cfg := writeTestConfig(t)
	p := cfg.pointByKey["1:游离碱"]
	if p.LCDB != 20 || p.LCOff != 60 || p.HCDB != 20 || p.HCOff != 62 {
		t.Fatalf("lc/hc offsets = %d|%d %d|%d", p.LCDB, p.LCOff, p.HCDB, p.HCOff)
	}
	if q := cfg.pointByKey["2:EC"]; q.LCDB != 0 || q.HCDB != 0 {
		t.Fatalf("point without lc/hc columns got offsets: %+v", q)
	}
	sim := newSimulation(cfg, SampleData{}, "", 0)
	if len(sim.dbAreas[20]) < 64 {
		t.Fatalf("DB20 not sized for lc/hc: %d bytes", len(sim.dbAreas[20]))
	}
	sim.applyFrame(Frame{Name: "t",
		LC: map[string]float64{"1:游离碱": 1.5, "2:EC": 7},
		HC: map[string]float64{"1:游离碱": 3.0}})
	buf := sim.dbAreas[20]
	if got := binary.BigEndian.Uint16(buf[60:]); got != 150 {
		t.Errorf("DB20.DBW60 (lc) = %d, want 150", got)
	}
	if got := binary.BigEndian.Uint16(buf[62:]); got != 300 {
		t.Errorf("DB20.DBW62 (hc) = %d, want 300", got)
	}
}

func TestSettingsWithoutLCHCColumns(t *testing.T) {
	cfg := writeTestConfig(t)
	p := filepath.Join(t.TempDir(), "old.db")
	db, _ := sql.Open("sqlite", p)
	for _, q := range []string{
		`CREATE TABLE dynamic_data_1 (id INTEGER PRIMARY KEY AUTOINCREMENT, timestamp TEXT NOT NULL, "1-dbw-10|0" REAL)`,
		`CREATE TABLE settings_history (timestamp TEXT NOT NULL, data_address TEXT NOT NULL, cv REAL, ha REAL, la REAL)`,
		`INSERT INTO dynamic_data_1 (timestamp, "1-dbw-10|0") VALUES ('2026-07-16T17:00:00+08:00', 2.4)`,
		`INSERT INTO settings_history VALUES ('2026-07-16T17:00:00+08:00', '1-dbw-10|0', 2.4, 3.5, 1.0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	src, err := openSQLiteSource(p, "", &cfg, defaultTestOpt(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	seen := false
	for i := 0; i < 10; i++ {
		f, _, err := src.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if f.HA["1:游离碱"] == 3.5 {
			seen = true
			if len(f.LC)+len(f.HC) != 0 {
				t.Errorf("unexpected lc/hc: %v %v", f.LC, f.HC)
			}
		}
	}
	if !seen {
		t.Fatal("settings from DB without lc/hc columns were not replayed")
	}
}

// All five setpoints (CV/LA/HA/LC/HC) go through the same path: CSV address
// columns -> settings_history replay -> applyFrame, for U16 and F32 points.
func TestAllSetpointsReplayed(t *testing.T) {
	csv := `[server],1,s7,127.0.0.1|0|1|102,0,1000
[point],1,游离碱,g/L,1,10|0,dbw,100,U16,20|2,20|4,20|6,1,,,,,,20|8,20|10
[point],3,Cu,ml/L,1,900|20,dbd,1,F32,900|264,900|268,900|260,1,,,,,,900|272,900|276
`
	cp := filepath.Join(t.TempDir(), "cfg.csv")
	if err := os.WriteFile(cp, []byte(csv), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(cp)
	if err != nil {
		t.Fatal(err)
	}
	dp := filepath.Join(t.TempDir(), "rec.db")
	db, _ := sql.Open("sqlite", dp)
	for _, q := range []string{
		`CREATE TABLE dynamic_data_1 (id INTEGER PRIMARY KEY AUTOINCREMENT, timestamp TEXT NOT NULL, "1-dbw-10|0" REAL)`,
		`CREATE TABLE dynamic_data_3 (id INTEGER PRIMARY KEY AUTOINCREMENT, timestamp TEXT NOT NULL, "3-dbd-900|20" REAL)`,
		`CREATE TABLE settings_history (timestamp TEXT NOT NULL, data_address TEXT NOT NULL, cv REAL, ha REAL, la REAL, hc REAL, lc REAL)`,
		`INSERT INTO dynamic_data_1 (timestamp, "1-dbw-10|0") VALUES ('2026-07-16T17:00:00+08:00', 2.4)`,
		`INSERT INTO dynamic_data_3 (timestamp, "3-dbd-900|20") VALUES ('2026-07-16T17:00:00+08:00', 1.25)`,
		`INSERT INTO settings_history VALUES ('2026-07-16T17:00:00+08:00', '1-dbw-10|0', 2.4, 3.5, 1.0, 3.2, 1.2)`,
		`INSERT INTO settings_history VALUES ('2026-07-16T17:00:00+08:00', '3-dbd-900|20', 1.5, 2.25, 0.75, 2.0, 1.0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	opt := defaultTestOpt()
	opt.Pulse = false
	src, err := openSQLiteSource(dp, "", &cfg, opt, false)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	sim := newSimulation(cfg, SampleData{}, "", 0)
	for {
		f, _, err := src.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		sim.applyFrame(f)
	}

	u16 := func(db, off int) uint16 { return binary.BigEndian.Uint16(sim.dbAreas[db][off:]) }
	f32 := func(db, off int) float32 {
		return math.Float32frombits(binary.BigEndian.Uint32(sim.dbAreas[db][off:]))
	}
	for _, c := range []struct {
		name      string
		got, want uint16
	}{
		{"cv", u16(20, 2), 240}, {"la", u16(20, 4), 100}, {"ha", u16(20, 6), 350},
		{"lc", u16(20, 8), 120}, {"hc", u16(20, 10), 320},
	} {
		if c.got != c.want {
			t.Errorf("U16 %s = %d, want %d", c.name, c.got, c.want)
		}
	}
	for _, c := range []struct {
		name      string
		got, want float32
	}{
		{"cv", f32(900, 264), 1.5}, {"la", f32(900, 268), 0.75}, {"ha", f32(900, 260), 2.25},
		{"lc", f32(900, 272), 1.0}, {"hc", f32(900, 276), 2.0}, {"value", f32(900, 20), 1.25},
	} {
		if c.got != c.want {
			t.Errorf("F32 %s = %g, want %g", c.name, c.got, c.want)
		}
	}
}

// Engineering values times the multiplier must round, not truncate:
// 8.45*100 is 844.999... in float64 and used to be stored as 844.
func TestIntegerWritesRound(t *testing.T) {
	cfg := writeTestConfig(t)
	sim := newSimulation(cfg, SampleData{}, "", 0)
	sim.applyFrame(Frame{Name: "t", KeepAlarms: true,
		Points: map[string]float64{"1:游离碱": 8.45},
		HC:     map[string]float64{"1:游离碱": 0.29}})
	if got := binary.BigEndian.Uint16(sim.dbAreas[10][0:]); got != 845 {
		t.Errorf("DB10.DBW0 = %d, want 845", got)
	}
	if got := binary.BigEndian.Uint16(sim.dbAreas[20][62:]); got != 29 {
		t.Errorf("DB20.DBW62 = %d, want 29", got)
	}
	sim.writeValue("db", 20, 0, "dbw", "S16", -2.5000001)
	if got := int16(binary.BigEndian.Uint16(sim.dbAreas[20][0:])); got != -3 {
		t.Errorf("S16 -2.5000001 = %d, want -3", got)
	}
}

func TestAlarmBitfieldAndPulseLeadAfterAlarm(t *testing.T) {
	csv := `[server],1,s7,127.0.0.1|0|1|102,0,1000
[state-trigger],1,31|0,dbx,0,1,1,,0,
[point],1,游离碱,g/L,1,10|0,dbw,100,U16,20|2,20|4,20|6,1
[alarm],1,30|13,dbx,0,1,报警bit0
[alarm],1,30|13,dbx,3,1,报警bit3
`
	cp := filepath.Join(t.TempDir(), "cfg.csv")
	if err := os.WriteFile(cp, []byte(csv), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(cp)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Alarms[1].Bit != 3 || cfg.Alarms[1].Value != 1 {
		t.Fatalf("alarm bit/val = %+v, want Bit=3 Value=1", cfg.Alarms[1])
	}

	dp := filepath.Join(t.TempDir(), "rec.db")
	db, _ := sql.Open("sqlite", dp)
	for _, q := range []string{
		`CREATE TABLE dynamic_data_1 (id INTEGER PRIMARY KEY AUTOINCREMENT, timestamp TEXT NOT NULL, "1-dbw-10|0" REAL)`,
		`CREATE TABLE alarms (id INTEGER PRIMARY KEY AUTOINCREMENT, timestamp TEXT NOT NULL, address TEXT NOT NULL, bitfield INTEGER, message TEXT)`,
		`INSERT INTO dynamic_data_1 (timestamp, "1-dbw-10|0") VALUES ('2026-07-16T17:00:00.000+08:00', 2.0), ('2026-07-16T17:00:04.000+08:00', 2.5)`,
		// Alarm on bit 3 arrives 200ms before the second data row (17:00:03.800).
		`INSERT INTO alarms (timestamp, address, bitfield, message) VALUES ('2026-07-16T17:00:03.800+08:00', 'dbx-30|13', 3, '')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	src, err := openSQLiteSource(dp, "", &cfg, defaultTestOpt(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	var highAt, lowAt, alarm3At []time.Duration
	var at time.Duration
	for {
		f, wait, err := src.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if v, ok := f.States["31:0:0"]; ok {
			if v == 1 {
				highAt = append(highAt, at)
			} else if at > 0 {
				lowAt = append(lowAt, at)
			}
		}
		if v, ok := f.States["30:13:3"]; ok && v == 1 {
			alarm3At = append(alarm3At, at)
		}
		at += wait
	}
	// First data row at v=2s (high at 1s, low at 2s).
	// Second data row at v=6s: despite the alarm at v=5.8s, trigger must still rise at 5s (full 1s PulseLead).
	if !equalDur(highAt, []time.Duration{time.Second, 5 * time.Second}) {
		t.Errorf("trigger high at %v, want [1s 5s]", highAt)
	}
	if !equalDur(lowAt, []time.Duration{2 * time.Second, 6 * time.Second}) {
		t.Errorf("trigger low at %v, want [2s 6s]", lowAt)
	}
	if !equalDur(alarm3At, []time.Duration{5800 * time.Millisecond}) {
		t.Errorf("alarm bit 3 raised at %v, want [5.8s]", alarm3At)
	}
}
