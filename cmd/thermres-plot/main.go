package main

import (
	"bytes"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/blacktop/go-termimg"
	"github.com/go-analyze/charts/chartdraw"
	_ "modernc.org/sqlite"
)

func main() {
	log.SetFlags(0)

	dbFlag := flag.String("db", "",
		"SQLite database path (default: ~/.local/share/thermres/thermres.db)")
	tag := flag.String("tag", "",
		"Comma-separated tag(s) for overlay series")
	since := flag.String("since", "",
		"Start time (RFC3339, e.g. '2025-01-01T00:00:00Z')")
	until := flag.String("until", "",
		"End time")
	timeBin := flag.String("time-bin", "",
		"Aggregate over time windows (e.g. '5m', '1h')")
	powerBin := flag.Float64("power-bin", 0,
		"Aggregate into N-watt power buckets")
	warmup := flag.Float64("warmup", 300,
		"Exclude samples within this many seconds after any non-normal event (startup/suspend/gap)")
	rolling := flag.Float64("rolling", 0,
		"Rolling average window in seconds; filters thermal mass transients (e.g. 300)")
	thermalResistance := flag.Bool("thermal-resistance", false,
		"Print thermal resistance (°C/W) and R², then exit without rendering a chart")
	dailyResistance := flag.Bool("daily-resistance", false,
		"Plot daily thermal resistance (°C/W) over time (your ADHD coping tool)")
	minSamples := flag.Int("min-samples", 10000,
		"Minimum raw samples per day to include in the R_th line (--daily-resistance)")
	output := flag.String("output", "",
		"Save PNG to file instead of terminal display")
	flag.Parse()

	dbPath := *dbFlag
	if dbPath == "" {
		dbPath = defaultDBPath()
	}

	var sinceTime, untilTime *time.Time
	if *since != "" {
		t, err := time.Parse(time.RFC3339, *since)
		if err != nil {
			log.Fatalf("bad --since: %v", err)
		}
		sinceTime = &t
	}
	if *until != "" {
		t, err := time.Parse(time.RFC3339, *until)
		if err != nil {
			log.Fatalf("bad --until: %v", err)
		}
		untilTime = &t
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	tags := []string{}
	if *tag != "" {
		for _, t := range strings.Split(*tag, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				tags = append(tags, t)
			}
		}
	}

	// Daily thermal resistance mode: completely separate chart.
	if *dailyResistance {
		var seriesList []chartdraw.Series
		var dayTicks []chartdraw.Tick
		colorIdx := 0
		tags := tags
		if len(tags) == 0 {
			tags = []string{""}
		}
		for _, t := range tags {
			var tp *string
			if t != "" {
				tp = &t
			}
			pts := querySamples(db, tp, sinceTime, untilTime, *warmup)
			label := t
			if label == "" {
				label = "all"
			}
			rth, count, ticks := buildDailyResistanceSeries(pts, label, colorIdx, *minSamples)
			if rth != nil {
				seriesList = append(seriesList, rth, count)
				// Use the ticks from the first series; they share the same x positions.
				if dayTicks == nil {
					dayTicks = ticks
				}
				colorIdx++
			}
		}
		if len(seriesList) == 0 {
			log.Fatalf("no daily data to plot")
		}

		// Scan all series to find max values for each axis so we can set
		// Range{Min:0, Max:realMax} — chartdraw ignores Range when Max==0.
		var maxRth, maxCount float64
		for _, s := range seriesList {
			cs := s.(chartdraw.ContinuousSeries)
			for _, v := range cs.YValues {
				if cs.YAxis == chartdraw.YAxisSecondary {
					if v > maxCount {
						maxCount = v
					}
				} else {
					if v > maxRth {
						maxRth = v
					}
				}
			}
		}

		graph := chartdraw.Chart{
			Title:  "Daily Thermal Resistance",
			Width:  800,
			Height: 500,
			XAxis: chartdraw.XAxis{
				Name:  "Date",
				Ticks: dayTicks,
				Style: chartdraw.Style{
					TextRotationDegrees: 45,
				},
			},
			YAxis: chartdraw.YAxis{
				Name:  "Thermal Resistance (°C/W)",
				Range: &chartdraw.ContinuousRange{Min: 0, Max: maxRth * 1.1},
			},
			YAxisSecondary: chartdraw.YAxis{
				Name:  "Samples per day",
				Range: &chartdraw.ContinuousRange{Min: 0, Max: maxCount * 1.1},
			},
			Background: chartdraw.Style{Padding: chartdraw.NewBox(20, 30, 20, 30)},
			Series:     seriesList,
		}
		buf, err := renderChart(&graph)
		if err != nil {
			log.Fatalf("render chart: %v", err)
		}
		if *output != "" {
			if err := os.WriteFile(*output, buf, 0644); err != nil {
				log.Fatalf("write output: %v", err)
			}
			log.Printf("saved to %s", *output)
		} else {
			tmp := filepath.Join(os.TempDir(), "thermres-plot-"+fmt.Sprint(time.Now().UnixNano())+".png")
			if err := os.WriteFile(tmp, buf, 0644); err != nil {
				log.Fatalf("write temp: %v", err)
			}
			defer os.Remove(tmp)
			termimg.PrintFile(tmp)
		}
		return
	}

	// When rolling average is requested without an explicit power-bin, default
	// to a 1W bin so the output is a clean line rather than 100k+ scatter dots.
	if *rolling > 0 && *powerBin == 0 && !*thermalResistance {
		*powerBin = 1.0
		log.Printf("INFO --rolling without --power-bin: defaulting to --power-bin 1")
	}

	var seriesList []chartdraw.Series
	colorIdx := 0

	// processPts applies the rolling average (if requested) and optionally
	// prints the thermal resistance, returning the processed points.
	processPts := func(pts []point, label string) []point {
		if *rolling > 0 {
			before := len(pts)
			pts = applyRollingAvg(pts, *rolling)
			log.Printf("INFO %s: rolling avg %.0fs: %d → %d points", label, *rolling, before, len(pts))
		}
		if *thermalResistance && len(pts) >= 2 {
			slope, _, r2 := linearRegression(pts)
			log.Printf("Thermal resistance (%s): %.3f °C/W  (R²: %.3f)", label, slope, r2)
		}
		return pts
	}

	if len(tags) == 0 {
		pts := querySamples(db, nil, sinceTime, untilTime, *warmup)
		if len(pts) == 0 {
			log.Fatalf("no samples found")
		}
		pts = processPts(pts, "all")
		if *thermalResistance {
			return
		}
		pts = applyTimeBin(pts, *timeBin)
		series := buildSeries(pts, *powerBin, "all", colorIdx)
		if series != nil {
			seriesList = append(seriesList, series)
			colorIdx++
		}
	} else {
		for _, t := range tags {
			pts := querySamples(db, &t, sinceTime, untilTime, *warmup)
			pts = processPts(pts, t)
			if *thermalResistance {
				continue
			}
			pts = applyTimeBin(pts, *timeBin)
			series := buildSeries(pts, *powerBin, t, colorIdx)
			if series != nil {
				seriesList = append(seriesList, series)
				colorIdx++
			}
		}
		if *thermalResistance {
			return
		}
	}

	if len(seriesList) == 0 {
		log.Fatalf("no data to plot (try a different tag or time range)")
	}

	const width, height = 800, 500

	graph := chartdraw.Chart{
		Title:  "Power vs Temperature",
		Width:  width,
		Height: height,
		XAxis: chartdraw.XAxis{
			Name: "Package Temperature (°C)",
		},
		YAxis: chartdraw.YAxis{
			Name: "Power (W)",
		},
		Background: chartdraw.Style{Padding: chartdraw.NewBox(20, 30, 20, 30)},
		Series:     seriesList,
	}


	buf, err := renderChart(&graph)
	if err != nil {
		log.Fatalf("render chart: %v", err)
	}

	if *output != "" {
		if err := os.WriteFile(*output, buf, 0644); err != nil {
			log.Fatalf("write output: %v", err)
		}
		log.Printf("saved to %s", *output)
	} else {
		tmp := filepath.Join(os.TempDir(), "thermres-plot-"+fmt.Sprint(time.Now().UnixNano())+".png")
		if err := os.WriteFile(tmp, buf, 0644); err != nil {
			log.Fatalf("write temp: %v", err)
		}
		defer os.Remove(tmp)
		termimg.PrintFile(tmp)
	}
}

type point struct {
	TS    float64
	Temp  float64
	Power float64
}

func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "thermres.db"
	}
	return filepath.Join(home, ".local", "share", "thermres", "thermres.db")
}

func querySamples(db *sql.DB, tag *string, since, until *time.Time, warmupSecs float64) []point {
	clauses := []string{
		"s.sample_type = 'normal'",
		"s.power_w IS NOT NULL",
		"s.pkg_temp_c IS NOT NULL",
	}
	// Exclude rows that fall within warmupSecs after any non-normal event.
	// There are very few non-normal rows so the NOT EXISTS correlated
	// subquery is fast regardless of table size.
	if warmupSecs > 0 {
		clauses = append(clauses, fmt.Sprintf(
			`NOT EXISTS (
			   SELECT 1 FROM samples anom
			   WHERE anom.sample_type != 'normal'
			     AND anom.ts >= s.ts - %.1f
			     AND anom.ts < s.ts
			 )`, warmupSecs))
	}
	args := []interface{}{}
	if tag != nil {
		clauses = append(clauses, "s.tag = ?")
		args = append(args, *tag)
	}
	if since != nil {
		clauses = append(clauses, "s.ts >= ?")
		args = append(args, float64(since.UnixMilli())/1000.0)
	}
	if until != nil {
		clauses = append(clauses, "s.ts <= ?")
		args = append(args, float64(until.UnixMilli())/1000.0)
	}
	q := fmt.Sprintf(
		"SELECT s.ts, s.pkg_temp_c, s.power_w FROM samples s WHERE %s ORDER BY s.ts",
		strings.Join(clauses, " AND "),
	)
	rows, err := db.Query(q, args...)
	if err != nil {
		log.Fatalf("query: %v", err)
	}
	defer rows.Close()

	var pts []point
	for rows.Next() {
		var p point
		if err := rows.Scan(&p.TS, &p.Temp, &p.Power); err != nil {
			log.Printf("scan: %v", err)
			continue
		}
		pts = append(pts, p)
	}

	// Discard any point whose gap from the raw predecessor exceeds 10 s
	// (suspend/resume artifact — the first sample after resume has a
	// bogus power_w computed from the suspend-accumulated energy delta).
	const maxGap = 10.0
	filtered := pts[:0]
	for i, p := range pts {
		if i == 0 || p.TS-pts[i-1].TS <= maxGap {
			filtered = append(filtered, p)
		}
	}
	return filtered
}

func applyTimeBin(pts []point, timeBin string) []point {
	if timeBin == "" {
		return pts
	}
	d, err := time.ParseDuration(timeBin)
	if err != nil {
		log.Fatalf("bad --time-bin: %v", err)
	}
	if d <= 0 {
		return pts
	}
	interval := d.Seconds()

	type bucket struct {
		sumTemp  float64
		sumPower float64
		count    int
	}
	m := make(map[int64]*bucket)
	keys := []int64{}

	for _, p := range pts {
		k := int64(math.Floor(p.TS / interval))
		b, ok := m[k]
		if !ok {
			b = &bucket{}
			m[k] = b
			keys = append(keys, k)
		}
		b.sumTemp += p.Temp
		b.sumPower += p.Power
		b.count++
	}

	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	out := make([]point, 0, len(keys))
	for _, k := range keys {
		b := m[k]
		out = append(out, point{
			TS:    float64(k)*interval + interval/2,
			Temp:  b.sumTemp / float64(b.count),
			Power: b.sumPower / float64(b.count),
		})
	}
	return out
}

// applyRollingAvg replaces each point with the mean of (power, temp) over the
// preceding windowSecs seconds. Points where the window contains any sub-gap
// larger than maxSubGap are discarded — those windows straddle a resume/restart
// boundary and the average would mix cold-start and equilibrium data.
func applyRollingAvg(pts []point, windowSecs float64) []point {
	const maxSubGap = 2.0 // seconds; gaps larger than this invalidate a window
	if windowSecs <= 0 || len(pts) == 0 {
		return pts
	}

	out := make([]point, 0, len(pts))
	for i, p := range pts {
		// Walk backwards collecting all points in [p.TS - windowSecs, p.TS].
		var sumP, sumT float64
		n := 0
		valid := true
		for j := i; j >= 0; j-- {
			if p.TS-pts[j].TS > windowSecs {
				break
			}
			// Check for sub-gap between consecutive points inside the window.
			// Also catches the gap between the oldest in-window point and
			// the point just before it (which would fall outside the window
			// boundary, meaning there's a hole at the window edge).
			if j < i && pts[j+1].TS-pts[j].TS > maxSubGap {
				valid = false
				break
			}
			// If this is the oldest point in the window and it isn't the
			// very first sample, check the gap to the point before it.
			// If that gap is large, the window doesn't have continuous
			// coverage back to windowSecs ago — discard.
			if j > 0 && p.TS-pts[j].TS < windowSecs && pts[j].TS-pts[j-1].TS > maxSubGap {
				// The window nominally covers [p.TS-windowSecs, p.TS] but
				// the data only goes back to pts[j].TS continuously.
				// Only accept if the continuous coverage is "full enough".
				// We require the window to be filled from at least (p.TS - windowSecs).
				if p.TS-pts[j].TS < windowSecs*0.9 {
					valid = false
				}
				break
			}
			sumP += pts[j].Power
			sumT += pts[j].Temp
			n++
		}
		if !valid || n == 0 {
			continue
		}
		out = append(out, point{
			TS:    p.TS,
			Power: sumP / float64(n),
			Temp:  sumT / float64(n),
		})
	}
	return out
}

// linearRegression fits T = intercept + slope*P via OLS.
// Returns slope (°C/W = thermal resistance), intercept, and R².
// Returns zeros if fewer than 2 points.
func linearRegression(pts []point) (slope, intercept, r2 float64) {
	n := float64(len(pts))
	if n < 2 {
		return 0, 0, 0
	}

	var sumP, sumT, sumPP, sumPT float64
	for _, p := range pts {
		sumP += p.Power
		sumT += p.Temp
		sumPP += p.Power * p.Power
		sumPT += p.Power * p.Temp
	}
	meanP := sumP / n
	meanT := sumT / n

	ssPT := sumPT - n*meanP*meanT
	ssPP := sumPP - n*meanP*meanP
	if ssPP == 0 {
		return 0, meanT, 0
	}

	slope = ssPT / ssPP
	intercept = meanT - slope*meanP

	// R²: 1 - SS_res / SS_tot
	var ssRes, ssTot float64
	for _, p := range pts {
		predicted := intercept + slope*p.Power
		ssRes += (p.Temp - predicted) * (p.Temp - predicted)
		ssTot += (p.Temp - meanT) * (p.Temp - meanT)
	}
	if ssTot > 0 {
		r2 = 1 - ssRes/ssTot
	}
	return slope, intercept, r2
}

func buildSeries(pts []point, powerBin float64, name string, colorIdx int) chartdraw.Series {
	if len(pts) == 0 {
		return nil
	}
	if len(pts) < 2 {
		log.Printf("WARN %s: only %d sample(s), skipping", name, len(pts))
		return nil
	}
	if powerBin > 0 {
		return buildBinnedLine(pts, powerBin, name, colorIdx)
	}
	return buildScatter(pts, name, colorIdx)
}

func buildScatter(pts []point, name string, colorIdx int) chartdraw.Series {
	colorIdx++
	xv := make([]float64, len(pts))
	yv := make([]float64, len(pts))
	for i, p := range pts {
		xv[i] = p.Temp
		yv[i] = p.Power
	}
	return chartdraw.ContinuousSeries{
		Name:    name,
		XValues: xv,
		YValues: yv,
		Style: chartdraw.Style{
			StrokeWidth: float64(chartdraw.Disabled),
			DotWidth:    3,
			DotColor:    chartdraw.GetDefaultColor(colorIdx),
		},
	}
}

func buildBinnedLine(pts []point, binSize float64, name string, colorIdx int) chartdraw.Series {
	colorIdx++
	type bucket struct {
		sumTemp float64
		count   int
	}
	m := make(map[int64]*bucket)
	keys := []int64{}

	for _, p := range pts {
		// Use integer bin index as map key to avoid float precision issues.
		// k=0 → [0, binSize), k=1 → [binSize, 2*binSize), etc.
		k := int64(math.Floor(p.Power / binSize))
		b, ok := m[k]
		if !ok {
			b = &bucket{}
			m[k] = b
			keys = append(keys, k)
		}
		b.sumTemp += p.Temp
		b.count++
	}

	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	xv := make([]float64, 0, len(keys))
	yv := make([]float64, 0, len(keys))
	for _, k := range keys {
		b := m[k]
		xv = append(xv, b.sumTemp/float64(b.count))
		yv = append(yv, (float64(k)+0.5)*binSize) // bin centre
	}

	if len(xv) < 2 {
		log.Printf("WARN %s: only %d power bin(s) — try a larger --power-bin", name, len(xv))
		return nil
	}

	c := chartdraw.GetDefaultColor(colorIdx)
	return chartdraw.ContinuousSeries{
		Name:    name,
		XValues: xv,
		YValues: yv,
		Style: chartdraw.Style{
			StrokeColor: c,
			StrokeWidth: 2,
			DotColor:    c,
			DotWidth:    4,
		},
	}
}

// buildDailyResistanceSeries groups pts by local calendar day, applies a
// rolling average within each day to filter thermal mass transients, and
// returns:
//   - rthSeries: line+dots on primary Y for days with >= minSamples raw samples
//   - countSeries: dots-only on secondary Y for ALL days (shows excluded days too)
//   - ticks: one tick per day across all days
const dailyRollingWindow = 2400.0

func buildDailyResistanceSeries(pts []point, name string, colorIdx, minSamples int) (rthSeries, countSeries chartdraw.Series, ticks []chartdraw.Tick) {
	if len(pts) == 0 {
		return nil, nil, nil
	}

	byDay := make(map[string][]point)
	dayOrder := []string{}
	for _, p := range pts {
		day := time.Unix(int64(p.TS), 0).Local().Format("2006-01-02")
		if _, seen := byDay[day]; !seen {
			dayOrder = append(dayOrder, day)
		}
		byDay[day] = append(byDay[day], p)
	}

	// Separate X/Y for R_th line (qualifying days) and count dots (all days).
	var rthX, rthY []float64
	var cntX, cntY []float64

	for _, day := range dayOrder {
		rawPts := byDay[day]
		t, _ := time.ParseInLocation("2006-01-02", day, time.Local)
		x := float64(t.Unix())
		rawN := len(rawPts)

		// Count dot for every day regardless of qualification.
		cntX = append(cntX, x)
		cntY = append(cntY, float64(rawN))
		ticks = append(ticks, chartdraw.Tick{Value: x, Label: day})

		if rawN < minSamples {
			log.Printf("INFO daily-resistance: %s skipped (%d raw samples < min %d)", day, rawN, minSamples)
			continue
		}

		dayPts := applyRollingAvg(rawPts, dailyRollingWindow)
		if len(dayPts) < 120 {
			log.Printf("INFO daily-resistance: %s skipped (%d samples after rolling avg)", day, len(dayPts))
			continue
		}
		slope, _, r2 := linearRegression(dayPts)
		if r2 < 0 {
			log.Printf("INFO daily-resistance: %s skipped (R²=%.3f)", day, r2)
			continue
		}
		log.Printf("INFO daily-resistance: %s  R_th=%.3f °C/W  R²=%.3f  n_raw=%d  n_smooth=%d", day, slope, r2, rawN, len(dayPts))
		rthX = append(rthX, x)
		rthY = append(rthY, slope)
	}

	if len(cntX) == 0 {
		return nil, nil, nil
	}

	// Count dots — no stroke, just dots; covers all days including excluded.
	countColor := chartdraw.ColorAlternateGray
	countSeries = chartdraw.ContinuousSeries{
		Name:    name + " samples",
		XValues: cntX,
		YValues: cntY,
		YAxis:   chartdraw.YAxisSecondary,
		Style: chartdraw.Style{
			StrokeWidth: float64(chartdraw.Disabled),
			DotColor:    countColor,
			DotWidth:    5,
		},
	}

	if len(rthX) < 2 {
		log.Printf("WARN daily-resistance %s: only %d qualifying day(s) for R_th line", name, len(rthX))
		return nil, countSeries, ticks
	}

	c := chartdraw.GetDefaultColor(colorIdx)
	rthSeries = chartdraw.ContinuousSeries{
		Name:    name + " R_th",
		XValues: rthX,
		YValues: rthY,
		Style: chartdraw.Style{
			StrokeColor: c,
			StrokeWidth: 2,
			DotColor:    c,
			DotWidth:    5,
		},
	}
	return rthSeries, countSeries, ticks
}

func renderChart(graph *chartdraw.Chart) ([]byte, error) {
	buf := new(bytes.Buffer)
	rp := func(w, h int) chartdraw.Renderer { return chartdraw.PNG(w, h) }
	if err := graph.Render(rp, buf); err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}
	return buf.Bytes(), nil
}
