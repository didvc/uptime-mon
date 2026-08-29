package tui

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// colour
// ---------------------------------------------------------------------------

// Styles are plain SGR sequences. Only the 8 basic colours plus bright
// variants are used, so the output inherits whatever palette the user's
// terminal theme defines instead of fighting it with hard-coded RGB.
const (
	sgrReset = "\x1b[0m"
	sgrBold  = "\x1b[1m"
	sgrDim   = "\x1b[2m"
	sgrRev   = "\x1b[7m"
	fgRed    = "\x1b[31m"
	fgGreen  = "\x1b[32m"
	fgYellow = "\x1b[33m"
	fgCyan   = "\x1b[36m"
	fgGrey   = "\x1b[90m"
)

// theme wraps text in styles, or leaves it bare when colour is disabled.
type theme struct{ on bool }

func (t theme) s(style, text string) string {
	if !t.on || style == "" {
		return text
	}
	return style + text + sgrReset
}

func (t theme) bold(s string) string   { return t.s(sgrBold, s) }
func (t theme) dim(s string) string    { return t.s(sgrDim, s) }
func (t theme) red(s string) string    { return t.s(fgRed, s) }
func (t theme) green(s string) string  { return t.s(fgGreen, s) }
func (t theme) yellow(s string) string { return t.s(fgYellow, s) }
func (t theme) cyan(s string) string   { return t.s(fgCyan, s) }
func (t theme) grey(s string) string   { return t.s(fgGrey, s) }

// upDown colours text by health.
func (t theme) health(avail float64, s string) string {
	switch {
	case math.IsNaN(avail):
		return t.grey(s)
	case avail >= 0.999:
		return t.green(s)
	case avail >= 0.99:
		return t.yellow(s)
	default:
		return t.red(s)
	}
}

// ---------------------------------------------------------------------------
// width-aware text
// ---------------------------------------------------------------------------

// runeWidth returns the number of terminal columns a rune occupies.
//
// This is a deliberately small wcwidth: zero for combining marks, two for the
// East Asian Wide and Fullwidth ranges, one otherwise. It exists because the
// imported monitors carry Japanese keywords and names, and a table that
// assumes one column per rune tears itself apart on the first of them.
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 0x20 || (r >= 0x7f && r < 0xa0):
		return 0
	// Combining marks and zero-width characters.
	case r >= 0x0300 && r <= 0x036f,
		r >= 0x200b && r <= 0x200f,
		r == 0xfeff,
		r >= 0xfe00 && r <= 0xfe0f:
		return 0
	// East Asian Wide / Fullwidth.
	case r >= 0x1100 && r <= 0x115f, // Hangul Jamo
		r >= 0x2e80 && r <= 0x303e, // CJK radicals, Kangxi, punctuation
		r >= 0x3041 && r <= 0x33ff, // Kana, Hangul Compat, CJK compat
		r >= 0x3400 && r <= 0x4dbf, // CJK Ext A
		r >= 0x4e00 && r <= 0x9fff, // CJK Unified
		r >= 0xa000 && r <= 0xa4cf, // Yi
		r >= 0xac00 && r <= 0xd7a3, // Hangul syllables
		r >= 0xf900 && r <= 0xfaff, // CJK compat ideographs
		r >= 0xfe30 && r <= 0xfe6f, // CJK compat forms
		r >= 0xff00 && r <= 0xff60, // Fullwidth forms
		r >= 0xffe0 && r <= 0xffe6,
		r >= 0x1f300 && r <= 0x1f64f, // emoji
		r >= 0x1f900 && r <= 0x1f9ff,
		r >= 0x20000 && r <= 0x3fffd: // CJK Ext B+
		return 2
	}
	return 1
}

// width returns the display width of s, ignoring any SGR sequences in it.
func width(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		if inEsc {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		if r == 0x1b {
			inEsc = true
			continue
		}
		n += runeWidth(r)
	}
	return n
}

// trunc shortens s to at most max columns, appending an ellipsis when it had
// to cut. It refuses to split a double-width rune across the boundary.
func trunc(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if width(s) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		w := runeWidth(r)
		if used+w > max-1 {
			break
		}
		b.WriteRune(r)
		used += w
	}
	b.WriteString("…")
	return b.String()
}

// padRight pads s with spaces to exactly w columns (truncating if needed).
func padRight(s string, w int) string {
	s = trunc(s, w)
	if d := w - width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// padLeft right-aligns s in w columns.
func padLeft(s string, w int) string {
	s = trunc(s, w)
	if d := w - width(s); d > 0 {
		return strings.Repeat(" ", d) + s
	}
	return s
}

// ---------------------------------------------------------------------------
// number and duration formatting
// ---------------------------------------------------------------------------

// ms formats a millisecond value with a precision that suits its magnitude:
// sub-millisecond timings deserve decimals, a 12-second timeout does not.
func ms(v float64) string {
	switch {
	case v == 0:
		return "-"
	case v < 10:
		return fmt.Sprintf("%.1f", v)
	case v < 10000:
		return fmt.Sprintf("%.0f", v)
	default:
		return fmt.Sprintf("%.1fs", v/1000)
	}
}

// pct formats an availability fraction. It never rounds 99.97% up to 100%,
// because "100%" and "no failures observed" are different claims and the
// difference is the entire subject of the tool.
func pct(f float64) string {
	if math.IsNaN(f) {
		return "  -  "
	}
	switch {
	case f >= 1:
		return "100%"
	case f > 0.9999:
		return "99.99%"
	case f >= 0.9995:
		return fmt.Sprintf("%.2f%%", math.Floor(f*10000)/100)
	case f >= 0.99:
		return fmt.Sprintf("%.2f%%", f*100)
	default:
		return fmt.Sprintf("%.1f%%", f*100)
	}
}

// shortDur renders a duration in the largest unit that keeps it readable.
func shortDur(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.0fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// ---------------------------------------------------------------------------
// charts
// ---------------------------------------------------------------------------

// blocks are the eighth-height bars used for sparklines and uptime strips.
var blocks = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// sparkline renders values as a single row of block characters. Values that
// are NaN render as a dim dot: a gap in the data is not a zero.
func sparkline(t theme, values []float64, lo, hi float64) string {
	if len(values) == 0 {
		return ""
	}
	if hi <= lo {
		hi = lo + 1
	}
	var b strings.Builder
	for _, v := range values {
		if math.IsNaN(v) {
			b.WriteString(t.grey("·"))
			continue
		}
		f := (v - lo) / (hi - lo)
		i := int(f * float64(len(blocks)))
		if i < 0 {
			i = 0
		}
		if i >= len(blocks) {
			i = len(blocks) - 1
		}
		b.WriteRune(blocks[i])
	}
	return b.String()
}

// uptimeStrip renders per-bucket availability as a coloured bar: full height
// and green for a clean bucket, shorter and redder as failures accumulate.
func uptimeStrip(t theme, avail []float64) string {
	var b strings.Builder
	for _, a := range avail {
		if math.IsNaN(a) {
			b.WriteString(t.grey("·"))
			continue
		}
		i := int(a * float64(len(blocks)))
		if i >= len(blocks) {
			i = len(blocks) - 1
		}
		if a > 0 && i == 0 {
			i = 0 // keep a visible sliver for "mostly down but not silent"
		}
		b.WriteString(t.health(a, string(blocks[i])))
	}
	return b.String()
}

// brailleChart draws a line chart using braille dots, giving 2x4 resolution
// per character cell — a 60x8 chart carries 120 horizontal samples and 32
// vertical levels in the space of eight terminal rows.
//
// Braille dot numbering within a cell is:
//
//	1 4      bit0 bit3
//	2 5      bit1 bit4
//	3 6      bit2 bit5
//	7 8      bit6 bit7
func brailleChart(values []float64, w, h int, lo, hi float64) []string {
	if w <= 0 || h <= 0 {
		return nil
	}
	grid := make([][]rune, h)
	for i := range grid {
		grid[i] = make([]rune, w)
		for j := range grid[i] {
			grid[i][j] = ' '
		}
	}
	if len(values) == 0 {
		return gridToLines(grid)
	}
	if hi <= lo {
		hi = lo + 1
	}

	dotsW, dotsH := w*2, h*4
	dots := make([][]bool, dotsH)
	for i := range dots {
		dots[i] = make([]bool, dotsW)
	}

	// Map each dot column to a value, then join consecutive points vertically
	// so the result reads as a line rather than a scatter.
	yAt := func(x int) (int, bool) {
		idx := x * len(values) / dotsW
		if idx >= len(values) {
			idx = len(values) - 1
		}
		v := values[idx]
		if math.IsNaN(v) {
			return 0, false
		}
		f := (v - lo) / (hi - lo)
		if f < 0 {
			f = 0
		}
		if f > 1 {
			f = 1
		}
		y := int(math.Round((1 - f) * float64(dotsH-1)))
		return y, true
	}

	prevY, prevOK := 0, false
	for x := range dotsW {
		y, ok := yAt(x)
		if !ok {
			prevOK = false
			continue
		}
		if prevOK {
			step := 1
			if y < prevY {
				step = -1
			}
			for yy := prevY; yy != y; yy += step {
				dots[yy][x] = true
			}
		}
		dots[y][x] = true
		prevY, prevOK = y, true
	}

	for y := range dotsH {
		for x := range dotsW {
			if !dots[y][x] {
				continue
			}
			cy, cx := y/4, x/2
			var bit uint8
			row, col := y%4, x%2
			switch {
			case row < 3 && col == 0:
				bit = 1 << row
			case row < 3 && col == 1:
				bit = 1 << (row + 3)
			case col == 0:
				bit = 1 << 6
			default:
				bit = 1 << 7
			}
			if grid[cy][cx] == ' ' {
				grid[cy][cx] = rune(0x2800)
			}
			grid[cy][cx] |= rune(bit)
		}
	}
	return gridToLines(grid)
}

func gridToLines(grid [][]rune) []string {
	out := make([]string, len(grid))
	for i, row := range grid {
		out[i] = string(row)
	}
	return out
}
