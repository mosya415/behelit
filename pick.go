package main

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// One menu, five callers: /setup's screens, /model, /role <n> model, /role <n>
// tier and /endpoint. It is written in LineEditor's idiom — the same
// "\r\033[J", print, "\033[<n>A" repaint, the same cooked fallback when raw
// mode is not available — because that is the only cursor technique in this
// tree and two of them would drift.
//
// The decisions live in pickState, which has no terminal in it: `go test` has
// no terminal either, so a picker whose behaviour lived in the drawing code
// would be a picker nothing checks.

// choice is one row. detail is DATA (model ids, windows, urls) and is printed
// verbatim; only the chrome around it is styled (ui.go rule 2). note and warn
// appear under the row while it is highlighted, so a caveat costs no column.
type choice struct {
	id     string // what the caller matches on afterwards
	label  string
	detail string // right-hand columns, pre-rendered by the caller
	note   string // faint line under the highlighted row
	warn   string // ◐ line under the highlighted row
	on     bool   // pre-ticked (multi) / the current one (single)
	// seq is this row's 1-based position in the ORDERED list the caller seeded
	// the ticks from (a role's models: chain). 0 = not seeded. result() gives
	// those rows back in that order, because a chain is a fallback order and
	// opening the menu to look at it must not rewrite it — see result().
	seq int
}

type pickOpts struct {
	title  string // section() header
	detail string // section()'s detail argument
	multi  bool   // space toggles; Enter returns every ticked row
	hint   string // key legend; "" = the default for multi/single
	height int    // 0 = from osTermSize()
}

var errPickCancel = errors.New("selection cancelled") // Ctrl-C / Ctrl-D / ESC ESC / EOF
var errNoTTY = errors.New("no terminal")

// pickState is the picker with no terminal: cursor, viewport, filter, ticks.
// pick() draws it and feeds it keys and does nothing else, so the behaviour is
// tested without a tty and the drawing code holds no decisions.
type pickState struct {
	title    string // the panel's own title; "" draws no frame at all
	detail   string
	rows     []choice
	view     []int // indices matching filter, in row order
	cur, top int   // cur indexes view, not rows
	filter   string
	multi    bool
	height   int
	hintText string
	sel      map[int]bool
	done     bool
	canc     bool
}

func newPickState(cs []choice, o pickOpts) *pickState {
	p := &pickState{title: o.title, detail: o.detail, rows: cs, multi: o.multi, height: o.height, hintText: o.hint, sel: map[int]bool{}}
	if p.height <= 0 {
		p.height = pickHeight()
	}
	for i, c := range cs {
		if c.on && o.multi {
			p.sel[i] = true
		}
	}
	p.refilter()
	// The cursor opens on the current value, and this has to happen AFTER
	// refilter builds the view: cur indexes the view, not the rows.
	if !o.multi {
		for i, ri := range p.view {
			if cs[ri].on {
				p.cur = i
				break
			}
		}
		p.clamp()
	}
	return p
}

// pickChrome is how many lines lines() draws that are NOT rows: the two courses
// of the frame, up to two wrapped legend lines, and the "… n more" tail.
//
// It has to be paid for. The reserve below was tuned for the old layout, where
// section() printed the title ABOVE the repainted block and outside it; the frame
// is built inside lines() now, so every repaint is two lines taller than it was.
// At ten rows the region became taller than the screen, which scrolls before the
// "\033[<n>A" executes — the walk-back then lands above where the menu now is and
// the next repaint's "\r\033[J" erases whatever was there.
// It counts only the chrome that is ALWAYS there. The highlighted row's own note
// and warning are extra — up to two wrapped lines each, and only ever on one row
// — which is why the reserve below keeps four more rows spare on top of it.
const pickChrome = 5

// pickHeight is the viewport: whatever the terminal has, minus the chrome lines
// lines() is known to add and the four it may add for the highlighted row's note
// — and a sane middle when there is no terminal to ask.
func pickHeight() int {
	_, rows := osTermSize()
	if rows <= 0 {
		return 12
	}
	return min(max(rows-pickChrome-4, 3), 20)
}

// refilter rebuilds the visible rows and keeps the cursor on the row it was on
// when that row survived the filter — retyping a character must not move the
// selection somewhere the operator did not look.
func (p *pickState) refilter() {
	want := -1
	if p.cur >= 0 && p.cur < len(p.view) {
		want = p.view[p.cur]
	}
	f := strings.ToLower(p.filter)
	p.view = nil
	for i, c := range p.rows {
		if f == "" || strings.Contains(strings.ToLower(c.label+" "+c.detail), f) {
			p.view = append(p.view, i)
		}
	}
	p.cur = 0
	for i, ri := range p.view {
		if ri == want {
			p.cur = i
		}
	}
	p.clamp()
}

func (p *pickState) clamp() {
	if len(p.view) == 0 {
		p.cur, p.top = 0, 0
		return
	}
	p.cur = min(max(p.cur, 0), len(p.view)-1)
	if p.cur < p.top {
		p.top = p.cur
	}
	if p.cur >= p.top+p.height {
		p.top = p.cur - p.height + 1
	}
	p.top = min(max(p.top, 0), max(len(p.view)-p.height, 0))
}

// key drives the state machine. The vocabulary is "up" "down" "pgup" "pgdn"
// "home" "end" "space" "enter" "cancel" "bs" "clear" "all" "none", or a literal
// rune as a one-rune string, which appends to the filter.
//
// Arrows navigate and letters filter. Binding j/k to navigation as well is
// ambiguous the moment a model id contains a j, and on a sixty-model gateway
// "pick with the arrow keys, not by typing ids" only holds if typing narrows:
// `qwen3-coder-480b-a35b-instruct` is reached by typing 480 and pressing Enter.
// all/none are therefore ^a/^n and not the letters a and n.
func (p *pickState) key(k string) {
	switch k {
	case "up":
		if len(p.view) > 0 {
			p.cur = (p.cur - 1 + len(p.view)) % len(p.view)
		}
	case "down":
		if len(p.view) > 0 {
			p.cur = (p.cur + 1) % len(p.view)
		}
	case "pgup":
		p.cur -= p.height
	case "pgdn":
		p.cur += p.height
	case "home":
		p.cur = 0
	case "end":
		p.cur = len(p.view) - 1
	case "space":
		if p.multi && p.cur < len(p.view) {
			i := p.view[p.cur]
			if p.sel[i] {
				delete(p.sel, i)
			} else {
				p.sel[i] = true
			}
		}
	case "all":
		if p.multi {
			for _, i := range p.view {
				p.sel[i] = true
			}
		}
	case "none":
		if p.multi {
			for _, i := range p.view {
				delete(p.sel, i)
			}
		}
	case "enter":
		if len(p.view) > 0 || p.multi {
			p.done = true
		}
	case "cancel":
		p.canc, p.done = true, true
	case "bs":
		if r := []rune(p.filter); len(r) > 0 {
			p.filter = string(r[:len(r)-1])
			p.refilter()
			return
		}
	case "clear":
		if p.filter != "" {
			p.filter = ""
			p.refilter()
			return
		}
	default:
		if len([]rune(k)) == 1 && k >= " " {
			p.filter += k
			p.refilter()
			return
		}
	}
	p.clamp()
}

// result is the selection: the rows the caller SEEDED first, in the order it
// seeded them, then everything else in ROW order (the gateway's /v1/models
// order). Tick order is deliberately not used — unticking and reticking a row
// would silently reorder a chain.
//
// The seeded half exists because row order is the gateway's, not the operator's:
// a role whose models: is [glm, qwen] is drawn with qwen first, so opening the
// menu and pressing Enter used to hand back [qwen, glm] and quietly demote the
// preferred model — for the reviewer role, onto the coder's own family. A row
// the filter hides stays ticked: sel is keyed on the row index, not the view.
func (p *pickState) result() []int {
	if p.canc {
		return nil
	}
	if !p.multi {
		if p.cur >= len(p.view) {
			return nil
		}
		return []int{p.view[p.cur]}
	}
	var seeded, rest []int
	for i := range p.rows {
		switch {
		case !p.sel[i]:
		case p.rows[i].on && p.rows[i].seq > 0:
			seeded = append(seeded, i)
		default:
			rest = append(rest, i)
		}
	}
	sort.SliceStable(seeded, func(a, b int) bool { return p.rows[seeded[a]].seq < p.rows[seeded[b]].seq })
	return append(seeded, rest...)
}

// lines is exactly what pick() prints, ANSI included, so the rendering is
// checkable without a terminal.
func (p *pickState) lines() []string {
	// The terminal's real width, not section()'s 76-column rule: a row is DATA,
	// and a served id like qwen3-coder-480b-a35b-instruct plus its window does not
	// fit in 76. Ellipsizing here is only to stop a wrap, which would desynchronise
	// the "\033[<n>A" walk-back.
	// The frame is built INSIDE this function and never around it: pick() walks the
	// cursor back by exactly len(lines())-1, so the top and bottom courses have to
	// be lines this function counted, and every row has to be measured against the
	// frame's inner width rather than the terminal's. A row that wraps
	// desynchronises the walk-back and the next repaint's "\r\033[J" then erases
	// whatever was above the menu.
	framed := p.title != ""
	// ONE reading of the terminal for the whole repaint. The frame, the rows, the
	// notes and the legend are all measured against this number and the walk-back
	// counts the lines it produced, so a window resized between two of those
	// measurements cannot leave the region half at one width and half at another —
	// which is the shear. panel.at() is what carries the snapshot into the frame.
	h := houseWidth()
	pnl := newPanel(p.title, p.detail)
	w := h + 1
	if framed {
		w = h - 4
	}
	labelW := 0
	for _, i := range p.window() {
		labelW = max(labelW, visibleWidth(p.rows[i].label))
	}
	// Half the content, never less than the 40 columns it always had: a served id
	// like qwen3-coder-480b-a35b-instruct is 30 columns before its window gauge,
	// and a sixty-model gateway is exactly where a wide window pays.
	labelW = min(labelW, max(40, (h-4)/2))
	var out []string
	add := func(s string) {
		// Cut from the RIGHT, not the middle. A row reads left to right in priority
		// order — the model id, then its window gauge, then the number, then where
		// the number came from — and middle-ellipsizing took the bite out of the
		// window column, which is the one thing on the row an operator cannot
		// guess. What is lost from the right is the family and any annotation, and
		// the highlighted row's note carries those anyway.
		if visibleWidth(s) > w {
			s = ellipsize(stripANSI(s), w)
		}
		if framed {
			pnl.Line("%s", s)
			return
		}
		out = append(out, s)
	}
	// inside a frame the rows keep their own indent but lose the outer one the
	// frame now provides
	lead := "   "
	if framed {
		lead = ""
	}
	if len(p.view) == 0 {
		add(lead + faint("nothing matches %q", p.filter))
	}
	for _, i := range p.window() {
		c := p.rows[i]
		// [ ] and [*] rather than a bare dot: a tick has to be readable as a tick
		// with no colour, and a bracket is the only thing that says "this is a box
		// that is empty" once the green is gone.
		tick := cFaint + "[ ]" + cReset
		if p.multi && p.sel[i] || !p.multi && c.on {
			tick = cGreen + "[" + gUp + "]" + cReset
		}
		cursor := " "
		here := p.cur < len(p.view) && p.view[p.cur] == i
		label := padTo(c.label, labelW, 0)
		if here {
			// the cursor row's own label is lit too, so "here" is stated twice: with
			// no colour at all ▸ is the only signal, and with colour the row the keys
			// will act on should be the row the eye lands on
			cursor = cYellow + cBold + gCursor + cReset
			label = cBold + padTo(c.label, labelW, 0) + cReset
		}
		add(lead + tick + " " + cursor + " " + label + "  " + c.detail)
		// A note is a sentence, and a sentence cut in the middle is unreadable, so
		// it WRAPS onto known extra lines instead of being ellipsized like a row.
		// The line count stays known either way, which is what the walk-back needs.
		if here && c.note != "" {
			for i, l := range clamp2(c.note, min(w-12, proseWidth())) {
				add(lead + "     " + faint("%s%s", map[bool]string{true: gHint + " ", false: strings.Repeat(" ", visibleWidth(gHint)+1)}[i == 0], l))
			}
		}
		if here && c.warn != "" {
			for i, l := range clamp2(c.warn, min(w-12, proseWidth())) {
				mark := cYellow + gPartial + cReset + " "
				if i > 0 {
					mark = strings.Repeat(" ", visibleWidth(gPartial)+1)
				}
				add(lead + "     " + mark + faint("%s", l))
			}
		}
	}
	if n := len(p.view) - (p.top + p.height); n > 0 {
		add(lead + faint("%s %d more", gEllipsis, n))
	}
	if framed {
		// The frame is pinned HERE and not before the rows went in, because it is
		// drawn to the rows: a 62-column menu inside a 161-column box on a 200-column
		// screen was 97 blank columns with a ║ at the end of them. It is still ONE
		// width for the whole repaint, and still derived from the single h reading
		// above — the rows were measured against h-4, so the frame they ask for cannot
		// exceed h and nothing in the region can be at two widths.
		pnl.at(min(h, max(min(panelFloor, h), pnl.contentWidth())))
		out = append(out, pnl.Lines()...)
	}
	// The legend WRAPS and is never ellipsized: at 80 columns "^a all · ^n none ·
	// type to filter" was being cut to "^a a…", so the two keys that make a
	// sixty-model gateway pickable — select-all and the filter — were invisible on
	// exactly the screens that need them. The line count stays known either way,
	// which is all the walk-back needs.
	for i, l := range wrapTo(p.legend(), h-4) {
		lead := "   " + faint("%s ", gHint)
		if i > 0 {
			lead = "     "
		}
		out = append(out, lead+faint("%s", l))
	}
	return out
}

// clamp2 is wrapTo with a hard stop at two lines, the second ellipsized. The
// picker's reserve (pickChrome + 4) pays for exactly two lines of note and two of
// warning on the one highlighted row; before this, the note was merely EXPECTED
// to be short, and a long one at a narrow terminal took four lines on its own and
// blew a budget the "\033[<n>A" walk-back depends on. The legend is deliberately
// not clamped: it is the only place ^a, ^n and "type to filter" are documented,
// and documentation that is cut has not been written.
func clamp2(s string, n int) []string {
	ls := wrapTo(s, n)
	if len(ls) > 2 {
		ls = []string{ls[0], ellipsize(strings.Join(ls[1:], " "), max(n, 20))}
	}
	return ls
}

// wrapTo breaks a sentence at spaces into lines of at most n columns.
func wrapTo(s string, n int) []string {
	if n < 20 {
		n = 20
	}
	var out []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = word
		case visibleWidth(line)+1+visibleWidth(word) <= n:
			line += " " + word
		default:
			out = append(out, line)
			line = word
		}
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}

// window is the row indices currently on screen.
func (p *pickState) window() []int {
	if len(p.view) == 0 {
		return nil
	}
	end := min(p.top+p.height, len(p.view))
	return p.view[p.top:end]
}

func (p *pickState) legend() string {
	if p.hintText != "" {
		return p.hintText
	}
	var parts []string
	if p.filter != "" {
		parts = append(parts, fmt.Sprintf("filter %q"+gSep+"%s narrows back"+gSep+"^u clears", p.filter, gBack))
	}
	// "arrow keys move", not "↑↓ move": the legend is the only place these keys are
	// documented, and on a locale that never said UTF-8 the two arrows arrive as
	// mojibake — while a rune swap reads as "updn move", which is worse than both.
	parts = append(parts, "arrow keys move")
	if p.multi {
		parts = append(parts, fmt.Sprintf("space ticks"+gSep+"%d chosen", len(p.result())), "^a all", "^n none")
	}
	parts = append(parts, "type to filter", "enter confirms", "ctrl-c aborts")
	return strings.Join(parts, gSep)
}

// keyName turns one byte (reading more when it is ESC) into pickState's key
// vocabulary. It shares readEscape with the line editor, so the two cannot
// disagree about what ↑ is.
func keyName(in *Input, b byte) string {
	switch b {
	case 3, 4: // Ctrl-C, Ctrl-D
		return "cancel"
	case 13, 10:
		return "enter"
	case 32:
		return "space"
	case 127, 8:
		return "bs"
	case 21: // Ctrl-U
		return "clear"
	case 1: // Ctrl-A
		return "all"
	case 14: // Ctrl-N
		return "none"
	case 27:
		switch readEscape(in) {
		case "A":
			return "up"
		case "B":
			return "down"
		case "H", "1~":
			return "home"
		case "F", "4~":
			return "end"
		case "5~":
			return "pgup"
		case "6~":
			return "pgdn"
		case "esc":
			return "cancel"
		}
		return ""
	}
	if b >= 32 {
		return string(readRuneIn(in, b))
	}
	return ""
}

// pickLoop reads bytes, names the key and drives p, calling draw after each
// one. pick() supplies a draw that paints the terminal; a test supplies nil and
// drives the same state machine the terminal would, which is how the escape
// decoding gets tested at all.
func pickLoop(in *Input, p *pickState, draw func()) error {
	for !p.done {
		b, err := in.ReadByte()
		if err != nil {
			return errPickCancel // EOF is not an answer
		}
		if k := keyName(in, b); k != "" {
			p.key(k)
		}
		if draw != nil && !p.done {
			draw()
		}
	}
	if p.canc {
		return errPickCancel
	}
	return nil
}

// pick draws the menu and returns the selected row indices.
//
// It refuses without a terminal BEFORE reading a byte and before entering raw
// mode, and leaves the buffer untouched, so a refused picker cannot eat the
// next command and the caller can fall back to its own text output.
func pick(in *Input, cs []choice, o pickOpts) ([]int, error) {
	if len(cs) == 0 {
		return nil, errors.New("nothing to choose from")
	}
	if !in.IsTTY() {
		return nil, errNoTTY
	}
	// no section() here any more: the title is set into the panel's own top course
	// by pickState.lines(), so it repaints with the rows instead of sitting above
	// them at a different width
	fmt.Println()
	p := newPickState(cs, o)
	// A scripted Input has no terminal discipline to switch off, so raw mode is
	// neither possible nor needed; a real one that refuses it (Windows) gets the
	// numbered cooked read instead of a hang.
	if fd := in.fd(); fd >= 0 {
		restore, err := makeRaw(fd)
		if err != nil {
			return pickCooked(in, p)
		}
		defer restore()
	}
	// outMu for the duration of each repaint and NEVER across the blocking read:
	// a background subagent's line printed between the rows and the "\033[<n>A"
	// walk-back lands the cursor above where the menu now is, and the next
	// repaint's "\r\033[J" then erases that subagent's output. confirm() has held
	// this lock from the start; the picker's five callers did not.
	draw := func() {
		ls := p.lines()
		out := "\r\033[J" + strings.Join(ls, "\r\n")
		if n := len(ls) - 1; n > 0 {
			out += fmt.Sprintf("\033[%dA", n)
		}
		outMu.Lock()
		fmt.Print(out + "\r")
		outMu.Unlock()
	}
	draw()
	err := pickLoop(in, p, draw)
	outMu.Lock()
	fmt.Print("\r\033[J") // the caller says what was chosen; the menu leaves no ruin
	outMu.Unlock()
	if err != nil {
		return nil, err
	}
	return p.result(), nil
}

// pickCooked is the picker without raw mode: the rows numbered, one line read.
// It exists so the non-unix build stays usable and so a terminal that refuses
// raw mode gets an answer instead of a hang.
func pickCooked(in *Input, p *pickState) ([]int, error) {
	// the cooked path scrolls, so it gets the lintel and not a frame: a frame
	// reprinted on every bad answer would stack boxes down the screen
	if p.title != "" {
		section(p.title, p.detail)
	}
	for {
		for i, c := range p.rows {
			mark := " "
			if c.on {
				mark = cGreen + gUp + cReset
			}
			fmt.Printf("   %s %2d  %s  %s\n", mark, i+1, c.label, c.detail)
		}
		if p.multi {
			hint("%s", "numbers, comma-separated (e.g. 1,3)"+gSep+"empty keeps the marked ones")
		} else {
			hint("%s", "one number"+gSep+"empty keeps the marked one")
		}
		fmt.Print("  " + cFaint + gPrompt + cReset + " ")
		line, err := in.ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			return nil, errPickCancel
		}
		line = strings.TrimSpace(line)
		if line == "" {
			if r := p.result(); len(r) > 0 {
				return r, nil
			}
			errLine("nothing is marked — pick a number")
			continue
		}
		var out []int
		bad := false
		for _, f := range strings.Split(line, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil || n < 1 || n > len(p.rows) {
				errLine("%q is not one of 1..%d", strings.TrimSpace(f), len(p.rows))
				bad = true
				break
			}
			out = append(out, n-1)
		}
		if bad {
			continue
		}
		if !p.multi && len(out) > 1 {
			errLine("one number, not %d", len(out))
			continue
		}
		return out, nil
	}
}

// pickOne is the single-select shorthand: the chosen row's index, or -1 when
// the operator kept what was there.
func pickOne(in *Input, cs []choice, o pickOpts) (int, error) {
	o.multi = false
	idx, err := pick(in, cs, o)
	if err != nil {
		return -1, err
	}
	if len(idx) == 0 {
		return -1, nil
	}
	return idx[0], nil
}

// askSecret reads one line that is never echoed and never remembered: • per
// rune, no history, no fences. The only caller is the wizard's plaintext api-key
// field, and both halves matter — the raw editor renders the buffer on every
// keystroke, and every non-empty submit used to land in the ↑ history of the
// prompt that opens right afterwards, one keystroke from being sent to the model
// as a user message.
func askSecret(in *Input, prompt string) (string, error) {
	if !in.IsTTY() {
		return "", errNoTTY
	}
	ed := NewLineEditor(in)
	ed.bare, ed.noHistory, ed.secret = true, true, true
	line, err := ed.ReadLine("  "+cFaint+gPrompt+cReset+" "+prompt, "")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// ask reads one line with the real line editor, prefilled with cur, so "Enter
// keeps it" is literally true. errLineCancel passes straight through.
//
// Like pick() and confirm() it refuses without a terminal BEFORE reading a byte.
// Its callers are all wizard steps, and the wizard is gated already — but a
// read that blocks forever is the one failure that cannot be recovered from, so
// every door to the keyboard carries its own lock rather than trusting the
// corridor it stands in.
func ask(in *Input, ed *LineEditor, prompt, cur string) (string, error) {
	if !in.IsTTY() {
		return "", errNoTTY
	}
	if ed == nil {
		ed = NewLineEditor(in)
	}
	line, err := ed.ReadLine("  "+cFaint+gPrompt+cReset+" "+prompt, cur)
	if err != nil {
		return "", err
	}
	// "Enter keeps it" is literally true on both paths: the raw editor starts with
	// cur in the buffer, and the cooked read hands it back on an empty line.
	if strings.TrimSpace(line) == "" {
		return cur, nil
	}
	return strings.TrimSpace(line), nil
}

// confirm is the wizard's yes/no. It deliberately does NOT go through Approver:
// a wizard question is not a tool approval, and -y / TrustAll must never
// silently write roles.yaml and config.json. It borrows Approver.Confirm's
// discipline instead — hold outMu, Drain, stash TakePending and Put it back —
// because that is what keeps a stray keystroke from answering a question it
// never saw, and subagent output from scrolling the question away.
func confirm(in *Input, question string, def bool) (bool, error) {
	if !in.IsTTY() {
		return false, errNoTTY
	}
	outMu.Lock()
	defer outMu.Unlock()
	legend := "[y/N]"
	if def {
		legend = "[Y/n]"
	}
	fmt.Printf("\n  %s %s%s%s %s%s%s ", question, cFaint, legend, cReset, cFaint, gPrompt, cReset)
	// Anything typed before the question appeared is not an answer to it: set it
	// aside and give it back afterwards, exactly as Approver.Confirm does. With no
	// terminal behind the Input there is no "before" — a scripted session's buffer
	// IS its answer stream — so the stash is skipped rather than eating the reply.
	if in.f != nil {
		in.Drain()
		stashed := in.TakePending()
		defer in.Put(stashed)
	}
	// Raw, one byte, exactly as pick() does it. A cooked ReadString leaves ISIG on,
	// so Ctrl-C at any of these questions did not abort the wizard — it killed lca
	// with SIGINT, including at the very first screen a new operator sees. The
	// cooked read below stays for a scripted Input and for a terminal that refuses
	// raw mode: there the buffer IS the answer stream and there is no signal to
	// intercept.
	if fd := in.fd(); fd >= 0 {
		if restore, err := makeRaw(fd); err == nil {
			defer restore()
			return confirmRaw(in, def)
		}
	}
	line, err := in.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		fmt.Println()
		return false, errPickCancel
	}
	switch strings.ToLower(strings.TrimSpace(strings.Trim(line, "\r\n"))) {
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	case "":
		return def, nil
	case "\x03":
		return false, errPickCancel
	}
	return false, nil
}

// confirmKey is the whole decision, with no terminal in it: one byte in, the
// answer out. Split out so the raw path's Ctrl-C handling is testable — makeRaw
// needs a real tty, which `go test` has not got.
//
// decided=false means "that key says nothing", and the reader waits for another.
func confirmKey(b byte, def bool) (ans, decided, cancel bool) {
	switch b {
	case 'y', 'Y':
		return true, true, false
	case 'n', 'N':
		return false, true, false
	case 13, 10: // Enter takes the default
		return def, true, false
	case 3, 4: // Ctrl-C, Ctrl-D — abort the wizard, do NOT kill the session
		return false, true, true
	}
	return false, false, false
}

// confirmRaw reads the answer a key at a time and echoes the letter itself, so
// the line still reads `set up now? [Y/n] › y` with the terminal's own echo off.
func confirmRaw(in *Input, def bool) (bool, error) {
	for {
		b, err := in.ReadByte()
		if err != nil {
			fmt.Print("\r\n")
			return false, errPickCancel
		}
		if b == 27 { // ESC ESC is the pickers' "get me out of here"
			if readEscape(in) == "esc" {
				fmt.Print("\r\n")
				return false, errPickCancel
			}
			continue
		}
		ans, decided, cancel := confirmKey(b, def)
		if !decided {
			continue
		}
		if cancel {
			fmt.Print("^C\r\n")
			return false, errPickCancel
		}
		fmt.Print(map[bool]string{true: "y", false: "n"}[ans] + "\r\n")
		return ans, nil
	}
}
