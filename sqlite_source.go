package main

import (
	"container/heap"
	"database/sql"
	"fmt"
	"io"
	"log"
	"maps"
	"time"
)

// SQLite replay source: merges collector tables into a time-ordered event
// stream, groups events into steps and schedules bit edges so a downstream
// collector re-records the replay. See docs/design/sqlite-replay-mode.md §7.

type replayOptions struct {
	From, To  time.Time
	Realtime  bool
	Speed     float64
	Interval  time.Duration
	MaxGap    time.Duration
	Loop      bool
	Pulse     bool
	PulseLead time.Duration
	AlarmHold time.Duration
	LogEvery  int
	PageSize  int
}

// ---- events ----

type evKind int

const (
	evData evKind = iota
	evSettings
	evAlarm
	evDose
)

type dbEvent struct {
	t          time.Time
	rawTS      string
	rowID      int64
	kind       evKind
	tank       int
	points     map[string]float64 // evData: point key -> value (scaled)
	key        string             // evSettings: point key
	cv, la, ha sql.NullFloat64
	lc, hc     sql.NullFloat64
	alarm      AlarmDef
	dose       DosingDef
	amount     float64
}

// ---- paged cursor over one table ----

type cursor struct {
	db       *sql.DB
	table    string
	cols     []string // selected columns after rowid, timestamp
	pageSize int
	minDate  string // optional YYYY-MM-DD lower bound from -db-from
	conv     func(ts time.Time, vals []any) (dbEvent, bool)

	lastTS   string
	lastID   int64
	started  bool
	startTS  string
	startID  int64
	hasStart bool
	buf      []dbEvent
	done     bool
	badTS    int
}

func (c *cursor) rewind() {
	c.lastTS = c.startTS
	c.lastID = c.startID
	c.started = c.hasStart
	c.buf = c.buf[:0]
	c.done = false
	c.badTS = 0
}

func (c *cursor) fill() error {
	for len(c.buf) == 0 && !c.done {
		sel := "rowid, timestamp"
		for _, col := range c.cols {
			sel += ", " + quoteIdent(col)
		}
		q := fmt.Sprintf("SELECT %s FROM %s", sel, quoteIdent(c.table))
		var args []any
		if c.started {
			q += " WHERE timestamp > ? OR (timestamp = ? AND rowid > ?)"
			args = []any{c.lastTS, c.lastTS, c.lastID}
		} else if c.minDate != "" {
			q += " WHERE timestamp >= ?"
			args = []any{c.minDate}
		}
		q += fmt.Sprintf(" ORDER BY timestamp, rowid LIMIT %d", c.pageSize)
		rows, err := c.db.Query(q, args...)
		if err != nil {
			return fmt.Errorf("query %s: %w", c.table, err)
		}
		n := 0
		for rows.Next() {
			vals := make([]any, len(c.cols)+2)
			ptrs := make([]any, len(vals))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return err
			}
			n++
			c.started = true
			c.lastID = toInt64(vals[0])
			c.lastTS = toString(vals[1])
			ts, err := parseTS(c.lastTS)
			if err != nil {
				c.badTS++
				continue
			}
			if ev, ok := c.conv(ts, vals[2:]); ok {
				ev.rawTS, ev.rowID = c.lastTS, c.lastID
				c.buf = append(c.buf, ev)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if n < c.pageSize {
			c.done = true
		}
	}
	return nil
}

func (c *cursor) peek() (*dbEvent, error) {
	if err := c.fill(); err != nil {
		return nil, err
	}
	if len(c.buf) == 0 {
		return nil, nil
	}
	return &c.buf[0], nil
}

func (c *cursor) pop() { c.buf = c.buf[1:] }

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	}
	return 0
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case []byte, string:
		var f float64
		if _, err := fmt.Sscan(toString(x), &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

func nullFloat(v any) sql.NullFloat64 {
	f, ok := toFloat(v)
	return sql.NullFloat64{Float64: f, Valid: ok}
}

func nullInt(v any) sql.NullInt64 {
	switch x := v.(type) {
	case int64:
		return sql.NullInt64{Int64: x, Valid: true}
	case float64:
		return sql.NullInt64{Int64: int64(x), Valid: true}
	}
	return sql.NullInt64{}
}

// ---- scheduled actions ----

type action struct {
	at    time.Duration // virtual (wall-clock) offset from replay start
	seq   int
	apply func(f *Frame)
	index int
}

type actionHeap []*action

func (h actionHeap) Len() int { return len(h) }
func (h actionHeap) Less(i, j int) bool {
	if h[i].at != h[j].at {
		return h[i].at < h[j].at
	}
	return h[i].seq < h[j].seq
}
func (h actionHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i]; h[i].index = i; h[j].index = j }
func (h *actionHeap) Push(x any)   { a := x.(*action); a.index = len(*h); *h = append(*h, a) }
func (h *actionHeap) Pop() any {
	old := *h
	a := old[len(old)-1]
	*h = old[:len(old)-1]
	a.index = -1
	return a
}

// ---- source ----

type sqliteSource struct {
	db      *sql.DB
	schema  *dbSchema
	mapping *dbMapping
	opt     replayOptions

	cursors []*cursor
	peeked  []dbEvent // next step (events sharing one timestamp)

	actions  actionHeap
	seq      int
	cur      time.Duration // time of last emitted action
	prevV    time.Duration // virtual time of last planned step
	prevT    time.Time     // source time of last planned step
	havePrev bool
	lastFall map[string]time.Duration // state bit key -> virtual time of last falling edge

	alarmClear map[string]*action // alarm bit key -> pending clear
	alarmRaise map[string]time.Duration

	steps  int
	pass   int
	firstT time.Time
}

func newSQLiteSource(db *sql.DB, schema *dbSchema, mapping *dbMapping, opt replayOptions) (*sqliteSource, error) {
	if opt.PageSize <= 0 {
		opt.PageSize = 500
	}
	if opt.Speed <= 0 {
		opt.Speed = 1
	}
	s := &sqliteSource{db: db, schema: schema, mapping: mapping, opt: opt}
	if opt.Pulse {
		// Start with every trigger/dosing bit inactive so the first replayed
		// edge is a real rising edge for the downstream collector.
		init := map[string]int{}
		for _, st := range mapping.cfg.States {
			init[stateBitKey(st)] = invertBit(st.Value)
		}
		s.schedule(0, func(f *Frame) { maps.Copy(f.States, init) })
	}
	if err := s.reset(); err != nil {
		return nil, err
	}
	if len(s.peeked) == 0 {
		return nil, fmt.Errorf("no replayable rows in the selected time window")
	}
	return s, nil
}

func (s *sqliteSource) Close() error { return s.db.Close() }

// buildCursors creates one cursor per mapped source table.
func (s *sqliteSource) buildCursors() []*cursor {
	m := s.mapping
	var cs []*cursor
	for _, dt := range s.schema.Dynamic {
		var cols []string
		var targets []pointTarget
		for _, c := range dt.Columns {
			if t, ok := m.point(c); ok {
				cols = append(cols, c)
				targets = append(targets, t)
			}
		}
		if len(cols) == 0 {
			continue
		}
		tank := dt.Tank
		cs = append(cs, &cursor{db: s.db, table: dt.Name, cols: cols, pageSize: s.opt.PageSize,
			conv: func(ts time.Time, vals []any) (dbEvent, bool) {
				ev := dbEvent{t: ts, kind: evData, tank: tank, points: map[string]float64{}}
				for i, v := range vals {
					if f, ok := toFloat(v); ok {
						ev.points[targets[i].def.Key] = f * targets[i].scale
					}
				}
				// Keep rows even when every mapped value is NULL: the row still
				// marks a trigger edge in the recording.
				return ev, true
			}})
	}
	if s.schema.has("settings_history", "timestamp", "data_address", "cv", "ha", "la") {
		// hc/lc were added to the collector later; select them only when present.
		cols := []string{"data_address", "cv", "la", "ha"}
		hasLC, hasHC := s.schema.has("settings_history", "lc"), s.schema.has("settings_history", "hc")
		if hasLC {
			cols = append(cols, "lc")
		}
		if hasHC {
			cols = append(cols, "hc")
		}
		cs = append(cs, &cursor{db: s.db, table: "settings_history", cols: cols, pageSize: s.opt.PageSize,
			conv: func(ts time.Time, vals []any) (dbEvent, bool) {
				t, ok := m.point(toString(vals[0]))
				if !ok {
					return dbEvent{}, false
				}
				ev := dbEvent{t: ts, kind: evSettings, key: t.def.Key, cv: nullFloat(vals[1]), la: nullFloat(vals[2]), ha: nullFloat(vals[3])}
				i := 4
				if hasLC {
					ev.lc = nullFloat(vals[i])
					i++
				}
				if hasHC {
					ev.hc = nullFloat(vals[i])
				}
				for _, f := range []*sql.NullFloat64{&ev.cv, &ev.la, &ev.ha, &ev.lc, &ev.hc} {
					f.Float64 *= t.scale
				}
				return ev, true
			}})
	}
	if s.schema.has("alarms", "timestamp", "address", "bitfield") {
		cols := []string{"address", "bitfield"}
		hasMsg := s.schema.has("alarms", "message")
		if hasMsg {
			cols = append(cols, "message")
		}
		cs = append(cs, &cursor{db: s.db, table: "alarms", cols: cols, pageSize: s.opt.PageSize,
			conv: func(ts time.Time, vals []any) (dbEvent, bool) {
				msg := ""
				if hasMsg {
					msg = toString(vals[2])
				}
				def, ok := m.alarm(toString(vals[0]), nullInt(vals[1]), msg)
				return dbEvent{t: ts, kind: evAlarm, alarm: def}, ok
			}})
	}
	if s.schema.has("dosing_records", "timestamp", "tank_id", "address", "dosage", "bitfield", "name") {
		cs = append(cs, &cursor{db: s.db, table: "dosing_records", cols: []string{"tank_id", "address", "bitfield", "name", "dosage"}, pageSize: s.opt.PageSize,
			conv: func(ts time.Time, vals []any) (dbEvent, bool) {
				def, ok := m.dosing(int(toInt64(vals[0])), toString(vals[1]), nullInt(vals[2]), toString(vals[3]))
				amt, okAmt := toFloat(vals[4])
				return dbEvent{t: ts, kind: evDose, dose: def, amount: amt}, ok && okAmt
			}})
	}
	var minDate string
	if !s.opt.From.IsZero() {
		// Safe coarse lower bound (1-day margin handles any timezone/format differences).
		minDate = s.opt.From.Add(-24 * time.Hour).Format("2006-01-02")
	}
	for _, c := range cs {
		c.minDate = minDate
	}
	return cs
}

// reset (re)opens all cursors at the start of the window.
func (s *sqliteSource) reset() error {
	if len(s.cursors) == 0 {
		s.cursors = s.buildCursors()
	} else {
		for _, c := range s.cursors {
			c.rewind()
		}
	}
	s.peeked = nil
	s.havePrev = false
	s.lastFall = map[string]time.Duration{}
	s.pass++
	return s.peekStep()
}

// peekStep loads the next group of events with identical timestamps.
func (s *sqliteSource) peekStep() error {
	s.peeked = s.peeked[:0]
	for {
		var minT time.Time
		found := false
		for _, c := range s.cursors {
			ev, err := c.peek()
			if err != nil {
				return err
			}
			if ev == nil {
				continue
			}
			if !found || ev.t.Before(minT) {
				minT, found = ev.t, true
			}
		}
		if !found {
			return nil
		}
		if !s.opt.To.IsZero() && !minT.Before(s.opt.To) {
			return nil
		}
		skip := !s.opt.From.IsZero() && minT.Before(s.opt.From)
		for _, c := range s.cursors {
			for {
				ev, err := c.peek()
				if err != nil {
					return err
				}
				if ev == nil || !ev.t.Equal(minT) {
					break
				}
				if !skip {
					s.peeked = append(s.peeked, *ev)
				} else if s.pass == 1 {
					// Record the last row before opt.From so subsequent loop
					// passes seek directly to the window start.
					c.startTS, c.startID, c.hasStart = ev.rawTS, ev.rowID, true
				}
				c.pop()
			}
		}
		if !skip {
			return nil
		}
	}
}

func (s *sqliteSource) schedule(at time.Duration, fn func(f *Frame)) *action {
	if at < s.cur {
		at = s.cur
	}
	s.seq++
	a := &action{at: at, seq: s.seq, apply: fn}
	heap.Push(&s.actions, a)
	return a
}

// nextStepV returns the virtual time of the peeked step.
func (s *sqliteSource) nextStepV() time.Duration {
	if !s.havePrev {
		return s.cur + s.opt.PulseLead*2
	}
	gap := s.opt.Interval
	if s.opt.Realtime {
		d := s.peeked[0].t.Sub(s.prevT)
		if d > s.opt.MaxGap {
			d = s.opt.MaxGap
		}
		if d < 0 {
			d = 0
		}
		gap = time.Duration(float64(d) / s.opt.Speed)
	}
	return s.prevV + gap
}

// planStep converts the peeked step into scheduled actions.
func (s *sqliteSource) planStep() error {
	v := s.nextStepV()
	t := s.peeked[0].t
	if s.steps == 0 && s.pass == 1 {
		s.firstT = t
	}
	s.steps++
	if s.opt.LogEvery > 0 && s.steps%s.opt.LogEvery == 0 {
		log.Printf("replay: step %d source time %s", s.steps, t.Format("2006-01-02 15:04:05.000"))
	}

	pre := newReplayFrame()
	at := newReplayFrame()

	raised := map[string]bool{}
	for _, ev := range s.peeked {
		switch ev.kind {
		case evSettings:
			dst := &at
			if s.opt.Pulse {
				dst = &pre
			}
			if ev.cv.Valid {
				dst.CV[ev.key] = ev.cv.Float64
			}
			if ev.la.Valid {
				dst.LA[ev.key] = ev.la.Float64
			}
			if ev.ha.Valid {
				dst.HA[ev.key] = ev.ha.Float64
			}
			if ev.lc.Valid {
				dst.LC[ev.key] = ev.lc.Float64
			}
			if ev.hc.Valid {
				dst.HC[ev.key] = ev.hc.Float64
			}
		case evData:
			trigs := s.mapping.triggers[ev.tank]
			if s.opt.Pulse && len(trigs) > 0 {
				for k, val := range ev.points {
					pre.Points[k] = val
				}
				for _, st := range trigs {
					pre.States[stateBitKey(st)] = st.Value
					at.States[stateBitKey(st)] = invertBit(st.Value)
				}
			} else {
				for k, val := range ev.points {
					at.Points[k] = val
				}
			}
		case evDose:
			key := fmt.Sprintf("%d:%s", ev.dose.TankID, ev.dose.Name)
			if s.opt.Pulse && ev.dose.Trigger != nil {
				pre.Dosing[key] = ev.amount
				pre.States[stateBitKey(*ev.dose.Trigger)] = ev.dose.Trigger.Value
				at.States[stateBitKey(*ev.dose.Trigger)] = invertBit(ev.dose.Trigger.Value)
			} else {
				at.Dosing[key] = ev.amount
			}
		case evAlarm:
			k := fmt.Sprintf("%d:%d:%d", ev.alarm.DB, ev.alarm.ByteOffset, ev.alarm.Bit)
			if !raised[k] {
				raised[k] = true
				s.planAlarm(ev.alarm, v)
			}
		}
	}

	if !frameEmpty(pre) {
		prevFall := s.cur
		for k := range at.States {
			if f, ok := s.lastFall[k]; ok && f > prevFall {
				prevFall = f
			}
		}
		lead := s.opt.PulseLead
		if half := (v - prevFall) / 2; half < lead {
			lead = half
		}
		s.schedule(v-lead, func(f *Frame) { mergeFrame(f, pre) })
		for k := range at.States {
			s.lastFall[k] = v
		}
	}
	if !frameEmpty(at) {
		s.schedule(v, func(f *Frame) { mergeFrame(f, at) })
	}
	s.prevV, s.prevT, s.havePrev = v, t, true
	return s.peekStep()
}

func (s *sqliteSource) planAlarm(a AlarmDef, v time.Duration) {
	key := fmt.Sprintf("%d:%d:%d", a.DB, a.ByteOffset, a.Bit)
	if s.alarmClear == nil {
		s.alarmClear = map[string]*action{}
		s.alarmRaise = map[string]time.Duration{}
	}
	// Make sure the previous raise of this alarm is cleared before the new one,
	// so the collector sees a fresh rising edge.
	if pending, ok := s.alarmClear[key]; ok && pending.index >= 0 && pending.at >= v {
		mid := (s.alarmRaise[key] + v) / 2
		if mid < s.cur {
			mid = s.cur
		}
		pending.at = mid
		heap.Fix(&s.actions, pending.index)
	}
	s.schedule(v, func(f *Frame) { f.States[key] = 1 })
	s.alarmRaise[key] = v
	s.alarmClear[key] = s.schedule(v+s.opt.AlarmHold, func(f *Frame) { f.States[key] = 0 })
}

// Next emits the frame for the next action time.
func (s *sqliteSource) Next() (Frame, time.Duration, error) {
	for {
		// Plan steps whose lead edge could precede the next pending action.
		for len(s.peeked) > 0 && (s.actions.Len() == 0 || s.nextStepV()-s.opt.PulseLead <= s.actions[0].at) {
			if err := s.planStep(); err != nil {
				return Frame{}, 0, err
			}
		}
		if s.actions.Len() > 0 {
			break
		}
		// Data exhausted.
		if bad := s.badTimestamps(); bad > 0 {
			log.Printf("replay: skipped %d rows with unparsable timestamps in pass %d", bad, s.pass)
		}
		if !s.opt.Loop {
			return Frame{}, 0, io.EOF
		}
		log.Printf("replay: end of data after pass %d, restarting", s.pass)
		if err := s.reset(); err != nil {
			return Frame{}, 0, err
		}
		if len(s.peeked) == 0 {
			return Frame{}, 0, io.EOF
		}
	}

	f := newReplayFrame()
	at := s.actions[0].at
	for s.actions.Len() > 0 && s.actions[0].at == at {
		a := heap.Pop(&s.actions).(*action)
		a.apply(&f)
	}
	s.cur = at

	// Make sure the following action is known before computing the wait.
	for len(s.peeked) > 0 && (s.actions.Len() == 0 || s.nextStepV()-s.opt.PulseLead <= s.actions[0].at) {
		if err := s.planStep(); err != nil {
			return Frame{}, 0, err
		}
	}
	var wait time.Duration
	if s.actions.Len() > 0 {
		wait = s.actions[0].at - at
	} else if s.opt.Loop {
		wait = s.opt.Interval
	}
	return f, wait, nil
}

func stateBitKey(st StateDef) string {
	if st.Area == "q" {
		return fmt.Sprintf("Q:%d:%d", st.ByteOffset, st.Bit)
	}
	return fmt.Sprintf("%d:%d:%d", st.DB, st.ByteOffset, st.Bit)
}

func invertBit(v int) int {
	if v != 0 {
		return 0
	}
	return 1
}

func frameEmpty(f Frame) bool {
	return len(f.Points)+len(f.CV)+len(f.LA)+len(f.HA)+len(f.LC)+len(f.HC)+len(f.Dosing)+len(f.States) == 0
}

func newReplayFrame() Frame {
	return Frame{Name: "sqlite", KeepAlarms: true, Points: map[string]float64{}, CV: map[string]float64{},
		LA: map[string]float64{}, HA: map[string]float64{}, LC: map[string]float64{}, HC: map[string]float64{},
		Dosing: map[string]float64{}, States: map[string]int{}}
}

func mergeFrame(dst *Frame, src Frame) {
	for _, pair := range [][2]map[string]float64{
		{dst.Points, src.Points}, {dst.CV, src.CV}, {dst.LA, src.LA}, {dst.HA, src.HA},
		{dst.LC, src.LC}, {dst.HC, src.HC}, {dst.Dosing, src.Dosing},
	} {
		maps.Copy(pair[0], pair[1])
	}
	maps.Copy(dst.States, src.States)
}

// sourceStats summarises cursor problems (for logs).
func (s *sqliteSource) badTimestamps() int {
	n := 0
	for _, c := range s.cursors {
		n += c.badTS
	}
	return n
}
