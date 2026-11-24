package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/ini.v1"
)

// Saved configs of older WebMUGEN builds (config_migrate.go).

// The parts of a 1.0-based web build's saved config that matter here: its own
// keyboard layouts, 1.0's empty P3/P4 and shipped pads, and settings that
// must come through untouched.
const legacyOther = `[Config]
Motif = data/system.def
[Sound]
MasterVolume      = 37
WavVolume         = 41
BGMVolume         = 52
[Video]
GameWidth  = 854
GameHeight = 480
VSync      = 0
[Netplay]
PlayerName      = OLDNAME
RollbackNetcode = 0
ListenPort      = 7601
[Input]
ButtonAssist   = 0
SOCDResolution = 2
`

func legacySlot(name string, joy string, values ...string) string {
	actions := []string{"up", "down", "left", "right", "a", "b", "c", "x", "y", "z", "start", "d", "w", "menu"}
	var b strings.Builder
	b.WriteString("[" + name + "]\nJoystick = " + joy + "\nGUID     = \n")
	for i, a := range actions {
		b.WriteString(a + " = " + values[i] + "\n")
	}
	return b.String()
}

func unusedSlot(name string) string {
	v := make([]string, 14)
	for i := range v {
		v[i] = "Not used"
	}
	return legacySlot(name, "-1", v...)
}

func shippedPad(name, joy string) string {
	return legacySlot(name, joy, "DP_U", "DP_D", "DP_L", "DP_R", "A", "B", "RT", "X", "Y", "RB", "START", "LB", "LT", "BACK") + "RumbleOn = 1\n"
}

var (
	webP1 = legacySlot("Keys_P1", "-1", "w", "s", "a", "d", "k", "l", "SEMICOLON", "i", "o", "p", "RETURN", "q", "e", "Not used")
	webP2 = legacySlot("Keys_P2", "-1", "UP", "DOWN", "LEFT", "RIGHT", "KP_1", "KP_2", "KP_3", "KP_4", "KP_5", "KP_6", "KP_ENTER", "KP_7", "KP_8", "Not used")
	pads  = shippedPad("Joystick_P1", "0") + shippedPad("Joystick_P2", "1") + shippedPad("Joystick_P3", "2") + shippedPad("Joystick_P4", "3")
)

// The web build's own keyboard 1 (events_js.go webKeyboardDefaults), as 2.4 wrote it into [Keys_P1].
var wantWebKeyboard1 = map[string]string{"Name": "Keyboard Controller 1", "DP_U": "w", "DP_D": "s", "DP_L": "a", "DP_R": "d",
	"A": "k", "B": "l", "X": "i", "Y": "o", "START": "RETURN", "BACK": "-", "LB": "e", "RB": "p", "LT": "q", "RT": "SEMICOLON",
	"LS_Y-": "-", "LS_Y+": "-", "LS_X-": "-", "LS_X+": "-", "RS_Y-": "-", "RS_Y+": "-", "RS_X-": "-", "RS_X+": "-", "LS": "-", "RS": "-"}

func migrateText(t *testing.T, text string) (*ini.File, []string) {
	t.Helper()
	if StringToKeyLUT == nil {
		initLUTs()
	}
	user, err := LoadINIText(text, ini.LoadOptions{SkipUnrecognizableLines: true, AllowShadows: true})
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := LoadINIText(platformDefaultConfig(string(defaultConfig)), ini.LoadOptions{SkipUnrecognizableLines: true})
	if err != nil {
		t.Fatal(err)
	}
	return user, migrateSavedControlsLog(user, defaults)
}

func iniText(f *ini.File) string {
	var b strings.Builder
	f.WriteTo(&b)
	return b.String()
}

func wantSection(t *testing.T, f *ini.File, name string, want map[string]string) {
	t.Helper()
	sec := iniSection(f, strings.ToLower(name))
	if sec == nil {
		t.Errorf("[%s] missing", name)
		return
	}
	if len(sec.Keys()) != len(want) {
		t.Errorf("[%s] has %d keys, want %d", name, len(sec.Keys()), len(want))
	}
	for k, v := range want {
		if got, _ := iniValueOf(sec, k); got != v {
			t.Errorf("[%s] %s = %q, want %q", name, k, got, v)
		}
	}
}

func noSections(t *testing.T, f *ini.File, prefixes ...string) {
	t.Helper()
	for _, sec := range f.Sections() {
		for _, p := range prefixes {
			if strings.HasPrefix(iniSectionID(sec), strings.ToLower(p)) {
				t.Errorf("[%s] should not be in the file", sec.Name())
			}
		}
	}
}

func otherSettingsKept(t *testing.T, f *ini.File) {
	t.Helper()
	before, _ := LoadINIText(legacyOther, ini.LoadOptions{})
	for _, sec := range before.Sections() {
		for _, k := range sec.Keys() {
			if got, _ := iniValueOf(iniSection(f, iniSectionID(sec)), k.Name()); got != k.String() {
				t.Errorf("[%s] %s = %q, want %q (kept)", sec.Name(), k.Name(), got, k.String())
			}
		}
	}
}

func with(m map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func TestMigrate24Default(t *testing.T) {
	f, log := migrateText(t, legacyOther+webP1+webP2+unusedSlot("Keys_P3")+unusedSlot("Keys_P4")+pads)
	wantSection(t, f, "ControllerMapping_Keyboard1", wantWebKeyboard1)
	wantSection(t, f, "ControllerMapping_Keyboard2", map[string]string{"Name": "Keyboard Controller 2",
		"DP_U": "UP", "DP_D": "DOWN", "DP_L": "LEFT", "DP_R": "RIGHT", "A": "KP_1", "B": "KP_2", "RT": "KP_3",
		"X": "KP_4", "Y": "KP_5", "RB": "KP_6", "START": "KP_ENTER", "LT": "KP_7", "LB": "KP_8", "BACK": "-",
		"LS_Y-": "-", "LS_Y+": "-", "LS_X-": "-", "LS_X+": "-", "RS_Y-": "-", "RS_Y+": "-", "RS_X-": "-", "RS_X+": "-", "LS": "-", "RS": "-"})
	// P3/P4 unbound, pads as shipped: nothing for them.
	noSections(t, f, "Keys_", "Joystick_", "ControllerMapping_Keyboard3", "ControllerMapping_Keyboard4", "ButtonSetup_")
	otherSettingsKept(t, f)
	if in := iniSection(f, "input"); len(in.Keys()) != 2 {
		t.Errorf("[Input] has %d keys, want the 2 it had", len(in.Keys()))
	}
	t.Logf("log: %q", log)
}

func TestMigrate24CustomKeyboard(t *testing.T) {
	p1 := legacySlot("Keys_P1", "-1", "t", "g", "f", "h", "j", "NOPE", "COMMA", "u", "i", "o", "SPACE", "Not used", "y", "BACKSPACE")
	p2 := legacySlot("keys_p2", "-1", "UP", "DOWN", "LEFT", "RIGHT", "KP_1", "KP_2", "KP_3", "KP_4", "KP_5", "KP_6", "KP_ENTER", "", "KP_8", "Not used")
	p3 := legacySlot("Keys_P3", "-1", "Not used", "Not used", "Not used", "Not used", "z", "x", "Not used", "Not used", "Not used", "Not used", "Not used", "Not used", "Not used", "Not used")
	f, _ := migrateText(t, legacyOther+p1+p2+p3+unusedSlot("Keys_P4")+pads)
	wantSection(t, f, "ControllerMapping_Keyboard1", with(wantWebKeyboard1,
		"DP_U", "t", "DP_D", "g", "DP_L", "f", "DP_R", "h", "A", "j", "B", "-", "RT", "COMMA",
		"X", "u", "Y", "i", "RB", "o", "START", "SPACE", "LT", "-", "LB", "y", "BACK", "BACKSPACE"))
	k2 := iniSection(f, "controllermapping_keyboard2")
	if v, _ := iniValueOf(k2, "LT"); v != "-" {
		t.Errorf("Keyboard2 LT = %q, want - (blank d)", v)
	}
	// P3 has two keys: a third keyboard, its setup and side 1 (P4 is unbound).
	wantSection(t, f, "ControllerMapping_Keyboard3", with(wantWebKeyboard1, "Name", "Keyboard Controller 3",
		"DP_U", "-", "DP_D", "-", "DP_L", "-", "DP_R", "-", "A", "z", "B", "x", "RT", "-", "X", "-", "Y", "-", "RB", "-",
		"START", "-", "LT", "-", "LB", "-"))
	in := iniSection(f, "input")
	for k, v := range map[string]string{"ButtonSetup.keyboard3": "standard", "PreferredSide.keyboard3": "1"} {
		if got, _ := iniValueOf(in, k); got != v {
			t.Errorf("[Input] %s = %q, want %q", k, got, v)
		}
	}
	noSections(t, f, "Keys_", "Joystick_", "ControllerMapping_Keyboard4", "ButtonSetup_")
	otherSettingsKept(t, f)
}

func TestMigrate24CustomPad(t *testing.T) {
	const id = "Xbox 360 Controller (XInput STANDARD GAMEPAD Vendor: 045e Product: 028e)"
	// P1 and P2 the same changed layout (one setup), P3 another, P4 as shipped.
	custom := []string{"DP_U", "DP_D", "DP_L", "DP_R", "X", "Y", "RS", "A", "B", "RB", "START", "LT", "LB", "HOME"}
	j1 := strings.Replace(legacySlot("Joystick_P1", "0", custom...), "GUID     = ", "GUID     = "+id, 1)
	j2 := legacySlot("Joystick_P2", "1", custom...)
	j3 := legacySlot("Joystick_P3", "2", "LS_Y-", "LS_Y+", "LS_X-", "LS_X+", "A", "B", "RT", "X", "Y", "RB", "START", "LB", "LT", "BACK")
	f, log := migrateText(t, legacyOther+webP1+webP2+j1+j2+j3+shippedPad("Joystick_P4", "3"))
	wantSection(t, f, "ControllerMapping_Keyboard1", wantWebKeyboard1)
	std := map[string]string{"Name": "Standard", "up": "DP_U", "down": "DP_D", "left": "DP_L", "right": "DP_R",
		"a": "A", "b": "B", "c": "RT", "x": "X", "y": "Y", "z": "RB", "start": "START", "d": "LT", "w": "LB", "menu": "BACK",
		"xa": "-", "yb": "-", "zc": "-", "xy": "-", "xz": "-", "yz": "-", "xyz": "RS", "ab": "-", "ac": "-", "bc": "-", "abc": "LS"}
	// The shipped setup is written too, or dropDeletedSetups would drop it.
	wantSection(t, f, "ButtonSetup_Standard", std)
	// RS is a single button here, so the xyz row loses it.
	wantSection(t, f, "ButtonSetup_oldlayout", with(std, "Name", "Old layout", "a", "X", "b", "Y", "c", "RS", "x", "A", "y", "B",
		"d", "LT", "w", "LB", "menu", "-", "xyz", "-"))
	wantSection(t, f, "ButtonSetup_oldlayout2", with(std, "Name", "Old layout 2", "up", "LS_Y-", "down", "LS_Y+", "left", "LS_X-", "right", "LS_X+",
		"d", "LB", "w", "LT"))
	noSections(t, f, "Keys_", "Joystick_", "ButtonSetup_oldlayout3")
	in := iniSection(f, "input")
	dev := legacyPadKey(id)
	got, has := iniValueOf(in, "ButtonSetup."+dev)
	if dev != "" && got != "oldlayout" {
		t.Errorf("[Input] ButtonSetup.%s = %q, want oldlayout", dev, got)
	}
	if dev == "" && has {
		t.Errorf("a device key was written for a GUID this platform cannot read")
	}
	otherSettingsKept(t, f)
	t.Logf("pad key %q; log: %q", dev, log)
}

// The 2.5 test builds (upstream modern-input before 07c692fb) saved the old names.
func TestMigrateTestBuildNames(t *testing.T) {
	const guid = "030000005e0400008e02000000007200"
	text := legacyOther + `[Input]
ControllerSetup.keyboard1 = custom1
ControllerSetup.` + guid + ` = standard
PreferredSide.keyboard1 = 1
[ButtonMapping_Keyboard1]
Name  = Keyboard Controller 1
DP_U  = t
DP_D  = g
[ButtonMapping_Keyboard2]
Name  = Keyboard Controller 2
DP_U  = UP
[ButtonMapping_` + guid + `]
Name = My pad
A    = B
B    = A
[ControllerSetup_Standard]
Name = Standard
a    = A
[ControllerSetup_custom1]
Name = Mine
a    = B
` + webP1 // the 2.4 slots a test build kept: superseded, left alone
	f, log := migrateText(t, text)
	wantSection(t, f, "ControllerMapping_Keyboard1", map[string]string{"Name": "Keyboard Controller 1", "DP_U": "t", "DP_D": "g"})
	wantSection(t, f, "ControllerMapping_Keyboard2", map[string]string{"Name": "Keyboard Controller 2", "DP_U": "UP"})
	wantSection(t, f, "ControllerMapping_"+guid, map[string]string{"Name": "My pad", "A": "B", "B": "A"})
	wantSection(t, f, "ButtonSetup_Standard", map[string]string{"Name": "Standard", "a": "A"})
	wantSection(t, f, "ButtonSetup_custom1", map[string]string{"Name": "Mine", "a": "B"})
	noSections(t, f, "ButtonMapping_", "ControllerSetup_")
	in := iniSection(f, "input")
	for k, v := range map[string]string{"ButtonSetup.keyboard1": "custom1", "ButtonSetup." + guid: "standard", "PreferredSide.keyboard1": "1"} {
		if got, _ := iniValueOf(in, k); got != v {
			t.Errorf("[Input] %s = %q, want %q", k, got, v)
		}
	}
	for _, k := range in.Keys() {
		if strings.HasPrefix(strings.ToLower(k.Name()), "controllersetup.") {
			t.Errorf("[Input] %s left behind", k.Name())
		}
	}
	if iniSection(f, "keys_p1") == nil {
		t.Errorf("[Keys_P1] removed from a config whose controls were already this engine's")
	}
	otherSettingsKept(t, f)
	t.Logf("log: %q", log)
}

func TestMigrateAlreadyDone(t *testing.T) {
	text := legacyOther + `[ControllerMapping_Keyboard1]
Name = Keyboard Controller 1
DP_U = t
[ButtonSetup_Standard]
Name = Standard
a    = A
`
	user, _ := LoadINIText(text, ini.LoadOptions{SkipUnrecognizableLines: true, AllowShadows: true})
	before := iniText(user)
	f, log := migrateText(t, text)
	if after := iniText(f); after != before || len(log) != 0 {
		t.Errorf("a migrated config changed:\n%s\nlog %q", after, log)
	}
	// And a config with no controls at all (a first boot's shipped file) too.
	if _, log := migrateText(t, legacyOther); len(log) != 0 {
		t.Errorf("log %q", log)
	}
}

// Old and new names side by side: the new ones always win and stay as they are.
func TestMigrateBothNames(t *testing.T) {
	text := legacyOther + `[ControllerMapping_Keyboard1]
Name = Keyboard Controller 1
DP_U = UP
[ButtonMapping_Keyboard1]
Name = Keyboard Controller 1
DP_U = t
[ButtonSetup_Standard]
Name = Standard
a    = A
[ControllerSetup_Standard]
Name = Standard
a    = X
[ControllerSetup_custom1]
Name = Mine
a    = B
[Input]
ButtonSetup.keyboard1 = standard
ControllerSetup.keyboard1 = custom1
ControllerSetup.keyboard2 = custom1
` + legacySlot("Keys_P1", "-1", "t", "g", "f", "h", "j", "k", "l", "u", "i", "o", "SPACE", "q", "e", "Not used") +
		legacySlot("Joystick_P1", "0", "DP_U", "DP_D", "DP_L", "DP_R", "X", "Y", "RT", "A", "B", "RB", "START", "LT", "LB", "BACK")
	f, _ := migrateText(t, text)
	wantSection(t, f, "ControllerMapping_Keyboard1", map[string]string{"Name": "Keyboard Controller 1", "DP_U": "UP"})
	wantSection(t, f, "ButtonSetup_Standard", map[string]string{"Name": "Standard", "a": "A"})
	wantSection(t, f, "ButtonSetup_custom1", map[string]string{"Name": "Mine", "a": "B"})
	// The old copies that lost are left as they were; no 2.4 slot is converted.
	for _, name := range []string{"buttonmapping_keyboard1", "controllersetup_standard", "keys_p1", "joystick_p1"} {
		if iniSection(f, name) == nil {
			t.Errorf("[%s] removed", name)
		}
	}
	noSections(t, f, "ButtonSetup_oldlayout", "ControllerMapping_Keyboard2")
	in := iniSection(f, "input")
	for k, v := range map[string]string{"ButtonSetup.keyboard1": "standard", "ControllerSetup.keyboard1": "custom1", "ButtonSetup.keyboard2": "custom1"} {
		if got, _ := iniValueOf(in, k); got != v {
			t.Errorf("[Input] %s = %q, want %q", k, got, v)
		}
	}
	otherSettingsKept(t, f)
}

// The whole load: a 2.4 saved file goes through loadConfig, which saves the
// result; the engine then reads the migrated controls, every other setting is
// as saved, and a second load changes nothing.
func TestMigrateLoadConfig(t *testing.T) {
	if StringToKeyLUT == nil {
		initLUTs()
	}
	p1 := legacySlot("Keys_P1", "-1", "t", "g", "f", "h", "j", "k", "l", "u", "i", "o", "SPACE", "q", "e", "Not used")
	j1 := legacySlot("Joystick_P1", "0", "DP_U", "DP_D", "DP_L", "DP_R", "X", "Y", "RT", "A", "B", "RB", "START", "LT", "LB", "BACK")
	file := filepath.Join(t.TempDir(), "config.ini")
	if err := os.WriteFile(file, []byte(legacyOther+p1+webP2+unusedSlot("Keys_P3")+unusedSlot("Keys_P4")+j1+shippedPad("Joystick_P2", "1")), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	m := c.ControllerMapping["controllermapping_keyboard1"]
	if m == nil || m.DpU != "t" || m.A != "j" || m.Start != "SPACE" || m.Back != "-" || m.LsYn != "-" {
		t.Errorf("keyboard 1 mapping %+v", m)
	}
	if m := c.ControllerMapping["controllermapping_keyboard2"]; m == nil || m.DpU != "UP" || m.RT != "KP_3" {
		t.Errorf("keyboard 2 mapping %+v", m)
	}
	for _, id := range []string{"buttonsetup_standard", "buttonsetup_oldlayout"} {
		if c.ButtonSetup[id] == nil {
			t.Errorf("setup %s missing (have %d)", id, len(c.ButtonSetup))
		}
	}
	if s := c.ButtonSetup["buttonsetup_oldlayout"]; s != nil && (s.Buttons["a"] != "X" || s.Buttons["d"] != "LT") {
		t.Errorf("old layout %+v", s.Buttons)
	}
	if c.Sound.MasterVolume != 37 || c.Netplay.PlayerName != "OLDNAME" || c.Netplay.ListenPort != 7601 || c.Video.VSync != 0 ||
		c.Input.ButtonAssist || c.Input.SOCDResolution != 2 || c.Netplay.RollbackNetcode {
		t.Errorf("settings changed: volume %d name %q port %d vsync %d assist %t socd %d rollback %t", c.Sound.MasterVolume,
			c.Netplay.PlayerName, c.Netplay.ListenPort, c.Video.VSync, c.Input.ButtonAssist, c.Input.SOCDResolution, c.Netplay.RollbackNetcode)
	}
	saved, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "[Keys_P") || strings.Contains(string(saved), "[Joystick_P") {
		t.Errorf("old sections saved again")
	}
	if !strings.Contains(string(saved), "[ButtonSetup_oldlayout]") {
		t.Errorf("old layout not saved")
	}
	if _, err := loadConfig(file); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(file)
	if string(again) != string(saved) {
		t.Errorf("a second load changed the saved file")
	}
}

// RumbleOn = 0 in a 1.0 pad slot turns rumble off for the pad the slot names,
// whether or not its layout changed. A slot with no GUID names no pad.
func TestMigrate24RumbleOff(t *testing.T) {
	if StringToKeyLUT == nil {
		initLUTs()
	}
	id := "Xbox 360 Controller (XInput STANDARD GAMEPAD Vendor: 045e Product: 028e)"
	if legacyPadKey(id) == "" {
		id = "030000005e0400008e02000000007200" // the desktop engine's slot GUID
	}
	other := "054c000068020000" + "0000000000000000"
	shipped := []string{"DP_U", "DP_D", "DP_L", "DP_R", "A", "B", "RT", "X", "Y", "RB", "START", "LB", "LT", "BACK"}
	withGUID := func(slot, guid string) string { return strings.Replace(slot, "GUID     = ", "GUID     = "+guid, 1) }
	j1 := withGUID(legacySlot("Joystick_P1", "0", shipped...), id) + "RumbleOn = 0\n"
	j2 := legacySlot("Joystick_P2", "1", shipped...) + "RumbleOn = 0\n"
	j3 := withGUID(legacySlot("Joystick_P3", "2", shipped...), other) + "RumbleOn = 1\n"
	// A hand-edited "off": 1.0 read bools with strconv.ParseBool, could not
	// read it, and kept rumble on.
	j4 := legacySlot("Joystick_P4", "3", shipped...) + "RumbleOn = off\n"
	file := filepath.Join(t.TempDir(), "config.ini")
	if err := os.WriteFile(file, []byte(legacyOther+webP1+webP2+j1+j2+j3+j4), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	key := legacyPadKey(id)
	if c.Input.ControllerRumble[key] != "0" || len(c.Input.ControllerRumble) != 1 {
		t.Errorf("ControllerRumble %v, want only %s = 0", c.Input.ControllerRumble, key)
	}
	// P2 names no pad: its rumble waits for the first pad in slot 2.
	if c.Input.PendingRumble["p2"] != "0" || len(c.Input.PendingRumble) != 1 {
		t.Errorf("PendingRumble %v, want only p2 = 0", c.Input.PendingRumble)
	}
	// The layouts are as shipped: no setup is written for them.
	if len(c.ButtonSetup) != 1 || c.ButtonSetup["buttonsetup_standard"] == nil {
		t.Errorf("setups %d, want only the shipped Standard", len(c.ButtonSetup))
	}
	saved, _ := os.ReadFile(file)
	if !strings.Contains(string(saved), "ControllerRumble."+key) || !strings.Contains(string(saved), "PendingRumble.p2") ||
		strings.Contains(string(saved), "[ButtonSetup_oldlayout") {
		t.Errorf("saved file:\n%s", saved)
	}
	if _, err := loadConfig(file); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(file); string(again) != string(saved) {
		t.Errorf("a second load changed the saved file")
	}
	t.Logf("pad key %q", key)
}

// A keyboard the defaults have, which the player cleared in 1.0 (every key
// "Not used"), stays cleared instead of getting the shipped keys back.
func TestMigrate24ClearedKeyboard(t *testing.T) {
	p1 := legacySlot("Keys_P1", "-1", "UP", "DOWN", "LEFT", "RIGHT", "KP_1", "KP_2", "KP_3", "KP_4", "KP_5", "KP_6", "KP_ENTER", "KP_7", "KP_8", "Not used")
	f, log := migrateText(t, legacyOther+p1+unusedSlot("Keys_P2")+unusedSlot("Keys_P3")+unusedSlot("Keys_P4")+pads)
	cleared := map[string]string{"Name": "Keyboard Controller 2"}
	for _, c := range virtualControlNames {
		cleared[c] = "-"
	}
	wantSection(t, f, "ControllerMapping_Keyboard2", cleared)
	wantSection(t, f, "ControllerMapping_Keyboard1", with(cleared, "Name", "Keyboard Controller 1",
		"DP_U", "UP", "DP_D", "DOWN", "DP_L", "LEFT", "DP_R", "RIGHT", "A", "KP_1", "B", "KP_2", "RT", "KP_3",
		"X", "KP_4", "Y", "KP_5", "RB", "KP_6", "START", "KP_ENTER", "LT", "KP_7", "LB", "KP_8"))
	// 1.0's empty P3/P4 have no keyboard in the defaults: still nothing for them.
	noSections(t, f, "Keys_", "Joystick_", "ControllerMapping_Keyboard3", "ControllerMapping_Keyboard4", "ButtonSetup_")
	if in := iniSection(f, "input"); len(in.Keys()) != 2 {
		t.Errorf("[Input] has %d keys, want the 2 it had", len(in.Keys()))
	}
	otherSettingsKept(t, f)
	t.Logf("log: %q", log)
}

// 1.0's Rumble row and its Key Config Default turn rumble off without writing
// a GUID. The first pad attached in that slot takes the setting, once.
func TestMigrate24RumbleOffNoGUID(t *testing.T) {
	if StringToKeyLUT == nil {
		initLUTs()
	}
	shipped := []string{"DP_U", "DP_D", "DP_L", "DP_R", "A", "B", "RT", "X", "Y", "RB", "START", "LB", "LT", "BACK"}
	j1 := legacySlot("Joystick_P1", "0", shipped...) + "RumbleOn = 0\n"
	j2 := legacySlot("Joystick_P2", "1", shipped...) + "RumbleOn = 0\n"
	file := filepath.Join(t.TempDir(), "config.ini")
	if err := os.WriteFile(file, []byte(legacyOther+webP1+webP2+j1+j2+shippedPad("Joystick_P3", "2")), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Input.ControllerRumble) != 0 || len(c.Input.PendingRumble) != 2 || c.Input.PendingRumble["p1"] != "0" || c.Input.PendingRumble["p2"] != "0" {
		t.Fatalf("ControllerRumble %v, PendingRumble %v; want none and p1, p2 = 0", c.Input.ControllerRumble, c.Input.PendingRumble)
	}
	padA := "030000005e0400008e02000000007200"
	padB := "030000004c050000cc09000000006800"
	padC := "03000000de2800000112000000007100"
	// A pad in slot 3 has no note.
	third := &InputDevice{Joy: 2, Key: padC, Connected: true}
	c.takePendingRumble(third)
	if !c.rumbleEnabled(third) || len(c.Input.PendingRumble) != 2 {
		t.Errorf("slot 3: rumble %t, PendingRumble %v", c.rumbleEnabled(third), c.Input.PendingRumble)
	}
	// The first pad in slot 1 takes it.
	first := &InputDevice{Joy: 0, Key: padA, Connected: true}
	if !c.rumbleEnabled(first) {
		t.Errorf("slot 1 rumbles before it attaches")
	}
	c.takePendingRumble(first)
	if c.rumbleEnabled(first) || c.Input.ControllerRumble[padA] != "0" || c.Input.PendingRumble["p1"] != "" {
		t.Errorf("slot 1: rumble %t, ControllerRumble %v, PendingRumble %v", c.rumbleEnabled(first), c.Input.ControllerRumble, c.Input.PendingRumble)
	}
	// Taken once: another pad in slot 1 later keeps rumble on.
	later := &InputDevice{Joy: 0, Key: padC, Connected: true}
	c.takePendingRumble(later)
	if !c.rumbleEnabled(later) {
		t.Errorf("a later pad in slot 1 took the note again")
	}
	// A pad that has a rumble setting of its own keeps it; the note goes.
	if err := c.rememberRumble(&InputDevice{Joy: 1, Key: padB}, true); err != nil {
		t.Fatal(err)
	}
	second := &InputDevice{Joy: 1, Key: padB, Connected: true}
	c.takePendingRumble(second)
	if !c.rumbleEnabled(second) || len(c.Input.PendingRumble) != 0 {
		t.Errorf("slot 2: rumble %t, PendingRumble %v", c.rumbleEnabled(second), c.Input.PendingRumble)
	}
	saved, _ := os.ReadFile(file)
	if strings.Contains(strings.ToLower(string(saved)), "pendingrumble") ||
		!strings.Contains(string(saved), "ControllerRumble."+padA+" = 0") || !strings.Contains(string(saved), "ControllerRumble."+padB+" = 1") {
		t.Errorf("saved file:\n%s", saved)
	}
	// Reloaded: the pad from slot 1 stays off wherever it attaches.
	c2, err := loadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if c2.rumbleEnabled(&InputDevice{Joy: 3, Key: padA}) || len(c2.Input.PendingRumble) != 0 {
		t.Errorf("after reload: rumble %t, PendingRumble %v", c2.rumbleEnabled(&InputDevice{Joy: 3, Key: padA}), c2.Input.PendingRumble)
	}
	if again, _ := os.ReadFile(file); string(again) != string(saved) {
		t.Errorf("a second load changed the saved file")
	}
}

func TestLegacyBoolOff(t *testing.T) {
	for v, want := range map[string]bool{"0": true, " 0 ": true, "f": true, "F": true, "false": true, "FALSE": true, "False": true,
		"1": false, "true": false, "": false, "off": false, "no": false, "n": false, "OFF": false} {
		if got := legacyBoolOff(v); got != want {
			t.Errorf("legacyBoolOff(%q) = %t, want %t", v, got, want)
		}
	}
}
