// thermres – thermal resistance (CPU temperature & power logger).
//
// Logs CPU temperatures, RAPL energy counters, frequency, and power-governor
// state once per second into a SQLite database.
//
// Privilege model
// ---------------
// RAPL energy_uj files under /sys/class/powercap/intel-rapl/ are root-only
// (mode 0400).  This binary is designed setuid-root so it can open those
// files at startup, then permanently drop root before the sampling loop.
// The opened file handles remain usable after dropping privileges.
//
// Install:
//
//	CGO_ENABLED=0 go build -o thermres .
//	sudo chown root thermres && sudo chmod u+s thermres
//
// Usage:
//
//	./thermres                             # default db
//	./thermres --db /tmp/test.db           # custom path
//	./thermres -i 5                        # sample every 5 s
//	./thermres --verbose                   # log each sample
//	./thermres --tag pre-repaste           # tag rows for filtering

package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
)

func main() {
	// ── CLI arguments ──────────────────────────────────────────────
	// flag is Go's stdlib argument parser (like argparse but simpler).
	dbFlag := flag.String("db", "",
		"SQLite database path (default: ~/.local/share/thermres/thermres.db)")
	interval := flag.Float64("interval", 1.0,
		"Sampling interval in seconds")
	verbose := flag.Bool("verbose", false,
		"Log each sample to stderr")
	tag := flag.String("tag", "",
		"Optional tag written into every row (e.g. 'pre-repaste')")
	maxGap := flag.Float64("max-gap", 60,
		"Skip samples whose gap from previous exceeds this many seconds (avoids suspend artifacts)")
	flag.Parse()

	log.SetFlags(log.Ltime)

	dbPath := *dbFlag
	if dbPath == "" {
		dbPath = defaultDBPath()
	}

	// ── Privileged phase: open RAPL files ─────────────────────────
	// Pin this goroutine to one OS thread so setuid/setgid (which
	// are per-thread syscalls on Linux) affect the right thread.
	runtime.LockOSThread()

	raplDomains := discoverRapl()

	// If the intel-rapl directory exists but we couldn't open any
	// energy_uj files, it means we're not running setuid-root.
	self, _ := os.Executable()
	if self == "" {
		self = "thermres"
	}
	if len(raplDomains) == 0 {
		if _, err := os.Stat(raplBase); err == nil && syscall.Geteuid() != 0 {
			log.Fatalf("FATAL RAPL files at %s are root-only but we are not setuid-root.\n"+
				"       Install with:\n"+
				"       sudo chown root %s && sudo chmod u+s %s", raplBase, self, self)
		}
	}

	if syscall.Geteuid() == 0 {
		// Drop supplementary groups (removes e.g. the sudo group).
		if err := syscall.Setgroups([]int{syscall.Getgid()}); err != nil {
			log.Printf("WARN drop supplementary groups: %v", err)
		}
		// setgid + setuid set ALL ID sets (real, effective, saved)
		// when called from a privileged process — privileges are
		// permanently gone after this.
		if err := syscall.Setgid(syscall.Getgid()); err != nil {
			log.Fatalf("FATAL drop GID: %v", err)
		}
		if err := syscall.Setuid(syscall.Getuid()); err != nil {
			log.Fatalf("FATAL drop UID: %v", err)
		}
		log.Printf("INFO privileges dropped")
	}

	runtime.UnlockOSThread()

	// ── Discover other sensors ───────────────────────────────────
	coretemp := discoverCoretemp()
	raplOK := len(raplDomains) > 0

	platformProfile := readPlatformProfile()
	profileOK := platformProfile != nil

	log.Printf("INFO sensors: %s  RAPL energy: %s  platform_profile: %s",
		condStr(coretemp != nil, "OK", "MISSING"),
		condStr(raplOK, "OK", "UNAVAILABLE (root-only)"),
		condStr(profileOK, "OK", "N/A"),
	)

	// ── Database ──────────────────────────────────────────────────
	db, err := initDB(dbPath)
	if err != nil {
		log.Fatalf("FATAL database: %v", err)
	}
	defer db.Close()
	defer insertEvent(db, "shutdown", "thermres stopped")

	tagStr := *tag
	if tagStr == "" {
		tagStr = "(none)"
	}
	insertEvent(db, "startup", fmt.Sprintf(
		"thermres started — interval=%.1fs, max-gap=%.0fs, tag=%s",
		*interval, *maxGap, tagStr,
	))

	// ── Restore previous state ────────────────────────────────────
	// Read the last row so the first power_w computation doesn't
	// produce a spike from the idle gap.
	var prevTS float64
	var prevTime *time.Time
	var prevPkgEnergy, prevPsysEnergy, prevDramEnergy *int64
	first := true

	last, err := getLastSample(db)
	if err != nil {
		log.Printf("WARN read last sample: %v", err)
	}
	if last != nil {
		prevTS = last.TS
		prevPkgEnergy = last.PkgEnergy
		prevPsysEnergy = last.PsysEnergy
		prevDramEnergy = last.DramEnergy
		log.Printf("DEBUG resumed from prior sample at t=%.3f", prevTS)
	}

	// ── Look up RAPL domains ──────────────────────────────────────
	var pkgDom, psysDom, dramDom *RaplDomain
	for i := range raplDomains {
		switch raplDomains[i].Name {
		case "package-0":
			pkgDom = &raplDomains[i]
		case "psys":
			psysDom = &raplDomains[i]
		case "dram":
			dramDom = &raplDomains[i]
		}
	}

	// ── Signal handling ───────────────────────────────────────────
	// A context is cancelled when SIGINT / SIGTERM arrives.
	// The main loop exits when ctx.Done() fires.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("INFO signal %d received – shutting down", sig)
		cancel()
	}()

	// ── Main loop ─────────────────────────────────────────────────
	log.Printf("INFO logging to %s (interval=%.1fs)  Ctrl+C to stop",
		dbPath, *interval)

	tickDuration := time.Duration(*interval * float64(time.Second))
	ticker := time.NewTicker(tickDuration)
	defer ticker.Stop()

	// Take first sample immediately (ticker fires after 1 interval).
	tagVal := tag
	if *tag == "" {
		tagVal = nil // NULL in DB when unused
	}

	sampleAndLog(db, &SampleArgs{
		coretemp:        coretemp,
		pkgDom:          pkgDom,
		psysDom:         psysDom,
		dramDom:         dramDom,
		prevTS:          &prevTS,
		prevTime:        &prevTime,
		prevPkgEnergy:   &prevPkgEnergy,
		prevPsysEnergy:  &prevPsysEnergy,
		prevDramEnergy:  &prevDramEnergy,
		platformProfile: platformProfile,
		verbose:         *verbose,
		tag:             tagVal,
		maxGap:          *maxGap,
		first:           &first,
	})

	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			// Return below.
		case <-ticker.C:
			sampleAndLog(db, &SampleArgs{
				coretemp:        coretemp,
				pkgDom:          pkgDom,
				psysDom:         psysDom,
				dramDom:         dramDom,
				prevTS:          &prevTS,
				prevTime:        &prevTime,
				prevPkgEnergy:   &prevPkgEnergy,
				prevPsysEnergy:  &prevPsysEnergy,
				prevDramEnergy:  &prevDramEnergy,
				platformProfile: platformProfile,
				verbose:         *verbose,
				tag:             tagVal,
				maxGap:          *maxGap,
				first:           &first,
			})
		}
	}

	n, err := countSamples(db)
	if err == nil {
		log.Printf("INFO done – %d samples recorded", n)
	}
}

// ─────────────────────────────────────────────────────────────────
//  Sampling
// ─────────────────────────────────────────────────────────────────

// SampleArgs bundles everything sampleAndLog needs.
// prev* pointers are mutated (updated to current values after each tick).
type SampleArgs struct {
	coretemp        *CoretempSensors
	pkgDom          *RaplDomain
	psysDom         *RaplDomain
	dramDom         *RaplDomain
	prevTS          *float64
	prevTime        **time.Time // monotonic+wall clock snapshot of previous tick
	prevPkgEnergy   **int64
	prevPsysEnergy  **int64
	prevDramEnergy  **int64
	platformProfile *string
	verbose         bool
	tag             *string
	maxGap          float64
	first           *bool
}

// sampleAndLog reads all sensors, computes power, writes to DB,
// updates prev* pointers, and optionally prints to stderr.
func sampleAndLog(db *sql.DB, a *SampleArgs) {
	curr := time.Now()
	ts := float64(curr.UnixNano()) / 1e9

	// ── Suspend detection via monotonic vs wall-clock comparison ──
	// CLOCK_MONOTONIC (used by Go's Sub) freezes during suspend.
	// Wall clock (UnixNano) keeps advancing.  A significant difference
	// between the two deltas means the system was suspended mid-interval.
	//
	// Threshold: 200 ms covers normal scheduler jitter but catches even
	// very brief suspends (screen-off, s2idle).
	const suspendThresholdMs = 200
	var suspendedMs int64
	if *a.prevTime != nil {
		prev := *a.prevTime
		wallDeltaNs := curr.UnixNano() - prev.UnixNano()
		monoDeltaNs := int64(curr.Sub(*prev))
		suspendedMs = (wallDeltaNs - monoDeltaNs) / 1_000_000
	}
	suspended := suspendedMs >= suspendThresholdMs

	// ── Temperature ───────────────────────────────────────────────
	var pkgTempC *float64
	var coreTemps []float64
	if a.coretemp != nil {
		pkgTempC, coreTemps = readCoreTemps(a.coretemp)
	}

	// ── RAPL energy ──────────────────────────────────────────────
	// rawPkg/Psys/Dram hold the actual counter values for prev* updates.
	// pkgEnergy etc. may be set to nil for non-normal sample types.
	var pkgEnergy, psysEnergy, dramEnergy *int64
	var rawPkgEnergy, rawPsysEnergy, rawDramEnergy *int64

	if a.pkgDom != nil {
		v, ok := a.pkgDom.ReadEnergy()
		if ok {
			pkgEnergy = &v
			rawPkgEnergy = pkgEnergy
		}
	}
	if a.psysDom != nil {
		v, ok := a.psysDom.ReadEnergy()
		if ok {
			psysEnergy = &v
			rawPsysEnergy = psysEnergy
		}
	}
	if a.dramDom != nil {
		v, ok := a.dramDom.ReadEnergy()
		if ok {
			dramEnergy = &v
			rawDramEnergy = dramEnergy
		}
	}

	// ── Frequency / governor / profile ───────────────────────────
	freqMHz, governor := readFreqAndGovernor()

	// ── Power computation ─────────────────────────────────────────
	// Skip if suspended: the RAPL counter resets on resume, so the
	// delta would be garbage (either huge from wrap-correction, or
	// near-zero from a reset before the interval elapsed).
	var powerW *float64
	if !suspended && pkgEnergy != nil && a.pkgDom != nil {
		powerW = computePower(
			*a.prevPkgEnergy, pkgEnergy,
			a.pkgDom.MaxEnergy,
			*a.prevTS, ts,
		)
	}

	// ── Determine sample type ─────────────────────────────────────
	// "startup" : first sample after process start (power delta is unreliable)
	// "suspend" : monotonic/wall-clock divergence detected (short gap)
	// "gap_skip": wall-clock gap > maxGap (long suspend or process restart)
	// "normal"  : everything nominal
	tsGap := ts - *a.prevTS
	isFirst := *a.first
	gapTooLarge := !isFirst && *a.prevPkgEnergy != nil && tsGap > a.maxGap
	*a.first = false

	sampleType := "normal"
	switch {
	case isFirst:
		sampleType = "startup"
	case gapTooLarge:
		sampleType = "gap_skip"
	case suspended:
		sampleType = "suspend"
	}

	// Energy values are unreliable/misleading for non-normal samples:
	// on suspend the RAPL counter resets, on gap_skip the baseline is stale.
	// Store NULL so analyses don't accidentally use them as deltas.
	if sampleType != "normal" {
		pkgEnergy = nil
		psysEnergy = nil
		dramEnergy = nil
	}

	sample := &Sample{
		TS:              ts,
		SampleType:      sampleType,
		PkgTempC:        pkgTempC,
		CoreTemps:       coreTemps,
		PkgEnergy:       pkgEnergy,
		PsysEnergy:      psysEnergy,
		DramEnergy:      dramEnergy,
		FreqMHz:         freqMHz,
		Governor:        governor,
		PlatformProfile: a.platformProfile,
		PowerW:          powerW,
		Tag:             a.tag,
	}
	if err := insertSample(db, sample); err != nil {
		log.Printf("ERROR insert: %v", err)
	}

	// ── Update previous values ────────────────────────────────────
	// Always use the raw (pre-null) energy readings as baseline for
	// the next tick's delta computation.
	*a.prevTS = ts
	*a.prevTime = &curr
	*a.prevPkgEnergy = rawPkgEnergy
	*a.prevPsysEnergy = rawPsysEnergy
	*a.prevDramEnergy = rawDramEnergy

	// ── Verbose output ────────────────────────────────────────────
	if a.verbose {
		coreStr := "?"
		if len(coreTemps) > 0 {
			parts := make([]string, len(coreTemps))
			for i, t := range coreTemps {
				parts[i] = fmt.Sprintf("%.0f", t)
			}
			coreStr = strings.Join(parts, ",")
		}

		log.Printf("INFO [%s] pkg=%.1f°C  power=%.2fW  freq=%.0fMHz  gov=%s  profile=%s  cores=[%s]",
			sampleType,
			valOrZero(pkgTempC),
			valOrZero(powerW),
			valOrZero(freqMHz),
			valOrElse(governor, "?"),
			valOrElse(a.platformProfile, "?"),
			coreStr,
		)
		if suspended {
			log.Printf("INFO suspend_detected ≈%dms — power_w/energy set to NULL", suspendedMs)
		}
		if gapTooLarge {
			log.Printf("INFO gap_skip ts_gap=%.1fs", tsGap)
		}
	}
}

// ─────────────────────────────────────────────────────────────────
//  Small helpers
// ─────────────────────────────────────────────────────────────────

func condStr(ok bool, t, f string) string {
	if ok {
		return t
	}
	return f
}

func valOrZero(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

func valOrElse(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}
