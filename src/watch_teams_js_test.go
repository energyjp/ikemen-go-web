//go:build js

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"syscall/js"
	"testing"

	"github.com/ikemen-engine/ggpo/transport"
)

// WATCHING TEAM MATCHES (watch.go, 2026-11-24): Simul, Tag and Turns at 2-4 a
// side. The host offers a team match once both teams are whole; a Turns match
// is one watch match across its turns (one GGPO session each), and the
// watcher follows each turn by its number.

// teamChars fills the select list with long, realistic character paths (a
// 4v4 select must still fit the page's 64 KiB ctl limit).
func teamChars(n int) {
	sys.sel.charlist = nil
	for i := 0; i < n; i++ {
		d := fmt.Sprintf("chars/CvS2 REV2 Character Number %02d (Pro Edition)/CvS2_REV2_Character_Number_%02d_ProEdition.def", i, i)
		sys.sel.charlist = append(sys.sel.charlist, SelectChar{def: d})
	}
}

func setTeams(mode TeamMode, n int32) {
	sys.tmode = [2]TeamMode{mode, mode}
	if mode == TM_Turns {
		sys.numSimul, sys.numTurns = [2]int32{1, 1}, [2]int32{n, n}
	} else {
		sys.numSimul, sys.numTurns = [2]int32{n, n}, [2]int32{n, n}
	}
}

func fakeConns() []transport.Connection {
	return []transport.Connection{transport.NewWebRTC(js.Global().Get("Object").New(), webrtcPeerIPForWatch, 7550)}
}

// A team match is offered only once BOTH teams are whole. The select screen
// picks side 1's whole team before side 2's, so the old "a character on each
// side" test would offer it at side 2's first member, with a partial roster.
func TestWatchTeamOfferWaitsForTheWholeTeams(t *testing.T) {
	for _, mode := range []struct {
		name string
		tm   TeamMode
	}{{"simul", TM_Simul}, {"tag", TM_Tag}, {"turns", TM_Turns}} {
		for n := 2; n <= 4; n++ {
			t.Run(fmt.Sprintf("%s%d", mode.name, n), func(t *testing.T) {
				saveWatchHostState(t)
				f := installFakeWatchHost(t)
				teamChars(2 * n)
				setTeams(mode.tm, int32(n))
				sys.consecutiveWins = [2]int32{3, 0}
				// a realistic header: this engine's own strict and host settings
				strict, err := collectSyncSettings(&sys.cfg, syncStrict)
				if err != nil {
					t.Fatal(err)
				}
				hostSet, err := collectSyncSettings(&sys.cfg, syncHost)
				if err != nil {
					t.Fatal(err)
				}
				sys.netplayOverride.Strict, sys.netplayOverride.AppliedHost = strict, hostSet
				watchHostLoadStart("versusresults=0, pausemenu=0, winscreen=0, rounds=2, time=99")
				// side 1's whole team
				for i := 0; i < n; i++ {
					sys.sel.selected[0] = append(sys.sel.selected[0], [2]int{i, i + 1})
					watchHostSelected(fmt.Sprintf("p%d.life=1000, p%d.power=3000, p%d.dizzypoints=1000", 2*i+1, 2*i+1, 2*i+1))
				}
				// side 2's members one by one: no offer until the last
				for i := 0; i < n; i++ {
					if len(f.offers) != 0 {
						t.Fatalf("offered with side 2 at %d of %d: %v", i, n, f.offers[0].NumChars)
					}
					sys.sel.selected[1] = append(sys.sel.selected[1], [2]int{n + i, i + 5})
					watchHostSelected(fmt.Sprintf("p%d.life=1000, p%d.power=3000", 2*i+2, 2*i+2))
				}
				if len(f.offers) != 1 {
					t.Fatalf("%d offers once both teams were whole, want 1", len(f.offers))
				}
				m := f.offers[0]
				if m.T != "select" {
					t.Fatalf("a %s %d match is a %s: %s", mode.name, n, m.T, m.Why)
				}
				if m.NumChars != [2]int32{int32(n), int32(n)} || m.TeamMode != [2]int32{int32(mode.tm), int32(mode.tm)} {
					t.Fatalf("numChars %v teamMode %v", m.NumChars, m.TeamMode)
				}
				if len(m.Chars) != 2*n {
					t.Fatalf("%d chars, want %d", len(m.Chars), 2*n)
				}
				for i, c := range m.Chars {
					side, member, pal := 1, i, i+1
					if i >= n {
						side, member = 2, i-n
						pal = member + 5
					}
					if c.Side != side || c.Def != sys.sel.charlist[i].def || c.Pal != pal {
						t.Fatalf("char %d: %+v, want side %d %s pal %d", i, c, side, sys.sel.charlist[i].def, pal)
					}
				}
				if m.ConsecutiveWins != [2]int32{3, 0} {
					t.Fatalf("consecutive wins %v", m.ConsecutiveWins)
				}
				last := fmt.Sprintf("p%d.life", 2*n)
				if !strings.Contains(m.Params, last) {
					t.Fatalf("the last member's override params are missing: %q", m.Params)
				}
				raw := mustJSON(&m)
				if len(raw) >= 64*1024 {
					t.Fatalf("the select is %d bytes, over the page's 64 KiB ctl limit", len(raw))
				}
				if n == 4 {
					t.Logf("%s4 select: %d bytes", mode.name, len(raw))
				}
				// it is offered once
				watchHostSelected("")
				if len(f.offers) != 1 {
					t.Fatal("offered twice")
				}
				// and its watchers are registered when the session is built
				var rs RollbackSession
				rs.watchTransport(fakeConns())
				if len(f.starts) != 1 || f.starts[0] != m.Match {
					t.Fatalf("watchers registered for %v, want [%d]", f.starts, m.Match)
				}
				rs.watchEnd()
			})
		}
	}
}

// A team match forced out (its session is built) while side 2's team is still
// being picked is a nowatch: never a partial roster, and nobody registered.
func TestWatchTeamForcedOfferIncompleteIsNowatch(t *testing.T) {
	saveWatchHostState(t)
	f := installFakeWatchHost(t)
	teamChars(6)
	setTeams(TM_Simul, 3)
	sys.sel.selected[0] = [][2]int{{0, 1}, {1, 1}, {2, 1}}
	sys.sel.selected[1] = [][2]int{{3, 1}}
	watchHostLoadStart("")
	if len(f.offers) != 0 {
		t.Fatal("offered with side 2 at 1 of 3")
	}
	var rs RollbackSession
	rs.watchTransport(fakeConns())
	if len(f.offers) != 1 || f.offers[0].T != "nowatch" {
		t.Fatalf("offers %+v, want the forced offer of a partial team to be a nowatch", f.offers)
	}
	if len(f.starts) != 0 {
		t.Fatalf("watchers registered for a partial team: %v", f.starts)
	}
}

// Turns: one watch match across its turns. The host builds a new GGPO session
// for every turn; the watchers of the match's select are registered with each
// (the page is asked again for the SAME match), and no new select goes out.
// A rematch (watchEnd with no next turn) is a new match, offered again.
func TestWatchTurnsKeepsOneMatch(t *testing.T) {
	saveWatchHostState(t)
	f := installFakeWatchHost(t)
	teamChars(4)
	setTeams(TM_Turns, 2)
	sys.sel.selected[0] = [][2]int{{0, 1}, {1, 1}}
	sys.sel.selected[1] = [][2]int{{2, 1}, {3, 2}}
	watchHostLoadStart("")
	if len(f.offers) != 1 || f.offers[0].T != "select" {
		t.Fatalf("offers %+v", f.offers)
	}
	m := f.offers[0].Match
	conns := fakeConns()
	var rs RollbackSession
	rs.watchTransport(conns) // turn 0
	if len(f.starts) != 1 || f.starts[0] != m || watchHost.turn != 0 {
		t.Fatalf("turn 0: starts %v turn %d", f.starts, watchHost.turn)
	}
	rs.watchEnd()
	watchHostNextTurn()
	rs.watchTransport(conns) // turn 1
	if len(f.offers) != 1 {
		t.Fatalf("a Turns turn was offered as a new match: %+v", f.offers[1:])
	}
	if len(f.starts) != 2 || f.starts[1] != m || watchHost.turn != 1 || watchHostMatch != m {
		t.Fatalf("turn 1: starts %v (want [%d %d]) turn %d match %d", f.starts, m, m, watchHost.turn, watchHostMatch)
	}
	rs.watchEnd()
	watchHostNextTurn()
	rs.watchTransport(conns) // turn 2 (a third turn after 1-1)
	if len(f.offers) != 1 || len(f.starts) != 3 || f.starts[2] != m || watchHost.turn != 2 {
		t.Fatalf("turn 2: offers %d starts %v turn %d", len(f.offers), f.starts, watchHost.turn)
	}
	// the match is over; a rematch reuses what is loaded and is a new match
	rs.watchEnd()
	rs.watchTransport(conns)
	if len(f.offers) != 2 || f.offers[1].T != "select" || f.offers[1].Match != m+1 || watchHost.turn != 0 {
		t.Fatalf("the rematch: offers %+v turn %d", f.offers, watchHost.turn)
	}
	if len(f.starts) != 4 || f.starts[3] != m+1 {
		t.Fatalf("rematch watchers registered for %v", f.starts)
	}
	rs.watchEnd()
}

// A Turns match that could not be watched stays unwatched on every turn: the
// next turn neither offers it again nor registers anybody.
func TestWatchTurnsAfterNowatch(t *testing.T) {
	saveWatchHostState(t)
	f := installFakeWatchHost(t)
	teamChars(4)
	setTeams(TM_Turns, 2)
	sys.sel.selected[0] = [][2]int{{0, 1}, {1, 1}}
	sys.sel.selected[1] = [][2]int{{2, 1}, {3, 2}}
	sys.sel.selectedStageNo = 0 // undescribable: no stage
	watchHostLoadStart("")
	conns := fakeConns()
	var rs RollbackSession
	rs.watchTransport(conns)
	if len(f.offers) != 1 || f.offers[0].T != "nowatch" || len(f.starts) != 0 {
		t.Fatalf("turn 0: offers %+v starts %v", f.offers, f.starts)
	}
	rs.watchEnd()
	watchHostNextTurn()
	rs.watchTransport(conns)
	if len(f.offers) != 1 || len(f.starts) != 0 {
		t.Fatalf("turn 1 after a nowatch: offers %+v starts %v", f.offers, f.starts)
	}
	rs.watchEnd()
}

// The next-turn hook routes by role: a guest does nothing; the host carries
// its watchers on to the next turn.
func TestWatchNextTurnHostRoute(t *testing.T) {
	saveWatchHostState(t)
	installFakeWatchHost(t)
	oldNC, oldMatch := sys.netConnection, watchHostMatch
	defer func() { sys.netConnection, watchHostMatch = oldNC, oldMatch }()
	watchHostMatch, watchHost.selMatch, watchHost.turn = 7, 7, 0
	sys.netConnection = &NetConnection{host: false}
	if !watchNextTurn(false) || watchHost.turn != 0 || watchHost.offered {
		t.Fatalf("a guest moved the host's offer: %+v", watchHost)
	}
	sys.netConnection = &NetConnection{host: true}
	if !watchNextTurn(false) || watchHost.turn != 1 || !watchHost.offered || !watchHost.ok {
		t.Fatalf("the host did not carry its watchers on: %+v", watchHost)
	}
}

// ---- the watcher side ----

// fakeWatcher is the watcher page's bridge: next() hands out the queued host
// messages, then "" (nothing yet; null would mean the host is gone).
type fakeWatcher struct{ q []string }

func installFakeWatcher(t *testing.T) *fakeWatcher {
	f := &fakeWatcher{}
	b := js.Global().Get("Object").New()
	b.Set("role", "watcher")
	b.Set("next", js.FuncOf(func(js.Value, []js.Value) any {
		if len(f.q) == 0 {
			return ""
		}
		s := f.q[0]
		f.q = f.q[1:]
		return s
	}))
	b.Set("log", js.FuncOf(func(js.Value, []js.Value) any { return nil }))
	js.Global().Set("ikemenWatch", b)
	oldSession, oldWatch := sys.rollback.session, sys.watch
	oldMenus, oldSetups, oldSeed, oldPm := sys.menuChannels, sys.sessionSetups, sys.randseed, sys.preMatchTime
	t.Cleanup(func() {
		js.Global().Delete("ikemenWatch")
		sys.rollback.session, sys.watch = oldSession, oldWatch
		sys.menuChannels, sys.sessionSetups, sys.preMatchTime = oldMenus, oldSetups, oldPm
		Srand(oldSeed)
	})
	return f
}

func (f *fakeWatcher) send(m watchMsg) { f.q = append(f.q, mustJSON(&m)) }

// A start, end or drop is keyed by its turn: one for this turn now, one for a
// later turn kept for it, one for an earlier turn ignored.
func TestWatchTakeKeysByTurn(t *testing.T) {
	f := installFakeWatcher(t)
	sys.watch = watchState{sel: &watchMsg{T: "select", Match: 3}, loading: 3}
	w := &sys.watch
	f.send(watchMsg{T: "start", Match: 3, Seed: 10})
	f.send(watchMsg{T: "start", Match: 2, Turn: 0, Seed: 99}) // another match
	w.poll()
	if w.start == nil || w.start.Seed != 10 || w.early != nil {
		t.Fatalf("turn 0's start: %+v early %+v", w.start, w.early)
	}
	// the host's next turn starts while this watcher still runs turn 0
	f.send(watchMsg{T: "start", Match: 3, Turn: 1, Seed: 11})
	w.poll()
	if w.start.Seed != 10 || w.early == nil || w.early.Seed != 11 {
		t.Fatalf("the next turn's start replaced this one's: start %+v early %+v", w.start, w.early)
	}
	// turn 1's end and a drop for turn 1 wait; turn 0's end is taken
	f.send(watchMsg{T: "end", Match: 3, Turn: 1, Last: 500})
	f.send(watchMsg{T: "end", Match: 3, Turn: 0, Last: 300, Tail: []watchTail{{F: 300, B: []byte{1, 2}}}})
	w.poll()
	if w.end == nil || w.end.Last != 300 || len(w.tail) != 1 || w.endNext == nil || w.endNext.Last != 500 {
		t.Fatalf("ends: this %+v next %+v", w.end, w.endNext)
	}
	f.send(watchMsg{T: "drop", Match: 3, Turn: 1, Why: "fell behind"})
	w.poll()
	if w.dropped != "" || w.dropNext != "fell behind" {
		t.Fatalf("a drop for turn 1 ended turn 0: dropped %q next %q", w.dropped, w.dropNext)
	}
}

// The next turn: a new spectator session, with the start (and end) the host
// already sent for that turn; then stale messages of the turn before are
// ignored.
func TestWatchNextTurnRearms(t *testing.T) {
	f := installFakeWatcher(t)
	sys.watch = watchState{sel: &watchMsg{T: "select", Match: 3}, loading: 3}
	w := &sys.watch
	f.send(watchMsg{T: "start", Match: 3, Seed: 10})
	f.send(watchMsg{T: "start", Match: 3, Turn: 1, Seed: 11})
	w.poll()
	sys.rollback.session = nil // postMatchSetup of turn 0
	if !watchNextTurn(true) {
		t.Fatal("the watcher did not follow the match to turn 1")
	}
	if w.turn != 1 || w.start == nil || w.start.Seed != 11 || w.end != nil || w.early != nil {
		t.Fatalf("turn %d start %+v end %+v early %+v", w.turn, w.start, w.end, w.early)
	}
	if sys.rollback.session == nil || !sys.rollback.session.spectating {
		t.Fatal("no spectator session for turn 1")
	}
	// turn 0's late end is stale; turn 1's end and turn 2's start arrive
	f.send(watchMsg{T: "end", Match: 3, Turn: 0, Last: 300})
	f.send(watchMsg{T: "end", Match: 3, Turn: 1, Last: 410})
	f.send(watchMsg{T: "start", Match: 3, Turn: 2, Seed: 12})
	w.poll()
	if w.end == nil || w.end.Last != 410 || w.early == nil || w.early.Seed != 12 {
		t.Fatalf("turn 1: end %+v early %+v", w.end, w.early)
	}
	// the synchronize of turn 2 takes the early start without waiting
	sys.rollback.session = nil
	if !watchNextTurn(true) || w.turn != 2 || w.start == nil || w.start.Seed != 12 {
		t.Fatalf("turn 2: turn %d start %+v", w.turn, w.start)
	}
	if err := w.synchronize(); err != nil || !w.running || sys.randseed != 12 {
		t.Fatalf("turn 2 synchronize: %v running %v seed %d", err, w.running, sys.randseed)
	}
	// a new select (a rematch) starts again at turn 0
	w.running = false
	f.send(watchMsg{T: "select", Match: 4})
	f.send(watchMsg{T: "start", Match: 4, Seed: 20})
	w.poll()
	if w.turn != 0 || w.start == nil || w.start.Seed != 20 || w.sel.Match != 4 {
		t.Fatalf("the rematch's turn-0 start was not taken: turn %d start %+v", w.turn, w.start)
	}
}

// A watcher that cannot follow the match on ends it after this turn, with no
// session left (it would read no local input until the next match).
func TestWatchNextTurnRefusesAfterDropOrGone(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(w *watchState, f *fakeWatcher)
	}{
		{"dropped", func(w *watchState, f *fakeWatcher) { w.dropped = "fell behind" }},
		{"dropped from the next turn", func(w *watchState, f *fakeWatcher) {
			f.send(watchMsg{T: "drop", Match: 3, Turn: 1, Why: "the host could not add this watcher"})
		}},
		{"left", func(w *watchState, f *fakeWatcher) { w.left = true }},
		{"gone", func(w *watchState, f *fakeWatcher) {
			js.Global().Get("ikemenWatch").Set("next", js.FuncOf(func(js.Value, []js.Value) any { return js.Null() }))
		}},
		{"another match", func(w *watchState, f *fakeWatcher) { f.send(watchMsg{T: "select", Match: 4}) }},
		{"a nowatch", func(w *watchState, f *fakeWatcher) { f.send(watchMsg{T: "nowatch", Match: 4, Why: "x"}) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := installFakeWatcher(t)
			sys.watch = watchState{sel: &watchMsg{T: "select", Match: 3}, loading: 3}
			c.set(&sys.watch, f)
			sys.rollback.session = nil
			if watchNextTurn(true) {
				t.Fatal("followed the match on")
			}
			if sys.rollback.session != nil {
				t.Fatal("a spectator session was left with no match to run")
			}
		})
	}
}

// THE WATCHED GATE: a lobby member who watched earlier matches keeps
// sys.watch.active set. When it PLAYS a Turns match, the next-turn hook must
// not turn its session into a spectator one.
func TestWatchNextTurnGate(t *testing.T) {
	installFakeWatcher(t)
	oldNC := sys.netConnection
	defer func() { sys.netConnection = oldNC }()
	sys.watch = watchState{active: true, sel: &watchMsg{T: "select", Match: 3}, loading: 3}
	player := NewRollbackSession(sys.cfg.Netplay.Rollback)
	sys.rollback.session = &player
	sys.netConnection = &NetConnection{host: false}
	if !watchNextTurn(false) {
		t.Fatal("a player's Turns match was ended")
	}
	if sys.rollback.session != &player || player.spectating || sys.watch.turn != 0 {
		t.Fatal("a player's session was replaced or made a spectator")
	}
}

// The ConsecutiveWins counters as the match began reach the watcher.
func TestWatchApplyCarriesConsecutiveWins(t *testing.T) {
	installFakeWatcher(t)
	cw, gm, mw, wins, md := sys.consecutiveWins, sys.gameMode, sys.matchWins, sys.wins, sys.maxDraws
	fpc, rt, mn, home, ai, remap := sys.curFramesPerCount, sys.maxRoundTime, sys.matchNo, sys.home, sys.aiLevel, sys.inputRemap
	defer func() {
		sys.consecutiveWins, sys.gameMode, sys.matchWins, sys.wins, sys.maxDraws = cw, gm, mw, wins, md
		sys.curFramesPerCount, sys.maxRoundTime, sys.matchNo, sys.home, sys.aiLevel, sys.inputRemap = fpc, rt, mn, home, ai, remap
	}()
	sys.consecutiveWins = [2]int32{0, 5} // what this watcher had from matches it watched
	var m watchMsg
	if err := json.Unmarshal([]byte(mustJSON(&watchMsg{T: "select", Match: 1, ConsecutiveWins: [2]int32{2, 0},
		MatchWins: [2]int32{2, 2}, InputRemap: [MaxPlayerNo]int{0, 1, 0, 1}})), &m); err != nil {
		t.Fatal(err)
	}
	sys.watch.applyRules(&m)
	if sys.consecutiveWins != [2]int32{2, 0} {
		t.Fatalf("consecutive wins %v, want the host's [2 0]", sys.consecutiveWins)
	}
}

// The fight screen's win counters ("WINS 0") as the match began reach the
// watcher: the host's, never the watcher's own from matches it watched.
func TestWatchCarriesWinCounts(t *testing.T) {
	saveWatchHostState(t)
	wcs := sys.fightScreen.winCounts
	defer func() { sys.fightScreen.winCounts = wcs }()
	sys.fightScreen.winCounts = [2]*FightScreenWinCount{newFightScreenWinCount(), newFightScreenWinCount()}
	f := installFakeWatchHost(t)
	teamChars(2)
	setTeams(TM_Single, 1)
	strict, err := collectSyncSettings(&sys.cfg, syncStrict)
	if err != nil {
		t.Fatal(err)
	}
	hostSet, err := collectSyncSettings(&sys.cfg, syncHost)
	if err != nil {
		t.Fatal(err)
	}
	sys.netplayOverride.Strict, sys.netplayOverride.AppliedHost = strict, hostSet
	sys.fightScreen.winCounts[0].wins, sys.fightScreen.winCounts[1].wins = 2, 0
	watchHostLoadStart("versusresults=0, pausemenu=0, winscreen=0, rounds=2, time=99")
	sys.sel.selected[0] = append(sys.sel.selected[0], [2]int{0, 1})
	watchHostSelected("")
	sys.sel.selected[1] = append(sys.sel.selected[1], [2]int{1, 1})
	watchHostSelected("")
	if len(f.offers) != 1 || f.offers[0].T != "select" {
		t.Fatalf("%d offers, want one select", len(f.offers))
	}
	var m watchMsg
	if err := json.Unmarshal([]byte(mustJSON(&f.offers[0])), &m); err != nil {
		t.Fatal(err)
	}
	if m.WinCounts != [2]int32{2, 0} {
		t.Fatalf("the select carries win counts %v, want the host's [2 0]", m.WinCounts)
	}
	// the watcher: what it counted itself is replaced by the host's
	installFakeWatcher(t)
	cw, gm, mw, wins, md := sys.consecutiveWins, sys.gameMode, sys.matchWins, sys.wins, sys.maxDraws
	fpc, rt, mn, home, ai, remap := sys.curFramesPerCount, sys.maxRoundTime, sys.matchNo, sys.home, sys.aiLevel, sys.inputRemap
	defer func() {
		sys.consecutiveWins, sys.gameMode, sys.matchWins, sys.wins, sys.maxDraws = cw, gm, mw, wins, md
		sys.curFramesPerCount, sys.maxRoundTime, sys.matchNo, sys.home, sys.aiLevel, sys.inputRemap = fpc, rt, mn, home, ai, remap
	}()
	sys.fightScreen.winCounts[0].wins, sys.fightScreen.winCounts[1].wins = 0, 5
	sys.watch.applyRules(&m)
	if got := [2]int32{sys.fightScreen.winCounts[0].wins, sys.fightScreen.winCounts[1].wins}; got != [2]int32{2, 0} {
		t.Fatalf("the watcher's win counts %v, want the host's [2 0]", got)
	}
}
