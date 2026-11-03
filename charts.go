package main

import (
	"fmt"
	"html/template"
	"math"
	"strconv"
	"strings"
	"time"
)

// Chart geometry is worked out here and drawn by the templates in plain
// HTML/CSS (bars, heatmap cells, bar lists) plus one inline SVG for
// sparklines. Nothing ships a charting library, and every chart follows the
// theme through CSS variables.

type barView struct {
	H     float64 // height, percent of the plot
	N     int
	Label string // tooltip date
}

type tickView struct {
	V   string
	Pos float64 // percent from the bottom
}

type xLabelView struct {
	Pos  float64 // percent from the left, bar centre
	Text string
}

type barChart struct {
	Bars    []barView
	Ticks   []tickView
	XLabels []xLabelView
	Empty   bool
}

// niceMax rounds up to 1, 2 or 5 times a power of ten, so the y-axis gets
// round ticks.
func niceMax(v int) (top, step int) {
	if v <= 4 {
		return 4, 1
	}
	raw := float64(v) / 4
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if s := m * mag; s >= raw {
			step = int(math.Ceil(s))
			break
		}
	}
	return step * 4, step
}

func makeBarChart(st *Stats) barChart {
	c := barChart{}
	maxN := 0
	for _, p := range st.Series {
		maxN = max(maxN, p.N)
	}
	c.Empty = maxN == 0
	top, step := niceMax(maxN)
	for v := 0; v <= top; v += step {
		c.Ticks = append(c.Ticks, tickView{V: compact(v), Pos: float64(v) / float64(top) * 100})
	}
	zone := time.FixedZone("", int(st.Offset))
	n := len(st.Series)
	every := 1
	switch {
	case st.Range.Bucket == time.Hour:
		every = 6
	case n > 60:
		every = 15
	case n > 14:
		every = 5
	}
	for i, p := range st.Series {
		t := time.Unix(p.Start, 0).In(zone)
		var label string
		if st.Range.Bucket == time.Hour {
			label = t.Format("Mon 15:04") + "–" + t.Add(time.Hour).Format("15:04")
		} else {
			label = t.Format("Mon, Jan 2")
		}
		c.Bars = append(c.Bars, barView{H: float64(p.N) / float64(top) * 100, N: p.N, Label: label})
		// Label from the newest bar backwards, so "today" always has one.
		if (n-1-i)%every == 0 {
			var text string
			switch {
			case st.Range.Bucket == time.Hour:
				text = t.Format("15:04")
			case n <= 7:
				text = t.Format("Mon")
			default:
				text = t.Format("Jan 2")
			}
			c.XLabels = append(c.XLabels, xLabelView{Pos: (float64(i) + 0.5) / float64(n) * 100, Text: text})
		}
	}
	return c
}

type heatCell struct {
	Level int
	N     int
	Label string
}

type heatRow struct {
	Day   string
	Cells []heatCell
}

var weekdays = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

// makeHeatmap buckets counts into five equal steps of the sequential ramp.
// Zero keeps its own neutral step, distinct from "a little".
func makeHeatmap(st *Stats) []heatRow {
	rows := make([]heatRow, 7)
	for d := range 7 {
		rows[d].Day = weekdays[d]
		for h := range 24 {
			n := st.Heat[d][h]
			lvl := 0
			if n > 0 && st.HeatMax > 0 {
				lvl = min(5, 1+int(float64(n)/float64(st.HeatMax)*4.999))
			}
			rows[d].Cells = append(rows[d].Cells, heatCell{
				Level: lvl, N: n,
				Label: fmt.Sprintf("%s %02d:00–%02d:00 · %s", weekdays[d], h, (h+1)%24, plural(n, "click")),
			})
		}
	}
	return rows
}

type barListRow struct {
	Label string
	Href  string
	N     int
	W     float64 // percent of the widest row
	Pct   string  // share of the total
}

func makeBarList(rows []statRow, total int, href func(statRow) string) []barListRow {
	out := []barListRow{}
	maxN := 1
	for _, r := range rows {
		maxN = max(maxN, r.N)
	}
	for _, r := range rows {
		b := barListRow{Label: r.Label, N: r.N, W: float64(r.N) / float64(maxN) * 100}
		if total > 0 {
			b.Pct = strconv.Itoa(int(math.Round(float64(r.N)/float64(total)*100))) + "%"
		}
		if href != nil {
			b.Href = href(r)
		}
		out = append(out, b)
	}
	return out
}

// sparkline draws a week of clicks as a 2px line with a faint wash under it.
func sparkline(vals []int) template.HTML {
	const w, h, pad = 72.0, 22.0, 2.0
	if len(vals) == 0 {
		vals = make([]int, 7)
	}
	maxV := 1
	for _, v := range vals {
		maxV = max(maxV, v)
	}
	var pts []string
	for i, v := range vals {
		x := pad + float64(i)*(w-2*pad)/float64(max(len(vals)-1, 1))
		y := h - pad - float64(v)/float64(maxV)*(h-2*pad)
		pts = append(pts, fmt.Sprintf("%.1f,%.1f", x, y))
	}
	line := strings.Join(pts, " ")
	if maxV == 1 && sumInts(vals) == 0 {
		// No clicks all week: a quiet baseline, not a blue line that reads as data.
		return template.HTML(fmt.Sprintf(`<svg class="block" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" aria-hidden="true">`+
			`<line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f" stroke="var(--line-strong)" stroke-width="1.5" stroke-dasharray="2 3" stroke-linecap="round"/></svg>`,
			w, h, w, h, pad, w-pad, h-pad, h-pad))
	}
	area := fmt.Sprintf("%.1f,%.1f %s %.1f,%.1f", pad, h-pad, line, w-pad, h-pad)
	return template.HTML(fmt.Sprintf(
		`<svg class="block" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" aria-hidden="true">`+
			`<polygon points="%s" fill="var(--series-wash)"/>`+
			`<polyline points="%s" fill="none" stroke="var(--series)" stroke-width="1.5" stroke-linejoin="round" stroke-linecap="round"/></svg>`,
		w, h, w, h, area, line))
}

// compact writes 1234 as 1.2k, for axis ticks and tiles.
func compact(n int) string {
	switch {
	case n >= 1_000_000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1e6)) + "M"
	case n >= 10_000:
		return strconv.Itoa(n/1000) + "k"
	case n >= 1000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1e3)) + "k"
	}
	return strconv.Itoa(n)
}

func trimZero(s string) string { return strings.TrimSuffix(s, ".0") }

// thousands writes 12345 as 12,345.
func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return thousands(int64(n)) + " " + word + "s"
}

func sumInts(vs []int) int {
	t := 0
	for _, v := range vs {
		t += v
	}
	return t
}
