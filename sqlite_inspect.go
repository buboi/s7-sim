package main

import (
	"database/sql"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// inspectDB prints the schema/mapping report described in the design doc §6.
func inspectDB(w io.Writer, db *sql.DB, path string, schema *dbSchema, m *dbMapping, opt replayOptions) error {
	fmt.Fprintf(w, "DB %s", path)
	if schema.has("mes_metadata", "key", "value") {
		var id string
		if err := db.QueryRow(`SELECT value FROM mes_metadata WHERE key='dataset_id'`).Scan(&id); err == nil {
			fmt.Fprintf(w, "  dataset_id=%s", id)
		}
	}
	fmt.Fprintln(w)

	// Overall range from all timestamped tables.
	var minT, maxT time.Time
	for _, t := range sortedTables(schema) {
		if !schema.has(t, "timestamp") {
			continue
		}
		var lo, hi sql.NullString
		if err := db.QueryRow(fmt.Sprintf("SELECT min(timestamp), max(timestamp) FROM %s", quoteIdent(t))).Scan(&lo, &hi); err != nil {
			return err
		}
		if a, err := parseTS(lo.String); err == nil && (minT.IsZero() || a.Before(minT)) {
			minT = a
		}
		if b, err := parseTS(hi.String); err == nil && b.After(maxT) {
			maxT = b
		}
	}
	if !minT.IsZero() {
		fmt.Fprintf(w, "range %s .. %s (%s)\n", minT.Format("2006-01-02 15:04:05"), maxT.Format("2006-01-02 15:04:05"), maxT.Sub(minT).Round(time.Second))
	}
	if !opt.From.IsZero() || !opt.To.IsZero() {
		lo, hi := opt.From, opt.To
		if lo.IsZero() {
			lo = minT
		}
		if hi.IsZero() {
			hi = maxT
		}
		fmt.Fprintf(w, "window %s .. %s\n", lo.Format("2006-01-02 15:04:05"), hi.Format("2006-01-02 15:04:05"))
	}
	fmt.Fprintln(w)

	// Dynamic tables.
	tanksWithData := map[int]bool{}
	for _, dt := range schema.Dynamic {
		var n int
		if err := db.QueryRow(fmt.Sprintf("SELECT count(*) FROM %s", quoteIdent(dt.Name))).Scan(&n); err != nil {
			return err
		}
		fmt.Fprintf(w, "%-16s rows=%-6d cols=%d", dt.Name, n, len(dt.Columns))
		if n == 0 {
			if len(m.triggers[dt.Tank]) == 0 {
				fmt.Fprintf(w, "  (no [state-trigger] for tank %d)", dt.Tank)
			}
			fmt.Fprintln(w)
			continue
		}
		tanksWithData[dt.Tank] = true
		if med, gaps, err := rowIntervals(db, dt.Name, opt.From, opt.To, opt.MaxGap); err == nil {
			fmt.Fprintf(w, "  median interval %s", med.Round(10*time.Millisecond))
			if gaps > 0 {
				fmt.Fprintf(w, "  gaps>%s: %d", opt.MaxGap, gaps)
			}
		}
		fmt.Fprintln(w)
		for _, c := range dt.Columns {
			var lo, hi sql.NullFloat64
			var nulls int
			q := fmt.Sprintf("SELECT min(%[1]s), max(%[1]s), sum(%[1]s IS NULL) FROM %[2]s", quoteIdent(c), quoteIdent(dt.Name))
			if err := db.QueryRow(q).Scan(&lo, &hi, &nulls); err != nil {
				return err
			}
			if t, ok := m.point(c); ok {
				fmt.Fprintf(w, "  %-14s -> %-12s %s %s x%d  min=%g max=%g nulls=%d [%s]\n", c, t.def.Key,
					t.def.RegisterType, t.def.DataType, t.def.Multiplier, lo.Float64, hi.Float64, nulls, t.how)
			} else {
				fmt.Fprintf(w, "  %-14s -> (unmapped: %s)\n", c, m.unmapped[c])
			}
		}
	}
	fmt.Fprintln(w)

	// settings_history.
	if schema.has("settings_history", "data_address") {
		keys, err := distinctStrings(db, "SELECT DISTINCT data_address FROM settings_history")
		if err != nil {
			return err
		}
		var n int
		db.QueryRow("SELECT count(*) FROM settings_history").Scan(&n)
		mapped := 0
		var bad []string
		for _, k := range keys {
			if _, ok := m.point(k); ok {
				mapped++
			} else {
				bad = append(bad, k)
			}
		}
		fmt.Fprintf(w, "%-16s rows=%-6d keys=%d mapped=%d\n", "settings_history", n, len(keys), mapped)
		for _, col := range []string{"cv", "la", "ha", "lc", "hc"} {
			if !schema.has("settings_history", col) {
				fmt.Fprintf(w, "  %s: column missing (older collector DB)\n", col)
				continue
			}
			var nn int
			db.QueryRow(fmt.Sprintf("SELECT count(%s) FROM settings_history", col)).Scan(&nn)
			addr := 0
			for _, p := range m.cfg.Points {
				if setpointDB(p, col) > 0 {
					addr++
				}
			}
			note := ""
			if nn > 0 && addr == 0 {
				note = "  (values recorded but no point has this address in the config)"
			}
			fmt.Fprintf(w, "  %s: %d non-null values, %d config points with an address%s\n", col, nn, addr, note)
		}
		for _, k := range bad {
			fmt.Fprintf(w, "  unmapped %s: %s\n", k, m.unmapped[k])
		}
	}

	// alarms.
	if schema.has("alarms", "address", "bitfield") {
		hasMsg := schema.has("alarms", "message")
		q := "SELECT address, bitfield, '' , count(*) FROM alarms GROUP BY 1, 2"
		if hasMsg {
			q = "SELECT address, bitfield, coalesce(max(message),''), count(*) FROM alarms GROUP BY 1, 2"
		}
		rows, err := db.Query(q)
		if err != nil {
			return err
		}
		var total, keys, mapped int
		var lines []string
		for rows.Next() {
			var addr, msg string
			var bit sql.NullInt64
			var cnt int
			if err := rows.Scan(&addr, &bit, &msg, &cnt); err != nil {
				rows.Close()
				return err
			}
			total += cnt
			keys++
			if def, ok := m.alarm(addr, bit, msg); ok {
				mapped++
				lines = append(lines, fmt.Sprintf("  %-14s bit %d -> %s x%d", addr, bit.Int64, def.Message, cnt))
			} else {
				lines = append(lines, fmt.Sprintf("  %-14s bit %d -> (unmapped) %q x%d", addr, bit.Int64, msg, cnt))
			}
		}
		rows.Close()
		fmt.Fprintf(w, "%-16s rows=%-6d keys=%d mapped=%d (rising-edge events)\n", "alarms", total, keys, mapped)
		fmt.Fprintln(w, strings.Join(lines, "\n"))
	}

	// dosing_records.
	if schema.has("dosing_records", "tank_id", "address", "bitfield", "name", "dosage") {
		rows, err := db.Query("SELECT tank_id, address, bitfield, coalesce(name,''), count(*), min(dosage), max(dosage) FROM dosing_records GROUP BY 1, 2, 3, 4")
		if err != nil {
			return err
		}
		var total, keys, mapped int
		var lines []string
		for rows.Next() {
			var tank, cnt int
			var addr, name string
			var bit sql.NullInt64
			var lo, hi float64
			if err := rows.Scan(&tank, &addr, &bit, &name, &cnt, &lo, &hi); err != nil {
				rows.Close()
				return err
			}
			total += cnt
			keys++
			if def, ok := m.dosing(tank, addr, bit, name); ok {
				mapped++
				trig := "-"
				if def.Trigger != nil {
					trig = stateBitKey(*def.Trigger)
				}
				lines = append(lines, fmt.Sprintf("  tank %d %s bit %d %s -> amount %d|%d %s, trigger %s  x%d (%g..%g)",
					tank, addr, bit.Int64, name, def.DB, def.ByteOffset, def.DataType, trig, cnt, lo, hi))
			} else {
				lines = append(lines, fmt.Sprintf("  tank %d %s bit %d %s -> (unmapped) x%d", tank, addr, bit.Int64, name, cnt))
			}
		}
		rows.Close()
		fmt.Fprintf(w, "%-16s rows=%-6d keys=%d mapped=%d\n", "dosing_records", total, keys, mapped)
		if len(lines) > 0 {
			fmt.Fprintln(w, strings.Join(lines, "\n"))
		}
	}
	fmt.Fprintln(w)

	// Config coverage.
	var missing []string
	seen := map[int]bool{}
	for _, p := range m.cfg.Points {
		if !tanksWithData[p.TankID] && !seen[p.TankID] {
			seen[p.TankID] = true
			missing = append(missing, fmt.Sprintf("%d:*", p.TankID))
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(w, "config tanks with no dynamic data: %s (setpoints only)\n", strings.Join(missing, ", "))
	}
	return nil
}

func sortedTables(s *dbSchema) []string {
	var ts []string
	for t := range s.Tables {
		ts = append(ts, t)
	}
	sort.Strings(ts)
	return ts
}

func distinctStrings(db *sql.DB, q string) ([]string, error) {
	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s.String)
	}
	return out, rows.Err()
}

// rowIntervals returns the median interval between rows and the number of gaps longer than maxGap.
func rowIntervals(db *sql.DB, table string, from, to time.Time, maxGap time.Duration) (time.Duration, int, error) {
	rows, err := db.Query(fmt.Sprintf("SELECT timestamp FROM %s ORDER BY timestamp", quoteIdent(table)))
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	var prev time.Time
	var ds []time.Duration
	gaps := 0
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return 0, 0, err
		}
		t, err := parseTS(s)
		if err != nil {
			continue
		}
		if (!from.IsZero() && t.Before(from)) || (!to.IsZero() && !t.Before(to)) {
			continue
		}
		if !prev.IsZero() {
			d := t.Sub(prev)
			ds = append(ds, d)
			if maxGap > 0 && d > maxGap {
				gaps++
			}
		}
		prev = t
	}
	if len(ds) == 0 {
		return 0, 0, rows.Err()
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2], gaps, rows.Err()
}

// setpointDB returns the configured DB for a setpoint kind, 0 if none.
func setpointDB(p PointDef, kind string) int {
	switch kind {
	case "cv":
		return p.ControlDB
	case "la":
		return p.LowDB
	case "ha":
		return p.HighDB
	case "lc":
		return p.LCDB
	case "hc":
		return p.HCDB
	}
	return 0
}
