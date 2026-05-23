package main

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-analyze/charts/chartdraw"
	_ "modernc.org/sqlite"
)

// liveDB returns the path to the real thermres database, or skips the test
// if the file does not exist.
func liveDB(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir:", err)
	}
	path := filepath.Join(home, ".local", "share", "thermres", "thermres.db")
	if _, err := os.Stat(path); err != nil {
		t.Skip("live DB not found:", path)
	}
	return path
}

// ── applyRollingAvg ──────────────────────────────────────────────

func TestApplyRollingAvg_Basic(t *testing.T) {
	// 10 clean 1-second-apart samples, all same power/temp.
	pts := make([]point, 10)
	for i := range pts {
		pts[i] = point{TS: float64(i), Power: 10, Temp: 60}
	}
	out := applyRollingAvg(pts, 5)
	if len(out) == 0 {
		t.Fatal("expected output, got none")
	}
	for _, p := range out {
		if math.Abs(p.Power-10) > 1e-9 || math.Abs(p.Temp-60) > 1e-9 {
			t.Errorf("unexpected averaged values: power=%.2f temp=%.2f", p.Power, p.Temp)
		}
	}
}

func TestApplyRollingAvg_GapInvalidatesWindow(t *testing.T) {
	// Points 0–4, then a 10s gap, then points 14–18.
	// Windows that straddle the gap (points 14..17 looking back into gap) must be discarded.
	var pts []point
	for i := 0; i < 5; i++ {
		pts = append(pts, point{TS: float64(i), Power: 5, Temp: 50})
	}
	// gap: next point is at ts=15 (10s jump)
	for i := 0; i < 5; i++ {
		pts = append(pts, point{TS: float64(15 + i), Power: 20, Temp: 80})
	}

	out := applyRollingAvg(pts, 8) // 8s window crosses the gap

	// Points on the "before gap" side: ts 0-4 → windows [0..0],[0..1],...,[0..4] all clean.
	// Points on the "after gap" side: ts 15-18 → the window [15-8, 15]=[7,15]
	// hits the gap at ts=14→5 (10s jump) → all 5 "after" points should be discarded
	// because looking back 8 seconds from ts=15 reaches into the gap.
	for _, p := range out {
		if p.TS >= 15 {
			t.Errorf("point at ts=%.0f should have been discarded (window straddles gap)", p.TS)
		}
	}
}

func TestApplyRollingAvg_Zero(t *testing.T) {
	pts := []point{{TS: 0, Power: 1, Temp: 2}, {TS: 1, Power: 3, Temp: 4}}
	out := applyRollingAvg(pts, 0) // disabled
	if len(out) != len(pts) {
		t.Errorf("rolling=0 should return pts unchanged, got %d/%d", len(out), len(pts))
	}
}

// ── linearRegression ─────────────────────────────────────────────

func TestLinearRegression_KnownSlope(t *testing.T) {
	// Perfect line: T = 40 + 2*P  →  slope=2, intercept=40, R²=1
	pts := make([]point, 20)
	for i := range pts {
		p := float64(i + 1)
		pts[i] = point{Power: p, Temp: 40 + 2*p}
	}
	slope, intercept, r2 := linearRegression(pts)
	if math.Abs(slope-2.0) > 1e-9 {
		t.Errorf("slope: want 2.0, got %.6f", slope)
	}
	if math.Abs(intercept-40.0) > 1e-9 {
		t.Errorf("intercept: want 40.0, got %.6f", intercept)
	}
	if math.Abs(r2-1.0) > 1e-9 {
		t.Errorf("R²: want 1.0, got %.6f", r2)
	}
}

func TestLinearRegression_TooFewPoints(t *testing.T) {
	slope, intercept, r2 := linearRegression([]point{{Power: 1, Temp: 2}})
	if slope != 0 || intercept != 0 || r2 != 0 {
		t.Errorf("want zeros for <2 points, got %.2f %.2f %.2f", slope, intercept, r2)
	}
}

// ── integration: live DB ─────────────────────────────────────────

func TestThermalResistance_LiveDB(t *testing.T) {
	db, err := sql.Open("sqlite", liveDB(t))
	if err != nil {
		t.Fatal("open db:", err)
	}
	defer db.Close()

	pts := querySamples(db, nil, nil, nil, 60)
	if len(pts) < 100 {
		t.Skipf("only %d samples, need at least 100", len(pts))
	}

	pts = applyRollingAvg(pts, 300)
	if len(pts) < 2 {
		t.Skip("not enough points after rolling average")
	}

	slope, _, r2 := linearRegression(pts)

	// For a real laptop, thermal resistance should be in the range 0.5–5 °C/W
	// and the fit should be decent (R² > 0.5).
	if slope < 0.5 || slope > 5.0 {
		t.Errorf("thermal resistance %.3f °C/W is outside plausible range [0.5, 5.0]", slope)
	}
	if r2 < 0.5 {
		t.Errorf("R²=%.3f is too low — model does not fit data well", r2)
	}
	t.Logf("Thermal resistance: %.3f °C/W  (R²: %.3f, n=%d)", slope, r2, len(pts))
}

// ── regression: small power bins still render ─────────────────────

// TestDailyResistanceSeries_XValues prints the X timestamps produced by
// buildDailyResistanceSeries so we can verify they sit correctly on the
// calendar day boundaries.
func TestDailyResistanceSeries_XValues(t *testing.T) {
	db, err := sql.Open("sqlite", liveDB(t))
	if err != nil {
		t.Fatal("open db:", err)
	}
	defer db.Close()

	pts := querySamples(db, nil, nil, nil, 300.0)
	if len(pts) < 120 {
		t.Skipf("only %d samples, need at least 120", len(pts))
	}

	rth, count, _ := buildDailyResistanceSeries(pts, "all", 0)
	if rth == nil {
		t.Fatal("buildDailyResistanceSeries returned nil (not enough days?)")
	}

	rthCS := rth.(chartdraw.ContinuousSeries)
	countCS := count.(chartdraw.ContinuousSeries)

	for i, x := range rthCS.XValues {
		ts := int64(x)
		day := time.Unix(ts, 0).Local().Format("2006-01-02 15:04:05")
		midnight := time.Unix(ts, 0).Local().Truncate(24 * time.Hour)
		offsetH := time.Unix(ts, 0).Local().Sub(midnight).Hours()
		t.Logf("day %-2d  %s  x=%d  R_th=%.3f  n=%.0f  offset_from_midnight=%.1fh",
			i, day, ts, rthCS.YValues[i], countCS.YValues[i], offsetH)
	}
}
// --power-bin < 1.0 caused int64(binSize)==0, mapping every point to key 0
// and producing a single-bucket chart the renderer rejected with:
// "zero x-range delta; there needs to be at least (2) values"
//
// Reproduces: thermres-plot --time-bin 5m --power-bin 0.5 --warmup 300
func TestRenderChart_SmallPowerBins(t *testing.T) {
	db, err := sql.Open("sqlite", liveDB(t))
	if err != nil {
		t.Fatal("open db:", err)
	}
	defer db.Close()

	pts := querySamples(db, nil, nil, nil, 300.0)
	if len(pts) < 2 {
		t.Fatalf("need at least 2 samples from live DB, got %d", len(pts))
	}

	for _, binSize := range []float64{1.0, 0.5, 0.1} {
		t.Run(fmt.Sprintf("power-bin=%.2f", binSize), func(t *testing.T) {
			series := buildSeries(pts, binSize, "test", 0)
			if series == nil {
				t.Skip("not enough bins for this bin size — skip is acceptable")
			}

			graph := &chartdraw.Chart{
				Width:  800,
				Height: 500,
				XAxis:  chartdraw.XAxis{Name: "Package Temperature (°C)"},
				YAxis:  chartdraw.YAxis{Name: "Power (W)"},
				Series: []chartdraw.Series{series},
			}
			if _, err := renderChart(graph); err != nil {
				t.Errorf("renderChart failed with binSize=%.2f: %v", binSize, err)
			}
		})
	}
}

