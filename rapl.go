// RAPL (Running Average Power Limit) energy counter reader.
//
// Reads energy_uj and max_energy_range_uj from the Intel RAPL sysfs interface
// at /sys/class/powercap/intel-rapl/.  These files are owned by root:root
// with mode 0400, so they can only be opened while the process has root
// privileges (from setuid).  We open the file handles during the privileged
// setup phase, then keep them open and seek+read them after dropping root.
//
// Sysfs files regenerate their content on every read(), and seeking to
// position 0 resets the kernel-side read pointer — so the same FD can be
// re-read indefinitely.

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const raplBase = "/sys/class/powercap/intel-rapl"

// RaplDomain represents one RAPL energy counter (e.g. "package-0", "psys").
//
//	The fd field holds the *os.File that was opened while privileged.
//	After we drop root, fd.Seek(0,0) + read still works because the
//	file descriptor was already opened by root.
type RaplDomain struct {
	Name      string
	fd        *os.File
	MaxEnergy uint64
}

// ReadEnergy seeks to the beginning of the open file and reads the
// current energy counter value in microjoules (µJ).
func (d *RaplDomain) ReadEnergy() (int64, bool) {
	// Seek to offset 0 — this resets the kernel's internal read
	// position so the next read returns fresh data.
	if _, err := d.fd.Seek(0, 0); err != nil {
		return 0, false
	}

	var val int64
	_, err := fmt.Fscanf(d.fd, "%d", &val)
	if err != nil {
		return 0, false
	}
	return val, true
}

// Close releases the file handle.
func (d *RaplDomain) Close() error {
	return d.fd.Close()
}

// ─────────────────────────────────────────────────────────────────

// discoverRapl walks /sys/class/powercap/intel-rapl/ and opens every
// energy_uj file it finds.  Requires CAP_SETUID / root (setuid binary).
//
// Call this during the privileged phase, before dropping root.
func discoverRapl() []RaplDomain {
	var domains []RaplDomain

	entries, err := os.ReadDir(raplBase)
	if err != nil {
		log.Printf("WARN intel-rapl not found – energy/power N/A")
		return nil
	}

	for _, entry := range entries {
		// Skip non-directories like "power", "uevent", "enabled"
		if !entry.IsDir() {
			continue
		}
		// Only consider intel-rapl:N directories
		if !strings.HasPrefix(entry.Name(), "intel-rapl:") {
			continue
		}

		dirPath := filepath.Join(raplBase, entry.Name())
		namePath := filepath.Join(dirPath, "name")
		energyPath := filepath.Join(dirPath, "energy_uj")
		maxEnergyPath := filepath.Join(dirPath, "max_energy_range_uj")

		// Read the human-readable domain name like "package-0"
		name, err := readFileLine(namePath)
		if err != nil {
			continue // not a valid RAPL domain
		}

		// Open the energy_uj file.  This is the privileged operation —
		// file is 0400 owned by root.
		fd, err := os.Open(energyPath)
		if err != nil {
			continue // main.go handles the fatal error with install instructions
		}

		// max_energy_range_uj tells us the counter's wrap-around point.
		// If it's 0 the counter probably never wraps in practice.
		var maxEnergy uint64
		if line, err := readFileLine(maxEnergyPath); err == nil {
			if v, err := strconv.ParseUint(line, 10, 64); err == nil {
				maxEnergy = v
			}
		}

		domains = append(domains, RaplDomain{
			Name:      name,
			fd:        fd,
			MaxEnergy: maxEnergy,
		})
	}

	sort.Slice(domains, func(i, j int) bool {
		return domains[i].Name < domains[j].Name
	})

	if len(domains) > 0 {
		names := make([]string, len(domains))
		for i, d := range domains {
			names[i] = d.Name
		}
		log.Printf("DEBUG RAPL: %s", strings.Join(names, ", "))
	}

	return domains
}

// ─────────────────────────────────────────────────────────────────
//  Helpers
// ─────────────────────────────────────────────────────────────────

// readFileLine reads one line from a text file and strips whitespace.
func readFileLine(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
