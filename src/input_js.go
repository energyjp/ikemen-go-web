//go:build js

package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"syscall/js"
)

// Browser input backend: replaces input_sdl.go for wasm builds.
//
// Keyboard: Key is this backend's own keycode enum; system_js.go translates
// KeyboardEvent.code via jsCodeToKey and feeds OnKeyPressed/OnKeyReleased.
// The config-file key names (KeyToStringLUT) match the SDL backend so
// save/config.json is portable between desktop and web.
//
// Gamepads: polled once per frame from navigator.getGamepads() (see
// pollGamepads, called by the system backend's event pump), as upstream's
// input_sdl.go contract: a slot per pad, sys.attachGamepad/detachGamepad on
// hot-plug, ControllerState in SDL's GameController layout (axes LX LY RX RY
// LT RT). The parts that need no JavaScript are in gamepad_js.go.

type Key = int32
type ModifierKey = uint16

const (
	KMOD_NONE  ModifierKey = 0
	KMOD_CTRL  ModifierKey = 1 << 0
	KMOD_ALT   ModifierKey = 1 << 1
	KMOD_SHIFT ModifierKey = 1 << 2
	KMOD_GUI   ModifierKey = 1 << 3
)

// Key codes: arbitrary unique values, meaningful only through the LUTs.
const (
	KeyUnknown Key = iota
	KeyEscape
	KeyEnter
	KeyInsert
	KeyF5
	KeyF12
	KeyPause
	KeyScrollLock
	keyBACKSPACE
	keyTAB
	keySPACE
	keyQUOTE
	keyCOMMA
	keyMINUS
	keyPERIOD
	keySLASH
	key0
	key1
	key2
	key3
	key4
	key5
	key6
	key7
	key8
	key9
	keySEMICOLON
	keyEQUALS
	keyLBRACKET
	keyBACKSLASH
	keyRBRACKET
	keyBACKQUOTE
	keyA
	keyB
	keyC
	keyD
	keyE
	keyF
	keyG
	keyH
	keyI
	keyJ
	keyK
	keyL
	keyM
	keyN
	keyO
	keyP
	keyQ
	keyR
	keyS
	keyT
	keyU
	keyV
	keyW
	keyX
	keyY
	keyZ
	keyCAPSLOCK
	keyF1
	keyF2
	keyF3
	keyF4
	keyF6
	keyF7
	keyF8
	keyF9
	keyF10
	keyF11
	keyF13
	keyF14
	keyF15
	keyF16
	keyF17
	keyF18
	keyF19
	keyF20
	keyF21
	keyF22
	keyF23
	keyF24
	keyPRINTSCREEN
	keyHOME
	keyPAGEUP
	keyDELETE
	keyEND
	keyPAGEDOWN
	keyRIGHT
	keyLEFT
	keyDOWN
	keyUP
	keyNUMLOCKCLEAR
	keyKP_DIVIDE
	keyKP_MULTIPLY
	keyKP_MINUS
	keyKP_PLUS
	keyKP_ENTER
	keyKP_1
	keyKP_2
	keyKP_3
	keyKP_4
	keyKP_5
	keyKP_6
	keyKP_7
	keyKP_8
	keyKP_9
	keyKP_0
	keyKP_PERIOD
	keyKP_EQUALS
	keyMENU
	keyLCTRL
	keyLSHIFT
	keyLALT
	keyLGUI
	keyRCTRL
	keyRSHIFT
	keyRALT
	keyRGUI
)

var KeyToStringLUT map[Key]string
var StringToKeyLUT map[string]Key
var jsCodeToKey map[string]Key

// GamepadButton is the platform's gamepad button id (shared input.go maps the
// digital virtual controls onto these constants). Same numbering as SDL's
// GameController buttons, which is how pollGamepads stores them.
type GamepadButton int8

const (
	GamepadButtonA GamepadButton = iota
	GamepadButtonB
	GamepadButtonX
	GamepadButtonY
	GamepadButtonBack
	GamepadButtonGuide
	GamepadButtonStart
	GamepadButtonLeftStick
	GamepadButtonRightStick
	GamepadButtonLeftShoulder
	GamepadButtonRightShoulder
	GamepadButtonDPadUp
	GamepadButtonDPadDown
	GamepadButtonDPadLeft
	GamepadButtonDPadRight
)

// ControllerState mirrors the SDL backend's shape - shared code
// (input.go gamepadControlLevel) reaches into these fields directly.
type ControllerState struct {
	Axes      [6]int8
	Buttons   map[GamepadButton]byte
	HasRumble bool
}

// Browser-side state of a pad slot (not visible to shared code).
type jsPad struct {
	slot   padSlot // Gamepad.index and id; index -1 = free
	ident  padIdentity
	guid   string
	layout padLayout
}

type Input struct {
	controllerstate [MaxPlayerNo]*ControllerState
	pads            [MaxPlayerNo]jsPad
}

var input Input

func initLUTs() {
	KeyToStringLUT = map[Key]string{
		KeyEnter: "RETURN", KeyEscape: "ESCAPE", keyBACKSPACE: "BACKSPACE",
		keyTAB: "TAB", keySPACE: "SPACE", keyQUOTE: "QUOTE", keyCOMMA: "COMMA",
		keyMINUS: "MINUS", keyPERIOD: "PERIOD", keySLASH: "SLASH",
		key0: "0", key1: "1", key2: "2", key3: "3", key4: "4",
		key5: "5", key6: "6", key7: "7", key8: "8", key9: "9",
		keySEMICOLON: "SEMICOLON", keyEQUALS: "EQUALS", keyLBRACKET: "LBRACKET",
		keyBACKSLASH: "BACKSLASH", keyRBRACKET: "RBRACKET", keyBACKQUOTE: "BACKQUOTE",
		keyA: "a", keyB: "b", keyC: "c", keyD: "d", keyE: "e", keyF: "f",
		keyG: "g", keyH: "h", keyI: "i", keyJ: "j", keyK: "k", keyL: "l",
		keyM: "m", keyN: "n", keyO: "o", keyP: "p", keyQ: "q", keyR: "r",
		keyS: "s", keyT: "t", keyU: "u", keyV: "v", keyW: "w", keyX: "x",
		keyY: "y", keyZ: "z",
		keyCAPSLOCK: "CAPSLOCK",
		keyF1:       "F1", keyF2: "F2", keyF3: "F3", keyF4: "F4", KeyF5: "F5",
		keyF6: "F6", keyF7: "F7", keyF8: "F8", keyF9: "F9", keyF10: "F10",
		keyF11: "F11", KeyF12: "F12", keyF13: "F13", keyF14: "F14", keyF15: "F15",
		keyF16: "F16", keyF17: "F17", keyF18: "F18", keyF19: "F19", keyF20: "F20",
		keyF21: "F21", keyF22: "F22", keyF23: "F23", keyF24: "F24",
		keyPRINTSCREEN: "PRINTSCREEN", KeyScrollLock: "SCROLLLOCK", KeyPause: "PAUSE",
		KeyInsert: "INSERT", keyHOME: "HOME", keyPAGEUP: "PAGEUP",
		keyDELETE: "DELETE", keyEND: "END", keyPAGEDOWN: "PAGEDOWN",
		keyRIGHT: "RIGHT", keyLEFT: "LEFT", keyDOWN: "DOWN", keyUP: "UP",
		keyNUMLOCKCLEAR: "NUMLOCKCLEAR",
		keyKP_DIVIDE:    "KP_DIVIDE", keyKP_MULTIPLY: "KP_MULTIPLY",
		keyKP_MINUS: "KP_MINUS", keyKP_PLUS: "KP_PLUS", keyKP_ENTER: "KP_ENTER",
		keyKP_1: "KP_1", keyKP_2: "KP_2", keyKP_3: "KP_3", keyKP_4: "KP_4",
		keyKP_5: "KP_5", keyKP_6: "KP_6", keyKP_7: "KP_7", keyKP_8: "KP_8",
		keyKP_9: "KP_9", keyKP_0: "KP_0", keyKP_PERIOD: "KP_PERIOD",
		keyKP_EQUALS: "KP_EQUALS", keyMENU: "MENU",
		keyLCTRL: "LCTRL", keyLSHIFT: "LSHIFT", keyLALT: "LALT", keyLGUI: "LGUI",
		keyRCTRL: "RCTRL", keyRSHIFT: "RSHIFT", keyRALT: "RALT", keyRGUI: "RGUI",
	}

	// KeyboardEvent.code -> Key
	jsCodeToKey = map[string]Key{
		"Enter": KeyEnter, "Escape": KeyEscape, "Backspace": keyBACKSPACE,
		"Tab": keyTAB, "Space": keySPACE, "Quote": keyQUOTE, "Comma": keyCOMMA,
		"Minus": keyMINUS, "Period": keyPERIOD, "Slash": keySLASH,
		"Digit0": key0, "Digit1": key1, "Digit2": key2, "Digit3": key3,
		"Digit4": key4, "Digit5": key5, "Digit6": key6, "Digit7": key7,
		"Digit8": key8, "Digit9": key9,
		"Semicolon": keySEMICOLON, "Equal": keyEQUALS, "BracketLeft": keyLBRACKET,
		"Backslash": keyBACKSLASH, "BracketRight": keyRBRACKET, "Backquote": keyBACKQUOTE,
		"KeyA": keyA, "KeyB": keyB, "KeyC": keyC, "KeyD": keyD, "KeyE": keyE,
		"KeyF": keyF, "KeyG": keyG, "KeyH": keyH, "KeyI": keyI, "KeyJ": keyJ,
		"KeyK": keyK, "KeyL": keyL, "KeyM": keyM, "KeyN": keyN, "KeyO": keyO,
		"KeyP": keyP, "KeyQ": keyQ, "KeyR": keyR, "KeyS": keyS, "KeyT": keyT,
		"KeyU": keyU, "KeyV": keyV, "KeyW": keyW, "KeyX": keyX, "KeyY": keyY,
		"KeyZ": keyZ,
		"CapsLock": keyCAPSLOCK,
		"F1":       keyF1, "F2": keyF2, "F3": keyF3, "F4": keyF4, "F5": KeyF5,
		"F6": keyF6, "F7": keyF7, "F8": keyF8, "F9": keyF9, "F10": keyF10,
		"F11": keyF11, "F12": KeyF12, "F13": keyF13, "F14": keyF14, "F15": keyF15,
		"F16": keyF16, "F17": keyF17, "F18": keyF18, "F19": keyF19, "F20": keyF20,
		"F21": keyF21, "F22": keyF22, "F23": keyF23, "F24": keyF24,
		"PrintScreen": keyPRINTSCREEN, "ScrollLock": KeyScrollLock, "Pause": KeyPause,
		"Insert": KeyInsert, "Home": keyHOME, "PageUp": keyPAGEUP,
		"Delete": keyDELETE, "End": keyEND, "PageDown": keyPAGEDOWN,
		"ArrowRight": keyRIGHT, "ArrowLeft": keyLEFT, "ArrowDown": keyDOWN, "ArrowUp": keyUP,
		"NumLock":        keyNUMLOCKCLEAR,
		"NumpadDivide":   keyKP_DIVIDE,
		"NumpadMultiply": keyKP_MULTIPLY,
		"NumpadSubtract": keyKP_MINUS, "NumpadAdd": keyKP_PLUS,
		"NumpadEnter": keyKP_ENTER,
		"Numpad1":     keyKP_1, "Numpad2": keyKP_2, "Numpad3": keyKP_3,
		"Numpad4": keyKP_4, "Numpad5": keyKP_5, "Numpad6": keyKP_6,
		"Numpad7": keyKP_7, "Numpad8": keyKP_8, "Numpad9": keyKP_9,
		"Numpad0": keyKP_0, "NumpadDecimal": keyKP_PERIOD, "NumpadEqual": keyKP_EQUALS,
		"ContextMenu": keyMENU,
		"ControlLeft": keyLCTRL, "ShiftLeft": keyLSHIFT, "AltLeft": keyLALT,
		"MetaLeft":     keyLGUI,
		"ControlRight": keyRCTRL, "ShiftRight": keyRSHIFT, "AltRight": keyRALT,
		"MetaRight": keyRGUI,
	}

	StringToKeyLUT = make(map[string]Key)
	for k, v := range KeyToStringLUT {
		StringToKeyLUT[v] = k
	}

	input = Input{}
	for i := range input.pads {
		input.pads[i].slot.index = -1
		input.controllerstate[i] = &ControllerState{Buttons: make(map[GamepadButton]byte)}
	}
}

func StringToKey(s string) Key {
	if key, ok := StringToKeyLUT[s]; ok {
		return key
	}
	return KeyUnknown
}

func KeyToString(k Key) string {
	if s, ok := KeyToStringLUT[k]; ok {
		return s
	}
	return ""
}

func expandModifiers(m ModifierKey) ModifierKey {
	// Browser bits are already combined (no left/right distinction).
	return m & (KMOD_CTRL | KMOD_ALT | KMOD_SHIFT | KMOD_GUI)
}

func NewModifierKey(ctrl, alt, shift bool) (mod ModifierKey) {
	if ctrl {
		mod |= KMOD_CTRL
	}
	if alt {
		mod |= KMOD_ALT
	}
	if shift {
		mod |= KMOD_SHIFT
	}
	return
}

// jsEventModifiers builds a ModifierKey from a KeyboardEvent.
func jsEventModifiers(ev js.Value) (mod ModifierKey) {
	if ev.Get("ctrlKey").Bool() {
		mod |= KMOD_CTRL
	}
	if ev.Get("altKey").Bool() {
		mod |= KMOD_ALT
	}
	if ev.Get("shiftKey").Bool() {
		mod |= KMOD_SHIFT
	}
	if ev.Get("metaKey").Bool() {
		mod |= KMOD_GUI
	}
	return
}

// Scratch reused by pollGamepads every frame.
var (
	padSnaps  []padSnapshot
	padSlots  [MaxPlayerNo]padSlot
	padDetach []int
	padAttach [][2]int
	padIgnore js.Func // catches vibrationActuator promises (a newer effect rejects the older one)
)

// A number from JavaScript, 0 when it is missing or not a number.
func jsNumber(v js.Value) float64 {
	if v.Type() != js.TypeNumber {
		return 0
	}
	return v.Float()
}

// Copies one Gamepad out of JavaScript. Only buttons 6 and 7 (the triggers)
// need their analog value.
func readPadSnapshot(p js.Value, snap *padSnapshot) {
	snap.index = int(jsNumber(p.Get("index")))
	snap.id = p.Get("id").String()
	snap.mapping = p.Get("mapping").String()
	c := p.Get("connected")
	snap.connected = c.IsUndefined() || c.Truthy()
	snap.buttons, snap.axes = snap.buttons[:0], snap.axes[:0]
	if btns := p.Get("buttons"); btns.Truthy() {
		for i, n := 0, btns.Length(); i < n && i < len(standardPadButtons); i++ {
			b := btns.Index(i)
			pb := padButton{pressed: b.Truthy() && b.Get("pressed").Truthy()}
			if (i == 6 || i == 7) && b.Truthy() {
				pb.value = jsNumber(b.Get("value"))
			}
			snap.buttons = append(snap.buttons, pb)
		}
	}
	if axes := p.Get("axes"); axes.Truthy() {
		for i, n := 0, axes.Length(); i < n; i++ {
			snap.axes = append(snap.axes, jsNumber(axes.Index(i)))
		}
	}
}

// The live Gamepad object at a Gamepad.index, or undefined.
func livePad(index int) js.Value {
	nav := js.Global().Get("navigator")
	if !nav.Truthy() || nav.Get("getGamepads").Type() != js.TypeFunction {
		return js.Undefined()
	}
	list := nav.Call("getGamepads")
	if !list.Truthy() {
		return js.Undefined()
	}
	for i, n := 0, list.Length(); i < n; i++ {
		if p := list.Index(i); p.Truthy() && int(jsNumber(p.Get("index"))) == index {
			return p
		}
	}
	return js.Undefined()
}

// nextPadSnap grows padSnaps by one reused entry.
func nextPadSnap() *padSnapshot {
	if len(padSnaps) < cap(padSnaps) {
		padSnaps = padSnaps[:len(padSnaps)+1]
	} else {
		padSnaps = append(padSnaps, padSnapshot{})
	}
	return &padSnaps[len(padSnaps)-1]
}

// padPackJS copies every pad's numbers into one Float64Array (the same reads
// as readPadSnapshot), so the engine takes them with a single copy. Walking
// the Gamepad objects from Go handed it a new JS reference for every object
// and string it touched (about 23 a frame per pad); each one is a finalizer
// the next garbage collection runs, and they made each collection's freeze
// longer. Strings (id, mapping) go as numbers into a table; the engine reads a
// string once, the first time it sees its number.
// Layout: [count, then per pad: index, connected, id#, mapping#, nbuttons,
// (pressed, value) per button, naxes, axes...]. pack() returns the bytes used,
// or -1 when the page has no getGamepads.
const padPackJS = `const st = { strs: [], keys: new Map() };
const f64 = new Float64Array(2048);
st.u8 = new Uint8Array(f64.buffer);
const num = (v) => typeof v === 'number' ? v : 0;
const sid = (v) => {
  const s = typeof v === 'string' ? v : (v === undefined ? '<undefined>' : v === null ? '<null>' : '<' + typeof v + '>');
  let k = st.keys.get(s);
  if (k === undefined) { k = st.strs.length; st.strs.push(s); st.keys.set(s, k); }
  return k;
};
st.str = (k) => st.strs[k];
st.pack = () => {
  const nav = globalThis.navigator;
  if (!nav || typeof nav.getGamepads !== 'function') return -1;
  const list = nav.getGamepads();
  let o = 1, n = 0;
  if (list) {
    for (let i = 0; i < list.length; i++) {
      const p = list[i];
      if (!p) continue;
      const b = p.buttons ? p.buttons : [], a = p.axes ? p.axes : [];
      const nb = Math.min(b.length, BTN), na = Math.min(a.length, 64);
      if (o + 6 + 2 * nb + na > f64.length) break;
      f64[o++] = num(p.index);
      f64[o++] = (p.connected === undefined || p.connected) ? 1 : 0;
      f64[o++] = sid(p.id);
      f64[o++] = sid(p.mapping);
      f64[o++] = nb;
      for (let j = 0; j < nb; j++) {
        const x = b[j];
        f64[o++] = x && x.pressed ? 1 : 0;
        f64[o++] = x && (j === 6 || j === 7) ? num(x.value) : 0;
      }
      f64[o++] = na;
      for (let j = 0; j < na; j++) f64[o++] = num(a[j]);
      n++;
    }
  }
  f64[0] = n;
  return o * 8;
};
return st;`

var padPack struct {
	tried bool
	st    js.Value // the packer's state: pack(), str(k), u8; undefined when it could not start
	u8    js.Value // st.u8, the bytes pack() writes
	raw   []byte
	strs  map[int]string
}

// readPadSnapsPacked fills padSnaps through padPackJS. ok false: the packer
// could not start (a page that forbids new Function); use readPadSnapshot.
func readPadSnapsPacked() (ok, apiMissing bool) {
	if !padPack.tried {
		padPack.tried = true
		padPack.st = js.Undefined()
		jsTry(func() {
			padPack.st = js.Global().Get("Function").New("BTN", padPackJS).Invoke(len(standardPadButtons))
		})
		if padPack.st.Truthy() {
			padPack.u8 = padPack.st.Get("u8")
		}
		padPack.raw = make([]byte, 2048*8)
		padPack.strs = map[int]string{}
	}
	if !padPack.st.Truthy() {
		return false, false
	}
	n := padPack.st.Call("pack").Int()
	if n < 0 {
		return true, true
	}
	n = js.CopyBytesToGo(padPack.raw[:n], padPack.u8)
	at := 0
	next := func() float64 {
		if at+8 > n {
			return 0
		}
		v := math.Float64frombits(binary.LittleEndian.Uint64(padPack.raw[at:]))
		at += 8
		return v
	}
	str := func(k int) string {
		s, found := padPack.strs[k]
		if !found {
			s = padPack.st.Call("str", k).String()
			padPack.strs[k] = s
		}
		return s
	}
	padSnaps = padSnaps[:0]
	for count := int(next()); count > 0; count-- {
		snap := nextPadSnap()
		snap.index = int(next())
		snap.connected = next() != 0
		snap.id = str(int(next()))
		snap.mapping = str(int(next()))
		snap.buttons, snap.axes = snap.buttons[:0], snap.axes[:0]
		for nb := int(next()); nb > 0; nb-- {
			pressed := next() != 0
			snap.buttons = append(snap.buttons, padButton{pressed: pressed, value: next()})
		}
		for na := int(next()); na > 0; na-- {
			snap.axes = append(snap.axes, next())
		}
	}
	return true, false
}

// pollGamepads snapshots navigator.getGamepads() once per frame (called by the
// system backend's pollEvents, before sys.pollDevices). Pads that went away
// are detached and new ones attached, as SDL's JOYDEVICEREMOVED/ADDED events
// do on desktop; then every attached slot reads its pad.
func pollGamepads() {
	if ok, apiMissing := readPadSnapsPacked(); apiMissing {
		return
	} else if !ok {
		nav := js.Global().Get("navigator")
		if !nav.Truthy() || nav.Get("getGamepads").Type() != js.TypeFunction {
			return
		}
		list := nav.Call("getGamepads")
		padSnaps = padSnaps[:0]
		if list.Truthy() {
			for i, n := 0, list.Length(); i < n; i++ {
				p := list.Index(i)
				if !p.Truthy() {
					continue
				}
				readPadSnapshot(p, nextPadSnap())
			}
		}
	}
	for s := range input.pads {
		padSlots[s] = input.pads[s].slot
	}
	padDetach, padAttach = padPlan(padSlots[:], padSnaps, padDetach, padAttach)
	for _, s := range padDetach {
		input.detachPad(s)
	}
	for _, a := range padAttach {
		input.attachPad(a[0], &padSnaps[a[1]])
	}
	for s := range input.pads {
		pad := &input.pads[s]
		if pad.slot.index < 0 {
			continue
		}
		for i := range padSnaps {
			if padSnaps[i].index == pad.slot.index {
				pad.layout.read(&padSnaps[i], input.controllerstate[s])
				break
			}
		}
	}
}

// Zeroes a slot's state; the Buttons map stays allocated, as on desktop.
func (input *Input) resetPadState(slot int) {
	st := input.controllerstate[slot]
	st.Axes = [6]int8{}
	for k := range st.Buttons {
		delete(st.Buttons, k)
	}
	st.HasRumble = false
}

func (input *Input) attachPad(slot int, snap *padSnapshot) {
	pad := &input.pads[slot]
	pad.slot = padSlot{index: snap.index, id: snap.id}
	pad.ident = parsePadID(snap.id)
	pad.guid = padGUID(pad.ident)
	pad.layout.capture(snap.axes)
	input.resetPadState(slot)
	if live := livePad(snap.index); live.Truthy() {
		act := live.Get("vibrationActuator")
		input.controllerstate[slot].HasRumble = act.Truthy() && act.Get("playEffect").Type() == js.TypeFunction
	}
	// One line per pad: the fastest way to see what the browser reports.
	js.Global().Get("console").Call("log", fmt.Sprintf("[gamepad %d] id=%q mapping=%q buttons=%d axes=%d index=%d guid=%s rumble=%t",
		slot, snap.id, snap.mapping, len(snap.buttons), len(snap.axes), snap.index, pad.guid, input.controllerstate[slot].HasRumble))
	sys.attachGamepad(slot)
}

func (input *Input) detachPad(slot int) {
	sys.detachGamepad(slot)
	pad := &input.pads[slot]
	js.Global().Get("console").Call("log", fmt.Sprintf("[gamepad %d] disconnected id=%q index=%d", slot, pad.slot.id, pad.slot.index))
	pad.slot = padSlot{index: -1}
	pad.ident, pad.guid = padIdentity{}, ""
	input.resetPadState(slot)
}

func (input *Input) UpdateGamepadMappings(path string) {
	// SDL controller mapping databases don't apply to the browser API.
}

func (input *Input) IsJoystickPresent(joy int) bool {
	return joy >= 0 && joy < len(input.pads) && input.pads[joy].slot.index >= 0
}

// The pad's name without the browser's decorations (vendor and product
// numbers, "STANDARD GAMEPAD").
func (input *Input) GetJoystickName(joy int) string {
	if !input.IsJoystickPresent(joy) {
		return ""
	}
	return input.pads[joy].ident.name
}

// joystick is the pad in a slot, or nil. input_sdl.go returns the SDL
// joystick; system.go's attachGamepad reads its name, serial and path for the
// controller identity log.
func (input *Input) joystick(joy int) *webJoystick {
	if !input.IsJoystickPresent(joy) {
		return nil
	}
	return &webJoystick{input: input, joy: joy}
}

// webJoystick is the browser's stand-in for an SDL joystick: browsers expose
// no serial number and no device path.
type webJoystick struct {
	input *Input
	joy   int
}

func (j *webJoystick) Name() string   { return j.input.GetJoystickName(j.joy) }
func (j *webJoystick) Serial() string { return "" }
func (j *webJoystick) Path() string   { return "" }

// IsRawJoystick is true on desktop for a pad SDL's controller database does
// not know. In the browser every pad is read through the Gamepad API's
// standard layout or through the fork's own layout for non-standard pads
// (gamepad_js.go), so no pad is raw and Controller Mapping capture reads the
// standard control names.
func (input *Input) IsRawJoystick(joy int) bool { return false }

// rawControlLevel and rawHeld read a raw joystick on desktop. No pad is raw
// in the browser.
func rawControlLevel(joy int, token string) int32 { return 0 }
func rawHeld(joy int) string                     { return "" }

// The desktop's starting mapping for a raw joystick. Never used in the
// browser (no pad is raw); a recognized pad maps every control to itself.
var defaultRawControllerMapping = identityControllerMapping()

// An SDL-shaped GUID built from Gamepad.id (padGUID).
func (input *Input) GetJoystickGUID(joy int) string {
	if !input.IsJoystickPresent(joy) {
		return ""
	}
	return input.pads[joy].guid
}

// GetJoystickBackend names the input API behind a pad (upstream's controller
// identity log).
func (input *Input) GetJoystickBackend(joy int) string {
	if !input.IsJoystickPresent(joy) {
		return ""
	}
	return "browser"
}

// GetJoystickKey tells identical pads apart on desktop (GUID plus hashes of
// the serial and the location). Browsers give neither, so it is the GUID;
// system.go's attachGamepad numbers a second identical pad (_n2), as upstream
// does for pads without a serial or a path.
func (input *Input) GetJoystickKey(joy int) string {
	return strings.ToLower(input.GetJoystickGUID(joy))
}

// Whether the pad in this slot can rumble (vibrationActuator).
func (input *Input) HasRumble(joy int) bool {
	return input.IsJoystickPresent(joy) && input.controllerstate[joy] != nil && input.controllerstate[joy].HasRumble
}

// GlyphPack picks the controller artwork for hints, in upstream's order: the
// pack chosen for this device, the keyboard, then the motif's GUID and name
// tables, then the vendor/product/name table (padGlyphPack) in place of SDL's
// controller type.
func (input *Input) GlyphPack(device *InputDevice, glyphs *ControllerGlyphsProperties) string {
	if device == nil {
		return "generic"
	}
	if chosen := sys.cfg.controllerGlyphs(device); chosen != "" {
		if set := glyphs.Controller[chosen]; set != nil && strings.EqualFold(set.Type, glyphDeviceType(device)) {
			return chosen
		}
	}
	if device.Joy < 0 {
		return "keyboard"
	}
	if !input.IsJoystickPresent(device.Joy) {
		return "generic"
	}
	if controller := glyphs.GUID[strings.ToLower(device.GUID)]; controller != "" {
		return strings.ToLower(controller)
	}
	// Longest matching name wins; equal lengths are decided alphabetically.
	name := strings.ToLower(input.GetJoystickName(device.Joy))
	match, controller := "", ""
	for pattern, candidate := range glyphs.Name {
		if pattern == "" || !strings.Contains(name, strings.ToLower(pattern)) {
			continue
		}
		if len(pattern) > len(match) || (len(pattern) == len(match) && pattern < match) {
			match, controller = pattern, candidate
		}
	}
	if controller != "" {
		return strings.ToLower(controller)
	}
	if pack := padGlyphPack(input.pads[device.Joy].ident); pack != "" {
		return pack
	}
	return "generic"
}

// GlyphSwapNintendo reports whether Nintendo pads report button positions
// instead of their printed labels (SDL's USE_BUTTON_LABELS=0 on desktop). The
// W3C standard mapping is positional (button 0 is the bottom face button,
// labeled B on a Nintendo pad), so the browser always swaps.
func (input *Input) GlyphSwapNintendo() bool { return true }

// RumbleController rumbles every pad of a participant (upstream's routing:
// nothing in replays, only the local participant in netplay).
func (input *Input) RumbleController(participant int, lo, hi uint16, ticks uint32) {
	if sys.replayFile != nil {
		return
	}
	nc := sys.netConnection
	if sys.rollback.session != nil {
		nc = sys.rollback.netConnection
	}
	if nc != nil {
		if participant != nc.locIn {
			return
		}
		participant = 0
	}
	for _, id := range sys.inputs.participantDevices(participant) {
		if device := sys.inputs.device(id); device != nil && device.Connected && device.Joy >= 0 && sys.cfg.rumbleEnabled(device) {
			input.rumbleJoystick(device.Joy, lo, hi, ticks)
		}
	}
}

// Plays SDL's Rumble(lo, hi, ms) on the pad's vibrationActuator ("dual-rumble";
// Firefox has none). ticks == 0 stops it.
func (input *Input) rumbleJoystick(joy int, lo, hi uint16, ticks uint32) {
	if !input.HasRumble(joy) {
		return
	}
	durationMs, strong, weak, stop, ok := padRumbleEffect(lo, hi, ticks, sys.gameLogicSpeed(), sys.turbo)
	if !ok {
		return
	}
	live := livePad(input.pads[joy].slot.index)
	if !live.Truthy() {
		return
	}
	act := live.Get("vibrationActuator")
	if !act.Truthy() || act.Get("playEffect").Type() != js.TypeFunction {
		return
	}
	if padIgnore.IsUndefined() {
		padIgnore = js.FuncOf(func(js.Value, []js.Value) any { return nil })
	}
	var promise js.Value
	if stop && act.Get("reset").Type() == js.TypeFunction {
		promise = act.Call("reset")
	} else {
		opts := js.Global().Get("Object").New()
		opts.Set("startDelay", 0)
		opts.Set("duration", durationMs)
		opts.Set("strongMagnitude", Clamp(strong, 0, 1))
		opts.Set("weakMagnitude", Clamp(weak, 0, 1))
		promise = act.Call("playEffect", "dual-rumble", opts)
	}
	if promise.Truthy() && promise.Get("catch").Type() == js.TypeFunction {
		promise.Call("catch", padIgnore)
	}
}
