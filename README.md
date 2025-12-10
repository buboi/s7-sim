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
- `alarms`: list of alarm text from the CSV to raise in that frame. All other alarms are cleared before applying the listed ones.

Frames rotate every `interval_ms` (default 2000ms). Adjust the JSON or add more frames to simulate different behaviors.

## Notes
- DB sizes are auto-grown based on the CSV offsets; point words land in DB10, control/setpoint offsets in DB20, alarms/states in DB30.
- The server uses `StartTo` from `gos7`, so ensure the `-listen` IP exists on the host NIC to avoid bind failures.
