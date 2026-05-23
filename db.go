// SQLite database layer for thermres.
//
// Uses modernc.org/sqlite — a pure-Go SQLite driver that needs no C compiler.
//
// Go database note: database/sql is Go's standard SQL interface.
// You open a connection, then call Query/Exec with placeholder syntax (?).
// Rows are scanned into Go variables with row.Scan(&dest1, &dest2, ...).

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // driver registration (init() runs, registers "sqlite")
)

const schemaVersion = 5

// Schema DDL — same layout as the Python version.
const createTablesSQL = `
CREATE TABLE IF NOT EXISTS schema_version (
    version    INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS samples (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    ts               REAL    NOT NULL,
    sample_type      TEXT    NOT NULL DEFAULT 'normal',
    pkg_temp_c       REAL,
    core_temps       TEXT,
    pkg_energy       INTEGER,
    psys_energy      INTEGER,
    dram_energy      INTEGER,
    freq_mhz         REAL,
    governor         TEXT,
    platform_profile TEXT,
    power_w          REAL,
    tag              TEXT
);

CREATE INDEX IF NOT EXISTS idx_samples_ts ON samples(ts);

CREATE TABLE IF NOT EXISTS events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ts          REAL    NOT NULL,
    event_type  TEXT    NOT NULL,
    message     TEXT
);

CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts);
`

// Sample holds one tick of sensor readings before DB insertion.
// Pointer fields (*float64 etc.) mean "nullable" — nil maps to SQL NULL.
type Sample struct {
	TS              float64
	SampleType      string // "normal", "suspend", "gap_skip"
	PkgTempC        *float64
	CoreTemps       []float64
	PkgEnergy       *int64
	PsysEnergy      *int64
	DramEnergy      *int64
	FreqMHz         *float64
	Governor        *string
	PlatformProfile *string
	PowerW          *float64
	Tag             *string
}

// LastSample holds the most recent row from a previous run, used to
// resume power computation without a spike after a restart gap.
type LastSample struct {
	TS         float64
	PkgEnergy  *int64
	PsysEnergy *int64
	DramEnergy *int64
}

// ─────────────────────────────────────────────────────────────────

// initDB opens (or creates) the SQLite database, applies schema
// migrations, and returns the connection handle.
func initDB(path string) (*sql.DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	// sql.Open("sqlite", path) uses the driver registered by modernc.org/sqlite.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Performance pragmas (same as Python version).
	mustExec(db, "PRAGMA journal_mode=WAL")
	mustExec(db, "PRAGMA synchronous=NORMAL")

	if err := ensureSchema(db); err != nil {
		db.Close()
		return nil, err
	}

	return db, nil
}

// ensureSchema creates tables and runs one-shot migrations.
// Migrations are tracked in the schema_version table by version number.
func ensureSchema(db *sql.DB) error {
	if _, err := db.Exec(createTablesSQL); err != nil {
		return fmt.Errorf("create tables: %w", err)
	}

	var currentVer int
	err := db.QueryRow(
		"SELECT COALESCE(MAX(version), 0) FROM schema_version",
	).Scan(&currentVer)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	if currentVer < schemaVersion {
		log.Printf("Migrating DB schema v%d → v%d", currentVer, schemaVersion)

		// v1 → v2: add tag column for --tag filtering.
		mustExecWhen(db, currentVer, 1, "ALTER TABLE samples ADD COLUMN tag TEXT")
		// v2 → v3: events table (handled by CREATE TABLE IF NOT EXISTS in DDL).
		// v3 → v4: add sample_type column.
		if currentVer < 4 {
			mustExecWhen(db, currentVer, currentVer,
				"ALTER TABLE samples ADD COLUMN sample_type TEXT NOT NULL DEFAULT 'normal'")
		}
		// v4 → v5: retroactively mark anomaly rows in old data.
		// Pass 1: rows where the gap from the previous row exceeds 1.5 s are
		//         boundary rows — the RAPL delta is over a huge time span and
		//         the resulting power_w is garbage.
		// Pass 2: the row immediately after a boundary often has a sub-second
		//         gap (thermres fires a tick then the ticker fires quickly) and
		//         its delta is computed from the reset-baseline, also bogus.
		// Only update rows still marked 'normal' so we don't overwrite the
		// typed rows written by the newer code.
		if currentVer < 5 {
			mustExecWhen(db, currentVer, currentVer, `
				UPDATE samples SET sample_type = 'anomaly'
				WHERE sample_type = 'normal'
				  AND id IN (
				    SELECT id FROM (
				      SELECT id,
				             ts - LAG(ts) OVER (ORDER BY id) AS gap
				      FROM samples
				    ) WHERE gap > 1.5 OR gap IS NULL
				  )`)
			// Pass 2 runs after pass 1 so the updated sample_type values
			// are visible for the LAG() lookup.
			mustExecWhen(db, currentVer, currentVer, `
				UPDATE samples SET sample_type = 'anomaly'
				WHERE sample_type = 'normal'
				  AND id IN (
				    SELECT id FROM (
				      SELECT id,
				             LAG(sample_type) OVER (ORDER BY id) AS prev_type,
				             ts - LAG(ts) OVER (ORDER BY id)     AS gap
				      FROM samples
				    ) WHERE prev_type = 'anomaly' AND gap < 1.5
				  )`)
		}

		if _, err := db.Exec(
			"INSERT INTO schema_version (version) VALUES (?)",
			schemaVersion,
		); err != nil {
			return fmt.Errorf("record migration: %w", err)
		}
	} else if currentVer > schemaVersion {
		return fmt.Errorf(
			"DB schema v%d is newer than this tool (v%d) — upgrade required",
			currentVer, schemaVersion,
		)
	}
	return nil
}

// insertSample writes one row into the samples table.
func insertSample(db *sql.DB, s *Sample) error {
	var coreJSON *string
	if len(s.CoreTemps) > 0 {
		b, err := json.Marshal(s.CoreTemps)
		if err != nil {
			return fmt.Errorf("marshal core temps: %w", err)
		}
		s := string(b)
		coreJSON = &s
	}

	_, err := db.Exec(
		`INSERT INTO samples
		 (ts, sample_type, pkg_temp_c, core_temps, pkg_energy, psys_energy, dram_energy,
		  freq_mhz, governor, platform_profile, power_w, tag)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.TS,
		s.SampleType,
		s.PkgTempC,
		coreJSON,
		s.PkgEnergy,
		s.PsysEnergy,
		s.DramEnergy,
		s.FreqMHz,
		s.Governor,
		s.PlatformProfile,
		s.PowerW,
		s.Tag,
	)
	return err
}

// getLastSample reads the newest row (for delta computation after restart).
// Returns nil when the table is empty.
func getLastSample(db *sql.DB) (*LastSample, error) {
	row := db.QueryRow(
		"SELECT ts, pkg_energy, psys_energy, dram_energy " +
			"FROM samples ORDER BY ts DESC LIMIT 1",
	)

	var ls LastSample
	var pkg, psys, dram sql.NullInt64

	if err := row.Scan(&ls.TS, &pkg, &psys, &dram); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	if pkg.Valid {
		ls.PkgEnergy = &pkg.Int64
	}
	if psys.Valid {
		ls.PsysEnergy = &psys.Int64
	}
	if dram.Valid {
		ls.DramEnergy = &dram.Int64
	}
	return &ls, nil
}

// countSamples returns the total number of recorded rows.
func countSamples(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow("SELECT COUNT(*) FROM samples").Scan(&n)
	return n, err
}

// insertEvent writes a row into the events table.
func insertEvent(db *sql.DB, eventType, message string) {
	ts := float64(time.Now().UnixNano()) / 1e9
	if _, err := db.Exec(
		"INSERT INTO events (ts, event_type, message) VALUES (?, ?, ?)",
		ts, eventType, message,
	); err != nil {
		log.Printf("ERROR insert event: %v", err)
	}
}

// defaultDBPath returns ~/.local/share/thermres/thermres.db.
func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "thermres.db" // fallback to CWD
	}
	return filepath.Join(home, ".local", "share", "thermres", "thermres.db")
}

// ─────────────────────────────────────────────────────────────────
//  Helper
// ─────────────────────────────────────────────────────────────────

// mustExec panics on error — safe for fixed PRAGMAs that never fail
// under normal conditions.
func mustExec(db *sql.DB, sql string) {
	if _, err := db.Exec(sql); err != nil {
		panic(fmt.Sprintf("sql: %s — %v", sql, err))
	}
}

// mustExecWhen runs sql only when currentVer == targetVer.
// Ignores "duplicate column" errors so the migration is idempotent.
func mustExecWhen(db *sql.DB, currentVer, targetVer int, sql string) {
	if currentVer != targetVer {
		return
	}
	if _, err := db.Exec(sql); err != nil {
		// "duplicate column name" is harmless on re-run — ignore.
		log.Printf("migrate (ignoring): %s — %v", sql, err)
	}
}
