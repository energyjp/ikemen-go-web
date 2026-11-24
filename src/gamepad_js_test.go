//go:build js

package main

import (
	"math"
	"testing"
)

func padState() *ControllerState {
	return &ControllerState{Buttons: make(map[GamepadButton]byte)}
}

func pressedButtons(st *ControllerState) []GamepadButton {
	var on []GamepadButton
	for _, b := range padAllButtons {
		if st.Buttons[b] != 0 {
			on = append(on, b)
		}
	}
	return on
}

// Each W3C standard button lands on its engine button; 6 and 7 are the
// trigger axes; sticks are clamped to -127..127.
func TestPadStandardButtonTable(t *testing.T) {
	want := map[int]GamepadButton{
		0: GamepadButtonA, 1: GamepadButtonB, 2: GamepadButtonX, 3: GamepadButtonY,
		4: GamepadButtonLeftShoulder, 5: GamepadButtonRightShoulder,
		8: GamepadButtonBack, 9: GamepadButtonStart, 10: GamepadButtonLeftStick, 11: GamepadButtonRightStick,
		12: GamepadButtonDPadUp, 13: GamepadButtonDPadDown, 14: GamepadButtonDPadLeft, 15: GamepadButtonDPadRight,
		16: GamepadButtonGuide,
	}
	var l padLayout
	for i := 0; i < 17; i++ {
		p := padSnapshot{mapping: "standard", connected: true, buttons: make([]padButton, 17), axes: make([]float64, 4)}
		p.buttons[i] = padButton{pressed: true, value: 1}
		st := padState()
		l.read(&p, st)
		on := pressedButtons(st)
		if i == 6 || i == 7 {
			if len(on) != 0 || st.Axes[4+i-6] != 127 {
				t.Errorf("button %d: buttons %v axes %v, want trigger axis %d = 127", i, on, st.Axes, 4+i-6)
			}
			continue
		}
		if len(on) != 1 || on[0] != want[i] {
			t.Errorf("button %d: got %v, want [%d]", i, on, want[i])
		}
		if st.Axes != [6]int8{} {
			t.Errorf("button %d moved axes %v", i, st.Axes)
		}
	}
	// A half-pulled trigger keeps its analog value; sticks clamp; NaN reads 0.
	p := padSnapshot{mapping: "standard", connected: true, buttons: make([]padButton, 17),
		axes: []float64{1.5, -1.2, 0.5, math.NaN(), 0.9}}
	p.buttons[6] = padButton{value: 0.5}
	st := padState()
	l.read(&p, st)
	if want := [6]int8{127, -127, 63, 0, 63, 0}; st.Axes != want {
		t.Errorf("axes %v, want %v", st.Axes, want)
	}
	// The previous frame's press is cleared.
	p.buttons[0].pressed = true
	l.read(&p, st)
	p.buttons[0].pressed = false
	l.read(&p, st)
	if on := pressedButtons(st); len(on) != 0 {
		t.Errorf("released button still reads %v", on)
	}
}

func TestParsePadID(t *testing.T) {
	cases := []struct {
		id      string
		vendor  uint16
		product uint16
		hasIDs  bool
		name    string
	}{
		// Chrome
		{"DualSense Wireless Controller (STANDARD GAMEPAD Vendor: 054c Product: 0ce6)", 0x054c, 0x0ce6, true, "DualSense Wireless Controller"},
		{"Wireless Controller (STANDARD GAMEPAD Vendor: 054c Product: 09cc)", 0x054c, 0x09cc, true, "Wireless Controller"},
		{"USB Gamepad (Vendor: 0079 Product: 0011)", 0x0079, 0x0011, true, "USB Gamepad"},
		{"Xbox 360 Controller (XInput STANDARD GAMEPAD)", 0, 0, false, "Xbox 360 Controller"},
		// Firefox (zero padded on Windows, not always elsewhere)
		{"054c-0ce6-DualSense Wireless Controller", 0x054c, 0x0ce6, true, "DualSense Wireless Controller"},
		{"45e-28e-Xbox 360 Wired Controller", 0x045e, 0x028e, true, "Xbox 360 Wired Controller"},
		{"057e-2009-Pro Controller", 0x057e, 0x2009, true, "Pro Controller"},
		{"xinput", 0, 0, false, "XInput Controller"},
		// Safari (GameController framework): no numbers
		{"DualSense Wireless Controller Extended Gamepad", 0, 0, false, "DualSense Wireless Controller"},
		{"Xbox Wireless Controller Extended Gamepad", 0, 0, false, "Xbox Wireless Controller"},
		// Not decorations: kept as they are
		{"Arcade Stick (v2)", 0, 0, false, "Arcade Stick (v2)"},
		{"054c-0ce6-Wireless Controller (PS4)", 0x054c, 0x0ce6, true, "Wireless Controller (PS4)"},
		{"Logitech-Dual-Action", 0, 0, false, "Logitech-Dual-Action"},
		{"Generic (Vendor: 0000 Product: 0000)", 0, 0, false, "Generic"},
		{"", 0, 0, false, "Gamepad"},
	}
	for _, c := range cases {
		p := parsePadID(c.id)
		if p.vendor != c.vendor || p.product != c.product || p.hasIDs != c.hasIDs || p.name != c.name {
			t.Errorf("%q: got %04x:%04x ids=%t %q, want %04x:%04x ids=%t %q",
				c.id, p.vendor, p.product, p.hasIDs, p.name, c.vendor, c.product, c.hasIDs, c.name)
		}
	}
}

func TestPadGUID(t *testing.T) {
	// SDL's own GUID for a USB DualSense opened by hidapi is
	// 030000004c050000e60c000000006800; ours differs only in byte 14.
	if g := padGUID(parsePadID("DualSense Wireless Controller (STANDARD GAMEPAD Vendor: 054c Product: 0ce6)")); g != "030000004c050000e60c000000006200" {
		t.Errorf("DualSense GUID %s", g)
	}
	// Chrome and Firefox give the same pad the same GUID.
	if a, b := padGUID(parsePadID("Xbox 360 Controller (STANDARD GAMEPAD Vendor: 045e Product: 028e)")),
		padGUID(parsePadID("45e-28e-Xbox 360 Wired Controller")); a != b {
		t.Errorf("same pad, two GUIDs: %s %s", a, b)
	}
	// Without numbers: bus 0, a stable hash of the cleaned name.
	x1 := padGUID(parsePadID("Xbox 360 Controller (XInput STANDARD GAMEPAD)"))
	x2 := padGUID(parsePadID("xbox 360 controller (XInput STANDARD GAMEPAD)"))
	ff := padGUID(parsePadID("xinput"))
	if len(x1) != 32 || x1[:8] != "00000000" || x1[28:30] != "62" || x1 != x2 {
		t.Errorf("name GUID %s / %s", x1, x2)
	}
	if ff == x1 {
		t.Errorf("different names share a GUID %s", ff)
	}
	if g := padGUID(parsePadID("054c-0ce6-DualSense Wireless Controller")); g[28:30] != "62" {
		t.Errorf("browser marker missing in %s", g)
	}
}

func TestPadGlyphPack(t *testing.T) {
	cases := map[string]string{
		"Xbox 360 Controller (STANDARD GAMEPAD Vendor: 045e Product: 028e)":      "xbox360",
		"Xbox Wireless Controller (STANDARD GAMEPAD Vendor: 045e Product: 0b13)": "xboxseries",
		"Xbox Wireless Controller (STANDARD GAMEPAD Vendor: 045e Product: 02ea)": "xbox",
		"Xbox 360 Controller (XInput STANDARD GAMEPAD)":                          "xbox360",
		"xinput": "xbox360",
		"PLAYSTATION(R)3 Controller (STANDARD GAMEPAD Vendor: 054c Product: 0268)":    "ps3",
		"Wireless Controller (STANDARD GAMEPAD Vendor: 054c Product: 09cc)":           "ps4",
		"054c-05c4-Wireless Controller":                                               "ps4",
		"DualSense Wireless Controller (STANDARD GAMEPAD Vendor: 054c Product: 0ce6)": "ps5",
		"054c-0df2-DualSense Edge Wireless Controller":                                "ps5",
		"DualSense Wireless Controller Extended Gamepad":                              "ps5",
		"Pro Controller (STANDARD GAMEPAD Vendor: 057e Product: 2009)":                "switch",
		"057e-2069-Controller":                                              "switch2",
		"Xbox Wireless Controller Extended Gamepad":                         "xbox",
		"USB Gamepad (Vendor: 0079 Product: 0011)":                          "",
		"8BitDo Pro 2 (Vendor: 2dc8 Product: 6006)":                         "",
		"Wireless Controller (STANDARD GAMEPAD Vendor: 054c Product: 1234)": "",
	}
	for id, want := range cases {
		if got := padGlyphPack(parsePadID(id)); got != want {
			t.Errorf("%q: %q, want %q", id, got, want)
		}
	}
}

func TestPadHatDirections(t *testing.T) {
	type dirs struct{ up, down, left, right bool }
	want := []dirs{
		{up: true}, {up: true, right: true}, {right: true}, {down: true, right: true},
		{down: true}, {down: true, left: true}, {left: true}, {up: true, left: true},
	}
	for pos, w := range want {
		v := float64(pos)*2/7 - 1
		for _, jitter := range []float64{0, 0.01, -0.01} {
			up, down, left, right := padHatDirections(v + jitter)
			if (dirs{up, down, left, right}) != w {
				t.Errorf("position %d (%.4f): got %v, want %v", pos, v+jitter, dirs{up, down, left, right}, w)
			}
		}
	}
	// Centered (Chrome reports 9/7), halfway between positions, NaN: nothing.
	for _, v := range []float64{9.0 / 7, 3.28571, -1.5, -1 + 1.0/7, math.NaN()} {
		if up, down, left, right := padHatDirections(v); up || down || left || right {
			t.Errorf("%.4f reads as a direction", v)
		}
	}
}

func TestPadRestBaselining(t *testing.T) {
	for _, c := range []struct{ v, rest float64 }{{0.03, 0}, {-0.2, 0}, {-1, -1}, {-0.98, -1}, {1, 1}, {0.7, 1}} {
		if got := padAxisRest(c.v); got != c.rest {
			t.Errorf("rest of %v = %v, want %v", c.v, got, c.rest)
		}
	}
	for _, c := range []struct {
		v, rest float64
		want    int8
	}{
		{-1, -1, 0}, {0, -1, 63}, {1, -1, 127}, {-1.2, -1, 0},
		{1, 1, 0}, {-1, 1, 127},
		{0.5, 0, 63}, {-1, 0, -127}, {2, 0, 127},
	} {
		if got := padAxisLevel(c.v, c.rest); got != c.want {
			t.Errorf("level %v rest %v = %d, want %d", c.v, c.rest, got, c.want)
		}
	}

	// A DirectInput-style pad: X Y, a trigger on Z resting at -1, Rx Ry,
	// another trigger resting at -1, and the hat as axis 6 (centered = 9/7).
	rest := []float64{0.02, -0.01, -1, 0.0, 0.0, -1, 9.0 / 7}
	p := padSnapshot{mapping: "", connected: true, buttons: make([]padButton, 12), axes: append([]float64(nil), rest...)}
	var l padLayout
	l.capture(p.axes)
	if want := []int8{0, 1, 4, 2, 3, 5, padAxisHat}; string(int8sToBytes(l.roles)) != string(int8sToBytes(want)) {
		t.Fatalf("roles %v, want %v", l.roles, want)
	}
	st := padState()
	l.read(&p, st)
	if st.Axes != [6]int8{2, -1, 0, 0, 0, 0} || len(pressedButtons(st)) != 0 {
		t.Errorf("at rest: axes %v buttons %v", st.Axes, pressedButtons(st))
	}
	p.axes[2] = 1       // LT fully pulled
	p.axes[6] = 1.0 / 7 // hat down
	p.axes[3] = -1      // right stick left
	p.buttons[9].pressed = true
	l.read(&p, st)
	if st.Axes != [6]int8{2, -1, -127, 0, 127, 0} {
		t.Errorf("pressed: axes %v", st.Axes)
	}
	if on := pressedButtons(st); len(on) != 2 || on[0] != GamepadButtonStart || on[1] != GamepadButtonDPadDown {
		t.Errorf("pressed: buttons %v", on)
	}

	// A hat that was pressed when the pad connected reads in range at first;
	// the first centered reading (out of range) turns it into a hat.
	q := padSnapshot{connected: true, axes: []float64{0, 0, -1}}
	var m padLayout
	m.capture(q.axes)
	q.axes[2] = 9.0 / 7
	st2 := padState()
	m.read(&q, st2)
	if m.roles[2] != padAxisHat || st2.Axes != [6]int8{} || len(pressedButtons(st2)) != 0 {
		t.Errorf("late hat: roles %v axes %v buttons %v", m.roles, st2.Axes, pressedButtons(st2))
	}
}

func int8sToBytes(v []int8) []byte {
	b := make([]byte, len(v))
	for i, x := range v {
		b[i] = byte(x)
	}
	return b
}

func TestPadPlanHotPlug(t *testing.T) {
	slots := make([]padSlot, MaxPlayerNo)
	for i := range slots {
		slots[i].index = -1
	}
	mic := padSnapshot{index: 0, id: "QuadCast 2 S (Vendor: 03f0 Product: 0f8b)", connected: true, buttons: make([]padButton, 7)}
	dinput := padSnapshot{index: 1, id: "USB Gamepad (Vendor: 0079 Product: 0011)", connected: true, axes: make([]float64, 5)}
	std := padSnapshot{index: 2, id: "DualSense Wireless Controller (STANDARD GAMEPAD Vendor: 054c Product: 0ce6)", mapping: "standard", connected: true, axes: make([]float64, 4)}
	apply := func(pads []padSnapshot) (detach []int, attach [][2]int) {
		detach, attach = padPlan(slots, pads, nil, nil)
		for _, s := range detach {
			slots[s] = padSlot{index: -1}
		}
		for _, a := range attach {
			slots[a[0]] = padSlot{index: pads[a[1]].index, id: pads[a[1]].id}
		}
		return
	}

	// Standard first, then the other pad; the microphone never gets a slot.
	d, a := apply([]padSnapshot{mic, dinput, std})
	if len(d) != 0 || len(a) != 2 || a[0] != [2]int{0, 2} || a[1] != [2]int{1, 1} {
		t.Fatalf("first poll: detach %v attach %v", d, a)
	}
	// Nothing changes: nothing to do.
	if d, a := apply([]padSnapshot{mic, dinput, std}); len(d) != 0 || len(a) != 0 {
		t.Fatalf("steady: detach %v attach %v", d, a)
	}
	// The standard pad is unplugged (gone from the list): its slot detaches.
	if d, a := apply([]padSnapshot{mic, dinput}); len(d) != 1 || d[0] != 0 || len(a) != 0 {
		t.Fatalf("unplug: detach %v attach %v", d, a)
	}
	// Chrome keeps a disconnected entry for a moment: also gone.
	if d, a := apply([]padSnapshot{mic, {index: 1, id: dinput.id, connected: false, axes: make([]float64, 5)}}); len(d) != 1 || d[0] != 1 || len(a) != 0 {
		t.Fatalf("disconnected entry: detach %v attach %v", d, a)
	}
	// Plugged back in: the first free slot again.
	if d, a := apply([]padSnapshot{mic, dinput, std}); len(d) != 0 || len(a) != 2 || a[0] != [2]int{0, 2} || a[1] != [2]int{1, 1} {
		t.Fatalf("replug: detach %v attach %v", d, a)
	}
	// Another pad takes the same index: detach and attach in one step, same slot.
	other := std
	other.id = "Xbox 360 Controller (XInput STANDARD GAMEPAD)"
	if d, a := apply([]padSnapshot{mic, dinput, other}); len(d) != 1 || d[0] != 0 || len(a) != 1 || a[0] != [2]int{0, 2} {
		t.Fatalf("swap at one index: detach %v attach %v", d, a)
	}
	// Firefox indexes can exceed the slot count.
	far := std
	far.index = 40
	if d, a := apply([]padSnapshot{mic, dinput, other, far}); len(d) != 0 || len(a) != 1 || a[0] != [2]int{2, 3} {
		t.Fatalf("index 40: detach %v attach %v", d, a)
	}
	// No free slot: the pad waits.
	full := make([]padSlot, 2)
	full[0], full[1] = padSlot{index: 1, id: dinput.id}, padSlot{index: 2, id: other.id}
	if d, a := padPlan(full, []padSnapshot{dinput, other, far}, nil, nil); len(d) != 0 || len(a) != 0 {
		t.Fatalf("full: detach %v attach %v", d, a)
	}
}

func TestPadRumbleEffect(t *testing.T) {
	// 60 fps: frames of ceil(16.67) = 17 ms, plus half a frame.
	ms, strong, weak, stop, ok := padRumbleEffect(65535, 32767, 10, 60, 1)
	if !ok || stop || ms != 10*17+8 || strong != 1 || math.Abs(weak-0.5) > 0.001 {
		t.Errorf("rumble: %d ms strong %v weak %v stop %t ok %t", ms, strong, weak, stop, ok)
	}
	if _, _, _, stop, ok := padRumbleEffect(65535, 65535, 0, 60, 1); !ok || !stop {
		t.Errorf("ticks 0 must stop")
	}
	if _, _, _, _, ok := padRumbleEffect(65535, 65535, 5, 0, 1); ok {
		t.Errorf("no game speed must play nothing")
	}
}
