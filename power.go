// Power computation from RAPL energy deltas.
//
// The CPU package power in watts is computed from the difference between
// two consecutive RAPL energy_uj readings divided by the time elapsed:
//
//	power_w = (curr_µJ - prev_µJ) / dt_s / 1_000_000
//
// If the counter wrapped around (curr < prev), we add max_energy_uj to
// account for the overflow.  This lets the tool survive across restarts
// without producing a spike.

package main

// computePower calculates package power in watts from two consecutive
// RAPL energy samples.
//
// Parameters:
//   - prev: energy from the previous tick (nil on first call → returns nil)
//   - curr: energy from the current tick
//   - max:  counter wrap-around value (0 = no wrap expected)
//   - prevTS, currTS: timestamps of the two samples
func computePower(prev *int64, curr *int64, max uint64, prevTS, currTS float64) *float64 {
	if prev == nil || curr == nil {
		return nil
	}
	dt := currTS - prevTS
	if dt <= 0 {
		return nil
	}

	de := *curr - *prev
	if de < 0 {
		// Counter wrapped around; add the max range.
		de += int64(max)
	}

	// Convert µJ/s to W (1 W = 1 J/s = 1_000_000 µJ/s).
	w := float64(de) / dt / 1_000_000.0
	return &w
}
