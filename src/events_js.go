//go:build js

package main

import (
	"math"
	"strings"
	"unicode/utf8"
)

// Browser events, the parts that need no JavaScript: where a pointer lands on
// the canvas, which mouse buttons are down, wheel notches, typed characters
// and the web keyboard defaults. system_js.go copies each DOM event into a
// jsEvent and calls these, so the js tests can drive them with plain Go values.

// The kinds of DOM events the event pump queues (system_js.go).
const (
	jsEvKeyDown = iota
	jsEvKeyUp
	jsEvPointer // pointerdown, pointermove, pointerup, pointercancel
	jsEvWheel
	jsEvFocus
	jsEvBlur // window blur, or the tab hidden
	jsEvText // text for an open text input: typed, pasted, IME commit, phone keyboard
	jsEvComposition
	jsEvBackspace // a phone keyboard's delete, which sends no usable keydown
	jsEvClipboard // navigator.clipboard.readText() finished
	jsEvFullscreen
)

// One DOM event, copied out of JavaScript when it fires. pollEvents applies
// the queue in order once per frame, as SDL's event pump does on desktop.
type jsEvent struct {
	kind        int
	key         Key
	mod         ModifierKey
	text        string // printable key, pasted or committed text
	insert      bool   // keydown: also type text into the open text input
	clientX     float64
	clientY     float64
	buttons     int // PointerEvent.buttons
	pointerType string
	deltaX      float64
	deltaY      float64
	deltaMode   int
	on          bool // focus/fullscreen state
}

// The canvas's content box on the page in CSS pixels (border excluded), and
// whether the picture inside it keeps its aspect (object-fit: contain, the
// browser's own style for a fullscreen element).
type canvasBox struct {
	left, top, width, height float64
	contain                  bool
}

// canvasPoint converts a pointer position in page CSS pixels into canvas
// backing pixels, the window coordinates MouseInput.position expects (on the
// web the window size and the drawable size are both the backing size). A
// point on the bars around a contained picture lands outside 0..cw/0..ch, so
// position() reports it as outside the game.
func canvasPoint(clientX, clientY float64, box canvasBox, cw, ch int32) (int32, int32) {
	if box.width <= 0 || box.height <= 0 || cw <= 0 || ch <= 0 {
		return -1, -1
	}
	left, top := box.left, box.top
	sx, sy := box.width/float64(cw), box.height/float64(ch)
	if box.contain {
		s := math.Min(sx, sy)
		left += (box.width - float64(cw)*s) / 2
		top += (box.height - float64(ch)*s) / 2
		sx, sy = s, s
	}
	return int32(math.Floor((clientX - left) / sx)), int32(math.Floor((clientY - top) / sy))
}

// canvasToPage is canvasPoint the other way round: a point in canvas backing
// pixels to page CSS pixels, plus the CSS pixels per backing pixel. Places the
// hidden text field over the game's text box.
func canvasToPage(x, y float64, box canvasBox, cw, ch int32) (float64, float64, float64, float64) {
	if box.width <= 0 || box.height <= 0 || cw <= 0 || ch <= 0 {
		return 0, 0, 0, 0
	}
	left, top := box.left, box.top
	sx, sy := box.width/float64(cw), box.height/float64(ch)
	if box.contain {
		s := math.Min(sx, sy)
		left += (box.width - float64(cw)*s) / 2
		top += (box.height - float64(ch)*s) / 2
		sx, sy = s, s
	}
	return left + x*sx, top + y*sy, sx, sy
}

// PointerEvent.buttons bits in SDL's button order: left, right, middle, back
// (X1), forward (X2). Touch and pen contact count as the left button.
func pointerButtons(buttons int, pointerType string) (down [mouseClickButtons]bool) {
	if pointerType == "touch" || pointerType == "pen" {
		down[0] = buttons&1 != 0
		return
	}
	for i := range down {
		down[i] = buttons&(1<<i) != 0
	}
	return
}

// A wheel notch in CSS pixels. Chrome and Safari report a mouse wheel notch as
// about 100 px; Firefox reports 3 lines when deltaMode is read first, as here.
const wheelNotchPx = 100.0

// Accumulates wheel movement into notches, so a trackpad's many small pixel
// deltas scroll a menu at the pace of a wheel instead of one row per event.
type wheelAccum struct {
	x, y float64
}

func wheelPixels(delta float64, mode int) float64 {
	switch mode {
	case 1: // DOM_DELTA_LINE
		return delta * wheelNotchPx / 3
	case 2: // DOM_DELTA_PAGE
		return delta * wheelNotchPx
	}
	return delta
}

func wheelAxis(acc *float64, delta float64, mode int) int32 {
	px := wheelPixels(delta, mode)
	if px == 0 || math.IsNaN(px) || math.IsInf(px, 0) {
		return 0
	}
	// A direction change starts over, so a leftover never eats the first notch back.
	if *acc != 0 && (*acc > 0) != (px > 0) {
		*acc = 0
	}
	*acc += px
	n := math.Trunc(*acc / wheelNotchPx)
	*acc -= n * wheelNotchPx
	return int32(n)
}

// add returns whole notches in SDL's convention: x positive = right, y
// positive = up (away from the user). DOM deltaY is positive downward.
func (w *wheelAccum) add(dx, dy float64, mode int) (int32, int32) {
	return wheelAxis(&w.x, dx, mode), -wheelAxis(&w.y, dy, mode)
}

func (w *wheelAccum) reset() { *w = wheelAccum{} }

// The text a keydown types into an open text input: KeyboardEvent.key when it
// is one character and no Ctrl or Meta shortcut is held. AltGr arrives as
// Ctrl+Alt on Windows and types characters (@ on many layouts), so Ctrl with
// Alt still types.
func keyText(key string, ctrl, alt, meta bool) (string, bool) {
	if meta || ctrl && !alt || utf8.RuneCountInString(key) != 1 {
		return "", false
	}
	return key, true
}

// Paste shortcuts: Ctrl+V, Cmd+V and Shift+Insert. While a text input is open
// their keydown is left to the browser, which then fires the paste event that
// carries the clipboard text (a page cannot read it any other way without a
// permission prompt).
func isPasteShortcut(code string, ctrl, shift, meta bool) bool {
	return code == "KeyV" && (ctrl || meta) || code == "Insert" && shift && !ctrl && !meta
}

// The web keyboard defaults (Decision 30): WebMUGEN's own layouts instead of
// upstream's, which put Keyboard Controller 2 on the numpad alone (many
// laptops have none) and Keyboard Controller 1 on the arrows. Controller 1 is
// WASD with the attacks on the home row and the row above, controller 2 the
// arrows with the numpad, through the Standard setup (d = LT, w = LB, Decision
// 13). Every control is stated: a control left out would keep upstream's key
// for it and collide (LS = e is the Standard setup's w here).
var webKeyboardDefaults = map[string][][2]string{
	"ControllerMapping_Keyboard1": {
		{"DP_U", "w"}, {"DP_D", "s"}, {"DP_L", "a"}, {"DP_R", "d"},
		{"A", "k"}, {"B", "l"}, {"X", "i"}, {"Y", "o"},
		{"START", "RETURN"}, {"BACK", "-"},
		{"LB", "e"}, {"RB", "p"}, {"LT", "q"}, {"RT", "SEMICOLON"},
		{"LS_Y-", "-"}, {"LS_Y+", "-"}, {"LS_X-", "-"}, {"LS_X+", "-"},
		{"RS_Y-", "-"}, {"RS_Y+", "-"}, {"RS_X-", "-"}, {"RS_X+", "-"},
		{"LS", "-"}, {"RS", "-"},
	},
	"ControllerMapping_Keyboard2": {
		{"DP_U", "UP"}, {"DP_D", "DOWN"}, {"DP_L", "LEFT"}, {"DP_R", "RIGHT"},
		{"A", "KP_1"}, {"B", "KP_2"}, {"X", "KP_4"}, {"Y", "KP_5"},
		{"START", "KP_ENTER"}, {"BACK", "-"},
		{"LB", "KP_8"}, {"RB", "KP_6"}, {"LT", "KP_7"}, {"RT", "KP_3"},
		{"LS_Y-", "-"}, {"LS_Y+", "-"}, {"LS_X-", "-"}, {"LS_X+", "-"},
		{"RS_Y-", "-"}, {"RS_Y+", "-"}, {"RS_X-", "-"}, {"RS_X+", "-"},
		{"LS", "-"}, {"RS", "-"},
	},
}

// overrideIniKeys rewrites the values of the named keys inside the named
// sections of an ini text, keeping every other line as it is. Keys a section
// lacks are added at its end. Section and key names match without case, as
// the config reader's lookups do.
func overrideIniKeys(text string, sections map[string][][2]string) string {
	nl := "\n"
	if strings.Contains(text, "\r\n") {
		nl = "\r\n"
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines)+8)
	var cur [][2]string
	var done map[string]bool
	flush := func() {
		if cur == nil {
			return
		}
		// Missing keys go after the section's last key line, before blank and
		// comment lines that belong to the next section.
		at := len(out)
		for at > 0 {
			t := strings.TrimSpace(out[at-1])
			if t == "" || strings.HasPrefix(t, ";") || strings.HasPrefix(t, "#") {
				at--
				continue
			}
			break
		}
		var add []string
		for _, kv := range cur {
			if !done[strings.ToLower(kv[0])] {
				add = append(add, kv[0]+" = "+kv[1])
			}
		}
		if len(add) > 0 {
			out = append(out[:at], append(add, out[at:]...)...)
		}
		cur, done = nil, nil
	}
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			flush()
			name := strings.TrimSpace(t[1 : len(t)-1])
			for sec, kvs := range sections {
				if strings.EqualFold(sec, name) {
					cur, done = kvs, map[string]bool{}
				}
			}
			out = append(out, line)
			continue
		}
		if cur != nil && !strings.HasPrefix(t, ";") && !strings.HasPrefix(t, "#") {
			if eq := strings.IndexByte(line, '='); eq > 0 {
				name := strings.TrimSpace(line[:eq])
				for _, kv := range cur {
					if strings.EqualFold(kv[0], name) {
						// Keep the key's own spelling and alignment.
						line = line[:eq+1] + " " + kv[1]
						done[strings.ToLower(kv[0])] = true
						break
					}
				}
			}
		}
		out = append(out, line)
	}
	flush()
	return strings.Join(out, nl)
}

// platformDefaultConfig is the shipped defaultConfig.ini as the browser build
// uses it (config.go loadConfig): WebMUGEN's keyboard layouts.
func platformDefaultConfig(text string) string {
	return overrideIniKeys(text, webKeyboardDefaults)
}
