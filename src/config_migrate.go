package main

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/ini.v1"
)

// SAVED CONFIGS FROM OLDER WEBMUGEN BUILDS (fork carry).
//
// A browser keeps the config.ini the engine wrote on the player's last visit
// and loads it over the shipped one, so a returning player brings the control
// sections of whatever engine they last played. This engine reads none of the
// older names: without this, their keys would fall back to the build's layouts.
// migrateSavedControls rewrites the user file in memory before loadConfig lays
// it over the defaults; loadConfig then saves the result, so it runs once.
//
//  1. Earlier modern-input builds (upstream before 07c692fb):
//     [ButtonMapping_X] -> [ControllerMapping_X], [ControllerSetup_X] ->
//     [ButtonSetup_X], [Input] ControllerSetup.<device> -> ButtonSetup.<device>.
//     Only where the new name is absent: a section or key under the new name
//     is never overwritten (the engine saves in-game remaps there), and the
//     old one is then left as it is.
//  2. Builds on upstream 1.0 ([Keys_Pn] / [Joystick_Pn]), when the file has
//     no [ControllerMapping_*] at all. Each keyboard slot with a bound key,
//     or one the defaults have a keyboard for, becomes
//     [ControllerMapping_KeyboardN], written whole (a control left out would
//     inherit the default key and could collide). A pad slot with RumbleOn
//     off turns rumble off for the pad its GUID names; with no GUID, for the
//     first pad attached in that slot (takePendingRumble). A pad slot that
//     differs from 1.0's shipped pad layout becomes a Button Setup named
//     "Old layout", used by the pad the slot was configured with when that is
//     known; a slot left at the shipped layout is dropped (pads use the
//     Standard setup). Once any setup is written, the shipped setups are
//     written too: the user file's setup list is authoritative
//     (dropDeletedSetups). Then the old sections are removed.
//
// It runs on every platform (the browser and a desktop build of this engine).

// A 1.0 slot's actions and the controls they sit on in the Standard setup.
var legacySlotControls = [...]struct{ action, control string }{
	{"up", "DP_U"}, {"down", "DP_D"}, {"left", "DP_L"}, {"right", "DP_R"},
	{"a", "A"}, {"b", "B"}, {"c", "RT"}, {"x", "X"}, {"y", "Y"}, {"z", "RB"},
	{"start", "START"}, {"d", "LT"}, {"w", "LB"}, {"menu", "BACK"},
}

// 1.0's shipped [Joystick_Pn] buttons (its defaultConfig.ini; d and w the
// other way round from the Standard setup).
var legacyPadDefault = map[string]string{
	"up": "DP_U", "down": "DP_D", "left": "DP_L", "right": "DP_R",
	"a": "A", "b": "B", "c": "RT", "x": "X", "y": "Y", "z": "RB",
	"start": "START", "d": "LB", "w": "LT", "menu": "BACK",
}

var legacySlotRe = regexp.MustCompile(`^(keys|joystick)_p([0-9]+)$`)

// A key of a section, matched without case; the first value when repeated.
func iniValueOf(sec *ini.Section, name string) (string, bool) {
	if sec == nil {
		return "", false
	}
	for _, k := range sec.Keys() {
		if strings.EqualFold(k.Name(), name) {
			v, _ := iniFirstValue(k)
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

func iniCopySection(dst *ini.File, name string, src *ini.Section) (*ini.Section, error) {
	sec, err := dst.NewSection(name)
	if err != nil {
		return nil, err
	}
	for _, k := range src.Keys() {
		v, _ := iniFirstValue(k)
		sec.Key(k.Name()).SetValue(v)
	}
	return sec, nil
}

// The legacy slot's binding as this engine spells it, or "-".
func legacyKeyName(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.EqualFold(v, "Not used") || StringToKey(v) == KeyUnknown {
		return "-"
	}
	return v
}

// A 1.0 bool the old engine read as false (its config reader used
// strconv.ParseBool); a value it could not read left its default, which was on.
func legacyBoolOff(v string) bool {
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	return err == nil && !b
}

func legacyControlName(v string) string {
	if vc := parseVirtualControl(v); vc != VC_None {
		return virtualControlNames[vc]
	}
	return "-"
}

func migrateSavedControls(user, defaults *ini.File) {
	for _, line := range migrateSavedControlsLog(user, defaults) {
		fmt.Printf("Config: %s\n", line)
	}
}

// The migration; returns one line per change.
func migrateSavedControlsLog(user, defaults *ini.File) []string {
	if user == nil {
		return nil
	}
	var log []string
	log = append(log, renameOldModernInput(user)...)
	log = append(log, migrateLegacySlots(user, defaults)...)
	return log
}

func renameOldModernInput(user *ini.File) []string {
	var log []string
	for _, r := range [...]struct{ from, to string }{
		{"ButtonMapping_", "ControllerMapping_"},
		{"ControllerSetup_", "ButtonSetup_"},
	} {
		var old []*ini.Section
		for _, sec := range user.Sections() {
			if n := sec.Name(); len(n) > len(r.from) && strings.EqualFold(n[:len(r.from)], r.from) {
				old = append(old, sec)
			}
		}
		for _, sec := range old {
			name := r.to + sec.Name()[len(r.from):]
			if iniSection(user, strings.ToLower(strings.ReplaceAll(name, " ", "_"))) != nil {
				log = append(log, fmt.Sprintf("[%s] kept as it is: [%s] exists", sec.Name(), name))
				continue
			}
			if _, err := iniCopySection(user, name, sec); err != nil {
				continue
			}
			user.DeleteSection(sec.Name())
			log = append(log, fmt.Sprintf("[%s] -> [%s]", sec.Name(), name))
		}
	}
	input := iniSection(user, "input")
	if input == nil {
		return log
	}
	const from, to = "ControllerSetup.", "ButtonSetup."
	var old []*ini.Key
	for _, k := range input.Keys() {
		if n := k.Name(); len(n) > len(from) && strings.EqualFold(n[:len(from)], from) {
			old = append(old, k)
		}
	}
	for _, k := range old {
		name := to + k.Name()[len(from):]
		if _, exists := iniValueOf(input, name); exists {
			log = append(log, fmt.Sprintf("[Input] %s kept as it is: %s exists", k.Name(), name))
			continue
		}
		v, _ := iniFirstValue(k)
		oldName := k.Name()
		input.DeleteKey(oldName)
		input.Key(name).SetValue(v)
		log = append(log, fmt.Sprintf("[Input] %s -> %s", oldName, name))
	}
	return log
}

func migrateLegacySlots(user, defaults *ini.File) []string {
	type slot struct {
		n   int
		sec *ini.Section
	}
	var keys, pads []slot
	for _, sec := range user.Sections() {
		m := legacySlotRe.FindStringSubmatch(iniSectionID(sec))
		if m == nil {
			if strings.HasPrefix(iniSectionID(sec), "controllermapping_") {
				return nil // already on this engine's controls
			}
			continue
		}
		n, _ := strconv.Atoi(m[2])
		if m[1] == "keys" {
			keys = append(keys, slot{n, sec})
		} else {
			pads = append(pads, slot{n, sec})
		}
	}
	if len(keys)+len(pads) == 0 {
		return nil
	}
	sort.SliceStable(keys, func(i, j int) bool { return keys[i].n < keys[j].n })
	sort.SliceStable(pads, func(i, j int) bool { return pads[i].n < pads[j].n })
	var log []string
	setInput := func(name, value string) {
		input := iniSection(user, "input")
		if input == nil {
			input = user.Section("Input")
		}
		if _, exists := iniValueOf(input, name); !exists {
			input.Key(name).SetValue(value)
		}
	}
	joystickOf := func(sec *ini.Section, def int) int {
		if v, ok := iniValueOf(sec, "Joystick"); ok {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
		return def
	}

	// Keyboards.
	for _, s := range keys {
		if joystickOf(s.sec, -1) != -1 {
			log = append(log, fmt.Sprintf("[%s] dropped: not a keyboard slot", s.sec.Name()))
			continue
		}
		bound := map[string]string{}
		someKey := false
		for _, a := range legacySlotControls {
			v, _ := iniValueOf(s.sec, a.action)
			bound[a.control] = legacyKeyName(v)
			someKey = someKey || bound[a.control] != "-"
		}
		id := fmt.Sprintf("keyboard%d", s.n)
		name := fmt.Sprintf("ControllerMapping_Keyboard%d", s.n)
		label := fmt.Sprintf("Keyboard Controller %d", s.n)
		var shipped *ini.Section
		if defaults != nil {
			shipped = iniSection(defaults, "controllermapping_"+id)
		}
		// A slot with nothing bound is skipped only when the defaults have no
		// keyboard for it (1.0's empty P3/P4). A keyboard the defaults do have
		// was cleared by the player and is written all "-", or it would come
		// back with the shipped keys.
		if !someKey && shipped == nil {
			continue
		}
		if v, ok := iniValueOf(shipped, "Name"); ok && v != "" {
			label = v
		}
		sec, err := user.NewSection(name)
		if err != nil {
			continue
		}
		sec.Key("Name").SetValue(label)
		for _, c := range virtualControlNames {
			v := bound[c]
			if v == "" {
				v = "-"
			}
			sec.Key(c).SetValue(v)
		}
		if shipped == nil {
			// A keyboard the defaults do not have: its setup and side as 1.0
			// placed the slot (odd players on side 1, even on side 2).
			setInput("ButtonSetup."+id, "standard")
			setInput("PreferredSide."+id, strconv.Itoa(2-s.n%2))
		}
		log = append(log, fmt.Sprintf("[%s] -> [%s]", s.sec.Name(), name))
	}

	// Pads (Decision 14): a changed layout becomes a named setup.
	type layout struct {
		id, name string
		rows     map[string]string
	}
	var layouts []*layout
	byText := map[string]*layout{}
	used := map[string]bool{}
	for _, f := range []*ini.File{user, defaults} {
		if f == nil {
			continue
		}
		for _, sec := range f.Sections() {
			if id, ok := strings.CutPrefix(iniSectionID(sec), "buttonsetup_"); ok {
				used[id] = true
				if v, ok := iniValueOf(sec, "Name"); ok {
					used["name:"+strings.ToLower(v)] = true
				}
			}
		}
	}
	for _, s := range pads {
		if joystickOf(s.sec, s.n-1) < 0 {
			continue
		}
		guid, _ := iniValueOf(s.sec, "GUID")
		dev := legacyPadKey(guid)
		// Rumble turned off in 1.0's Key Config: off for that pad here too,
		// whatever its layout. 1.0 wrote the GUID only while capturing a
		// button (its Rumble row and its Default leave it blank), and it read
		// the flag of [Joystick_Pn] for the pad in slot n. A slot with no GUID
		// leaves the flag on that slot: PendingRumble.p<n>, taken by the first
		// pad attached there.
		if v, ok := iniValueOf(s.sec, "RumbleOn"); ok && legacyBoolOff(v) {
			if dev != "" {
				setInput("ControllerRumble."+dev, "0")
				log = append(log, fmt.Sprintf("[%s] RumbleOn = 0 -> ControllerRumble.%s = 0", s.sec.Name(), dev))
			} else {
				setInput(fmt.Sprintf("PendingRumble.p%d", s.n), "0")
				log = append(log, fmt.Sprintf("[%s] RumbleOn = 0 -> PendingRumble.p%d = 0 (the first pad in slot %d)", s.sec.Name(), s.n, s.n))
			}
		}
		rows := map[string]string{}
		var text []string
		changed := false
		for _, a := range legacySlotControls {
			v, ok := iniValueOf(s.sec, a.action)
			if !ok {
				v = legacyPadDefault[a.action]
			}
			c := legacyControlName(v)
			rows[a.action] = c
			text = append(text, a.action+"="+c)
			changed = changed || c != legacyPadDefault[a.action]
		}
		if !changed {
			continue
		}
		key := strings.Join(text, ",")
		l := byText[key]
		if l == nil {
			l = &layout{rows: rows}
			for n := 1; ; n++ {
				l.id, l.name = "oldlayout", "Old layout"
				if n > 1 {
					l.id, l.name = fmt.Sprintf("oldlayout%d", n), fmt.Sprintf("Old layout %d", n)
				}
				if !used[l.id] && !used["name:"+strings.ToLower(l.name)] {
					break
				}
			}
			used[l.id], used["name:"+strings.ToLower(l.name)] = true, true
			byText[key] = l
			layouts = append(layouts, l)
		}
		if dev != "" {
			setInput("ButtonSetup."+dev, l.id)
			log = append(log, fmt.Sprintf("[%s] -> [ButtonSetup_%s] (%s), ButtonSetup.%s", s.sec.Name(), l.id, l.name, dev))
		} else {
			log = append(log, fmt.Sprintf("[%s] -> [ButtonSetup_%s] (%s)", s.sec.Name(), l.id, l.name))
		}
	}
	if len(layouts) > 0 {
		// The shipped setups first: once the user file holds a setup, every
		// setup it leaves out is dropped.
		var standard *ini.Section
		if defaults != nil {
			for _, sec := range defaults.Sections() {
				id := iniSectionID(sec)
				if !strings.HasPrefix(id, "buttonsetup_") {
					continue
				}
				if id == "buttonsetup_standard" {
					standard = sec
				}
				if iniSection(user, id) == nil {
					iniCopySection(user, sec.Name(), sec)
				}
			}
		}
		for _, l := range layouts {
			sec, err := user.NewSection("ButtonSetup_" + l.id)
			if err != nil {
				continue
			}
			sec.Key("Name").SetValue(l.name)
			single := map[string]bool{}
			for _, a := range legacySlotControls {
				sec.Key(a.action).SetValue(l.rows[a.action])
				single[strings.ToLower(l.rows[a.action])] = true
			}
			// Combination rows as the Standard setup has them, unless this
			// layout puts a single button on that control.
			if standard != nil {
				for _, k := range standard.Keys() {
					name := strings.ToLower(k.Name())
					if name == "name" || setupButtonMask(name) == 0 {
						continue
					}
					if _, isSingle := legacyPadDefault[name]; isSingle {
						continue
					}
					v, _ := iniFirstValue(k)
					v = strings.TrimSpace(v)
					if single[strings.ToLower(v)] {
						v = "-"
					}
					sec.Key(k.Name()).SetValue(v)
				}
			}
		}
	}
	for _, s := range append(keys, pads...) {
		user.DeleteSection(s.sec.Name())
	}
	log = append(log, fmt.Sprintf("removed %d [Keys_Pn] / [Joystick_Pn] section(s)", len(keys)+len(pads)))
	return log
}

// A rumble setting migrateLegacySlots kept for pad slot n (PendingRumble.p<n>)
// goes to the first pad attached in that slot, unless that pad has a rumble
// setting of its own; either way the slot's note is then dropped.
func (c *Config) takePendingRumble(device *InputDevice) {
	if c == nil || device == nil || device.Joy < 0 || len(c.Input.PendingRumble) == 0 {
		return
	}
	slot := fmt.Sprintf("p%d", device.Joy+1)
	value, ok := c.Input.PendingRumble[slot]
	if !ok {
		return
	}
	key := device.preferenceKey()
	if key == "" {
		return
	}
	if c.devicePreference(device, "ControllerRumble", c.Input.ControllerRumble) == "" {
		if err := c.rememberPreference("ControllerRumble", key, strings.TrimSpace(value), ""); err == nil {
			LogMessage("Config: PendingRumble.%s -> ControllerRumble.%s = %s", slot, key, strings.TrimSpace(value))
		}
	} else {
		LogMessage("Config: PendingRumble.%s dropped: ControllerRumble is set for %s", slot, key)
	}
	delete(c.Input.PendingRumble, slot)
	name := "PendingRumble." + slot
	if c.IniFile != nil {
		iniDeleteKeyFold(iniSection(c.IniFile, "input"), name)
	}
	disk, _, err := LoadINIFile(c.Def, ini.LoadOptions{SkipUnrecognizableLines: true})
	if err == nil && iniDeleteKeyFold(iniSection(disk, "input"), name) {
		SaveINI(disk, c.Def)
	}
}

// Removes every spelling of a key from a section; reports whether one was there.
func iniDeleteKeyFold(sec *ini.Section, name string) bool {
	if sec == nil {
		return false
	}
	found := false
	for _, k := range sec.Keys() {
		if strings.EqualFold(k.Name(), name) {
			sec.DeleteKey(k.Name())
			found = true
		}
	}
	return found
}
