package main

import (
	"maps"
	"strings"
	"testing"

	"gopkg.in/ini.v1"
)

// The input layer in the desync checksum (rollback.go LiveChecksum): both
// peers must get the same number for the same Button Setups, whatever
// order the map is walked in, and a setup edit (the pause menu's Controller
// Setup, saveSessionSetup) must move it.
func TestSessionSetupsHash(t *testing.T) {
	old := sys.sessionSetups
	defer func() { sys.sessionSetups = old }()

	sys.sessionSetups = nil
	if h := sessionSetupsHash(); h != 0 {
		t.Fatalf("no session: hash %08x, want 0", h)
	}

	std := &ButtonSetupProperties{Name: "Standard", Buttons: map[string]string{
		"up": "DP_U", "down": "DP_D", "left": "DP_L", "right": "DP_R",
		"a": "A", "b": "B", "c": "RT", "x": "X", "y": "Y", "z": "RB", "start": "START",
	}}
	sys.sessionSetups = map[int]SessionSetup{0: newSessionSetup(std), 1: newSessionSetup(std)}
	h1 := sessionSetupsHash()
	for i := 0; i < 20; i++ {
		// a fresh map each time: Go walks maps in a different order
		sys.sessionSetups = maps.Clone(sys.sessionSetups)
		if h := sessionSetupsHash(); h != h1 {
			t.Fatalf("same setups, hash %08x then %08x", h1, h)
		}
	}

	// the guest swaps a and b in its pause menu
	swapped := *std
	swapped.Buttons = maps.Clone(std.Buttons)
	swapped.Buttons["a"], swapped.Buttons["b"] = "B", "A"
	sys.sessionSetups = maps.Clone(sys.sessionSetups)
	sys.sessionSetups[1] = newSessionSetup(&swapped)
	if h := sessionSetupsHash(); h == h1 {
		t.Fatalf("a setup edit did not move the hash (%08x)", h)
	}

	// the same edit on the other channel is a different session
	h2 := sessionSetupsHash()
	sys.sessionSetups = map[int]SessionSetup{0: newSessionSetup(&swapped), 1: newSessionSetup(std)}
	if h := sessionSetupsHash(); h == h2 {
		t.Fatalf("the edit on channel 0 hashes like the edit on channel 1 (%08x)", h)
	}
}

func TestProjectedMask(t *testing.T) {
	var b [14]bool
	if projectedMask(b) != 0 {
		t.Fatal("no button: mask not 0")
	}
	b[0], b[4], b[13] = true, true, true // up, a, menu
	if m := projectedMask(b); m != 1|1<<4|1<<13 {
		t.Fatalf("mask %04x", m)
	}
}

// matchGone ends the match the way a disconnect does (the rollback loop then
// breaks), with no warning box: the page shows its own notice.
func TestMatchGoneEndsTheMatch(t *testing.T) {
	oldEnd, oldWarn := sys.endMatch, sys.sessionWarning
	defer func() { sys.endMatch, sys.sessionWarning = oldEnd, oldWarn }()
	sys.endMatch, sys.sessionWarning = false, ""
	r := &RollbackSession{}
	r.matchGone()
	if !sys.endMatch {
		t.Fatal("matchGone did not end the match")
	}
	if sys.sessionWarning != "" {
		t.Fatalf("matchGone set a warning: %q", sys.sessionWarning)
	}
}

// A netplay handshake sends participant 0's Button Setup (netplay.go,
// the host and guest payloads); with none, it sends setup=null and that
// player's buttons project to nothing on every peer. The shipped config of
// this platform (the browser's keyboards included, platformDefaultConfig)
// must therefore carry a setup that any device falls back to, and every
// device preference it ships must name a setup it has.
func TestShippedConfigHasAButtonSetup(t *testing.T) {
	f, err := LoadINIText(platformDefaultConfig(string(defaultConfig)), ini.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var setups []string
	for _, sec := range f.Sections() {
		if strings.HasPrefix(strings.ToLower(sec.Name()), "buttonsetup_") {
			setups = append(setups, strings.ToLower(strings.TrimPrefix(strings.ToLower(sec.Name()), "buttonsetup_")))
		}
	}
	if len(setups) == 0 {
		t.Fatal("the shipped config has no [ButtonSetup_*] section")
	}
	ic := InputConfig{setups: map[string]ButtonSetup{}}
	for _, id := range setups {
		var p ButtonSetupProperties
		p.read(iniSection(f, "buttonsetup_"+id))
		ic.setups[id] = p.setup()
		ic.setupOrder = append(ic.setupOrder, id)
	}
	// a device with no preference, or one naming a setup that is gone
	for _, pref := range []string{"", "nosuchsetup"} {
		if id, s := ic.setup(pref); id == "" || s == nil || len(s[4]) == 0 {
			t.Fatalf("setup(%q) = %q: no setup with an A button to fall back to", pref, id)
		}
	}
	for _, sec := range f.Sections() {
		for _, k := range sec.Keys() {
			if strings.HasPrefix(strings.ToLower(k.Name()), "buttonsetup.") {
				if _, ok := ic.setups[strings.ToLower(k.String())]; !ok {
					t.Errorf("[%s] %s = %s names no shipped setup", sec.Name(), k.Name(), k.String())
				}
			}
		}
	}
}
