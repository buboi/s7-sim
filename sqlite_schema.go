package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Schema discovery and key mapping for databases written by data_collector_new.
// See docs/design/sqlite-replay-mode.md.

var dynamicTableRe = regexp.MustCompile(`^dynamic_data_(\d+)$`)

type dbSchema struct {
	Tables  map[string][]string // table -> column names (all columns)
	Dynamic []dynamicTable      // sorted by tank
}

type dynamicTable struct {
	Name    string
	Tank    int
	Columns []string // value columns (excluding id/timestamp)
}

func openReplayDB(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func discoverSchema(db *sql.DB) (*dbSchema, error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()

	s := &dbSchema{Tables: map[string][]string{}}
	for _, n := range names {
		cols, err := tableColumns(db, n)
		if err != nil {
			return nil, err
		}
		s.Tables[n] = cols
		if m := dynamicTableRe.FindStringSubmatch(n); m != nil {
			tank, _ := strconv.Atoi(m[1])
			dt := dynamicTable{Name: n, Tank: tank}
			for _, c := range cols {
				if c != "id" && c != "timestamp" {
					dt.Columns = append(dt.Columns, c)
				}
			}
			s.Dynamic = append(s.Dynamic, dt)
		}
	}
	sort.Slice(s.Dynamic, func(i, j int) bool { return s.Dynamic[i].Tank < s.Dynamic[j].Tank })
	if len(s.Dynamic) == 0 {
		return nil, fmt.Errorf("not a collector DB: no dynamic_data_<tank> tables found")
	}
	return s, nil
}

func (s *dbSchema) has(table string, cols ...string) bool {
	have, ok := s.Tables[table]
	if !ok {
		return false
	}
	for _, c := range cols {
		found := false
		for _, h := range have {
			if h == c {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func tableColumns(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", quoteIdent(table)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var (
			cid     int
			name    string
			typ     string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols = append(cols, name)
	}
	return cols, rows.Err()
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// ---- key normalisation ----

// normAddrKey normalises "<reg>-<DB|off>" (or "<reg>-<byte>" for Q) into a
// canonical "<reg>-<db>|<off>" string.
func normAddrKey(reg, addr string) (string, error) {
	reg = strings.ToLower(strings.TrimSpace(reg))
	area := areaFromRegType(reg)
	db, off, err := parseAreaOffset(area, addr)
	if err != nil {
		return "", err
	}
	if area == "q" {
		return fmt.Sprintf("q-%d", off), nil
	}
	return fmt.Sprintf("%s-%d|%d", reg, db, off), nil
}

// parseColumnKey splits a collector column name "<tank>-<reg>-<addr>".
func parseColumnKey(col string) (tank int, addrKey string, err error) {
	parts := strings.SplitN(strings.TrimSpace(col), "-", 3)
	if len(parts) != 3 {
		return 0, "", fmt.Errorf("unparsable column %q", col)
	}
	tank, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, "", fmt.Errorf("unparsable tank in column %q", col)
	}
	addrKey, err = normAddrKey(parts[1], parts[2])
	return tank, addrKey, err
}

// parseAddressKey splits an alarm/dosing address "<reg>-<addr>".
func parseAddressKey(addr string) (string, error) {
	parts := strings.SplitN(strings.TrimSpace(addr), "-", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("unparsable address %q", addr)
	}
	return normAddrKey(parts[0], parts[1])
}

func pointColumnKey(p PointDef) string {
	k, _ := normAddrKey(p.RegisterType, fmt.Sprintf("%d|%d", p.DB, p.ByteOffset))
	return fmt.Sprintf("%d-%s", p.TankID, k)
}

func bitKey(addrKey string, bit int) string { return fmt.Sprintf("%s#%d", addrKey, bit) }

// ---- mapping ----

type dbMapFile struct {
	Points map[string]string  `json:"points"` // DB column -> "<tank>:<name>"
	Alarms map[string]string  `json:"alarms"` // "<address>#<bit>" or "<address>" -> alarm message
	Dosing map[string]string  `json:"dosing"` // "<tank>:<name>" (DB) -> "<tank>:<name>" (config)
	Scale  map[string]float64 `json:"scale"`  // DB column -> factor applied before the CSV multiplier
}

func loadDBMap(path string) (*dbMapFile, error) {
	m := &dbMapFile{}
	if path == "" {
		return m, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return m, nil
}

type pointTarget struct {
	def   PointDef
	scale float64
	how   string
}

// dbMapping resolves DB keys to config definitions.
type dbMapping struct {
	cfg        *Config
	overrides  *dbMapFile
	points     map[string]pointTarget // raw DB column/data_address -> point
	unmapped   map[string]string      // raw key -> reason
	alarmByKey map[string]AlarmDef    // normalised addr#bit -> alarm
	doseByKey  map[string]DosingDef   // "<tank>:<name>" -> dosing
	doseByAddr map[string]DosingDef   // "<tank>:<addr#bit>" -> dosing
	triggers   map[int][]StateDef     // tank -> trigger bits
	pointByCol map[string]PointDef    // canonical column key -> point
}

func newDBMapping(cfg *Config, overrides *dbMapFile) *dbMapping {
	m := &dbMapping{
		cfg:        cfg,
		overrides:  overrides,
		points:     map[string]pointTarget{},
		unmapped:   map[string]string{},
		alarmByKey: map[string]AlarmDef{},
		doseByKey:  map[string]DosingDef{},
		doseByAddr: map[string]DosingDef{},
		triggers:   map[int][]StateDef{},
		pointByCol: map[string]PointDef{},
	}
	for _, p := range cfg.Points {
		m.pointByCol[pointColumnKey(p)] = p
	}
	for _, a := range cfg.Alarms {
		reg := a.RegisterType
		if reg == "" {
			reg = "dbx"
		}
		k, _ := normAddrKey(reg, fmt.Sprintf("%d|%d", a.DB, a.ByteOffset))
		m.alarmByKey[bitKey(k, a.Bit)] = a
	}
	for _, d := range cfg.Dosing {
		m.doseByKey[fmt.Sprintf("%d:%s", d.TankID, d.Name)] = d
		if d.Trigger != nil {
			m.doseByAddr[fmt.Sprintf("%d:%s", d.TankID, bitKey(stateAddrKey(*d.Trigger), d.Trigger.Bit))] = d
		}
	}
	for _, st := range cfg.States {
		if st.Kind == "trigger" {
			m.triggers[st.TankID] = append(m.triggers[st.TankID], st)
		}
	}
	return m
}

func stateAddrKey(st StateDef) string {
	k, _ := normAddrKey(st.RegisterType, fmt.Sprintf("%d|%d", st.DB, st.ByteOffset))
	if st.Area == "q" {
		k = fmt.Sprintf("q-%d", st.ByteOffset)
	}
	return k
}

// point resolves a dynamic column / settings data_address.
func (m *dbMapping) point(raw string) (pointTarget, bool) {
	if t, ok := m.points[raw]; ok {
		return t, true
	}
	if _, ok := m.unmapped[raw]; ok {
		return pointTarget{}, false
	}
	scale := 1.0
	if f, ok := m.overrides.Scale[raw]; ok && f != 0 {
		scale = f
	}
	if target, ok := m.overrides.Points[raw]; ok {
		if def, ok := m.cfg.pointByKey[target]; ok {
			t := pointTarget{def: def, scale: scale, how: "map"}
			m.points[raw] = t
			return t, true
		}
		m.unmapped[raw] = fmt.Sprintf("-db-map target %q not in config", target)
		return pointTarget{}, false
	}
	tank, addrKey, err := parseColumnKey(raw)
	if err != nil {
		m.unmapped[raw] = err.Error()
		return pointTarget{}, false
	}
	def, ok := m.pointByCol[fmt.Sprintf("%d-%s", tank, addrKey)]
	if !ok {
		m.unmapped[raw] = "no [point] with this tank/address"
		return pointTarget{}, false
	}
	t := pointTarget{def: def, scale: scale, how: "exact"}
	m.points[raw] = t
	return t, true
}

func (m *dbMapping) alarm(address string, bit sql.NullInt64, message string) (AlarmDef, bool) {
	b := 0
	if bit.Valid {
		b = int(bit.Int64)
	}
	raw := bitKey(strings.TrimSpace(address), b)
	if target, ok := m.overrides.Alarms[raw]; ok {
		def, ok := m.cfg.alarmByText[target]
		return def, ok
	}
	if target, ok := m.overrides.Alarms[strings.TrimSpace(address)]; ok {
		def, ok := m.cfg.alarmByText[target]
		return def, ok
	}
	if k, err := parseAddressKey(address); err == nil {
		if def, ok := m.alarmByKey[bitKey(k, b)]; ok {
			return def, true
		}
	}
	if message = strings.TrimSpace(message); message != "" {
		if def, ok := m.cfg.alarmByText[message]; ok {
			return def, true
		}
	}
	return AlarmDef{}, false
}

func (m *dbMapping) dosing(tank int, address string, bit sql.NullInt64, name string) (DosingDef, bool) {
	key := fmt.Sprintf("%d:%s", tank, strings.TrimSpace(name))
	if target, ok := m.overrides.Dosing[key]; ok {
		def, ok := m.doseByKey[target]
		return def, ok
	}
	if def, ok := m.doseByKey[key]; ok {
		return def, true
	}
	if k, err := parseAddressKey(address); err == nil {
		b := 0
		if bit.Valid {
			b = int(bit.Int64)
		}
		if def, ok := m.doseByAddr[fmt.Sprintf("%d:%s", tank, bitKey(k, b))]; ok {
			return def, true
		}
	}
	return DosingDef{}, false
}

// ---- timestamps ----

var tsLayouts = []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999"}

func parseTS(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, l := range tsLayouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparsable timestamp %q", s)
}

// parseUserTime parses -db-from / -db-to.
func parseUserTime(s string) (time.Time, error) {
	if strings.TrimSpace(s) == "" {
		return time.Time{}, nil
	}
	for _, l := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(l, strings.TrimSpace(s), time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid time %q (use RFC3339 or 2006-01-02 15:04:05)", s)
}

// openSQLiteSource opens the DB, resolves the mapping and either prints the
// inspect report (returns nil source) or returns a ready replay source.
func openSQLiteSource(path, mapPath string, cfg *Config, opt replayOptions, inspectOnly bool) (FrameSource, error) {
	db, err := openReplayDB(path)
	if err != nil {
		return nil, err
	}
	schema, err := discoverSchema(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	overrides, err := loadDBMap(mapPath)
	if err != nil {
		db.Close()
		return nil, err
	}
	m := newDBMapping(cfg, overrides)
	if inspectOnly {
		defer db.Close()
		return nil, inspectDB(os.Stdout, db, path, schema, m, opt)
	}
	mapped := 0
	for _, dt := range schema.Dynamic {
		for _, c := range dt.Columns {
			if _, ok := m.point(c); ok {
				mapped++
			}
		}
	}
	if mapped == 0 {
		db.Close()
		return nil, fmt.Errorf("no DB column matches a [point] in the config; run with -db-inspect and check -config / -db-map")
	}
	checkUnmappedTables(db, schema, m)
	if len(m.unmapped) > 0 {
		keys := make([]string, 0, len(m.unmapped))
		for k, why := range m.unmapped {
			keys = append(keys, fmt.Sprintf("%s (%s)", k, why))
		}
		sort.Strings(keys)
		log.Printf("sqlite replay: %d unmapped keys skipped: %s", len(keys), strings.Join(keys, ", "))
	}
	src, err := newSQLiteSource(db, schema, m, opt)
	if err != nil {
		db.Close()
		return nil, err
	}
	mode := "realtime"
	if !opt.Realtime {
		mode = "fixed " + opt.Interval.String()
	}
	log.Printf("sqlite replay: %s, %d mapped columns, mode=%s speed=%g max-gap=%s pulse=%v lead=%s alarm-hold=%s loop=%v",
		path, mapped, mode, opt.Speed, opt.MaxGap, opt.Pulse, opt.PulseLead, opt.AlarmHold, opt.Loop)
	return src, nil
}

// checkUnmappedTables checks settings_history, alarms and dosing_records keys
// at startup so any unmapped entries are reported once in m.unmapped.
func checkUnmappedTables(db *sql.DB, schema *dbSchema, m *dbMapping) {
	if schema.has("settings_history", "data_address") {
		if keys, err := distinctStrings(db, "SELECT DISTINCT data_address FROM settings_history"); err == nil {
			for _, k := range keys {
				m.point(k)
			}
		}
	}
	if schema.has("alarms", "address", "bitfield") {
		q := "SELECT address, bitfield, '' FROM alarms GROUP BY 1, 2"
		if schema.has("alarms", "message") {
			q = "SELECT address, bitfield, coalesce(max(message),'') FROM alarms GROUP BY 1, 2"
		}
		if rows, err := db.Query(q); err == nil {
			defer rows.Close()
			for rows.Next() {
				var addr, msg string
				var bit sql.NullInt64
				if rows.Scan(&addr, &bit, &msg) == nil {
					if _, ok := m.alarm(addr, bit, msg); !ok {
						m.unmapped[fmt.Sprintf("alarm:%s#%d", strings.TrimSpace(addr), bit.Int64)] = "no [alarm] match"
					}
				}
			}
		}
	}
	if schema.has("dosing_records", "tank_id", "address", "bitfield", "name") {
		if rows, err := db.Query("SELECT DISTINCT tank_id, address, bitfield, coalesce(name,'') FROM dosing_records"); err == nil {
			defer rows.Close()
			for rows.Next() {
				var tank int
				var addr, name string
				var bit sql.NullInt64
				if rows.Scan(&tank, &addr, &bit, &name) == nil {
					if _, ok := m.dosing(tank, addr, bit, name); !ok {
						m.unmapped[fmt.Sprintf("dosing:%d:%s", tank, strings.TrimSpace(name))] = "no [state-dosing] match"
					}
				}
			}
		}
	}
}
