//go:build js

package main

import (
	"strings"
	"testing"

	"gopkg.in/ini.v1"
)

func TestCanvasPointWindowed(t *testing.T) {
	// The full page: a 1280x720 backing shown at 960x540 CSS pixels, 1px
	// border already taken off by canvasBox, 13px from the left, 40px down.
	box := canvasBox{left: 13, top: 40, width: 960, height: 540}
	for _, c := range []struct {
		cx, cy float64
		x, y   int32
	}{
		{13, 40, 0, 0},
		{13 + 480, 40 + 270, 640, 360},
		{13 + 959.9, 40 + 539.9, 1279, 719},
		{13 + 0.74, 40 + 0.74, 0, 0}, // 0.74 CSS px = 0.99 backing px
		{13 + 0.76, 40 + 0.76, 1, 1},
		{12, 39, -2, -2}, // off the canvas: outside, never clamped in
		{13 + 960, 40 + 540, 1280, 720},
	} {
		x, y := canvasPoint(c.cx, c.cy, box, 1280, 720)
		if x != c.x || y != c.y {
			t.Errorf("windowed (%v,%v): got (%d,%d), want (%d,%d)", c.cx, c.cy, x, y, c.x, c.y)
		}
	}
	// Stretched (object-fit: fill): each axis scales on its own.
	x, y := canvasPoint(100, 100, canvasBox{width: 200, height: 400}, 640, 480)
	if x != 320 || y != 120 {
		t.Errorf("fill: got (%d,%d), want (320,120)", x, y)
	}
	if x, y := canvasPoint(5, 5, canvasBox{}, 640, 480); x != -1 || y != -1 {
		t.Errorf("empty box: got (%d,%d), want (-1,-1)", x, y)
	}
}

func TestCanvasPointLetterboxed(t *testing.T) {
	// Fullscreen on a 2560x1600 screen (16:10): the browser stretches the
	// canvas element over the screen and contains a 16:9 picture in it, so
	// 2560x1440 with 80px bars above and below.
	box := canvasBox{left: 0, top: 0, width: 2560, height: 1600, contain: true}
	for _, c := range []struct {
		cx, cy float64
		x, y   int32
	}{
		{0, 80, 0, 0},
		{1280, 800, 640, 360},
		{2559, 1519, 1279, 719},
		{1280, 40, 640, -20}, // on the top bar: above the game
		{1280, 1560, 640, 740},
	} {
		x, y := canvasPoint(c.cx, c.cy, box, 1280, 720)
		if x != c.x || y != c.y {
			t.Errorf("letterboxed (%v,%v): got (%d,%d), want (%d,%d)", c.cx, c.cy, x, y, c.x, c.y)
		}
	}
	// Pillarboxed: a 4:3 game on a 16:9 screen, 240px bars left and right.
	box = canvasBox{width: 1920, height: 1080, contain: true}
	if x, y := canvasPoint(240, 0, box, 640, 480); x != 0 || y != 0 {
		t.Errorf("pillarbox left edge: got (%d,%d), want (0,0)", x, y)
	}
	if x, y := canvasPoint(960, 540, box, 640, 480); x != 320 || y != 240 {
		t.Errorf("pillarbox center: got (%d,%d), want (320,240)", x, y)
	}
	if x, _ := canvasPoint(100, 540, box, 640, 480); x >= 0 {
		t.Errorf("pillarbox bar: x = %d, want < 0", x)
	}
	// A contained box of the picture's own shape has no bars.
	x1, y1 := canvasPoint(300, 200, canvasBox{width: 960, height: 540, contain: true}, 1280, 720)
	x2, y2 := canvasPoint(300, 200, canvasBox{width: 960, height: 540}, 1280, 720)
	if x1 != x2 || y1 != y2 {
		t.Errorf("contain without bars differs from fill: (%d,%d) vs (%d,%d)", x1, y1, x2, y2)
	}
}

func TestCanvasToPageRoundTrip(t *testing.T) {
	for _, box := range []canvasBox{
		{left: 13, top: 40, width: 960, height: 540},
		{width: 2560, height: 1600, contain: true},
		{width: 1920, height: 1080, contain: true},
	} {
		for _, p := range [][2]float64{{0, 0}, {640, 360}, {1000.5, 700.5}} {
			px, py, sx, sy := canvasToPage(p[0], p[1], box, 1280, 720)
			if sx <= 0 || sy <= 0 {
				t.Fatalf("%+v: scale %v,%v", box, sx, sy)
			}
			x, y := canvasPoint(px, py, box, 1280, 720)
			if x != int32(p[0]) || y != int32(p[1]) {
				t.Errorf("%+v: %v -> page (%v,%v) -> (%d,%d)", box, p, px, py, x, y)
			}
		}
	}
}

func TestPointerButtons(t *testing.T) {
	// PointerEvent.buttons: 1 left, 2 right, 4 middle, 8 back, 16 forward,
	// in SDL's order LEFT RIGHT MIDDLE X1 X2 (MouseInput.down).
	for _, c := range []struct {
		buttons int
		typ     string
		want    [mouseClickButtons]bool
	}{
		{0, "mouse", [mouseClickButtons]bool{}},
		{1, "mouse", [mouseClickButtons]bool{true}},
		{2, "mouse", [mouseClickButtons]bool{false, true}},
		{4, "mouse", [mouseClickButtons]bool{false, false, true}},
		{8, "mouse", [mouseClickButtons]bool{false, false, false, true}},
		{16, "mouse", [mouseClickButtons]bool{false, false, false, false, true}},
		{3, "mouse", [mouseClickButtons]bool{true, true}},
		{1, "touch", [mouseClickButtons]bool{true}},
		{0, "touch", [mouseClickButtons]bool{}},
		{3, "pen", [mouseClickButtons]bool{true}}, // barrel button held: still the left button
		{32, "pen", [mouseClickButtons]bool{}},    // eraser hovering
		{1, "", [mouseClickButtons]bool{true}},
	} {
		if got := pointerButtons(c.buttons, c.typ); got != c.want {
			t.Errorf("pointerButtons(%d, %q) = %v, want %v", c.buttons, c.typ, got, c.want)
		}
	}
}

func TestWheelNormalization(t *testing.T) {
	var w wheelAccum
	// A mouse wheel in Chrome: 100 px per notch, down is positive in the DOM
	// and negative in SDL's convention (y positive = up).
	if x, y := w.add(0, 100, 0); x != 0 || y != -1 {
		t.Errorf("one notch down: (%d,%d), want (0,-1)", x, y)
	}
	if x, y := w.add(0, -300, 0); x != 0 || y != 3 {
		t.Errorf("three notches up in one event: (%d,%d), want (0,3)", x, y)
	}
	// Firefox: lines, 3 per notch.
	w.reset()
	if _, y := w.add(0, 3, 1); y != -1 {
		t.Errorf("3 lines down: %d, want -1", y)
	}
	if _, y := w.add(0, -6, 1); y != 2 {
		t.Errorf("6 lines up: %d, want 2", y)
	}
	// Pages: one notch each.
	w.reset()
	if _, y := w.add(0, 1, 2); y != -1 {
		t.Errorf("1 page down: %d, want -1", y)
	}
	// A trackpad: many small pixel deltas add up to whole notches.
	w.reset()
	total := int32(0)
	for i := 0; i < 25; i++ { // 25 x 12 px = 300 px
		_, y := w.add(0, 12, 0)
		total += y
		if i < 8 && y != 0 {
			t.Fatalf("trackpad: a notch after only %d px", (i+1)*12)
		}
	}
	if total != -3 {
		t.Errorf("trackpad 300 px: %d notches, want -3", total)
	}
	// Turning back starts over: the 0 px leftover above, then 60 px down and
	// 60 px up: no notch either way (without the reset the up move would
	// first have to cancel the 60 px down).
	w.reset()
	if _, y := w.add(0, 60, 0); y != 0 {
		t.Errorf("60 px down: %d", y)
	}
	if _, y := w.add(0, -60, 0); y != 0 {
		t.Errorf("60 px up after 60 down: %d", y)
	}
	if _, y := w.add(0, -40, 0); y != 1 {
		t.Errorf("then 40 px more up: %d, want 1 (100 px up since the turn)", y)
	}
	// Horizontal: right is positive in both conventions.
	w.reset()
	if x, y := w.add(100, 0, 0); x != 1 || y != 0 {
		t.Errorf("one notch right: (%d,%d), want (1,0)", x, y)
	}
	if x, _ := w.add(-200, 0, 0); x != -2 {
		t.Errorf("two notches left: %d, want -2", x)
	}
	// Junk deltas are ignored.
	w.reset()
	nan := 0.0
	nan = nan / nan
	if x, y := w.add(nan, nan, 0); x != 0 || y != 0 || w.x != 0 || w.y != 0 {
		t.Errorf("NaN delta: (%d,%d) acc %v,%v", x, y, w.x, w.y)
	}
}

func TestKeyText(t *testing.T) {
	for _, c := range []struct {
		key             string
		ctrl, alt, meta bool
		want            string
		ok              bool
	}{
		{"a", false, false, false, "a", true},
		{"A", false, false, false, "A", true},
		{"é", false, false, false, "é", true},
		{" ", false, false, false, " ", true},
		{"@", true, true, false, "@", true}, // AltGr on Windows = Ctrl+Alt
		{"v", true, false, false, "", false},
		{"v", false, false, true, "", false},
		{"Enter", false, false, false, "", false},
		{"Backspace", false, false, false, "", false},
		{"Dead", false, false, false, "", false},
		{"", false, false, false, "", false},
	} {
		got, ok := keyText(c.key, c.ctrl, c.alt, c.meta)
		if got != c.want || ok != c.ok {
			t.Errorf("keyText(%q, ctrl=%v alt=%v meta=%v) = %q,%v want %q,%v", c.key, c.ctrl, c.alt, c.meta, got, ok, c.want, c.ok)
		}
	}
	if !isPasteShortcut("KeyV", true, false, false) || !isPasteShortcut("KeyV", false, false, true) ||
		!isPasteShortcut("Insert", false, true, false) {
		t.Error("Ctrl+V, Cmd+V and Shift+Insert must be paste shortcuts")
	}
	if isPasteShortcut("KeyV", false, false, false) || isPasteShortcut("Insert", false, false, false) ||
		isPasteShortcut("KeyC", true, false, false) {
		t.Error("V, Insert and Ctrl+C must not be paste shortcuts")
	}
}

func TestWebKeyboardDefaults(t *testing.T) {
	text := platformDefaultConfig(string(defaultConfig))
	f, err := ini.Load([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	// Every other section and line stays as shipped.
	orig, _ := ini.Load(defaultConfig)
	for _, sec := range orig.Sections() {
		name := sec.Name()
		if strings.HasPrefix(name, "ControllerMapping_Keyboard") {
			continue
		}
		for _, k := range sec.Keys() {
			if got := f.Section(name).Key(k.Name()).String(); got != k.String() {
				t.Errorf("[%s] %s changed: %q -> %q", name, k.Name(), k.String(), got)
			}
		}
	}
	if a, b := strings.Count(text, "\n"), strings.Count(string(defaultConfig), "\n"); a != b {
		t.Errorf("line count %d, shipped %d (keys were added or lost)", a, b)
	}
	want := map[string]map[string]string{
		"ControllerMapping_Keyboard1": {"Name": "Keyboard Controller 1", "DP_U": "w", "DP_D": "s", "DP_L": "a", "DP_R": "d",
			"A": "k", "B": "l", "RT": "SEMICOLON", "X": "i", "Y": "o", "RB": "p", "START": "RETURN", "LT": "q", "LB": "e",
			"BACK": "-", "LS": "-", "RS": "-"},
		"ControllerMapping_Keyboard2": {"Name": "Keyboard Controller 2", "DP_U": "UP", "DP_D": "DOWN", "DP_L": "LEFT", "DP_R": "RIGHT",
			"A": "KP_1", "B": "KP_2", "RT": "KP_3", "X": "KP_4", "Y": "KP_5", "RB": "KP_6", "START": "KP_ENTER", "LT": "KP_7", "LB": "KP_8",
			"BACK": "-", "LS": "-", "RS": "-"},
	}
	used := map[string]string{}
	for sec, keys := range want {
		s := f.Section(sec)
		if len(s.Keys()) != 25 {
			t.Errorf("[%s] has %d keys, want 25 (name + 24 controls)", sec, len(s.Keys()))
		}
		for k, v := range keys {
			if got := s.Key(k).String(); got != v {
				t.Errorf("[%s] %s = %q, want %q", sec, k, got, v)
			}
		}
		// No key drives two controls, on one keyboard controller or across both.
		for _, k := range s.Keys() {
			if k.Name() == "Name" || k.String() == "-" {
				continue
			}
			if prev, dup := used[k.String()]; dup {
				t.Errorf("key %s bound twice: %s and [%s] %s", k.String(), prev, sec, k.Name())
			}
			used[k.String()] = "[" + sec + "] " + k.Name()
		}
	}
	// The Standard setup reads d from LT and w from LB (Decision 13), so the
	// 2.4 keys land on the same game buttons: d = q, w = e on controller 1.
	std := f.Section("ButtonSetup_Standard")
	if std.Key("d").String() != "LT" || std.Key("w").String() != "LB" || std.Key("c").String() != "RT" || std.Key("z").String() != "RB" {
		t.Errorf("ButtonSetup_Standard changed: d=%s w=%s c=%s z=%s", std.Key("d"), std.Key("w"), std.Key("c"), std.Key("z"))
	}
}

func TestOverrideIniKeys(t *testing.T) {
	in := "[A]\r\nx  = 1\r\n; note\r\ny = 2\r\n\r\n; about B\r\n[b]\r\nx = 3\r\n"
	got := overrideIniKeys(in, map[string][][2]string{"a": {{"X", "9"}, {"z", "7"}}, "B": {{"x", "8"}}})
	want := "[A]\r\nx  = 9\r\n; note\r\ny = 2\r\nz = 7\r\n\r\n; about B\r\n[b]\r\nx = 8\r\n"
	if got != want {
		t.Errorf("overrideIniKeys:\n%q\nwant\n%q", got, want)
	}
	// A section at the end of the text gets its missing keys too.
	if got := overrideIniKeys("[A]\nx = 1", map[string][][2]string{"A": {{"y", "2"}}}); got != "[A]\nx = 1\ny = 2" {
		t.Errorf("last section: %q", got)
	}
}

// Focus loss releases every held key and button, in queue order after the
// presses that came before it.
func TestFocusLossReleasesHeldInput(t *testing.T) {
	saved := sys.keyState
	defer func() { sys.keyState = saved }()
	sys.keyState = map[Key]bool{}
	sys.escPending, sys.esc, sys.netMenuEscapeHeld = false, false, true
	sys.mouse.down = [mouseClickButtons]bool{true, false, true}
	w := &Window{focused: true}
	w.wheel.y = 50
	w.apply([]jsEvent{
		{kind: jsEvKeyDown, key: KeyEscape},
		{kind: jsEvKeyDown, key: keyRIGHT},
		{kind: jsEvBlur},
	})
	if w.focused {
		t.Error("still focused after blur")
	}
	for k, down := range sys.keyState {
		if down {
			t.Errorf("key %v still held after blur", k)
		}
	}
	if !sys.escPending {
		t.Error("the Esc press before the blur was lost (the latch must keep it)")
	}
	if sys.netMenuEscapeHeld || sys.mouse.down != [mouseClickButtons]bool{} || w.wheel.y != 0 {
		t.Errorf("held state left: netMenuEscapeHeld=%v mouse.down=%v wheel=%v", sys.netMenuEscapeHeld, sys.mouse.down, w.wheel.y)
	}
	// A press after focus comes back is a new press.
	w.apply([]jsEvent{{kind: jsEvFocus, on: true}, {kind: jsEvKeyDown, key: keyRIGHT}})
	if !w.focused || !sys.keyState[keyRIGHT] || sys.keyInput != keyRIGHT {
		t.Errorf("after focus: focused=%v held=%v keyInput=%v", w.focused, sys.keyState[keyRIGHT], sys.keyInput)
	}
	sys.escPending, sys.keyInput = false, KeyUnknown
}
