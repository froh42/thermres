# thermres – thermal resistance (CPU temperature & power logger)

## What this is

A Go tool that samples CPU temperatures, RAPL energy counters, frequency, and
power-governor state once per second and logs everything into a SQLite database
for long-term analysis of cooling efficiency.

## Context

The user is planning to repaste a Lenovo P1 Gen 2 (Linux Mint).  Before doing
that they want to collect a baseline of thermal behaviour so they can compare
afterwards.  The tool was developed on a Dell XPS (similar Intel H-series CPU)
but the sensors interface is the same.

## Schema

`schema_version` – migration tracking (v1 current)
`samples` – one row per tick:
  ts, pkg_temp_c, core_temps (JSON), pkg_energy, psys_energy, dram_energy,
  freq_mhz, governor, platform_profile, power_w (computed from RAPL deltas)

## Current status

- The binary is fully functional and tested.
- Writes to `~/.local/share/thermres/thermres.db` by default, or `--db <path>`.
- RAPL may need setuid-root (the tool warns at startup).

## If you get here as an agent

- The user may ask you to analyse collected data, extend the schema, add
  visualisation, or run comparisons.
- The database lives in `~/.local/share/thermres/` by default.
- Schema upgrades: bump `schemaVersion` and add migration logic in
  `ensureSchema()`.
- If the tool is restarted after a gap, power computation picks up from the
  last stored row (so there's no spike from the idle gap).
