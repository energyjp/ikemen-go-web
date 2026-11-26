package main

// WATCHING A ROLLBACK MATCH (spectating), the engine half.
//
// A watcher is a third engine that runs the players' match from the host's
// CONFIRMED inputs: the host's GGPO session registers it as a spectator, and
// it simulates each frame once those inputs arrive, a few frames behind the
// players. It sends nothing that can reach the simulation, so it cannot desync
// the players; the GGPO library makes sure it cannot stall or crash them
// either (third_party/ggpo: SetSpectatorEndpoint).
//
// WHO SENDS WHAT, per match (all JSON over the watcher's own reliable "ctl"
// channel from the host's page, never through the lobby; the page relays what
// the engine hands it, public/ikemen/webrtc.js):
//
//	select  when the host's Lua calls loadStart: the roster, stage, team modes,
//	        rules, the sync settings (the replay header) and the host's
//	        screenpack and fight screen files. The watcher loads the same
//	        match from it while the players load theirs.
//	start   when the host's GGPO session for the match is built (InitP1): the
//	        seed and pre-match time the players agreed (what writeReplaySync
//	        records) and the match clock. The watcher is registered as a
//	        spectator in that same session; it starts at frame 0.
//	drop    the host dropped the watcher from this match, with the reason
//	        (never connected in time, fell behind, left). The watcher ends
//	        its match and waits for the next select.
//	end     the host's match is over: its last frame and the inputs of its
//	        last frames as the host simulated them, so the watcher finishes
//	        exactly where the host did even if the tail never came over GGPO.
//	nowatch this match cannot be watched (delay netcode, or a match the host
//	        could not describe).
//
// TEAM MATCHES. Simul and Tag are one GGPO session like a single match: the
// select lists every member of both teams in order (NumChars per side), and
// is sent only once both teams are complete (watchTeamNeed), never with a
// partial roster. Turns runs one game() for the whole match but a NEW GGPO
// session for every turn (fight(): a member is KO'd, the next one comes in).
// It stays ONE watch match across its turns: no new select; the host keeps
// this match's watchers and registers them again with each turn's session
// (watchHostNextTurn), and start, drop and end carry the turn number (Turn,
// 0 first). The watcher keys those three by turn and builds a new spectator
// session at the same point of game() where the host builds its next one
// (watchNextTurn, after the turn loop's break test). A start for the next
// turn that arrives early is kept for it; a drop for a later turn ends the
// match after this turn. A rematch with no next turn is a new match, offered
// again, as before. The watcher side keys on "this game() started as a
// watched match" (captured before the turn loop), never on sys.watch.active:
// a lobby member who watched and then plays keeps active set.
//
// A watcher that arrives during a match waits for the next select: joining
// mid-match needs a state transfer and is later work. The players never wait
// on a watcher: a watcher is registered when the match starts if it is
// connected and verified, and if its GGPO line is not through within
// protocol.SpectatorMaxPendingFrames (8 s) it is dropped from that match.
//
// THE WAY IN: the online menu's WATCH A MATCH (main.lua main.t_itemname
// watchmatch, web build only) opens the page's watch panel (watchStart) and
// runs main.f_watch, a loop of watchNext, watchApply, loadStart and game()
// with a waiting screen between matches, until the watcher leaves. The page's
// dev switch (?devnet=1&watch=CODE) starts the engine with -watch instead,
// which runs the same loop after the screenpack has loaded.

import (
	"encoding/json"
	"fmt"
	"strings"

	ggpo "github.com/ikemen-engine/ggpo"
	"github.com/ikemen-engine/ggpo/transport"
)

const (
	watchHostPort = 7600 // where a watcher addresses the host (the transport stamps it)
	watchBasePort = 7650 // watcher slot n is registered at watchBasePort+n
	// A watcher simulates two frames per display frame while more than this
	// many have arrived and wait, so it stays a few frames behind.
	watchCatchUpFrames = 8
	// One player's input as GGPO carries it: one virtual controller frame.
	watchInputSize = virtualFrameBytes
	// Frames of the host's simulated inputs kept for the end message.
	watchTailFrames = 256
	// The address GGPO matches the host and the watchers by; the page
	// transports stamp it (see netplay_js.go, webrtcPeerIP).
	webrtcPeerIPForWatch = "webrtc"
)

type watchChar struct {
	Side int    `json:"side"` // 1 or 2
	Def  string `json:"def"`
	Pal  int    `json:"pal"`
}

type watchMsg struct {
	T     string `json:"t"`
	Match int    `json:"match"`
	Why   string `json:"why,omitempty"`
	// Turns: which turn (GGPO session) of the match a start, drop or end is
	// about, 0 first. Every other mode has turn 0 only.
	Turn int `json:"turn,omitempty"`

	// select
	GameMode       string               `json:"gameMode,omitempty"`
	Header         *ReplayHeader        `json:"header,omitempty"`
	Chars          []watchChar          `json:"chars,omitempty"`
	Stage          string               `json:"stage,omitempty"`
	TeamMode       [2]int32             `json:"teamMode"`
	NumChars       [2]int32             `json:"numChars"`
	MatchWins      [2]int32             `json:"matchWins"`
	Wins           [2]int32             `json:"wins"`
	MaxDraws       [2]int32             `json:"maxDraws"`
	RoundTime      int32                `json:"roundTime"`
	FramesPerCount int32                `json:"framesPerCount"`
	MatchNo        int32                `json:"matchNo"`
	Home           int                  `json:"home"`
	AiLevel        [MaxPlayerNo]float32 `json:"aiLevel"`
	InputRemap     [MaxPlayerNo]int     `json:"inputRemap"`
	Params         string               `json:"params,omitempty"`
	// The ConsecutiveWins trigger's two counters as the match begins. The
	// players reset them in main.f_default; a watcher does not, so a lobby
	// member who watched earlier matches would carry its own.
	ConsecutiveWins [2]int32 `json:"cwins"`
	// The fight screen's win counters ([WinCount], "WINS 0") as the match
	// begins: they go on from match to match (the lobby's players reset them
	// in main.f_default, a room's rematches do not), and a watcher that
	// counted its own would show its own history, or none.
	WinCounts [2]int32 `json:"wcount"`
	// The host's screenpack and fight screen (the menus load the build's own;
	// a quick match loads the fight screen alone), so the watcher draws the
	// match with the same fight screen.
	Motif string `json:"motif,omitempty"`
	Fight string `json:"fight,omitempty"`

	// start
	Seed    int32 `json:"seed"`
	PmTime  int32 `json:"pmTime"`
	NetTime int32 `json:"netTime"`
	// The players' Button Setups (what turns each channel's frames into
	// buttons, pause menu edits included) and menu channels as the match
	// begins, so the watcher reads the same inputs the same way.
	Setups []ReplaySetup      `json:"setups,omitempty"`
	Menu   []watchMenuChannel `json:"menu,omitempty"`

	// end
	Last int         `json:"last"`
	Tail []watchTail `json:"tail,omitempty"`
}

type watchTail struct {
	F int    `json:"f"`
	B []byte `json:"b"` // both players' inputs, as GGPO carries them
}

// watchMenuChannel is one MenuChannel as the players have it when the match
// starts. The players keep theirs from the menus before the match (a mouse
// pointer's context and generation count every menu it was in); a watcher that
// started with zeroes would take a menu's mouse click where they ignore it,
// or the other way round.
type watchMenuChannel struct {
	Escape      int32                        `json:"esc,omitempty"`
	EscapeTaken bool                         `json:"escTaken,omitempty"`
	Capture     bool                         `json:"capture,omitempty"`
	Context     string                       `json:"ctx,omitempty"`
	Generation  uint16                       `json:"gen,omitempty"`
	Received    uint16                       `json:"recv,omitempty"`
	Target      uint32                       `json:"target,omitempty"`
	Sequence    byte                         `json:"seq,omitempty"`
	Seen        bool                         `json:"seen,omitempty"`
	Active      bool                         `json:"active,omitempty"`
	Moved       bool                         `json:"moved,omitempty"`
	Remote      bool                         `json:"remote,omitempty"`
	Held        [len(mouseButtonNames)]int32 `json:"held"`
}

func watchMenuSnapshot() []watchMenuChannel {
	out := make([]watchMenuChannel, len(sys.menuChannels))
	for i := range sys.menuChannels {
		c, m := &sys.menuChannels[i], &sys.menuChannels[i].mouse
		out[i] = watchMenuChannel{Escape: c.escape, EscapeTaken: c.escapeTaken, Capture: c.capture,
			Context: m.context, Generation: m.generation, Received: m.received, Target: m.target,
			Sequence: m.sequence, Seen: m.seen, Active: m.input.active, Moved: m.input.moved,
			Remote: m.input.remote, Held: m.input.held}
	}
	return out
}

// watchMenuRestore puts the host's menu channels in place on a watcher. The
// stepped latch starts clear, so the first frame steps every channel.
func watchMenuRestore(in []watchMenuChannel) {
	sys.menuChannels = [MaxPlayerNo]MenuChannel{}
	for i := 0; i < len(in) && i < len(sys.menuChannels); i++ {
		w, c := &in[i], &sys.menuChannels[i]
		c.escape, c.escapeTaken, c.capture = w.Escape, w.EscapeTaken, w.Capture
		c.mouse.context, c.mouse.generation, c.mouse.received = w.Context, w.Generation, w.Received
		c.mouse.target, c.mouse.sequence, c.mouse.seen = w.Target, w.Sequence, w.Seen
		c.mouse.input.active, c.mouse.input.moved, c.mouse.input.remote = w.Active, w.Moved, w.Remote
		c.mouse.input.held = w.Held
	}
}

// watchSessionSetups is a watcher's sessionSetups from the host's list (the
// way a replay's header becomes them, readReplay).
func watchSessionSetups(in []ReplaySetup) map[int]SessionSetup {
	if len(in) == 0 {
		return nil
	}
	out := make(map[int]SessionSetup, len(in))
	for _, rs := range in {
		out[rs.Channel] = newSessionSetup(rs.Setup)
	}
	return out
}

// ---------------------------------------------------------------------------
// Host side

// watchHostMatch counts the matches this host has offered to watchers.
var watchHostMatch int

// watchHost is the host's offer for the match being set up. loadStart opens
// it; it goes out once the match's characters are selected: with background
// loading (Config.VsScreenLoading, on by default since 2.5) the Lua selects
// them AFTER loadStart, so loadStart alone would describe an empty match.
var watchHost struct {
	pending bool   // loadStart ran, the offer waits for the selection
	offered bool   // this match was offered (a select or a nowatch)
	ok      bool   // the offer was a select: this match's watchers may be registered
	params  string // loadStart's params plus each character's override params
	// Turns: the turn (GGPO session) of this match being played, 0 first, and
	// the last match whose offer was a select (a next turn may register
	// watchers only when it is this match).
	turn     int
	selMatch int
}

// watchHostLoadStart opens the offer of the match the host's Lua started
// loading, and makes it at once when the selection is already complete.
func watchHostLoadStart(params string) {
	if !platformWatchHosting() {
		return
	}
	watchHost.pending, watchHost.offered, watchHost.ok, watchHost.params = true, false, false, params
	watchHostTryOffer(false)
}

// watchHostSelected is a character selected on the host while the offer
// waits: its override params go with the offer, as loadStart params.
func watchHostSelected(override string) {
	if !watchHost.pending {
		return
	}
	if override = strings.TrimSpace(override); override != "" {
		if strings.TrimSpace(watchHost.params) != "" {
			watchHost.params += ", "
		}
		watchHost.params += override
	}
	watchHostTryOffer(false)
}

// watchHostTryOffer makes the waiting offer once the selection is complete
// (a match that cannot be watched at once), or now when forced.
func watchHostTryOffer(force bool) {
	if !watchHost.pending {
		return
	}
	if !force && watchUnwatchable() == "" && !watchSelectionComplete() {
		return
	}
	watchHost.pending = false
	watchHostSelect(watchHost.params)
}

// watchSelectionComplete: a stage and each side's whole team. The select
// screen picks side 1's team before side 2's, so "a character on each side"
// would offer a team match at side 2's first member.
func watchSelectionComplete() bool {
	sys.selMutex.RLock()
	defer sys.selMutex.RUnlock()
	if sys.sel.selectedStageNo <= 0 {
		return false
	}
	for side := 0; side < 2; side++ {
		if len(sys.sel.selected[side]) < watchTeamNeed(side) {
			return false
		}
	}
	return true
}

// watchTeamNeed is how many characters side's team selects: the team size in
// Turns (numTurns), Simul and Tag (numSimul), else 1. Read under selMutex
// (setTeamMode writes these under it).
func watchTeamNeed(side int) int {
	n := int32(1)
	switch sys.tmode[side] {
	case TM_Turns:
		n = sys.numTurns[side]
	case TM_Simul, TM_Tag:
		n = sys.numSimul[side]
	}
	if n < 1 {
		n = 1
	}
	return int(n)
}

// watchHostSelect offers the match the host's Lua is loading. With no watcher
// bridge on the page (the host did not allow watchers, or this is a guest) it
// does nothing at all.
func watchHostSelect(params string) {
	if !platformWatchHosting() {
		return
	}
	watchHostMatch++
	watchHost.offered, watchHost.ok, watchHost.turn = true, false, 0
	m := watchMsg{T: "select", Match: watchHostMatch}
	if why := watchUnwatchable(); why != "" {
		m.T, m.Why = "nowatch", why
		platformWatchOffer(watchHostMatch, mustJSON(&m))
		return
	}
	// Each side's whole team, in member order (the order the watcher's Lua
	// selects them in, so P1, P3, P5... are the host's).
	var need [2]int
	sys.selMutex.RLock()
	for side := 0; side < 2; side++ {
		for _, sel := range sys.sel.selected[side] {
			if sel[0] < 0 || sel[0] >= len(sys.sel.charlist) {
				continue
			}
			m.Chars = append(m.Chars, watchChar{Side: side + 1, Def: sys.sel.charlist[sel[0]].def, Pal: sel[1]})
		}
		m.NumChars[side] = int32(len(sys.sel.selected[side]))
		need[side] = watchTeamNeed(side)
	}
	if n := sys.sel.selectedStageNo; n > 0 && n <= len(sys.sel.stagelist) {
		m.Stage = sys.sel.stagelist[n-1].def
	}
	sys.selMutex.RUnlock()
	m.GameMode = sys.gameMode
	m.Header = sys.currentReplayHeader()
	for i := 0; i < 2; i++ {
		m.TeamMode[i] = int32(sys.tmode[i])
	}
	m.MatchWins, m.Wins, m.MaxDraws = sys.matchWins, sys.wins, sys.maxDraws
	m.RoundTime, m.FramesPerCount, m.MatchNo = sys.maxRoundTime, sys.curFramesPerCount, sys.matchNo
	m.Home, m.AiLevel, m.InputRemap = sys.home, sys.aiLevel, sys.inputRemap
	m.ConsecutiveWins = sys.consecutiveWins
	for i, wc := range sys.fightScreen.winCounts {
		if wc != nil {
			m.WinCounts[i] = wc.wins
		}
	}
	m.Params = params
	m.Motif, m.Fight = sys.motif.Def, sys.fightScreen.def
	if m.Header != nil {
		// Known from the second match of a session on (Synchronize sets them
		// up); the start message carries them for every match.
		m.Header.Setups = sys.replaySetups()
	}
	if m.Stage == "" || m.Header == nil || !watchTeamsWhole(m.Chars, m.NumChars, need) {
		m.T, m.Why = "nowatch", "the host's match could not be described to a watcher"
	}
	watchHost.ok = m.T == "select"
	if watchHost.ok {
		watchHost.selMatch = watchHostMatch
	}
	platformWatchOffer(watchHostMatch, mustJSON(&m))
}

// watchTeamsWhole: each side lists exactly its NumChars characters (none was
// skipped as invalid), at least its whole team and at least one. A forced
// offer with a team still being picked is a nowatch, never a partial roster.
func watchTeamsWhole(chars []watchChar, numChars [2]int32, need [2]int) bool {
	for side := 0; side < 2; side++ {
		n := 0
		for _, c := range chars {
			if c.Side == side+1 {
				n++
			}
		}
		if n < 1 || n != int(numChars[side]) || n < need[side] {
			return false
		}
	}
	return true
}

// watchUnwatchable says why the current selection cannot be watched ("" when
// it can). Every team mode of a versus match is watched (Single, Simul, Tag,
// Turns at any size); delay netcode has no spectator path. The online co-op
// runs (ARCADE CO-OP, SURVIVAL CO-OP: both players on side 1 against the
// computer) stay refused, as they were while every team mode was: a run
// carries state from match to match (life kept between matches, the next
// survival opponent loaded ahead, continues), and no watcher has been
// proven to follow it.
func watchUnwatchable() string {
	if !sys.cfg.Netplay.RollbackNetcode {
		return "watching needs rollback netcode; this match uses delay netcode"
	}
	switch sys.gameMode {
	case "netplayteamcoop", "netplaysurvivalcoop":
		return "watching co-op runs is not supported yet"
	}
	return ""
}

// watchHostNextTurn: the host's Turns match goes on to its next turn (game()'s
// turn loop, watchNextTurn). It is the same watch match: no new offer, and
// the watchers of this match's select are registered again with the next
// turn's GGPO session (watchTransport, then addWatchers). watchEnd cleared
// the offer at the end of the turn; this puts it back as it was.
func watchHostNextTurn() {
	if !platformWatchHosting() || watchHostMatch == 0 {
		return
	}
	watchHost.pending = false
	watchHost.offered = true
	watchHost.ok = watchHost.selMatch == watchHostMatch
	watchHost.turn++
}

// watchNextTurn runs at the point of game()'s Turns loop where the next turn
// begins (after the break test). watched: this game() began as a watched
// match (sys.spectating() before the turn loop). A watcher builds the next
// turn's spectator session there, or ends the match (false) when it cannot
// follow it; the host carries its watchers on to the next turn.
func watchNextTurn(watched bool) bool {
	if watched {
		return sys.watch.nextTurn()
	}
	if sys.netConnection != nil && sys.netConnection.host {
		watchHostNextTurn()
	}
	return true
}

type watchReg struct {
	slot   int
	handle ggpo.PlayerHandle
}

// watchTransport is the host's GGPO transport for this match: unchanged when
// no watcher is to be registered, else the players' transport wrapped so that
// watcher slots reach their own connections.
func (rs *RollbackSession) watchTransport(conns []transport.Connection) []transport.Connection {
	rs.watchSlots = nil
	if platformWatchHosting() && (watchHost.pending || !watchHost.offered) {
		// Not offered yet: a match that started without a loadStart for it
		// (a rematch that reuses what is loaded), or one whose selection never
		// completed. Offer it now; a watcher that loads in time watches it.
		watchHost.pending = true
		watchHostTryOffer(true)
	}
	// Only the watchers of a select for THIS match: after a nowatch they
	// are not loading anything and would be dropped once the host gave up on them.
	if !platformWatchHosting() || watchUnwatchable() != "" || watchHostMatch == 0 || !watchHost.ok {
		platformWatchHubClose()
		return conns
	}
	rs.watchSlots = platformWatchStart(watchHostMatch)
	if len(rs.watchSlots) == 0 {
		platformWatchHubClose()
		return conns
	}
	return platformWatchHub(conns)
}

// addWatchers registers this match's watchers with the host's session (before
// it starts, as GGPO requires) and sends each its start message.
func (rs *RollbackSession) addWatchers(peer *ggpo.Peer) {
	rs.watchers = nil
	if len(rs.watchSlots) == 0 || sys.netConnection == nil {
		return
	}
	start := watchMsg{T: "start", Match: watchHostMatch, Turn: watchHost.turn,
		Seed: sys.netConnection.lastSeed, PmTime: sys.netConnection.preMatchTime, NetTime: rs.netTime,
		Setups: sys.replaySetups(), Menu: watchMenuSnapshot()}
	for _, slot := range rs.watchSlots {
		player := ggpo.NewSpectatorPlayer(20, webrtcPeerIPForWatch, watchBasePort+slot)
		var handle ggpo.PlayerHandle
		if err := peer.AddPlayer(&player, &handle); err != nil {
			platformWatchSend(slot, mustJSON(&watchMsg{T: "drop", Match: watchHostMatch, Turn: watchHost.turn,
				Why: "the host could not add this watcher: " + err.Error()}))
			continue
		}
		rs.watchers = append(rs.watchers, watchReg{slot: slot, handle: handle})
		platformWatchSend(slot, mustJSON(&start))
	}
	rs.watchTail = make([]watchTail, watchTailFrames)
	for i := range rs.watchTail {
		rs.watchTail[i].F = -1
	}
	if watchHost.turn > 0 || sys.tmode[0] == TM_Turns || sys.tmode[1] == TM_Turns {
		platformWatchLog(fmt.Sprintf("match %d turn %d: %d watcher(s) registered", watchHostMatch, watchHost.turn, len(rs.watchers)))
	} else {
		platformWatchLog(fmt.Sprintf("match %d: %d watcher(s) registered", watchHostMatch, len(rs.watchers)))
	}
}

// watchRecord keeps the inputs the host simulated a frame with, latest wins
// (a rollback re-simulation overwrites the guess), for the end message.
func (rs *RollbackSession) watchRecord(frame int, values [][]byte) {
	if len(rs.watchers) == 0 || frame < 0 {
		return
	}
	t := &rs.watchTail[frame%watchTailFrames]
	t.F = frame
	t.B = t.B[:0]
	for _, v := range values {
		t.B = append(t.B, v...)
	}
}

// watchCheckGone drops the watchers whose page connection closed. Called once
// per display frame on the host; a few atomic loads.
func (rs *RollbackSession) watchCheckGone() {
	if len(rs.watchers) == 0 {
		return
	}
	peer, ok := rs.backend.(*ggpo.Peer)
	if !ok {
		return
	}
	for _, w := range rs.watchers {
		if platformWatchSlotGone(w.slot) && peer.SpectatorDropReason(w.handle) == "" {
			peer.DropSpectator(w.handle, "the watcher's connection closed")
		}
	}
}

// onWatcherEvent is the host's handling of a GGPO event about a watcher. It
// never touches the players' match.
func (rs *RollbackSession) onWatcherEvent(info *ggpo.Event) {
	if info.Code != ggpo.EventCodeDisconnectedFromPeer {
		return
	}
	peer, ok := rs.backend.(*ggpo.Peer)
	if !ok {
		return
	}
	for _, w := range rs.watchers {
		if w.handle == info.Player {
			why := peer.SpectatorDropReason(w.handle)
			if why == "" {
				why = "the watcher disconnected"
			}
			platformWatchLog(fmt.Sprintf("watcher %d dropped: %s", w.slot, why))
			platformWatchSend(w.slot, mustJSON(&watchMsg{T: "drop", Match: watchHostMatch, Turn: watchHost.turn, Why: why}))
		}
	}
}

// watchEnd sends the registered watchers still in the match the host's last
// frame and its last simulated inputs.
func (rs *RollbackSession) watchEnd() {
	// The match's hub stops reading the page now, watched or not: a next match
	// with no watcher must not inherit it.
	platformWatchHubClose()
	// The next match is offered again, loadStart or not; a Turns match's next
	// turn puts this match's offer back (watchHostNextTurn).
	watchHost.pending, watchHost.offered, watchHost.ok = false, false, false
	if len(rs.watchers) == 0 {
		return
	}
	peer, ok := rs.backend.(*ggpo.Peer)
	if !ok {
		return
	}
	last := peer.CurrentFrame() - 1
	end := watchMsg{T: "end", Match: watchHostMatch, Turn: watchHost.turn, Last: last}
	for f := last - watchTailFrames + 1; f <= last; f++ {
		if f < 0 {
			continue
		}
		if t := rs.watchTail[f%watchTailFrames]; t.F == f {
			end.Tail = append(end.Tail, watchTail{F: f, B: append([]byte(nil), t.B...)})
		}
	}
	raw := mustJSON(&end)
	for _, w := range rs.watchers {
		if peer.SpectatorDropReason(w.handle) == "" {
			platformWatchSend(w.slot, raw)
		}
	}
	rs.watchers = nil
}

// ---------------------------------------------------------------------------
// Watcher side

type watchState struct {
	active  bool // this engine is a watcher (-watch)
	gone    bool // the connection to the host closed
	sel     *watchMsg
	next    *watchMsg // a select that arrived while a match was running
	start   *watchMsg
	end     *watchMsg
	dropped string
	running bool
	tail    map[int][]byte
	added   map[string]int // def path -> char ref, so a rematch reuses it
	stages  map[string]int // def path -> stage ref
	notice  string
	loading int  // the match this watcher loaded (apply)
	escKey  bool // this watcher's own Esc key, pressed while it watches a match
	left    bool // ... so it left watching
	// Turns: the turn this watcher runs (0 first), and what the host already
	// said about the next one while this one still ran: its start, its end
	// (a turn shorter than the watcher's lag) and a drop.
	turn     int
	early    *watchMsg
	endNext  *watchMsg
	dropNext string
}

// resetTurns: a new match starts at turn 0 with nothing kept for a next turn.
func (w *watchState) resetTurns() {
	w.turn, w.early, w.endNext, w.dropNext = 0, nil, nil, ""
}

// leaveLocal: the watcher pressed Esc while it loaded or watched a match. Like
// a replay, Esc ends watching: the page closes the connection to the host, and
// the Lua's next watchNext says the host is gone. The players never notice
// beyond the host dropping a watcher.
func (w *watchState) leaveLocal() {
	w.escKey = false
	if w.left {
		return
	}
	w.left = true
	platformWatchLog("left watching (Esc)")
	platformWatchLeave()
}

// poll moves the host's messages from the page into the watcher's state.
func (w *watchState) poll() {
	for {
		raw, ok := platformWatchNext()
		if !ok {
			if !w.gone {
				w.gone = true
				platformWatchLog("the connection to the host closed")
			}
			return
		}
		if raw == "" {
			return
		}
		var m watchMsg
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			platformWatchLog("unreadable message from the host: " + err.Error())
			continue
		}
		w.take(m)
	}
}

// take is one message from the host. A start, drop or end is about this
// watcher's match only, and about the turn it names (Turns; every other mode
// has turn 0 only): one for this turn takes effect now, a start, end or drop
// for a later turn waits for that turn (nextTurn), one for an earlier turn is
// stale and ignored.
func (w *watchState) take(m watchMsg) {
	switch m.T {
	case "select", "nowatch":
		mm := m
		if w.running {
			w.next = &mm
		} else {
			w.sel, w.start, w.end, w.dropped = &mm, nil, nil, ""
			w.resetTurns()
		}
		if m.T == "nowatch" {
			w.notice = m.Why
			platformWatchLog("this match cannot be watched: " + m.Why)
		}
	case "start":
		if w.sel != nil && m.Match == w.sel.Match {
			mm := m
			switch {
			case m.Turn == w.turn:
				w.start = &mm
			case m.Turn > w.turn:
				w.early = &mm
			}
		}
	case "drop":
		if w.sel != nil && m.Match == w.sel.Match {
			switch {
			case m.Turn == w.turn:
				w.dropped = m.Why
				platformWatchLog("dropped from this match: " + m.Why)
			case m.Turn > w.turn:
				w.dropNext = m.Why
				platformWatchLog(fmt.Sprintf("dropped from turn %d of this match: %s", m.Turn, m.Why))
			}
		}
	case "end":
		if w.sel != nil && m.Match == w.sel.Match {
			mm := m
			switch {
			case m.Turn == w.turn:
				w.setEnd(&mm)
			case m.Turn > w.turn:
				w.endNext = &mm
			}
		}
	}
}

// setEnd takes the host's end of the turn this watcher runs, with its tail.
func (w *watchState) setEnd(m *watchMsg) {
	w.end = m
	w.tail = map[int][]byte{}
	for _, t := range m.Tail {
		w.tail[t.F] = t.B
	}
}

// nextTurn is the watcher's side of a Turns match going on to its next turn,
// at the point of game() where the host builds that turn's session: a new
// spectator session for it, which the next fight() synchronizes with the
// host's start for that turn. False, with no session, when this watcher
// cannot follow the match on: the host is gone, it left, it was dropped (from
// this turn or the next), or the host moved on to another match.
func (w *watchState) nextTurn() bool {
	w.poll()
	if w.gone || w.left || w.dropped != "" || w.dropNext != "" ||
		w.sel == nil || w.sel.T != "select" || w.sel.Match != w.loading {
		sys.rollback.session = nil
		return false
	}
	w.turn++
	w.start, w.end, w.tail = nil, nil, nil
	if w.early != nil && w.early.Turn == w.turn {
		w.start = w.early
	}
	if w.endNext != nil && w.endNext.Turn == w.turn {
		w.setEnd(w.endNext)
	}
	w.early, w.endNext = nil, nil
	s := NewRollbackSession(sys.cfg.Netplay.Rollback)
	s.spectating = true
	sys.rollback.session = &s
	return true
}

// watchNextMatch is the select to load next (nil while there is none), and
// false once the host is gone.
func (w *watchState) nextMatch() (*watchMsg, bool) {
	w.poll()
	if w.next != nil && !w.running {
		w.sel, w.next, w.start, w.end, w.dropped = w.next, nil, nil, nil, ""
		w.resetTurns()
	}
	if w.gone {
		return nil, false
	}
	if w.sel == nil || w.sel.T != "select" || w.start != nil || w.dropped != "" || w.end != nil {
		return nil, true
	}
	return w.sel, true
}

// apply puts the host's sync settings into effect and finds (or adds) the
// characters and stage, returning their refs for the Lua to select.
func (w *watchState) apply(m *watchMsg) (chars [][3]int, stageRef int, err error) {
	if m.Header == nil || m.Header.SyncVersion != syncConfigVersion {
		return nil, 0, fmt.Errorf("the host runs a different sync version")
	}
	localStrict, err := collectSyncSettings(&sys.cfg, syncStrict)
	if err != nil {
		return nil, 0, err
	}
	if err := validateStrictCompatibility(localStrict, m.Header.Strict, true); err != nil {
		return nil, 0, err
	}
	if m.Fight != "" && !strings.EqualFold(m.Fight, sys.fightScreen.def) {
		fs, err := loadFightScreen(m.Fight)
		if err != nil {
			return nil, 0, fmt.Errorf("the host's fight screen %s: %v", m.Fight, err)
		}
		sys.fightScreen = *fs
		platformWatchLog("fight screen: " + m.Fight)
	}
	if m.Motif != "" && !strings.EqualFold(m.Motif, sys.motif.Def) {
		platformWatchLog("the host's screenpack is " + m.Motif + ", this page's " + sys.motif.Def)
	}
	if sys.netplayOverride.Active {
		if err := sys.endSyncSessionOverride(); err != nil {
			return nil, 0, err
		}
	}
	if err := sys.beginSessionOverride("watch", m.Header.Strict, m.Header.Host,
		m.Header.ContentFingerprint, m.Header.RoundFadeTime, m.Header.RoundFadeInTime); err != nil {
		return nil, 0, err
	}
	// The players' setups, as beginReplaySession sets a replay's (after the
	// override); the start message brings them again as the match begins.
	sys.sessionSetups = watchSessionSetups(m.Header.Setups)
	w.loading = m.Match
	if w.added == nil {
		w.added, w.stages = map[string]int{}, map[string]int{}
	}
	for _, c := range m.Chars {
		key := strings.ToLower(c.Def)
		ref, ok := w.added[key]
		if !ok {
			if sys.sel.AddChar(c.Def) == nil {
				return nil, 0, fmt.Errorf("character not found: %s", c.Def)
			}
			ref = len(sys.sel.charlist) - 1
			w.added[key] = ref
		}
		chars = append(chars, [3]int{c.Side, ref, c.Pal})
	}
	key := strings.ToLower(m.Stage)
	stageRef, ok := w.stages[key]
	if !ok {
		if _, err := sys.sel.AddStage(m.Stage); err != nil {
			return nil, 0, fmt.Errorf("stage not found: %s", m.Stage)
		}
		stageRef = len(sys.sel.stagelist)
		w.stages[key] = stageRef
	}
	return chars, stageRef, nil
}

// applyRules sets the rules the host's select screen set, after the Lua has
// made the selection (setTeamMode resets part of it).
func (w *watchState) applyRules(m *watchMsg) {
	sys.gameMode = m.GameMode
	sys.matchWins, sys.wins, sys.maxDraws = m.MatchWins, m.Wins, m.MaxDraws
	sys.curFramesPerCount, sys.maxRoundTime, sys.matchNo = m.FramesPerCount, m.RoundTime, m.MatchNo
	sys.home, sys.aiLevel, sys.inputRemap = m.Home, m.AiLevel, m.InputRemap
	sys.consecutiveWins = m.ConsecutiveWins
	for i, wc := range sys.fightScreen.winCounts {
		if wc != nil {
			wc.wins = m.WinCounts[i]
		}
	}
	// A watcher's match runs in the rollback loop with a spectator session:
	// usesRollbackMatch and the input path key off this being non-nil.
	session := NewRollbackSession(sys.cfg.Netplay.Rollback)
	session.spectating = true
	sys.rollback.session = &session
	w.start, w.end, w.dropped, w.tail = nil, nil, "", nil
	w.resetTurns()
}

// synchronize is the watcher's counterpart of NetConnection.Synchronize: it
// waits for the host's start message and seeds exactly as the players did.
func (w *watchState) synchronize() error {
	err := w.awaitStart()
	if err != nil {
		// No match runs: nothing would end this spectator session, and
		// while it lasts the engine reads no local input (spectating).
		sys.rollback.session = nil
		return err
	}
	Srand(w.start.Seed)
	sys.preMatchTime = w.start.PmTime
	if sys.rollback.session != nil {
		sys.rollback.session.netTime = w.start.NetTime
	}
	// The players' setups and menu channels as their match begins.
	if len(w.start.Setups) > 0 {
		sys.sessionSetups = watchSessionSetups(w.start.Setups)
	}
	watchMenuRestore(w.start.Menu)
	w.running = true
	return nil
}

func (w *watchState) awaitStart() error {
	for w.start == nil {
		w.poll()
		if w.gone {
			return Error("the host closed the connection")
		}
		if w.dropped != "" {
			return Error("dropped from this match: " + w.dropped)
		}
		// The host offered another match while this one loaded (it went
		// back from its versus screen): load that one instead.
		if w.sel == nil || w.sel.Match != w.loading {
			return Error("the host changed the match")
		}
		if sys.esc || w.escKey || !sys.await(sys.gameRenderSpeed(), false) {
			if w.escKey || sys.esc {
				w.leaveLocal()
			}
			return Error("watching canceled")
		}
	}
	return nil
}

// InitWatch builds this match's spectator session over the page's link to the
// host. Without a page transport (desktop) ggpo binds a UDP socket, which can
// fail; the error goes back to preMatchSetup.
func (rs *RollbackSession) InitWatch() error {
	// The players' ggpo input: one virtual controller frame per player.
	inputSize := virtualFrameBytes
	spec := ggpo.NewSpectator(rs, watchBasePort, 2, inputSize, webrtcPeerIPForWatch, watchHostPort)
	rs.backend = &spec
	if err := spec.InitializeConnection(platformWatchTransport()...); err != nil {
		return err
	}
	rs.currentPlayer = 1
	rs.playerNo = 1
	rs.watchFrame = 0
	spec.Start()
	return nil
}

// runWatchFrame simulates the next frame (or two, to catch up) from the
// host's confirmed inputs; while none has arrived it simulates nothing.
func (rs *RollbackSystem) runWatchFrame() bool {
	s := rs.session
	w := &sys.watch
	if w.escKey {
		// This watcher's own Esc, also while it waits for the host.
		w.leaveLocal()
		sys.esc = true
		return false
	}
	w.poll()
	if w.dropped != "" || w.gone {
		if sys.sessionWarning == "" {
			why := w.dropped
			if why == "" {
				why = "the host closed the connection"
			}
			sys.sessionWarning = "Watching ended: " + why
		}
		sys.endMatch = true
		return false
	}
	spec, ok := s.backend.(*ggpo.Spectator)
	if !ok {
		return false
	}
	steps := 1
	if spec.Buffered() > watchCatchUpFrames {
		steps = 2
	}
	for i := 0; i < steps; i++ {
		var values [][]byte
		if spec.Frame() == s.watchFrame {
			var flags int
			if v, err := spec.SyncInput(&flags); err == nil {
				values = v
			}
		}
		if values == nil && w.tail != nil {
			if b, ok := w.tail[s.watchFrame]; ok && len(b) == 2*watchInputSize {
				half := len(b) / 2
				values = [][]byte{b[:half], b[half:]}
			}
		}
		if values == nil {
			if w.end != nil && s.watchFrame > w.end.Last {
				// Past the host's last frame and the match did not end here:
				// stop where the host stopped.
				sys.endMatch = true
				return false
			}
			return true // waiting for the host
		}
		rs.advanced = true
		rs.ggpoInputs = decodeInputs(values)
		keepRunning := rs.simulateFrame()
		crc := s.LiveChecksum()
		platformWatchTrail(s.watchFrame, crc)
		s.watchFrame++
		if spec.Frame() == s.watchFrame {
			spec.AdvanceFrame(crc)
		}
		if sys.esc || !keepRunning {
			return false
		}
	}
	return true
}

// onWatchEvent is the watcher's handling of its spectator session's events.
func (rs *RollbackSession) onWatchEvent(info *ggpo.Event) {
	switch info.Code {
	case ggpo.EventCodeRunning:
		platformWatchLog("in sync with the host")
	case ggpo.EventCodeDisconnectedFromPeer:
		why := "the host closed the connection"
		if spec, ok := rs.backend.(*ggpo.Spectator); ok && spec.FailReason() != "" {
			why = spec.FailReason()
		}
		if sys.watch.end != nil {
			return // the host's match is over; the tail finishes ours
		}
		if sys.watch.dropped == "" {
			sys.watch.dropped = why
		}
		platformWatchLog("watching ended: " + why)
	}
}

func mustJSON(m *watchMsg) string {
	b, err := json.Marshal(m)
	if err != nil {
		return `{"t":"nowatch","why":"internal error"}`
	}
	return string(b)
}

// watchEndOverride gives back this engine's own settings once watching ends.
// A watched match runs under the host's sync settings (apply), and before this
// they stayed in effect after the watcher left: the next netplay session then
// skipped its own settings handshake (an override was still active) and failed
// with "Synchronization error" (a lobby member who watched, then played).
func watchEndOverride() {
	if sys.netplayOverride.Active && sys.netplayOverride.Source == "watch" {
		if err := sys.endSyncSessionOverride(); err != nil {
			platformWatchLog("restoring the settings after watching: " + err.Error())
		}
	}
}

// watchResultSide is the watched match's winning side from its rounds: 1 or 2,
// 0 a draw, -1 none decided. A watcher's match ends when the host's does
// (endMatch), so getWinnerTeam says -1 there.
func watchResultSide() int {
	w1, w2 := sys.wins[0] >= sys.matchWins[0], sys.wins[1] >= sys.matchWins[1]
	switch {
	case w1 && !w2:
		return 1
	case w2 && !w1:
		return 2
	case w1 && w2:
		return 0
	}
	return -1
}

// watchMatchSummary is the current match's rounds as the stats log has them:
// round, timer, and per side its life and how the round went.
func watchMatchSummary() string {
	m := sys.statsLog.currentStatsMatch()
	if m == nil {
		return "no rounds recorded"
	}
	out := fmt.Sprintf("wins %d-%d", sys.wins[0], sys.wins[1])
	for _, r := range m.Rounds {
		out += fmt.Sprintf(" | r%d t=%d", r.Index, r.Timer)
		// every fighter of each side (a team's partners, each Turns turn's
		// member), so the line host and watcher print covers them all
		for side := 0; side < 2; side++ {
			for i, f := range r.Fighters[side] {
				res := "lose"
				switch {
				case f.DrawGame:
					res = "draw"
				case f.Win && f.WinKO:
					res = "win KO"
				case f.Win && f.WinTime:
					res = "win time"
				case f.Win:
					res = "win"
				}
				if i == 0 {
					out += fmt.Sprintf(" P%d life=%d %s", side+1, f.Life, res)
				} else {
					out += fmt.Sprintf(" P%d.%d life=%d %s", side+1, i+1, f.Life, res)
				}
			}
		}
	}
	return out
}
