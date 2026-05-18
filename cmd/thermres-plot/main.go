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

	var seriesList []chartdraw.Series
	colorIdx := 0

	if len(tags) == 0 {
		pts := querySamples(db, nil, sinceTime, untilTime)
		if len(pts) == 0 {
			log.Fatalf("no samples found")
		}
		pts = applyTimeBin(pts, *timeBin)
		series := buildSeries(pts, *powerBin, "all", colorIdx)
		if series != nil {
			seriesList = append(seriesList, series)
			colorIdx++
		}
	} else {
		for _, t := range tags {
			pts := querySamples(db, &t, sinceTime, untilTime)
			pts = applyTimeBin(pts, *timeBin)
			series := buildSeries(pts, *powerBin, t, colorIdx)
			if series != nil {
				seriesList = append(seriesList, series)
				colorIdx++
			}
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

func querySamples(db *sql.DB, tag *string, since, until *time.Time) []point {
	clauses := []string{"power_w IS NOT NULL", "pkg_temp_c IS NOT NULL"}
	args := []interface{}{}
	if tag != nil {
		clauses = append(clauses, "tag = ?")
		args = append(args, *tag)
	}
	if since != nil {
		clauses = append(clauses, "ts >= ?")
		args = append(args, float64(since.UnixMilli())/1000.0)
	}
	if until != nil {
		clauses = append(clauses, "ts <= ?")
		args = append(args, float64(until.UnixMilli())/1000.0)
	}
	q := fmt.Sprintf(
		"SELECT ts, pkg_temp_c, power_w FROM samples WHERE %s ORDER BY ts",
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
		k := int64(math.Floor(p.Power/binSize)) * int64(binSize)
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
		yv = append(yv, float64(k)+binSize/2)
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

func renderChart(graph *chartdraw.Chart) ([]byte, error) {
	buf := new(bytes.Buffer)
	rp := func(w, h int) chartdraw.Renderer { return chartdraw.PNG(w, h) }
	if err := graph.Render(rp, buf); err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}
	return buf.Bytes(), nil
}
