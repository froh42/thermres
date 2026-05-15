// coretemp hwmon — per-core and package temperature via /sys/class/hwmon.
//
// The Linux coretemp driver creates one hwmon directory per CPU package.
// Inside each hwmonN directory there are temp*_input files (values in
// millidegrees Celsius) and temp*_label files that tell us which sensor
// is which ("Package id N" vs "Core N").

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CoretempSensors holds file paths discovered during setup.
// Unlike RAPL, these files are world-readable, so we just store paths
// and open+read them on each tick.
type CoretempSensors struct {
	PkgPath  string
	CorePaths []string
}

// ─────────────────────────────────────────────────────────────────

// discoverCoretemp finds hwmon entries for the coretemp driver.
// Returns nil if coretemp is not available.
func discoverCoretemp() *CoretempSensors {
	// The coretemp driver typically lives under
	// /sys/devices/platform/coretemp.0/hwmon/hwmon*/
	// but we also check the old hwmon class path.
	patterns := []string{
		"/sys/devices/platform/coretemp.0/hwmon/hwmon*",
		"/sys/class/hwmon/hwmon*",
	}

	for _, pattern := range patterns {
		dirs, err := filepath.Glob(pattern)
		if err != nil || len(dirs) == 0 {
			continue
		}

		for _, dir := range dirs {
			sensors := scanHwmonDir(dir)
			if sensors != nil {
				return sensors
			}
		}
	}

	log.Printf("WARN coretemp not found – temperatures unavailable")
	return nil
}

// scanHwmonDir reads label and input files in one hwmon directory and
// classifies them as package or core sensors.
func scanHwmonDir(dir string) *CoretempSensors {
	// Phase 1: read all temp*_label files to identify sensors.
	labels := make(map[string]string)  // key "temp1" → label text
	labelGlob := filepath.Join(dir, "temp*_label")
	labelFiles, _ := filepath.Glob(labelGlob)
	for _, lp := range labelFiles {
		key := extractKey(lp) // "temp1_label" → "temp1"
		label, err := readFileLine(lp)
		if err != nil {
			continue
		}
		labels[key] = strings.ToLower(label)
	}

	// Phase 2: read temp*_input files and match to labels.
	var pkgPath string
	var corePaths []string

	inputGlob := filepath.Join(dir, "temp*_input")
	inputFiles, _ := filepath.Glob(inputGlob)
	for _, ip := range inputFiles {
		key := extractKey(ip) // "temp1_input" → "temp1"
		label := labels[key]

		if strings.Contains(label, "package") {
			pkgPath = ip
		} else if strings.Contains(label, "core") {
			corePaths = append(corePaths, ip)
		}
	}

	// Return what we found (may be just package or just cores).
	if pkgPath != "" || len(corePaths) > 0 {
		sort.Strings(corePaths)
		return &CoretempSensors{
			PkgPath:   pkgPath,
			CorePaths: corePaths,
		}
	}
	return nil
}

// readCoreTemps reads the current temperature values.
// Returns (package_temp_C, slice_of_core_temps_C).
// Package temp can be nil if the sensor doesn't have a package reading.
func readCoreTemps(sensors *CoretempSensors) (*float64, []float64) {
	var pkg *float64
	if sensors.PkgPath != "" {
		if v, err := readInt64(sensors.PkgPath); err == nil {
			f := float64(v) / 1000.0
			pkg = &f
		}
	}

	cores := make([]float64, 0, len(sensors.CorePaths))
	for _, cp := range sensors.CorePaths {
		if v, err := readInt64(cp); err == nil {
			cores = append(cores, float64(v)/1000.0)
		}
	}

	return pkg, cores
}

// ─────────────────────────────────────────────────────────────────
//  Helpers
// ─────────────────────────────────────────────────────────────────

// extractKey pulls the sensor key from a filename like "temp1_label" → "temp1".
func extractKey(path string) string {
	base := filepath.Base(path)
	if idx := strings.Index(base, "_"); idx >= 0 {
		return base[:idx]
	}
	return base
}

// readInt64 reads a file containing one integer.
func readInt64(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var v int64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &v); err != nil {
		return 0, err
	}
	return v, nil
}
