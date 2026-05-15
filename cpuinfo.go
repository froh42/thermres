// CPU frequency, scaling governor, and ACPI platform profile.

package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ─────────────────────────────────────────────────────────────────
//  Frequency & governor
// ─────────────────────────────────────────────────────────────────

// readFreqAndGovernor samples all online CPUs for current frequency
// and the active scaling governor. Returns (average_freq_MHz, governor_name).
func readFreqAndGovernor() (*float64, *string) {
	cpus, err := filepath.Glob("/sys/devices/system/cpu/cpu[0-9]*")
	if err != nil || len(cpus) == 0 {
		return nil, nil
	}
	sort.Strings(cpus)

	var freqs []float64
	govSet := make(map[string]bool)

	for _, cpuDir := range cpus {
		// Current CPU frequency in kHz (from cpufreq scaling_cur_freq).
		freqPath := filepath.Join(cpuDir, "cpufreq", "scaling_cur_freq")
		if v, err := readInt64cpu(freqPath); err == nil {
			freqs = append(freqs, float64(v)/1000.0) // kHz → MHz
		}

		// Scaling governor (e.g. "powersave", "performance").
		govPath := filepath.Join(cpuDir, "cpufreq", "scaling_governor")
		if gov, err := readLine(govPath); err == nil {
			govSet[gov] = true
		}
	}

	if len(freqs) == 0 {
		return nil, nil
	}

	var sum float64
	for _, f := range freqs {
		sum += f
	}
	avg := sum / float64(len(freqs))
	avg = math.Round(avg*100) / 100
	freqMHz := &avg

	if len(govSet) == 1 {
		for g := range govSet {
			gov := g
			return freqMHz, &gov
		}
	}

	return freqMHz, nil
}

// ─────────────────────────────────────────────────────────────────
//  Platform profile
// ─────────────────────────────────────────────────────────────────

// readPlatformProfile returns /sys/firmware/acpi/platform_profile
// (e.g. "balanced", "performance", "low-power") or nil if not available.
//
// This file is set by platform_profile services such as
// power-profiles-daemon or tuned.
func readPlatformProfile() *string {
	v, err := readLine("/sys/firmware/acpi/platform_profile")
	if err != nil {
		return nil
	}
	return &v
}

// ─────────────────────────────────────────────────────────────────
//  Helpers
// ─────────────────────────────────────────────────────────────────

func readInt64cpu(path string) (int64, error) {
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

func readLine(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
