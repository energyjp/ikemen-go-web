package main

import (
	"encoding/json"
	"maps"
	"testing"
)

// The start message carries the players' Button Setups and menu channels
// as their match begins; the watcher's synchronize puts them in place, so its
// setups hash (in LiveChecksum, so in every frame of its trail) and its menu
// pointers equal the players'.
func TestWatchStartCarriesSetupsAndMenus(t *testing.T) {
	oldSetups, oldMenus, oldSession, oldSeed := sys.sessionSetups, sys.menuChannels, sys.rollback.session, sys.randseed
	defer func() {
		sys.sessionSetups, sys.menuChannels, sys.rollback.session = oldSetups, oldMenus, oldSession
		Srand(oldSeed)
	}()

	std := &ButtonSetupProperties{Name: "Standard", Buttons: map[string]string{
		"up": "DP_U", "down": "DP_D", "left": "DP_L", "right": "DP_R",
		"a": "A", "b": "B", "c": "RT", "x": "X", "y": "Y", "z": "RB", "start": "START",
	}}
	edited := *std
	edited.Buttons = maps.Clone(std.Buttons)
	edited.Buttons["a"], edited.Buttons["b"] = "B", ""

	// the host, as its match starts: the guest edited its setup earlier, and
	// both pointers went through some menus
	sys.sessionSetups = map[int]SessionSetup{0: newSessionSetup(std), 1: newSessionSetup(&edited)}
	sys.menuChannels = [MaxPlayerNo]MenuChannel{}
	sys.menuChannels[0].mouse.setContext("versus")
	sys.menuChannels[1].mouse.setContext("pause")
	sys.menuChannels[1].mouse.setContext("match")
	sys.menuChannels[1].mouse.received = sys.menuChannels[1].mouse.generation
	sys.menuChannels[1].escape = 2
	hostHash := sessionSetupsHash()
	hostMenus := sys.menuChannels
	raw := mustJSON(&watchMsg{T: "start", Match: 3, Seed: 1234, PmTime: 77, NetTime: 0,
		Setups: sys.replaySetups(), Menu: watchMenuSnapshot()})

	// the watcher: fresh state, the start message arrives
	sys.sessionSetups, sys.menuChannels = nil, [MaxPlayerNo]MenuChannel{}
	var m watchMsg
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	session := RollbackSession{spectating: true}
	sys.rollback.session = &session
	w := watchState{sel: &watchMsg{T: "select", Match: 3}, loading: 3, start: &m}
	if err := w.synchronize(); err != nil {
		t.Fatal(err)
	}
	if !w.running {
		t.Fatal("synchronize did not start the watched match")
	}
	if h := sessionSetupsHash(); h != hostHash {
		t.Fatalf("watcher setups hash %08x, host %08x", h, hostHash)
	}
	if s := sys.channelSetup(1); s == nil || len(s[4]) == 0 || s[4][0] != VC_B {
		t.Fatalf("channel 1 does not read A as the guest's edit says (B): %v", s)
	}
	for i := range hostMenus {
		h, g := &hostMenus[i], &sys.menuChannels[i]
		if h.escape != g.escape || h.mouse.context != g.mouse.context || h.mouse.generation != g.mouse.generation ||
			h.mouse.received != g.mouse.received || h.mouse.available() != g.mouse.available() {
			t.Fatalf("menu channel %d: host %+v, watcher %+v", i, *h, *g)
		}
	}
	// the players' next menu is the same context change on every engine
	before := sys.menuChannels[1].mouse.generation
	sys.menuChannels[1].mouse.setContext("match")
	if sys.menuChannels[1].mouse.generation != before {
		t.Fatal("the watcher counts a context change the players do not")
	}
	if sys.randseed != 1234 {
		t.Fatalf("seed %d, want the host's 1234", sys.randseed)
	}
}

// A select the watcher loaded is replaced while it waits for the start (the
// host went back from its versus screen and offered another match): the
// watcher must load the new one, never run the old characters with the new
// match's start.
func TestWatchSynchronizeRefusesAReplacedMatch(t *testing.T) {
	oldSession := sys.rollback.session
	defer func() { sys.rollback.session = oldSession }()
	session := RollbackSession{spectating: true}
	sys.rollback.session = &session
	w := watchState{sel: &watchMsg{T: "select", Match: 5}, loading: 4}
	if err := w.synchronize(); err == nil {
		t.Fatal("synchronize waited for a match the watcher did not load")
	}
	if w.running {
		t.Fatal("the replaced match was started")
	}
	if sys.rollback.session != nil {
		t.Fatal("a watcher with no match kept its spectator session (no local input until the next one)")
	}
}
