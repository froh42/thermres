# thermres — CPU thermal resistance logger

Logs CPU package temperature, RAPL energy counters, frequency, and power-governor
state once per second into a SQLite database.  Designed to measure cooling
efficiency changes (e.g. before vs. after repasting a laptop).

## Architecture

The system is intentionally split into two separate concerns:

**`thermres` (collector)** — records raw sensor data as faithfully as possible.
It performs only the minimal processing needed to detect anomalies *in its own
operation*: wraparound of RAPL energy counters, suspend/resume events detected
via monotonic vs. wall-clock divergence, and gaps longer than the expected
sampling interval.  These events are marked in the database but the raw records
are kept.  The collector does not interpret or filter data.

**`thermres-plot` (analyser)** — all analysis and visualisation logic lives
here.  It can be extended with new analysis modes without touching the
collector.  Different flags produce fundamentally different views of the same
underlying data.

**Data quality philosophy**: keep every record, including anomalous ones.  Each
row carries a `sample_type` that lets the analysis layer decide what to include.
Nothing is ever deleted; bogus rows are simply classified so they can be
excluded.  This preserves the ability to re-analyse historical data with
different filters.

### Analysis modes in `thermres-plot`

| Mode | Flags | What it shows |
|------|-------|---------------|
| Scatter | *(default)* | Raw pkg temp vs power, one point per sample |
| Time-binned | `--time-bin 5m` | Mean temp/power averaged over fixed time windows |
| Power-binned | `--power-bin 1` | Mean temp at each N-watt power level (line chart) |
| Thermal resistance | `--rolling N --thermal-resistance` | Equilibrium R_th estimate via OLS on rolling averages |

The `--warmup N` flag (default 30 s) excludes rows that fall within N seconds
after any non-normal sample (startup, suspend, gap).  This focuses analysis on
periods where the system is already in thermal equilibrium rather than warming
up from cold.

## Quick start

```bash
make
sudo chown root thermres && sudo chmod u+s thermres
./thermres --verbose --tag pre-repaste
```

The setuid bit is needed because RAPL `energy_uj` files are root-owned.  The
binary opens them, then **permanently drops privileges** via `LockOSThread` +
`Setuid`/`Setgid` before entering the sampling loop.

Alternatively the `install.sh` script builds and sets up setuid in one step:

```bash
./install.sh
./thermres --verbose
```

## Build

```bash
make          # builds both thermres and thermres-plot
make clean    # removes both binaries
```

## Usage

### Logger (`thermres`)

```
Usage: thermres [flags]

  -db string       SQLite database path (default: ~/.local/share/thermres/thermres.db)
  -interval float  Sampling interval in seconds (default 1)
  -tag string      Optional tag written into every row (e.g. 'pre-repaste')
  -verbose         Log each sample to stderr
```

### Plotter (`thermres-plot`)

```
Usage: thermres-plot [flags]

  -db string        SQLite database path
  -tag string       Comma-separated tag(s) for overlay series
  -since string     Start time (RFC3339, e.g. '2025-01-01T00:00:00Z')
  -until string     End time
  -time-bin string  Aggregate over time windows (e.g. '5m', '1h')
  -power-bin float  Aggregate into N-watt power buckets (produces line chart)
  -warmup int       Exclude rows within N seconds after any anomaly (default 30)
  -rolling int      Apply rolling N-second time average before analysis
  -thermal-resistance  Print thermal resistance estimate (°C/W) and exit
  -output string    Save PNG instead of rendering to terminal
```

#### Examples

```bash
# Scatter plot of all data, terminal display
thermres-plot

# Overlay pre- and post-repaste series
thermres-plot --tag pre-repaste,post-repaste

# Power-bin line chart (avg temp per 1 W bucket, 5-minute warmup filter)
thermres-plot --power-bin 1 --warmup 300 --tag pre-repaste,post-repaste

# 15-minute time averages, save to PNG
thermres-plot --time-bin 15m --output chart.png

# Estimate thermal resistance from equilibrium data
thermres-plot --warmup 60 --rolling 2400 --thermal-resistance
```

## Database

Default location: `~/.local/share/thermres/thermres.db`

### Schema

`samples` — one row per tick:

| Column | Type | Description |
|--------|------|-------------|
| `ts` | DATETIME | Timestamp (ISO 8601) |
| `pkg_temp_c` | REAL | CPU package temperature (°C) |
| `core_temps` | TEXT | Per-core temps as JSON array |
| `pkg_energy` | REAL | Package RAPL energy (µJ) |
| `psys_energy` | REAL | Platform RAPL energy (µJ) |
| `dram_energy` | REAL | DRAM RAPL energy (µJ) |
| `freq_mhz` | REAL | Current CPU frequency (MHz) |
| `governor` | TEXT | Scaling governor |
| `platform_profile` | TEXT | Platform power profile |
| `power_w` | REAL | Computed power draw (W); NULL for non-normal rows |
| `tag` | TEXT | Optional label for filtering |
| `sample_type` | TEXT | Row classification (see below) |

#### `sample_type` values

| Value | Meaning |
|-------|---------|
| `normal` | Good sample; used by all analysis |
| `startup` | First sample after process start; power_w not meaningful |
| `suspend` | Short suspend detected (monotonic/wall-clock divergence); energy delta invalid |
| `gap_skip` | Gap longer than max allowed; row skipped entirely |
| `anomaly` | Retroactively marked row (pre-dates sample_type column) |

Only `normal` rows are used for plotting and thermal resistance estimation.

The `power_w` column is computed from RAPL energy deltas.  If the tool is
restarted after a gap the computation picks up from the last stored row so
there is no artificial spike.

## Typical workflow

1. **Collect baseline**: `thermres --verbose --tag pre-repaste`
2. Repaste the CPU
3. **Collect after**: `thermres --verbose --tag post-repaste`
4. **Compare power/temp curve**: `thermres-plot --tag pre-repaste,post-repaste --power-bin 1`
5. **Compare thermal resistance**: `thermres-plot --tag pre-repaste,post-repaste --rolling 2400 --thermal-resistance`

The thermal resistance (°C/W) is the slope of the temperature vs. power
regression after rolling-average smoothing filters out thermal mass transients.
Larger rolling windows give more accurate results — R² typically reaches 0.96+
at 2400 s.  After a successful repaste you expect R_th to decrease noticeably.

For a laptop with dried-out paste you typically see idle temps drop 20–30 °C
and a lower °C/W curve under load.

## Build notes

- Requires Go 1.26.3+ (needed by `go-termimg`)
- Pure-Go SQLite via `modernc.org/sqlite` — no CGo
- Static binary (set `CGO_ENABLED=0`)
- Chart rendering via `go-analyze/charts/chartdraw`
- Terminal image display via `blacktop/go-termimg` (supports Kitty, sixel,
  iTerm2, and halfblock fallback)
