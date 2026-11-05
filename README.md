# Snap7 simulator from CSV

This spins up a Snap7 server that mirrors the layout described in `topway-data0-s7.csv`, populates DB10/20/30 values, and cycles through sample values so any S7 client can read changing data.

## Prereqs

- Go 1.21+
- Snap7 development package (`libsnap7` and headers) available on your machine; the server bindings use cgo to call `Srv_*` APIs.

## Setup & run

```bash
# run the simulator (ensure libsnap7 is installed on the system)
go run . -config topway-data0-s7.csv -data simdata/sample_values.json \
  -listen 0.0.0.0 -port 1102
```

Notes:

- Default port is taken from the CSV `[server]` connection string `IP|rack|slot|port` (port optional). If absent it falls back to 1102 to avoid root. Use `-port 102` only if you can bind low ports (root or `setcap 'cap_net_bind_service=+ep' $(which go)`).
- Leave `-listen` empty to let Snap7 pick an interface, or specify a local IP/NIC address; `0.0.0.0` usually works.

Flags:

- `-config` CSV file following the existing format.
- `-data` JSON file with simulation frames (see below).
- `-listen` address to bind; defaults to the address in the CSV. Use `0.0.0.0` if unsure.

## Simulation data

`simdata/sample_values.json` contains three frames (startup, low-alkali alarm, silane high EC). Each frame exposes keys:

- `states`: map of `DB:BYTE:BIT` to 0/1 for DB30 bits.
- `points`: map of `<tankId>:<pointName>` to engineering values; code multiplies by the CSV multiplier before writing WORDs.
- `cv`, `la`, `ha`, `lc`, `hc`: map of `<tankId>:<pointName>` to setpoint values (control value, low/high alarm, control low/high). They are written to the point's 控制值/下限值/上限值/控制下限值/控制上限值 offsets, with the point's multiplier and type.
- `alarms`: list of alarm text from the CSV to raise in that frame. All other alarms are cleared before applying the listed ones.

Frames rotate every `interval_ms` (default 2000ms). Adjust the JSON or add more frames to simulate different behaviors.

## SQLite replay mode

Instead of `-data`, replay real data recorded by `data_collector_new` from an S7 PLC:

```bash
# check how the DB maps onto the config (no server started)
go run . -config topway-data0-s7.csv -db topway-data0-s7-mes.db -db-inspect

# replay with the original timing, looping
go run . -config topway-data0-s7.csv -db topway-data0-s7-mes.db -port 1102
```

- DB columns such as `1-dbw-10|0` are matched to `[point]` rows by tank and address. `settings_history` drives CV/LA/HA/LC/HC, `alarms` raise alarm bits, and `dosing_records` write dosing amounts.
- With `-pulse` (on by default), `[state-trigger]` and dosing Q bits rise about 1 s before each recorded time and fall at it. Alarm bits rise at the recorded time and clear within `-alarm-hold` (5s). A collector pointed at the simulator re-records the same data.
- Useful flags: `-speed 4`, `-replay fixed -interval 1s`, `-db-from/-db-to "2026-07-16 17:05:00"`, `-max-gap 10s`, `-loop=false`, `-db-map overrides.json`.
- Design: [docs/design/sqlite-replay-mode.md](docs/design/sqlite-replay-mode.md).

## S7-200 SMART mode

Emulates an S7-200 SMART CPU, so `data_collector_new` configs with `protocol=s7smart` can be tested. It turns on automatically when the CSV `[server]` row says `s7smart`, or force it with `-smart` (and `-smart=false` to turn it off).

```bash
go run . -config topway-data0-s7smart.csv -data simdata/sample_values_smart.json -port 1102
```

`topway-data0-s7smart.csv` and `simdata/sample_values_smart.json` are the `topway-data0-s7` sample moved into DB1 (DB10→V0, DB20→V1000, DB30→V2000, DB31→V3000).

What changes compared with the normal S7 mode:

- **TSAP check.** Only connection requests with remote TSAP `0x0301` are accepted, which is what the collector sends with `protocol=s7smart`. Any other request is refused and the connection closed, like a real SMART CPU, and the simulator logs `smart: rejected ... remote TSAP 0x0102`. A collector misconfigured as `protocol=s7` therefore fails here too. Use `-smart-tsap any` to accept every TSAP, or `-smart-tsap 0x....` to accept a different one.
- **PDU 240.** The negotiated PDU is 240 bytes, like the real CPU, so the collector's request chunking is exercised.
- **Memory layout.** V memory is the only data block, exposed as DB1 and sized by `-smart-vsize` (8192 bytes by default; set it to your CPU's V memory size). Reads past the end fail, as they do on the PLC. The CPU also has I and Q (128 bytes each) and M (32 bytes).
- **Startup check.** Every address in the CSV, and every state/word key in the `-data` frames, must be in DB1 (for example `1|1002`, or `1:3000:0` in JSON) or in Q0–Q127. If not, the simulator refuses to start and lists what is wrong.

Snap7 itself accepts any TSAP, so in this mode a small proxy listens on the public port and checks the TSAP, while Snap7 runs on a private loopback port.

## Notes

- DB sizes are auto-grown based on the CSV offsets; point words land in DB10, control/setpoint offsets in DB20, alarms/states in DB30.
- The server uses `StartTo` from `gos7`, so ensure the `-listen` IP exists on the host NIC to avoid bind failures.
