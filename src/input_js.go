//go:build js

package main

import (
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
// pollGamepads, called by the system backend's event pump). Pads with the
// W3C "standard" mapping are translated to the SDL GameController layout
// the engine expects (same button order, axes LX LY RX RY LT RT). Real
// controllers are ranked ahead of junk HID devices (USB microphones
// advertise buttons and would otherwise claim the P1 slot).

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
var ButtonToStringLUT map[int]string
var StringToButtonLUT map[string]int
var jsCodeToKey map[string]Key

// SDL GameController button order the engine expects from
// GetJoystickButtons (matches input_sdl.go's buttonOrder).
// Index in this list -> browser standard-mapping button index.
var sdlOrderToBrowser = [15]int{
	0,  // A
	1,  // B
	2,  // X
	3,  // Y
	8,  // BACK
	16, // GUIDE
	9,  // START
	10, // LEFTSTICK
	11, // RIGHTSTICK
	4,  // LEFTSHOULDER
	5,  // RIGHTSHOULDER
	12, // DPAD_UP
	13, // DPAD_DOWN
	14, // DPAD_LEFT
	15, // DPAD_RIGHT
}

// ControllerState mirrors the SDL backend's shape - shared code
// (input.go LocalAnalogInput) reaches into these fields directly.
type ControllerState struct {
	Axes      [6]int8
	Buttons   map[int]byte
	HasRumble bool
}

// Browser-side metadata per pad slot (not visible to shared code).
type jsPadMeta struct {
	name     string
	standard bool
	axisRest []float32
	rumble   js.Value
	logged   bool
}

type Input struct {
	controllerstate [MaxPlayerNo]*ControllerState
	meta            [MaxPlayerNo]jsPadMeta
}

// Identity order: our buttons are already stored in SDL GameController
// numbering (0..14). Shared code iterates this to map button ids.
var buttonOrder []int

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

	// Same numbering scheme as input_sdl.go: 0-14 buttons in SDL order,
	// 15-24 synthetic axis tokens, 25 = disabled.
	ButtonToStringLUT = map[int]string{
		0: "A", 1: "B", 2: "X", 3: "Y", 4: "BACK", 5: "HOME", 6: "START",
		7: "LS", 8: "RS", 9: "LB", 10: "RB",
		11: "DP_U", 12: "DP_D", 13: "DP_L", 14: "DP_R",
		15: "LS_Y-", 16: "LS_X-", 17: "LS_X+", 18: "LS_Y+",
		19: "LT", 20: "RT",
		21: "RS_Y-", 22: "RS_X-", 23: "RS_X+", 24: "RS_Y+",
		25: "Not used",
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
	StringToButtonLUT = make(map[string]int)
	for k, v := range KeyToStringLUT {
		StringToKeyLUT[v] = k
	}
	for k, v := range ButtonToStringLUT {
		StringToButtonLUT[v] = k
	}

	buttonOrder = make([]int, 15)
	for i := range buttonOrder {
		buttonOrder[i] = i
	}

	input = Input{}
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

// pollGamepads snapshots navigator.getGamepads() once per frame (called by
// the system backend). Standard-mapping pads are translated to the SDL
// GameController layout; others pass through raw with axis rest-value
// baselining so a resting hat/trigger never reads as held input.
func pollGamepads() {
	nav := js.Global().Get("navigator")
	if nav.IsUndefined() || nav.Get("getGamepads").IsUndefined() {
		return
	}
	pads := nav.Call("getGamepads")
	// Rank: standard-mapping pads, then unrecognized pads with axes,
	// then button-only devices (USB mics etc.) last.
	rank := func(p js.Value) int {
		if p.Get("mapping").String() == "standard" {
			return 0
		}
		if p.Get("axes").Length() > 0 {
			return 1
		}
		return 2
	}
	toI8 := func(f float64) int8 {
		if f > 1 {
			f = 1
		} else if f < -1 {
			f = -1
		}
		return int8(f * 127)
	}
	slot := 0
	for r := 0; r <= 2 && slot < len(input.controllerstate); r++ {
		for i, n := 0, pads.Length(); i < n && slot < len(input.controllerstate); i++ {
			p := pads.Index(i)
			if !p.Truthy() || rank(p) != r {
				continue
			}
			if input.controllerstate[slot] == nil {
				input.controllerstate[slot] = &ControllerState{Buttons: make(map[int]byte)}
			}
			g := input.controllerstate[slot]
			m := &input.meta[slot]
			m.name = p.Get("id").String()
			m.standard = p.Get("mapping").String() == "standard"
			m.rumble = p.Get("vibrationActuator")
			g.HasRumble = m.rumble.Truthy()
			btns, axes := p.Get("buttons"), p.Get("axes")
			nb, na := btns.Length(), axes.Length()
			if !m.logged {
				m.logged = true
				js.Global().Get("console").Call("log",
					"[gamepad]", slot, m.name, p.Get("mapping").String())
			}
			if m.standard {
				for si, bi := range sdlOrderToBrowser {
					g.Buttons[si] = 0
					if bi < nb && btns.Index(bi).Get("pressed").Bool() {
						g.Buttons[si] = 1
					}
				}
				for a := 0; a < 4; a++ {
					g.Axes[a] = 0
					if a < na {
						g.Axes[a] = toI8(axes.Index(a).Float())
					}
				}
				// Triggers: standard buttons 6/7 -> SDL trigger axes 4/5
				// (0..127, rest 0 - matching SDL GameController semantics).
				for ti, bi := range [2]int{6, 7} {
					g.Axes[4+ti] = 0
					if bi < nb {
						g.Axes[4+ti] = toI8(btns.Index(bi).Get("value").Float())
					}
				}
			} else {
				// Raw pass-through with rest baselining.
				if len(m.axisRest) != na {
					m.axisRest = make([]float32, na)
					for a := 0; a < na; a++ {
						m.axisRest[a] = float32(axes.Index(a).Float())
					}
				}
				for a := 0; a < 6; a++ {
					g.Axes[a] = 0
				}
				for a := 0; a < na && a < 6; a++ {
					g.Axes[a] = toI8(axes.Index(a).Float() - float64(m.axisRest[a]))
				}
				for b := 0; b < 15; b++ {
					g.Buttons[b] = 0
					if b < nb && btns.Index(b).Get("pressed").Bool() {
						g.Buttons[b] = 1
					}
				}
			}
			slot++
		}
	}
	for ; slot < len(input.controllerstate); slot++ {
		if input.controllerstate[slot] != nil {
			input.controllerstate[slot] = nil
			input.meta[slot] = jsPadMeta{}
		}
	}
}

func (input *Input) UpdateGamepadMappings(path string) {
	// SDL controller mapping databases don't apply to the browser API.
}

func (input *Input) GetMaxJoystickCount() int {
	return len(input.controllerstate)
}

func (input *Input) IsJoystickPresent(joy int) bool {
	return joy >= 0 && joy < len(input.controllerstate) && input.controllerstate[joy] != nil
}

func (input *Input) GetJoystickName(joy int) string {
	if !input.IsJoystickPresent(joy) {
		return ""
	}
	return input.meta[joy].name
}

func (input *Input) GetJoystickAxes(joy int) [6]float32 {
	if !input.IsJoystickPresent(joy) {
		return [6]float32{}
	}
	return NormalizeAxes(&input.controllerstate[joy].Axes)
}

func (input *Input) GetJoystickButtons(joy int) []byte {
	if !input.IsJoystickPresent(joy) {
		return []byte{}
	}
	buttons := make([]byte, len(buttonOrder))
	for i, button := range buttonOrder {
		buttons[i] = input.controllerstate[joy].Buttons[button]
	}
	return buttons
}

func (input *Input) GetJoystickPath(joy int) string {
	return input.GetJoystickName(joy)
}

func (input *Input) GetJoystickGUID(joy int) string {
	return input.GetJoystickName(joy)
}

func (input *Input) RumbleController(joy int, lo, hi uint16, ticks uint32) {
	if !input.IsJoystickPresent(joy) || joy >= len(sys.joystickConfig) {
		return
	}
	if !sys.joystickConfig[joy].rumbleOn {
		return
	}
	act := input.meta[joy].rumble
	if !act.Truthy() || act.Get("playEffect").IsUndefined() {
		return
	}
	gls := sys.gameLogicSpeed()
	if gls <= 0 || sys.turbo <= 0 {
		return
	}
	durationMs := float64(ticks) * (1.0 / float64(gls) * float64(sys.turbo) * 1000.0)
	opts := js.Global().Get("Object").New()
	if ticks > 0 {
		opts.Set("duration", durationMs)
		opts.Set("strongMagnitude", float64(lo)/65535)
		opts.Set("weakMagnitude", float64(hi)/65535)
	} else {
		opts.Set("duration", 0)
		opts.Set("strongMagnitude", 0)
		opts.Set("weakMagnitude", 0)
	}
	act.Call("playEffect", "dual-rumble", opts)
}

func CheckAxisForDpad(axes *[6]float32, base int) string {
	var s string = ""
	if (*axes)[0] > sys.cfg.Input.ControllerStickSensitivity {
		s = ButtonToStringLUT[2+base]
	} else if -(*axes)[0] > sys.cfg.Input.ControllerStickSensitivity {
		s = ButtonToStringLUT[1+base]
	}
	if (*axes)[1] > sys.cfg.Input.ControllerStickSensitivity {
		s = ButtonToStringLUT[3+base]
	} else if -(*axes)[1] > sys.cfg.Input.ControllerStickSensitivity {
		s = ButtonToStringLUT[base]
	}
	if (*axes)[2] > sys.cfg.Input.ControllerStickSensitivity {
		s = ButtonToStringLUT[8+base]
	} else if -(*axes)[2] > sys.cfg.Input.ControllerStickSensitivity {
		s = ButtonToStringLUT[7+base]
	}
	if (*axes)[3] > sys.cfg.Input.ControllerStickSensitivity {
		s = ButtonToStringLUT[9+base]
	} else if -(*axes)[3] > sys.cfg.Input.ControllerStickSensitivity {
		s = ButtonToStringLUT[6+base]
	}
	return s
}

func CheckAxisForTrigger(axes *[6]float32) string {
	var s string = ""
	for _, i := range [2]int{4, 5} {
		if (*axes)[i] > 0 {
			s = ButtonToStringLUT[15+i]
			break
		}
	}
	return s
}

// getJoystickKey returns the first active button/axis token and joystick
// index (options-menu binding scanner).
func getJoystickKey(controllerIdx int) (string, int) {
	var s string
	min, max := 0, input.GetMaxJoystickCount()
	if controllerIdx >= 0 && controllerIdx < max {
		min, max = controllerIdx, controllerIdx+1
	}
	for joy := min; joy < max; joy++ {
		if !input.IsJoystickPresent(joy) {
			continue
		}
		axes := input.GetJoystickAxes(joy)
		btns := input.GetJoystickButtons(joy)
		s = CheckAxisForDpad(&axes, len(btns))
		if s != "" {
			return s, joy
		}
		s = CheckAxisForTrigger(&axes)
		if s != "" {
			return s, joy
		}
		for i := range btns {
			if btns[i] > 0 {
				s = ButtonToStringLUT[i]
			}
		}
		if s != "" && strings.ToLower(s) != "not used" {
			return s, joy
		}
	}
	return "", -1
}
