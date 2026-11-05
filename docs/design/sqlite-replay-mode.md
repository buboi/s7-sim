# Design: SQLite replay mode for s7-sim

Status: approved v2 · 2026-11-05
Reference data: `topway-data0-s7-mes.db` + `topway-data0-s7.csv`

## 1. Goal

Add a second data source to s7-sim. Today values come from a hand-written JSON
frame file (`-data simdata/*.json`). The new mode reads **real historical data**
from a SQLite DB written by `data_collector_new` while it was polling an S7 PLC.
It replays that data into the S7 areas, so a collector or HMI connected to the
simulator sees what the plant produced, including the timing.

The mode must:

1. **Inspect**: find the DB schema (tables, columns, address keys, row counts,
   time range) and report how each key maps to the CSV config.
2. **Replay**: step through the recorded events in time order and write process
   values, setpoints (CV/LA/HA), alarms, dosing amounts and trigger/dosing bits,
   using the existing `writeValue` / `setBit` code.

Out of scope: writing to the DB, merging several DBs, and changing the CSV
config format.

## 2. Current s7-sim structure

- Config: the sectioned CSV (`[server]`, `[point]`, `[alarm]`, `[state-trigger]`,
  `[state-dosing]`), parsed by `loadConfig` in `main.go`.
- Data: the JSON `SampleData{interval_ms, frames[]}`. `main` rotates through the
  frames on a ticker and calls `Simulation.applyFrame(frame)`.
- `applyFrame` already writes `points`, `cv`, `la`, `ha`, `dosing`, `alarms`,
  `states` and `words`, and applies the CSV multiplier and data type.

**Core idea:** the DB reader is a new *frame source*. It turns DB events into
`Frame` values, and `applyFrame` / `writeValue` / `setBit` are reused unchanged.

## 3. Source DB structure (from `topway-data0-s7-mes.db`)

### 3.1 Tables

| Table | Rows | Content |
|---|---|---|
| `dynamic_data_<tank>` (1–6) | 189 each for tanks 1, 3, 6. **0** for tanks 2, 4, 5 | `id`, `timestamp`, one `REAL` column per point of that tank |
| `settings_history` | 2444 (188 timestamps × 13 keys) | `timestamp`, `data_address`, `cv`, `ha`, `la`, `hc`, `lc` |
| `alarms` | 188 | `timestamp`, `address`, `bitfield`, `mes_type_id`, `mes_id_source`, `message` |
| `dosing_records` | 125 | `timestamp`, `tank_id`, `address`, `dosage`, `bitfield`, `mes_*`, `name`, `unit` |
| `mes_metadata` | 1 | `dataset_id`. Shown in the report, otherwise ignored. |

Time range: 2026-07-16 17:02:33 → 17:17:27 (+08:00), about 15 minutes.

### 3.2 Key formats and how they map to the CSV

| DB key | Example | CSV row | Rule |
|---|---|---|---|
| point column | `1-dbw-10\|0` | `[point],1,游离碱,…,10\|0,dbw,100,U16,…` | `<tank>-<regtype>-<DB\|off>` |
| `settings_history.data_address` | `1-dbw-10\|8` | the same point. Its `控制值/下限/上限偏移` columns give the write targets | same key as the point |
| `alarms.address` + `bitfield` | `dbx-30\|13`, bit 0 | `[alarm],1,30\|13,dbx,0,…,硅烷槽EC值过高` | `<regtype>-<DB\|off>` + bit. `message` is the CSV text. |
| `dosing_records.address` + `bitfield` + `tank_id` + `name` | `Q-1`, bit 1, tank 1, `色浆` | `[state-dosing],1,1,Q,1,1,1,,0,色浆,20\|130,dbw,ml,1,U16` | trigger key `<regtype>-<byte>` + bit. `name` is the chemical name. |

All 13 point columns, all 13 settings keys, all 3 alarm keys and both dosing
keys in the reference DB match the CSV exactly.

### 3.3 Collector behaviour that the replay must reproduce

These facts come from `data_collector_new/internal/collector/collector.go`, and
the reference data confirms them:

1. **Dynamic rows are written on the falling edge of `[state-trigger]`.** Only
   tanks 1, 3 and 6 have triggers (`31|0`, `31|2`, `31|4`). That is why
   `dynamic_data_2/4/5` are empty. Rows are about 4 s apart, and all tanks share
   the same timestamp.
2. **Settings are written with every persisted row**, not only when they change.
   This includes the setpoints of the tanks that have no trigger.
3. **Alarms are recorded on the rising edge only** (`lastAlarms`: record when
   the previous state was false). So an alarm row means *alarm became active at
   t*, not *alarm is active at t*. The DB does not record when an alarm cleared.
   In the reference data the alarms fall about 2 s *between* data rows.
4. **Dosing is recorded on the falling edge of the Q bit.** The dosage is read
   from `加药量地址` at that moment. Dosing timestamps coincide with data rows.
5. Values are stored in **engineering units**: `2.4` g/L, not the raw `240`.
   The CSV multiplier and type (U16/S16/…) are applied when writing, exactly as
   the JSON `points` are handled today.

## 4. CLI

```
s7-sim -config topway-data0-s7.csv -db topway-data0-s7-mes.db [options]
```

| Flag | Default | Meaning |
|---|---|---|
| `-db <path>` | — | Turns on SQLite mode. Cannot be combined with `-data`. |
| `-db-inspect` | false | Print the schema and mapping report (§6), then exit. |
| `-db-from`, `-db-to` | full range | Time window (RFC3339 or `2006-01-02 15:04:05`, local time). |
| `-replay` | `realtime` | `realtime`: keep the original time gaps between events. `fixed`: one data row per `-interval`. |
| `-speed` | 1.0 | Speed multiplier in realtime mode (e.g. `4` = 4× faster). |
| `-interval` | 1s | Step length in fixed mode. |
| `-max-gap` | 10s | Longer gaps between events are cut to this length. |
| `-loop` | true | Start again from the beginning after the last event. |
| `-pulse` | true | Drive trigger and dosing bits as edges, so a downstream collector records the replay (§7.3). |
| `-pulse-lead` | max(1s, cycle) | How long trigger and dosing bits stay high before the recorded time. |
| `-alarm-hold` | 5s | The longest an alarm bit stays on after a raise. |
| `-db-map <file>` | — | Optional overrides for keys that do not match (§5.2). |

The DB is opened read-only (`file:<path>?mode=ro`).

## 5. Mapping

### 5.1 Automatic (exact) matching, the normal path

Keys are normalised before matching: trim, lowercase the register type, and
treat `:` and `|` as the same character.

| DB entity | Config target |
|---|---|
| column `T-reg-DB\|off` | `PointDef` with `TankID==T`, `RegisterType==reg`, `DB==DB`, `ByteOffset==off` |
| settings `data_address` | that point's `ControlDB/Off`, `LowDB/Off`, `HighDB/Off` (`cv`/`la`/`ha`). `hc`/`lc` are ignored, see §11. |
| alarm `reg-DB\|off` + bit | `AlarmDef` with the same DB/offset/bit. Fallback: an exact match on `message`. |
| dosing `tank_id` + `name` | `DosingDef` (`cfg.doseByName` is keyed by name, so it becomes tank+name) plus the related `[state-dosing]` Q bit. Fallback: trigger address + bit. |

A key that is still unmatched is reported once at startup and skipped during
replay.

### 5.2 Override file (`-db-map`, JSON, optional)

This is for DBs recorded with an older or different config, e.g. after a point
was moved to another offset.

```json
{
  "points": { "1-dbw-10|0": "1:游离碱" },
  "alarms": { "dbx-30|13#0": "硅烷槽EC值过高" },
  "dosing": { "1:色浆": "1:色浆" },
  "scale":  { "1-dbw-10|0": 1.0 }
}
```

Explicit entries take priority over automatic matching. `scale` multiplies the
DB value before the CSV multiplier is applied.

## 6. Inspect report (`-db-inspect`)

Expected output for the reference DB:

```
DB topway-data0-s7-mes.db  dataset_id=c12b1900-…
range 2026-07-16 17:02:33 .. 17:17:27 (14m54s)  median row interval 4.0s

dynamic_data_1  rows=189  cols=3
  1-dbw-10|0   -> 1:游离碱  dbw U16 x100  min=1.1 max=2.9  nulls=0
  1-dbw-10|8   -> 1:总碱    dbw U16 x100  ...
  1-dbw-10|16  -> 1:温度    dbw S16 x10   ...
dynamic_data_2  rows=0   (no [state-trigger] for tank 2)
...
settings_history  rows=2444 keys=13 mapped=13
alarms            rows=188  keys=3  mapped=3   (rising-edge events)
dosing_records    rows=125  keys=2  mapped=2   (溶剂 x63, 色浆 x62)
config points with no DB data: 2:*, 4:*, 5:*  (setpoints only)
unmapped: none
```

Schema discovery only uses `sqlite_master` and `PRAGMA table_info`. The only
names that are fixed in code are the four table names and the
`dynamic_data_<n>` pattern.

## 7. Replay engine

### 7.1 Event timeline (streaming)

Every DB row becomes a timestamped **event**:

| Source | Event |
|---|---|
| `dynamic_data_T` row | `DataRow{tank, values}` |
| `settings_history` rows sharing a timestamp | `Settings{key→cv/la/ha}` |
| `alarms` row | `AlarmRaise{alarm}` |
| `dosing_records` row | `Dose{def, amount}` |

Each source is read through a paged cursor
(`WHERE timestamp >= ? ORDER BY timestamp, id LIMIT 500`). A **k-way merge**
on the parsed time (`time.Parse(RFC3339Nano)`, not string compare) produces one
ordered stream, so memory use stays flat for large DBs. Events with exactly the
same timestamp form one **step**.

### 7.2 Applying a step

For each step at time `t`, build a `Frame` and call `applyFrame`:

1. `Settings` → `CV/LA/HA`. Only values that changed are written.
2. `DataRow` → `Points`. A NULL keeps the previous value. A row whose values
   are all NULL is still kept, because it still marks a trigger edge.
3. `Dose` → `Dosing` amount (written to `加药量地址`), then the Q bit edge (§7.3).
4. `AlarmRaise` → set the alarm bit, then clear it later (§7.3).

Required change to `applyFrame`: today it *clears every alarm* on each frame.
For DB frames, alarm handling switches to explicit set/clear lists. A new field
`Frame.AlarmsOff []string` is added, plus a flag `Frame.KeepAlarms bool`. The
JSON behaviour stays as it is.

Timing: in `realtime` mode, sleep `min(t_next − t, max-gap) / speed` between
steps. In `fixed` mode, each distinct timestamp is one step, and steps are
`-interval` apart. At the end of the data,
if `-loop` is set, the cursors are reopened at `-db-from`. Otherwise the last
state is held. The source timestamp is logged every N steps so you can see
where the replay is.

### 7.3 Edges (`-pulse`), so a downstream collector records the replay

The goal is that pointing `data_collector_new` at the simulator produces a DB
equivalent to the source DB.

| Event | Bit sequence |
|---|---|
| `DataRow` for tank T | Raise T's `[state-trigger]` bit at the *previous* step, or `pulse-lead` (default 1 s) before `t`. Write the values, then **lower the bit at `t`**. The collector writes the row on that falling edge. |
| `Dose` | Write the amount, raise the Q bit `pulse-lead` before `t`, and lower it at `t`. |
| `AlarmRaise` | Set the bit at `t`. Clear it after `alarm-hold` (default: just before the next raise of the same alarm, but at most 5 s), so the next raise is a new rising edge. |

Implementation: a small **scheduler** (a min-heap of `{at, action}`) sits
alongside the event stream. A raise "before t" is scheduled when the step that
precedes it is processed. Because of this, the engine looks one step ahead.
The scheduler runs on the same goroutine and uses the same `dbMu` lock as
`applyFrame`.

The collector polls every 1000 ms (`[server]` 采样周期), so each edge must last
at least one poll cycle. `pulse-lead` defaults to `max(1s, cycle)`.

With `-pulse=false`, trigger and dosing bits stay at their CSV defaults, alarms
are only set, and values are written. This is enough for HMI viewing.

Trigger rows whose tank ID is computed (`槽ID计算方式=1` with `槽ID地址`) are
not in this config. In v1 they are only pulsed, and the tank-id word is not
written (future work).

## 8. Code layout

```
main.go           flags; frame source = JSON | SQLite; main loop drives the source
source.go         type FrameSource interface { Next() (Frame, time.Duration, error); Close() error }
                  jsonSource wraps the existing rotation (and finally honours Frame.WaitMS)
sqlite_schema.go  discovery: tables, columns, key parsing, stats
sqlite_mapping.go exact match + -db-map overrides -> resolved mapping
sqlite_source.go  cursors, k-way merge, steps, edge scheduler
sqlite_inspect.go -db-inspect report
```

Driver: **`modernc.org/sqlite`** (pure Go). It adds no second C toolchain
dependency next to libsnap7. The alternative, `mattn/go-sqlite3`, also works
because cgo is already in use, but its first build is slower.

## 9. Errors and edge cases

| Case | Behaviour |
|---|---|
| No `dynamic_data_*` table | Fatal: "not a collector DB". |
| No mapped keys at all | Fatal, with a hint to run `-db-inspect` and check `-config`. |
| Some keys unmapped | Warn once with a list, then skip them. |
| NULL value | Keep the previous value. |
| Negative value into U16 / overflow | Existing `writeValue` clamping. Out-of-range values are counted and reported. |
| Unparsable timestamp | Skip the row and count it. |
| Empty window after `-db-from`/`-db-to` | Fatal. |
| Tank with an empty table (2/4/5) | Its points stay at 0. Its setpoints are still replayed from `settings_history`. |
| DB still being written by a collector | Read-only open. New rows are picked up on the next loop pass. |

## 10. Testing

- **Unit tests**: key parsing and normalisation (`|` vs `:`), the mapping
  priority, the k-way merge with empty and uneven tables, step grouping, and
  the edge scheduler ordering.
- **Fixture DB**: the test builds a small DB with the collector's
  `CREATE TABLE` statements and checks the generated frames and edge timings.
- **Golden data**: replay `topway-data0-s7-mes.db` with `-speed 20 -loop=false`,
  read DB10/DB20/DB30/Q with `../s7poll`, and compare with the DB values after
  applying the CSV multiplier (e.g. `1-dbw-10|0 = 2.4` → DB10.DBW0 = 240).
- **Round trip**: run `data_collector_new` against the replay with `-pulse` on.
  The new DB should have the same row count per tank, the same values, the
  same alarm and dosing counts, and timestamp spacing within ±1 poll cycle.

## 11. Decisions (2026-11-05)

1. **Alarm duration:** each raise is held until just before the next raise of
   the same alarm, at most 5 s (`-alarm-hold`, default 5s). This also applies
   with `-pulse=false`, so alarms never stay latched on.
2. **`hc`/`lc`:** skipped. They carry no useful information for the simulator.
3. **Default replay mode:** `realtime` at 1×.
4. **`-pulse`:** on by default.

## 12. Implementation notes

- Config fixes that were needed:
  - `[state-*]` Q addresses that are a single number (e.g. `8`) are now parsed
    as the Q *byte*. Before, they were parsed as DB 8 / byte 0, which left
    phantom bits Q0.x set in JSON mode.
  - `StateDef` now has `Kind` and `TankID`. `DosingDef.Trigger` links each
    dosing amount to its Q bit. Dosing is also indexed by `"<tank>:<name>"`.
- `Frame.KeepAlarms` (not exposed in JSON) turns off the "clear all alarms"
  step. DB frames drive alarm bits through `States`.
- `snap7server.go` sets `#cgo CFLAGS: -std=gnu17`. GCC 15 defaults to C23, and
  C23 rejects `typedef int bool` in `snap7.h`.
- Round-trip check (2026-11-05): `data_collector_new` was pointed at a 60 s
  replay of `topway-data0-s7-mes.db`. It re-recorded 15/15/15 rows for tanks
  1/3/6, 15 alarms and 10 dosing records. The values were identical, and the
  spacing matched the source within one poll cycle.
