package homeweb

import (
	"strconv"
	"strings"
)

// The sparklines and bars are SVG built here, so the page needs no inline
// style: sizes are SVG attributes in a viewBox stretched to the box
// (preserveAspectRatio="none"); the strokes do not scale (home.css,
// vector-effect), and a dot is a zero-length round-capped stroke.

const sparkH = 30.0 // viewBox height; the width is one unit per hour

// sparkHour is one hour of a sparkline: the hit area with its tooltip.
type sparkHour struct {
	X     string // left edge of the hour (viewBox units)
	Mid   string // its middle
	Dot   string // path of the hover dot
	Label string // "14:00–15:00 — 3 запуска" (title and the hover label)
}

// spark is a rendered sparkline.
type spark struct {
	ViewBox string
	Line    string // path of the line
	Area    string // path of the area under it
	Marks   string // path of the marked hours (failed runs): dots
	Hours   []sparkHour
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// buildSpark draws vals (one per hour); marked hours get a dot; labels are
// the per-hour texts. Nil without values.
func buildSpark(vals []float64, marked []bool, labels []string) *spark {
	n := len(vals)
	if n == 0 {
		return nil
	}
	maxV := 0.0
	for _, v := range vals {
		maxV = max(maxV, v)
	}
	if maxV == 0 {
		maxV = 1
	}
	y := func(v float64) float64 { return sparkH - 2 - v/maxV*(sparkH-6) }
	var line, marks strings.Builder
	s := &spark{ViewBox: "0 0 " + strconv.Itoa(n) + " " + num(sparkH)}
	for i, v := range vals {
		x := float64(i) + 0.5
		if i == 0 {
			line.WriteString("M")
		} else {
			line.WriteString("L")
		}
		pt := num(x) + " " + num(y(v))
		line.WriteString(pt)
		if i < len(marked) && marked[i] {
			marks.WriteString("M" + pt + "h0")
		}
		h := sparkHour{X: strconv.Itoa(i), Mid: num(x), Dot: "M" + pt + "h0"}
		if i < len(labels) {
			h.Label = labels[i]
		}
		s.Hours = append(s.Hours, h)
	}
	s.Line, s.Marks = line.String(), marks.String()
	s.Area = s.Line + "L" + num(float64(n)-0.5) + " " + num(sparkH) + "L0.50 " + num(sparkH) + "Z"
	return s
}

// barPart is one segment of a bar.
type barPart struct {
	Class, X, W, Title string
}

// bar is a proportional bar of parts (viewBox: the sum of the values).
type bar struct {
	ViewBox string
	Parts   []barPart
}

// part is one value of a bar.
type part struct {
	class, title string
	n            int
}

// buildBar lays the non-zero parts out left to right; nil when all are zero.
func buildBar(parts ...part) *bar {
	total := 0
	for _, p := range parts {
		total += p.n
	}
	if total <= 0 {
		return nil
	}
	b := &bar{ViewBox: "0 0 " + strconv.Itoa(total) + " 1"}
	x := 0
	for _, p := range parts {
		if p.n <= 0 {
			continue
		}
		b.Parts = append(b.Parts, barPart{Class: p.class, X: strconv.Itoa(x), W: strconv.Itoa(p.n), Title: p.title})
		x += p.n
	}
	return b
}
