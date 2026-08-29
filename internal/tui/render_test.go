package tui

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestWidthHandlesWideRunesAndEscapes(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"abc", 3},
		{"", 0},
		{"日本語", 6},
		{"a日b", 4},
		{"\x1b[31mred\x1b[0m", 3},
		{"\x1b[1m\x1b[32m日本\x1b[0m", 4},
	} {
		if got := width(c.in); got != c.want {
			t.Errorf("width(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestTruncNeverSplitsWideRunes(t *testing.T) {
	// "日本語" is 6 columns; truncating to 5 must leave 日本 + ellipsis = 5.
	got := trunc("日本語", 5)
	if width(got) > 5 {
		t.Errorf("trunc gave width %d: %q", width(got), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis, got %q", got)
	}
	if trunc("abc", 10) != "abc" {
		t.Error("short strings should pass through untouched")
	}
	if trunc("abc", 0) != "" {
		t.Error("zero width should give empty")
	}
}

func TestPadding(t *testing.T) {
	if got := padRight("ab", 5); got != "ab   " {
		t.Errorf("padRight = %q", got)
	}
	if got := padLeft("ab", 5); got != "   ab" {
		t.Errorf("padLeft = %q", got)
	}
	// Wide text must be padded by columns, not runes.
	if got := padRight("日本", 6); width(got) != 6 {
		t.Errorf("padRight width = %d: %q", width(got), got)
	}
}

func TestPctNeverRoundsUpToFullHealth(t *testing.T) {
	// A 99.97% endpoint is not a 100% endpoint; that distinction is the
	// entire product.
	if got := pct(0.9997); got == "100%" {
		t.Errorf("pct(0.9997) = %q, must not claim 100%%", got)
	}
	if got := pct(1.0); got != "100%" {
		t.Errorf("pct(1.0) = %q", got)
	}
	if got := pct(0.5); got != "50.0%" {
		t.Errorf("pct(0.5) = %q", got)
	}
	if got := pct(math.NaN()); strings.TrimSpace(got) != "-" {
		t.Errorf("pct(NaN) = %q", got)
	}
}

func TestMsAndShortDur(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want string
	}{{0, "-"}, {1.25, "1.2"}, {42, "42"}, {15000, "15.0s"}} {
		if got := ms(c.in); got != c.want {
			t.Errorf("ms(%g) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, c := range []struct {
		in   time.Duration
		want string
	}{
		{0, "-"},
		{500 * time.Millisecond, "500ms"},
		{90 * time.Second, "1m30s"},
		{3*time.Hour + 5*time.Minute, "3h05m"},
		{50 * time.Hour, "2d02h"},
	} {
		if got := shortDur(c.in); got != c.want {
			t.Errorf("shortDur(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSparklineAndGaps(t *testing.T) {
	th := theme{on: false}
	got := sparkline(th, []float64{0, 1, 2, 3}, 0, 3)
	if width(got) != 4 {
		t.Fatalf("sparkline width = %d: %q", width(got), got)
	}
	if []rune(got)[0] != '▁' || []rune(got)[3] != '█' {
		t.Errorf("expected a rising ramp, got %q", got)
	}
	// A NaN is a hole in the data, not a zero.
	withGap := sparkline(th, []float64{1, math.NaN(), 3}, 0, 3)
	if !strings.Contains(withGap, "·") {
		t.Errorf("gap not rendered as a hole: %q", withGap)
	}
}

func TestUptimeStrip(t *testing.T) {
	th := theme{on: false}
	got := uptimeStrip(th, []float64{1, 0.5, 0, math.NaN()})
	if width(got) != 4 {
		t.Fatalf("strip width = %d: %q", width(got), got)
	}
	r := []rune(got)
	if r[0] != '█' {
		t.Errorf("perfect bucket should be a full block, got %q", string(r[0]))
	}
	if r[3] != '·' {
		t.Errorf("empty bucket should be a dot, got %q", string(r[3]))
	}
}

func TestBrailleChartShape(t *testing.T) {
	rows := brailleChart([]float64{0, 1, 2, 3, 4, 5}, 10, 4, 0, 5)
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4", len(rows))
	}
	for i, r := range rows {
		if width(r) != 10 {
			t.Errorf("row %d width = %d, want 10: %q", i, width(r), r)
		}
	}
	// Every glyph must be a space or a braille pattern.
	for _, r := range strings.Join(rows, "") {
		if r != ' ' && (r < 0x2800 || r > 0x28ff) {
			t.Fatalf("unexpected glyph %U in chart", r)
		}
	}
	// A rising series should put ink low on the left and high on the right.
	firstRow, lastRow := rows[0], rows[len(rows)-1]
	if strings.TrimSpace(firstRow) == "" || strings.TrimSpace(lastRow) == "" {
		t.Errorf("expected ink in both top and bottom rows:\n%s", strings.Join(rows, "\n"))
	}
	leftOfTop := strings.IndexFunc(firstRow, func(r rune) bool { return r != ' ' })
	leftOfBottom := strings.IndexFunc(lastRow, func(r rune) bool { return r != ' ' })
	if leftOfTop <= leftOfBottom {
		t.Errorf("rising series should reach the top row later than the bottom row\n%s",
			strings.Join(rows, "\n"))
	}
}

func TestBrailleChartEdgeCases(t *testing.T) {
	if rows := brailleChart(nil, 5, 2, 0, 1); len(rows) != 2 {
		t.Fatalf("empty values should still produce a blank chart, got %d rows", len(rows))
	}
	if rows := brailleChart([]float64{1}, 0, 2, 0, 1); rows != nil {
		t.Error("zero width should give nil")
	}
	// A flat series must not divide by zero.
	rows := brailleChart([]float64{5, 5, 5}, 6, 3, 5, 5)
	if len(rows) != 3 {
		t.Fatalf("got %d rows", len(rows))
	}
	// NaNs break the line rather than being drawn at zero.
	rows = brailleChart([]float64{1, math.NaN(), 1}, 6, 3, 0, 2)
	if len(rows) != 3 {
		t.Fatalf("got %d rows", len(rows))
	}
}

func TestThemeOffEmitsNoEscapes(t *testing.T) {
	th := theme{on: false}
	for _, s := range []string{th.red("x"), th.green("x"), th.bold("x"), th.health(0.5, "x")} {
		if strings.Contains(s, "\x1b") {
			t.Errorf("colour disabled but got escapes: %q", s)
		}
	}
	on := theme{on: true}
	if !strings.Contains(on.red("x"), "\x1b[31m") {
		t.Error("colour enabled but no escape emitted")
	}
}

func TestStripSGRAndSplitCells(t *testing.T) {
	styled := "\x1b[31ma\x1b[0m" + "b" + "\x1b[32mc\x1b[0m"
	if got := stripSGR(styled); got != "abc" {
		t.Errorf("stripSGR = %q", got)
	}
	cells := splitCells(styled)
	if len(cells) != 3 {
		t.Fatalf("splitCells gave %d cells: %q", len(cells), cells)
	}
	for i, c := range cells {
		if width(c) != 1 {
			t.Errorf("cell %d has width %d: %q", i, width(c), c)
		}
	}
}

func TestClampScroll(t *testing.T) {
	// Selection above the viewport scrolls up to it.
	if got := clampScroll(10, 3, 5, 100); got != 3 {
		t.Errorf("got %d", got)
	}
	// Selection below scrolls down just far enough.
	if got := clampScroll(0, 7, 5, 100); got != 3 {
		t.Errorf("got %d", got)
	}
	// Everything fits: no scroll.
	if got := clampScroll(4, 2, 10, 5); got != 0 {
		t.Errorf("got %d", got)
	}
	// Never scroll past the end.
	if got := clampScroll(99, 99, 5, 100); got != 95 {
		t.Errorf("got %d", got)
	}
}
