//go:build js

package main

import (
	"encoding/json"
	"strings"
	"syscall/js"
	"testing"

	"github.com/ikemen-engine/ggpo/transport"
)

// The host's offer to watchers (watch.go watchHost). With background loading
// (Config.VsScreenLoading, on by default) the Lua calls loadStart BEFORE it
// selects the characters, so an offer made in loadStart described an empty
// match ("nowatch") and nobody could watch. The offer now waits for the
// selection; a match that starts with no offer for it (a rematch that reuses
// what is loaded) is offered as its session is built; the watchers of a
// nowatch are never registered.
type fakeWatchHost struct {
	offers []watchMsg
	starts []int
}

func installFakeWatchHost(t *testing.T) *fakeWatchHost {
	f := &fakeWatchHost{}
	b := js.Global().Get("Object").New()
	b.Set("role", "host")
	b.Set("offer", js.FuncOf(func(_ js.Value, a []js.Value) any {
		var m watchMsg
		if err := json.Unmarshal([]byte(a[1].String()), &m); err != nil {
			t.Errorf("unreadable offer: %v", err)
		}
		if m.Match != a[0].Int() {
			t.Errorf("offer %d carries match %d", a[0].Int(), m.Match)
		}
		f.offers = append(f.offers, m)
		return nil
	}))
	b.Set("start", js.FuncOf(func(_ js.Value, a []js.Value) any {
		f.starts = append(f.starts, a[0].Int())
		return js.Global().Get("Array").New()
	}))
	b.Set("log", js.FuncOf(func(js.Value, []js.Value) any { return nil }))
	js.Global().Set("ikemenWatch", b)
	return f
}

func saveWatchHostState(t *testing.T) {
	sel := struct {
		chars    []SelectChar
		stages   []SelectStage
		selected [2][][2]int
		stageNo  int
	}{sys.sel.charlist, sys.sel.stagelist, sys.sel.selected, sys.sel.selectedStageNo}
	tmode, rb, ov, host, match := sys.tmode, sys.cfg.Netplay.RollbackNetcode, sys.netplayOverride, watchHost, watchHostMatch
	setups := sys.sessionSetups
	numSimul, numTurns, cwins := sys.numSimul, sys.numTurns, sys.consecutiveWins
	gameMode := sys.gameMode
	t.Cleanup(func() {
		js.Global().Delete("ikemenWatch")
		sys.sel.charlist, sys.sel.stagelist, sys.sel.selected, sys.sel.selectedStageNo = sel.chars, sel.stages, sel.selected, sel.stageNo
		sys.tmode, sys.cfg.Netplay.RollbackNetcode, sys.netplayOverride, watchHost, watchHostMatch = tmode, rb, ov, host, match
		sys.sessionSetups = setups
		sys.numSimul, sys.numTurns, sys.consecutiveWins = numSimul, numTurns, cwins
		sys.gameMode = gameMode
	})
	sys.sel.charlist = []SelectChar{{def: "chars/kfm/kfm.def"}, {def: "chars/ryudx/RyuDX.def"}}
	sys.sel.stagelist = []SelectStage{{def: "stages/kfm.def"}}
	sys.sel.selected = [2][][2]int{}
	sys.sel.selectedStageNo = 1
	sys.tmode = [2]TeamMode{TM_Single, TM_Single}
	sys.numSimul, sys.numTurns = [2]int32{1, 1}, [2]int32{1, 1}
	sys.consecutiveWins = [2]int32{}
	sys.gameMode = "netplayversus"
	sys.cfg.Netplay.RollbackNetcode = true
	sys.netplayOverride = SessionConfigOverride{Active: true, Source: "netplay", SyncVersion: syncConfigVersion}
	sys.sessionSetups = nil
	watchHost.pending, watchHost.offered, watchHost.ok = false, false, false
	watchHost.turn, watchHost.selMatch = 0, 0
}

func TestWatchOfferWaitsForTheSelection(t *testing.T) {
	saveWatchHostState(t)
	f := installFakeWatchHost(t)

	// background loading: loadStart first, then the characters
	watchHostLoadStart("versusresults=1, pausemenu=1")
	if len(f.offers) != 0 {
		t.Fatalf("offered before the characters were selected: %+v", f.offers[0])
	}
	sys.sel.selected[0] = append(sys.sel.selected[0], [2]int{0, 1})
	watchHostSelected("p1.1.life=500")
	if len(f.offers) != 0 {
		t.Fatal("offered with one side selected")
	}
	sys.sel.selected[1] = append(sys.sel.selected[1], [2]int{1, 3})
	watchHostSelected("")
	if len(f.offers) != 1 {
		t.Fatalf("%d offers once both sides were selected, want 1", len(f.offers))
	}
	m := f.offers[0]
	if m.T != "select" || len(m.Chars) != 2 || m.Stage != "stages/kfm.def" || m.Header == nil {
		t.Fatalf("not a full select: %+v", m)
	}
	if m.Chars[1].Def != "chars/ryudx/RyuDX.def" || m.Chars[1].Side != 2 || m.Chars[1].Pal != 3 {
		t.Fatalf("side 2: %+v", m.Chars[1])
	}
	if !strings.Contains(m.Params, "versusresults=1") || !strings.Contains(m.Params, "p1.1.life=500") {
		t.Fatalf("params %q lack loadStart's or the character's override params", m.Params)
	}
	// later selections of the same load add nothing
	watchHostSelected("")
	if len(f.offers) != 1 {
		t.Fatal("offered twice")
	}

	// the match starts: its watchers are registered (the page is asked)
	var rs RollbackSession
	conns := []transport.Connection{transport.NewWebRTC(js.Global().Get("Object").New(), webrtcPeerIPForWatch, 7550)}
	rs.watchTransport(conns)
	if len(f.starts) != 1 || f.starts[0] != m.Match {
		t.Fatalf("watchers registered for %v, want [%d]", f.starts, m.Match)
	}

	// it ends; a rematch reuses what is loaded, with no loadStart: it is
	// offered as its session is built, then its watchers registered
	rs.watchEnd()
	rs.watchTransport(conns)
	if len(f.offers) != 2 || f.offers[1].T != "select" || f.offers[1].Match != m.Match+1 {
		t.Fatalf("the rematch was not offered: %+v", f.offers)
	}
	if len(f.starts) != 2 || f.starts[1] != m.Match+1 {
		t.Fatalf("rematch watchers registered for %v", f.starts)
	}
	rs.watchEnd()
}

// Loading without background loading: the characters are selected before
// loadStart, so the offer goes at once (2.4's order).
func TestWatchOfferAtLoadStartWhenSelected(t *testing.T) {
	saveWatchHostState(t)
	f := installFakeWatchHost(t)
	sys.sel.selected[0] = [][2]int{{0, 1}}
	sys.sel.selected[1] = [][2]int{{0, 2}}
	watchHostLoadStart("")
	if len(f.offers) != 1 || f.offers[0].T != "select" {
		t.Fatalf("offers %+v", f.offers)
	}
}

// Delay netcode is refused at once, and a match after a nowatch registers no
// watcher (it would only drop it 480 frames in). Turns, Tag and Simul VERSUS
// matches are watched since 2026-11-24 (TestWatchTeamOfferWaitsForTheWholeTeams);
// the online co-op runs (both players on side 1, so always a team mode) stay
// refused as before, whole teams selected or not.
func TestWatchNowatchRegistersNobody(t *testing.T) {
	for _, c := range []struct {
		name     string
		tmode    TeamMode
		delay    bool
		gameMode string
		why      string
	}{
		{"delay", TM_Single, true, "netplayversus", "delay netcode"},
		{"delay-simul", TM_Simul, true, "netplayversus", "delay netcode"},
		{"arcade-coop-simul", TM_Simul, false, "netplayteamcoop", "co-op"},
		{"arcade-coop-tag", TM_Tag, false, "netplayteamcoop", "co-op"},
		{"arcade-coop-turns", TM_Turns, false, "netplayteamcoop", "co-op"},
		{"survival-coop-simul", TM_Simul, false, "netplaysurvivalcoop", "co-op"},
		{"survival-coop-turns", TM_Turns, false, "netplaysurvivalcoop", "co-op"},
	} {
		t.Run(c.name, func(t *testing.T) {
			saveWatchHostState(t)
			f := installFakeWatchHost(t)
			sys.tmode = [2]TeamMode{c.tmode, TM_Single}
			sys.cfg.Netplay.RollbackNetcode = !c.delay
			sys.gameMode = c.gameMode
			if c.tmode != TM_Single {
				// the whole teams: a co-op run is refused at once anyway
				sys.numSimul, sys.numTurns = [2]int32{2, 1}, [2]int32{2, 1}
				sys.sel.selected[0] = [][2]int{{0, 1}, {1, 1}}
				sys.sel.selected[1] = [][2]int{{0, 2}}
			}
			watchHostLoadStart("")
			if len(f.offers) != 1 || f.offers[0].T != "nowatch" || !strings.Contains(f.offers[0].Why, c.why) {
				t.Fatalf("offers %+v, want one nowatch saying %q", f.offers, c.why)
			}
			var rs RollbackSession
			rs.watchTransport([]transport.Connection{transport.NewWebRTC(js.Global().Get("Object").New(), webrtcPeerIPForWatch, 7550)})
			if len(f.starts) != 0 {
				t.Fatalf("watchers registered after a nowatch: %v", f.starts)
			}
		})
	}
}

// A select whose description failed (no stage) is a nowatch too, and its
// match registers nobody.
func TestWatchUndescribableMatchRegistersNobody(t *testing.T) {
	saveWatchHostState(t)
	f := installFakeWatchHost(t)
	sys.sel.selected[0] = [][2]int{{0, 1}}
	sys.sel.selected[1] = [][2]int{{0, 2}}
	sys.sel.selectedStageNo = 0
	watchHostLoadStart("")
	if len(f.offers) != 0 {
		t.Fatal("offered without a stage")
	}
	var rs RollbackSession
	rs.watchTransport([]transport.Connection{transport.NewWebRTC(js.Global().Get("Object").New(), webrtcPeerIPForWatch, 7550)})
	if len(f.offers) != 1 || f.offers[0].T != "nowatch" {
		t.Fatalf("offers %+v, want the forced offer to be a nowatch", f.offers)
	}
	if len(f.starts) != 0 {
		t.Fatalf("watchers registered for an undescribed match: %v", f.starts)
	}
}
