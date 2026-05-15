# thermres — CPU thermal resistance logger

Logs CPU package temperature, RAPL energy counters, frequency, and power-governor
state once per second into a SQLite database.  Designed to measure cooling
efficiency changes (e.g. before vs. after repasting a laptop).

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

  -db string       SQLite database path
  -tag string      Comma-separated tag(s) for overlay series
  -since string    Start time (RFC3339, e.g. '2025-01-01T00:00:00Z')
  -until string    End time
  -time-bin string Aggregate over time windows (e.g. '5m', '1h')
  -power-bin float Aggregate into N-watt power buckets (produces line chart)
  -output string   Save PNG instead of rendering to terminal
```

#### Examples

```bash
# Scatter plot of all data, terminal display
thermres-plot

# Overlay pre- and post-repaste series
thermres-plot --tag pre-repaste,post-repaste

# Power-bin line chart (avg temp per 5 W bucket)
thermres-plot --power-bin 5 --tag pre-repaste,post-repaste

# 15-minute time averages, save to PNG
thermres-plot --time-bin 15m --output chart.png
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
| `power_w` | REAL | Computed power draw (W) |
| `tag` | TEXT | Optional label for filtering |

The `power_w` column is computed from RAPL energy deltas.  If the tool is
restarted after a gap the computation picks up from the last stored row so
there is no artificial spike.

## Typical workflow

1. **Collect baseline**: `thermres --verbose --tag pre-repaste`
2. Repaste the CPU
3. **Collect after**: `thermres --verbose --tag post-repaste`
4. **Compare**: `thermres-plot --tag pre-repaste,post-repaste --power-bin 5`

For a laptop with dried-out paste you typically see idle temps drop 20–30 °C
and a flatter °C/W curve under load.

## Build notes

- Requires Go 1.26.3+ (needed by `go-termimg`)
- Pure-Go SQLite via `modernc.org/sqlite` — no CGo
- Static binary (set `CGO_ENABLED=0`)
- Chart rendering via `go-analyze/charts/chartdraw`
- Terminal image display via `blacktop/go-termimg` (supports Kitty, sixel,
  iTerm2, and halfblock fallback)
