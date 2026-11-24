//go:build js

package main

import (
	"encoding/binary"
	"encoding/hex"
	"hash/fnv"
	"math"
	"strconv"
	"strings"
)

// Browser gamepads, the parts that need no JavaScript: reading Gamepad.id,
// the SDL-shaped identity, glyph detection, the standard and non-standard
// layouts, hat decoding, rest baselining, slot planning and rumble timing.
// input_js.go reads navigator.getGamepads() into padSnapshot values and calls
// these, so the js tests can drive them with plain Go values.

// One Gamepad from navigator.getGamepads(), copied out of JavaScript.
type padSnapshot struct {
	index     int // Gamepad.index
	id        string
	mapping   string
	connected bool
	buttons   []padButton
	axes      []float64
}

type padButton struct {
	pressed bool
	value   float64
}

func (p *padSnapshot) standard() bool { return p.mapping == "standard" }

// A device that is not a usable pad: no standard mapping and no axes. USB
// microphones and similar HID devices show up as button-only "gamepads".
func (p *padSnapshot) junk() bool { return !p.standard() && len(p.axes) == 0 }

// Attach order inside one poll: standard pads first, then the others, so a
// real pad gets the lower slot (and the lower device ID) when both appear.
func (p *padSnapshot) rank() int {
	if p.standard() {
		return 0
	}
	return 1
}

// Engine button behind each W3C standard-mapping button index. 6 and 7 are
// the analog triggers (axes 4 and 5), not buttons.
const padNoButton GamepadButton = -1

var standardPadButtons = [17]GamepadButton{
	GamepadButtonA, GamepadButtonB, GamepadButtonX, GamepadButtonY,
	GamepadButtonLeftShoulder, GamepadButtonRightShoulder, padNoButton, padNoButton,
	GamepadButtonBack, GamepadButtonStart, GamepadButtonLeftStick, GamepadButtonRightStick,
	GamepadButtonDPadUp, GamepadButtonDPadDown, GamepadButtonDPadLeft, GamepadButtonDPadRight,
	GamepadButtonGuide,
}

// Every engine button a pad can report, to clear them each frame.
var padAllButtons = [15]GamepadButton{
	GamepadButtonA, GamepadButtonB, GamepadButtonX, GamepadButtonY,
	GamepadButtonBack, GamepadButtonGuide, GamepadButtonStart,
	GamepadButtonLeftStick, GamepadButtonRightStick,
	GamepadButtonLeftShoulder, GamepadButtonRightShoulder,
	GamepadButtonDPadUp, GamepadButtonDPadDown, GamepadButtonDPadLeft, GamepadButtonDPadRight,
}

// What Gamepad.id says about a pad.
type padIdentity struct {
	vendor, product uint16
	hasIDs          bool   // vendor and product were in the id
	name            string // the id without the browser's decorations
}

// Reads Gamepad.id. Chrome writes "<name> (STANDARD GAMEPAD Vendor: 054c
// Product: 0ce6)", "<name> (Vendor: 0079 Product: 0011)" or, for XInput pads,
// "<name> (XInput STANDARD GAMEPAD)"; Firefox writes "054c-0ce6-<name>" (the
// numbers can lose leading zeros) and "xinput" for XInput pads; Safari's
// GameController path writes "<name> Extended Gamepad". A pad without numbers
// is identified by its cleaned name.
func parsePadID(id string) padIdentity {
	var p padIdentity
	name := strings.TrimSpace(id)
	// Chrome: the numbers sit in the last parenthesis.
	chrome := false
	if open := strings.LastIndex(name, " ("); open >= 0 && strings.HasSuffix(name, ")") {
		inner := name[open+2 : len(name)-1]
		v, okV := hexAfter(inner, "Vendor: ")
		d, okP := hexAfter(inner, "Product: ")
		if okV && okP {
			p.vendor, p.product, p.hasIDs = v, d, true
		}
		if okV || okP || strings.Contains(inner, "STANDARD GAMEPAD") || strings.Contains(inner, "XInput") {
			name, chrome = strings.TrimSpace(name[:open]), true
		}
	}
	if parts := strings.SplitN(name, "-", 3); !chrome && len(parts) == 3 && isShortHex(parts[0]) && isShortHex(parts[1]) {
		// Firefox (and WebKit's HID path): vendor-product-name.
		v, _ := strconv.ParseUint(parts[0], 16, 16)
		d, _ := strconv.ParseUint(parts[1], 16, 16)
		p.vendor, p.product, p.hasIDs = uint16(v), uint16(d), true
		name = strings.TrimSpace(parts[2])
	}
	name = strings.TrimSpace(strings.TrimSuffix(name, " Extended Gamepad"))
	if strings.EqualFold(name, "xinput") {
		name = "XInput Controller"
	}
	if name == "" {
		name = "Gamepad"
	}
	// 0000:0000 is what some Bluetooth stacks report; it identifies nothing.
	if p.hasIDs && p.vendor == 0 && p.product == 0 {
		p.hasIDs = false
	}
	p.name = name
	return p
}

// Four hex digits after a label, as Chrome writes them.
func hexAfter(s, label string) (uint16, bool) {
	i := strings.Index(s, label)
	if i < 0 || len(s) < i+len(label)+4 {
		return 0, false
	}
	v, err := strconv.ParseUint(s[i+len(label):i+len(label)+4], 16, 16)
	return uint16(v), err == nil
}

func isShortHex(s string) bool {
	if len(s) < 1 || len(s) > 4 {
		return false
	}
	_, err := strconv.ParseUint(s, 16, 16)
	return err == nil
}

// Byte 14 of a browser GUID ('b'). SDL keeps that byte for the driver that
// opened the pad ('h' hidapi, 'x' XInput, ...); desktop builds read this one
// as an unknown driver.
const padBrowserMarker = 0x62

// An SDL-shaped GUID: bus 0x0003 (USB), the vendor in bytes 4-5 and the
// product in bytes 8-9 (little endian), as SDL builds it for a USB pad. A pad
// without numbers gets bus 0 and a hash of its cleaned name in bytes 4-11.
func padGUID(p padIdentity) string {
	var g [16]byte
	if p.hasIDs {
		g[0] = 0x03
		binary.LittleEndian.PutUint16(g[4:6], p.vendor)
		binary.LittleEndian.PutUint16(g[8:10], p.product)
	} else {
		h := fnv.New64a()
		h.Write([]byte(strings.ToLower(p.name)))
		binary.BigEndian.PutUint64(g[4:12], h.Sum64())
	}
	g[14] = padBrowserMarker
	return hex.EncodeToString(g[:])
}

// Glyph pack for a pad the motif does not name, in place of SDL's controller
// type: vendor and product first, then a few well-known names. Empty when
// nothing matches (the caller falls back to "generic").
func padGlyphPack(p padIdentity) string {
	if p.hasIDs {
		switch p.vendor {
		case 0x045e: // Microsoft
			switch p.product {
			case 0x028e, 0x028f, 0x0291, 0x0719:
				return "xbox360"
			case 0x0b12, 0x0b13:
				return "xboxseries"
			}
			return "xbox"
		case 0x054c: // Sony
			switch p.product {
			case 0x0268:
				return "ps3"
			case 0x05c4, 0x09cc, 0x0ba0:
				return "ps4"
			case 0x0ce6, 0x0df2:
				return "ps5"
			}
		case 0x057e: // Nintendo
			switch p.product {
			case 0x2009, 0x2006, 0x2007, 0x200e: // Pro Controller, Joy-Con L, R, grip
				return "switch"
			case 0x2069: // Switch 2 Pro Controller
				return "switch2"
			}
		}
	}
	name := strings.ToLower(p.name)
	for _, rule := range padGlyphNames {
		if strings.Contains(name, rule[0]) {
			return rule[1]
		}
	}
	return ""
}

// Name rules, most specific first. Chrome names every XInput pad "Xbox 360
// Controller" and Firefox names it "xinput": SDL reports XInput-only pads as
// Xbox 360 too.
var padGlyphNames = [][2]string{
	{"xbox 360", "xbox360"}, {"xinput", "xbox360"},
	{"xbox series", "xboxseries"}, {"xbox", "xbox"},
	{"dualsense", "ps5"}, {"dualshock 4", "ps4"}, {"dualshock4", "ps4"},
	{"playstation(r)3", "ps3"}, {"dualshock 3", "ps3"},
	{"switch 2", "switch2"}, {"pro controller", "switch"}, {"joy-con", "switch"},
}

// Decodes a POV hat that a browser reports as one axis. The hat's eight
// positions 0 (up) to 7 (up-left), clockwise, are spread over -1..1 in steps
// of 2/7; centered reads outside that range (Chrome: 9/7).
func padHatDirections(v float64) (up, down, left, right bool) {
	if math.IsNaN(v) || v < -1.05 || v > 1.05 {
		return
	}
	pos := (v + 1) * 3.5
	n := math.Round(pos)
	if math.Abs(pos-n) > 0.25 {
		return
	}
	switch int(n) {
	case 0:
		up = true
	case 1:
		up, right = true, true
	case 2:
		right = true
	case 3:
		down, right = true, true
	case 4:
		down = true
	case 5:
		down, left = true, true
	case 6:
		left = true
	case 7:
		up, left = true, true
	}
	return
}

// An axis that ever reads outside -1..1 is a hat (its centered value is out
// of range).
func padAxisIsHat(v float64) bool { return v < -1.05 || v > 1.05 }

// Rest value of a non-standard axis, from its first reading: the nearest of
// -1, 0 and 1. Sticks rest at 0; triggers (and some pedals) rest at -1 or 1.
// Snapping keeps a little stick drift at connect time from becoming a bias.
func padAxisRest(v float64) float64 {
	switch {
	case v <= -0.5:
		return -1
	case v >= 0.5:
		return 1
	}
	return 0
}

// Level of a baselined axis. A centered axis keeps its sign (-127..127); an
// axis resting at -1 or 1 becomes a trigger that rests at 0 and reads 127 at
// the other end, as SDL reports triggers. A trigger that rests at -1 would read
// as held forever (the 2026-07 "pad not recognized" bug).
func padAxisLevel(v, rest float64) int8 {
	if math.IsNaN(v) {
		return 0
	}
	if rest != 0 {
		v = (v - rest) * -rest / 2
		return int8(Clamp(v, 0, 1) * 127)
	}
	return int8(Clamp(v, -1, 1) * 127)
}

// Role of each raw axis of a non-standard pad: 0-3 sticks (LX LY RX RY), 4-5
// triggers (LT RT), padAxisHat, or padAxisUnused. Centered axes fill the sticks
// in order, axes resting at -1 or 1 fill the triggers in order: a positional
// guess that players correct with the Controller Mapping screen.
const (
	padAxisUnused int8 = -1
	padAxisHat    int8 = -2
)

func padAxisRoles(rest []float64, hat []bool, roles []int8) []int8 {
	roles = roles[:0]
	stick, trigger := int8(0), int8(4)
	for i := range rest {
		role := padAxisUnused
		switch {
		case i < len(hat) && hat[i]:
			role = padAxisHat
		case rest[i] == 0 && stick < 4:
			role, stick = stick, stick+1
		case rest[i] != 0 && trigger < 6:
			role, trigger = trigger, trigger+1
		}
		roles = append(roles, role)
	}
	return roles
}

// What the browser layer keeps per slot to read a pad.
type padLayout struct {
	rest  []float64
	hat   []bool
	roles []int8
}

// First reading of a non-standard pad: the rest values and the hats.
func (l *padLayout) capture(axes []float64) {
	l.rest, l.hat = l.rest[:0], l.hat[:0]
	for _, v := range axes {
		l.rest = append(l.rest, padAxisRest(v))
		l.hat = append(l.hat, padAxisIsHat(v))
	}
	l.roles = padAxisRoles(l.rest, l.hat, l.roles)
}

// Fills a ControllerState from one snapshot. Standard pads use the W3C
// layout directly. Other pads are read by position into the same layout: the
// buttons by the standard indexes, the axes by padAxisRoles, a hat into the
// d-pad.
func (l *padLayout) read(p *padSnapshot, st *ControllerState) {
	for _, b := range padAllButtons {
		st.Buttons[b] = 0
	}
	st.Axes = [6]int8{}
	for i, b := range p.buttons {
		if i >= len(standardPadButtons) {
			break
		}
		if eb := standardPadButtons[i]; eb != padNoButton {
			if b.pressed {
				st.Buttons[eb] = 1
			}
		} else {
			// Standard buttons 6 and 7 are LT and RT (0..1, rest 0).
			v := b.value
			if b.pressed && !p.standard() {
				v = 1
			}
			st.Axes[4+i-6] = int8(Clamp(v, 0, 1) * 127)
		}
	}
	if p.standard() {
		for a := 0; a < 4 && a < len(p.axes); a++ {
			v := p.axes[a]
			if math.IsNaN(v) {
				v = 0
			}
			st.Axes[a] = int8(Clamp(v, -1, 1) * 127)
		}
		return
	}
	if len(l.rest) != len(p.axes) {
		l.capture(p.axes)
	}
	changed := false
	for i, v := range p.axes {
		if !l.hat[i] && padAxisIsHat(v) {
			l.hat[i], changed = true, true
		}
	}
	if changed {
		l.roles = padAxisRoles(l.rest, l.hat, l.roles)
	}
	for i, role := range l.roles {
		v := p.axes[i]
		switch {
		case role == padAxisHat:
			up, down, left, right := padHatDirections(v)
			for _, d := range [4]struct {
				on bool
				b  GamepadButton
			}{{up, GamepadButtonDPadUp}, {down, GamepadButtonDPadDown}, {left, GamepadButtonDPadLeft}, {right, GamepadButtonDPadRight}} {
				if d.on {
					st.Buttons[d.b] = 1
				}
			}
		case role >= 4:
			st.Axes[role] = max(st.Axes[role], padAxisLevel(v, l.rest[i]))
		case role >= 0:
			st.Axes[role] = padAxisLevel(v, l.rest[i])
		}
	}
}

// A pad slot: the Gamepad.index it reads and the id it had when it attached.
// index -1 is a free slot.
type padSlot struct {
	index int
	id    string
}

// One step of hot-plug: the slots to detach and the pads to attach (slot,
// position in pads). A slot whose pad is gone, disconnected or now reports a
// different id is detached; a usable pad that no slot reads takes the first
// free slot, standard pads first, then by Gamepad.index. A different id at
// the same index is a detach and an attach in the same step.
func padPlan(slots []padSlot, pads []padSnapshot, detach []int, attach [][2]int) ([]int, [][2]int) {
	detach, attach = detach[:0], attach[:0]
	held := func(index int) bool {
		for s := range slots {
			if slots[s].index == index && !containsInt(detach, s) {
				return true
			}
		}
		return false
	}
	for s := range slots {
		if slots[s].index < 0 {
			continue
		}
		found := false
		for i := range pads {
			if pads[i].connected && pads[i].index == slots[s].index && pads[i].id == slots[s].id && !pads[i].junk() {
				found = true
				break
			}
		}
		if !found {
			detach = append(detach, s)
		}
	}
	free := func() int {
		for s := range slots {
			if (slots[s].index < 0 || containsInt(detach, s)) && !slotTaken(attach, s) {
				return s
			}
		}
		return -1
	}
	for rank := 0; rank <= 1; rank++ {
		for {
			// Lowest Gamepad.index of this rank not yet placed.
			best := -1
			for i := range pads {
				p := &pads[i]
				if !p.connected || p.junk() || p.rank() != rank || held(p.index) || padPlaced(attach, i) {
					continue
				}
				if best < 0 || p.index < pads[best].index {
					best = i
				}
			}
			if best < 0 {
				break
			}
			s := free()
			if s < 0 {
				return detach, attach
			}
			attach = append(attach, [2]int{s, best})
		}
	}
	return detach, attach
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func slotTaken(attach [][2]int, slot int) bool {
	for _, a := range attach {
		if a[0] == slot {
			return true
		}
	}
	return false
}

func padPlaced(attach [][2]int, pad int) bool {
	for _, a := range attach {
		if a[1] == pad {
			return true
		}
	}
	return false
}

// A vibrationActuator effect for SDL's Rumble(lo, hi, ms) as upstream's
// rumbleJoystick computes it: ticks frames of frame_ms each plus half a frame,
// so back-to-back requests overlap instead of gapping. lo drives the strong
// (low-frequency) motor and hi the weak one. stop is a ticks == 0 request.
// ok is false when the game speed gives no frame length (no effect at all).
func padRumbleEffect(lo, hi uint16, ticks uint32, gameLogicSpeed int32, turbo float32) (durationMs uint32, strong, weak float64, stop, ok bool) {
	if gameLogicSpeed <= 0 || turbo <= 0 {
		return 0, 0, 0, false, false
	}
	if ticks == 0 {
		return 0, 0, 0, true, true
	}
	frameMs := uint32(math.Ceil(1.0 / float64(gameLogicSpeed) * float64(turbo) * 1000.0))
	return ticks*frameMs + frameMs>>1, float64(lo) / 65535, float64(hi) / 65535, false, true
}
