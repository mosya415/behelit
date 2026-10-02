package main

import (
	"strings"
	"testing"
)

// The operator's screen filled with dozens of stacked copies of their own
// half-typed question: a 153-column line in an 80-column terminal, repainted
// once per keystroke. Printed whole, the terminal soft-wraps it onto a second
// row and every walk-back below is off by that row.
func TestTheInputRowNeverWraps(t *testing.T) {
	line := []rune("есть еще проблема, в гейтвее не отображается еще одна модель glm5.3, в конфигураторе нельзя сейчас выбрать видеокарту под замеры, сейчас только b200")
	for _, room := range []int{20, 40, 71, 100, 152} {
		for _, pos := range []int{0, 1, 17, len(line) / 2, len(line) - 1, len(line)} {
			lo, hi, cutL, cutR := inputWindow(line, pos, room)
			width := visibleWidth(string(line[lo:hi]))
			if cutL {
				width += visibleWidth(gEllipsis)
			}
			if cutR {
				width += visibleWidth(gEllipsis)
			}
			if width > room {
				t.Fatalf("room=%d pos=%d: window is %d columns wide — the row would wrap", room, pos, width)
			}
			if pos < lo || pos > hi {
				t.Fatalf("room=%d pos=%d: the cursor fell outside the window [%d,%d)", room, pos, lo, hi)
			}
		}
	}
}

// A line that fits is shown whole, with no markers: the window must not be a
// cost paid by every ordinary line.
func TestAShortInputIsShownWhole(t *testing.T) {
	rs := []rune("собери и запусти тесты")
	lo, hi, cutL, cutR := inputWindow(rs, len(rs), 80)
	if lo != 0 || hi != len(rs) || cutL || cutR {
		t.Fatalf("a %d-column line in 80 columns must be whole: [%d,%d) cuts %v/%v", visibleWidth(string(rs)), lo, hi, cutL, cutR)
	}
}

// Wide runes are two columns each, so a window measured in runes would shear
// exactly the lines this exists to keep whole.
func TestTheWindowCountsColumnsNotRunes(t *testing.T) {
	rs := []rune(strings.Repeat("漢", 60)) // 120 columns
	lo, hi, cutL, cutR := inputWindow(rs, 30, 40)
	width := visibleWidth(string(rs[lo:hi]))
	if cutL {
		width += visibleWidth(gEllipsis)
	}
	if cutR {
		width += visibleWidth(gEllipsis)
	}
	if width > 40 {
		t.Fatalf("%d columns in a 40-column row", width)
	}
	if hi-lo > 20 {
		t.Fatalf("%d wide runes cannot fit: they are two columns each", hi-lo)
	}
}

// The window is computed from the cursor every repaint and holds no state, so
// walking a long line end to end must never produce an invalid one.
func TestWalkingALongLineKeepsTheWindowValid(t *testing.T) {
	rs := []rune(strings.Repeat("abcdefghij ", 40)) // 440 columns
	for pos := 0; pos <= len(rs); pos++ {
		lo, hi, cutL, cutR := inputWindow(rs, pos, 60)
		if lo > hi || pos < lo || pos > hi {
			t.Fatalf("pos=%d: window [%d,%d)", pos, lo, hi)
		}
		w := visibleWidth(string(rs[lo:hi]))
		if cutL {
			w += visibleWidth(gEllipsis)
		}
		if cutR {
			w += visibleWidth(gEllipsis)
		}
		if w > 60 {
			t.Fatalf("pos=%d: %d columns in a 60-column row", pos, w)
		}
	}
}

// The banner's hints are packed to the terminal rather than split by hand: the
// hand-split version fixed one line at 85 columns and left another at 99, which
// wrapped at 80 and put a hint's tail on a row of its own with no lead-in.
func TestHintsArePackedToTheTerminal(t *testing.T) {
	segs := []string{"Ctrl-C interrupts", "/setup picks the models and gives them roles", "lca init does it from the shell"}
	for _, room := range []int{40, 60, 78, 100, 158} {
		rows := packHints(room, segs)
		var joined []string
		for _, r := range rows {
			parts := strings.Split(r, gSep)
			// A row may exceed the room only when it holds ONE segment: a hint is
			// never cut, because half a hint is worse than a wrapped one.
			if w := visibleWidth(r); w > room && len(parts) > 1 {
				t.Fatalf("room=%d: row %q is %d columns across %d segments", room, r, w, len(parts))
			}
			joined = append(joined, parts...)
		}
		if strings.Join(joined, "|") != strings.Join(segs, "|") {
			t.Fatalf("room=%d: the hints came back changed: %q", room, joined)
		}
	}
}
