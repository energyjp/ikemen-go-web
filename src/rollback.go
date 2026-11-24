package main

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"log"
	"math"
	"net"
	"os"
	"strings"
	"time"

	ggpo "github.com/ikemen-engine/ggpo"
)

type RollbackSystem struct {
	session          *RollbackSession
	netConnection    *NetConnection
	ggpoInputs       []VirtualFrame
	inputSubmitted bool
	advanced       bool
	menuUpdating   bool
}

type RollbackProperties struct {
	Port                  int  `ini:"Port"`
	FrameDelay            int  `ini:"FrameDelay" sync:"host"`
	DisconnectNotifyStart int  `ini:"DisconnectNotifyStart" sync:"host"`
	DisconnectTimeout     int  `ini:"DisconnectTimeout" sync:"host"`
	StateLogsEnabled      bool `ini:"StateLogsEnabled" sync:"host"`
	GgpoLogsEnabled       bool `ini:"GgpoLogsEnabled" sync:"host"`
	SaveStageData         bool `ini:"SaveStageData" sync:"host"`
	DesyncTest            bool `ini:"DesyncTest" sync:"host"`
	DesyncTestFrames      int  `ini:"DesyncTestFrames" sync:"host"`
	DesyncTestAI          bool `ini:"DesyncTestAI" sync:"host"`
}

const netMenuDelay = ggpo.MaxPredictionFrames + 1

// TODO: Merge with system.go
func (rs *RollbackSystem) hijackRunMatch() bool {
	// Shared part up until this point already handled by sys.runMatch()

	// Reset variables
	rs.ggpoInputs = make([]VirtualFrame, 2)
	rs.inputSubmitted = false

	// Initialize rollback network session and synchronize state
	if err := rs.preMatchSetup(); err != nil {
		rs.session.Close()
		sys.netConnection.fail(err)
		return false
	}
	// Zero (no deadline) in the browser build: see platformNetDeadlines.
	syncDeadline := netDeadline(sys.cfg.Netplay.SyncTimeout)

	var running bool

	// Loop until end of match
	// Not "sys.keepMatchRunning()" because rollback loop ends earlier than local loop
	for !sys.endMatch {
		rs.session.now = time.Now().UnixMilli()
		err := rs.session.backend.Idle(
			int(math.Max(0, float64(rs.session.next-rs.session.now-1))))
		if err != nil {
			panic(err)
		}
		// The browser page dropped this match because the other player left
		// the lobby or was kicked (platformMatchGone). End it at once, the way
		// GGPO's disconnect does, but also in the post-match, where OnEvent
		// ignores a disconnect, and without the warning box: the page shows
		// its own notice.
		if rs.netConnection != nil && platformMatchGone() {
			rs.session.matchGone()
			break
		}
		// GGPO's disconnect timeout only runs after initial synchronization.
		if rs.netConnection != nil && !rs.session.synchronized && !syncDeadline.IsZero() && time.Now().After(syncDeadline) {
			rs.netConnection.fail(fmt.Errorf("Timed out establishing rollback connection to %s (local UDP port %d).\nCheck UDP forwarding and firewall settings on both peers.",
				net.JoinHostPort(rs.session.remoteIp, fmt.Sprint(rs.netConnection.rollbackRemotePort)), rs.netConnection.rollbackLocalPort))
			break
		}

		// Desync/disconnect callbacks may request a session abort outside the normal input path.
		if sys.esc {
			break
		}

		// Sync speculative inputs and run a speculative frame
		running = rs.runFrame()

		if sys.fightLoopEnd && !sys.postMatchFlg {
			break
		}

		rs.session.next = rs.session.now + 1000/60

		if sys.esc || !running {
			break
		}
		if !rs.advanced {
			// Keep the last rendered frame while pumping input and network events.
			sys.frameSkip = true
			running = sys.await(sys.gameRenderSpeed(), true)
			sys.frameSkip = false
			if !running {
				break
			}
			continue
		}

		// These are outside runFrame(), so they won't be needlessly executed during a rollback
		sys.startNextMatchPreload()
		sys.tickSound()
		sys.cueDraw()
		sys.renderFrame()

		//rs.session.loopTimer.usToWaitThisLoop()
		running = sys.update()

		if sys.esc || !running {
			break
		}
	}

	rs.postMatchSetup()

	return false
}

// rollbackPeer sets session.remoteIp and returns the UDP ports for
// InitP1/InitP2. A platform override comes first: the browser's WebRTC stream
// has no IP address (a *net.TCPAddr assertion on it would panic), and its peer
// address and ports are fixed values the transport stamps, which a stream
// address must not overwrite (fork carry 13; netplay_js.go). Otherwise this is
// upstream's TCP path, unchanged.
func (rs *RollbackSystem) rollbackPeer() (localPort, remotePort int, err error) {
	if remoteIp, lp, rp, ok := platformRollbackPeer(sys.netConnection.host); ok {
		rs.session.remoteIp = remoteIp
		log.Printf("Rollback startup: peer=%s; local port %d, remote port %d (platform transport)", remoteIp, lp, rp)
		return lp, rp, nil
	}

	// Use the actual TCP peer address (also resolves client-side hostnames).
	remoteIP := sys.netConnection.conn.RemoteAddr().(*net.TCPAddr).IP
	localIP := sys.netConnection.conn.LocalAddr().(*net.TCPAddr).IP
	if remoteIP.To4() == nil {
		return 0, 0, Error("Rollback currently requires an IPv4 peer address")
	}

	localPort = sys.netConnection.rollbackLocalPort
	remotePort = sys.netConnection.rollbackRemotePort

	if localPort < 1 || localPort > 65535 || remotePort < 1 || remotePort > 65535 {
		return 0, 0, Error("Rollback UDP ports were not negotiated")
	}

	// A local/proxy peer needs different UDP ports or packets loop back into our own rollback socket.
	if (remoteIP.IsLoopback() || remoteIP.Equal(localIP)) && localPort == remotePort {
		return 0, 0, fmt.Errorf("Cannot use UDP port %d for both rollback players on the same machine.\nUse 127.0.0.1 for automatic guest port selection, or configure different Rollback.Port values.", localPort)
	}

	rs.session.remoteIp = remoteIP.String()
	log.Printf("Rollback startup: TCP local=%s peer=%s; UDP local=0.0.0.0:%d peer=%s",
		sys.netConnection.conn.LocalAddr(), sys.netConnection.conn.RemoteAddr(), localPort,
		net.JoinHostPort(rs.session.remoteIp, fmt.Sprint(remotePort)))
	return localPort, remotePort, nil
}

func (rs *RollbackSystem) preMatchSetup() error {
	if rs.session != nil && sys.netConnection != nil {
		if !sys.netConnection.IsConnected() || sys.netConnection.isClosing() || sys.esc || sys.gameEnd {
			return Error("Rollback connection was closed before startup")
		}
		localPort, remotePort, err := rs.rollbackPeer()
		if err != nil {
			return err
		}
		if rs.session.host != "" {
			// Initialize client as P2
			err = rs.session.InitP2(2, localPort, remotePort, rs.session.remoteIp)
			rs.session.playerNo = 2
		} else {
			// Initialize host as P1
			err = rs.session.InitP1(2, localPort, remotePort, rs.session.remoteIp)
			rs.session.playerNo = 1
		}
		if err != nil {
			return fmt.Errorf("Cannot start rollback on UDP port %d: %w", localPort, err)
		}

		// Synchronize matchTime at match start
		sys.matchTime = rs.session.netTime //s.time = rs.session.netTime // Old typo?
		sys.preMatchTime = sys.netConnection.preMatchTime

		// Borrow netConnection replay recording
		rs.session.recording = sys.netConnection.recording

		// Transfer the active netConnection to the rollback system
		rs.netConnection = sys.netConnection
		sys.netConnection = nil

	} else if rs.session != nil && rs.session.spectating {
		// A watcher: a spectator session fed by the host (watch.go). Its
		// clock and seed came with the host's start message.
		if err := rs.session.InitWatch(); err != nil {
			sys.watch.running = false
			return fmt.Errorf("Cannot start watching: %w", err)
		}
		sys.matchTime = rs.session.netTime
	} else if sys.netConnection == nil && rs.session == nil {
		// If offline, initialize a local rollback sync test session
		session := NewRollbackSession(sys.cfg.Netplay.Rollback)
		rs.session = &session
		rs.session.InitSyncTest(2)
	}

	// Reset rollback session timer
	rs.session.netTime = 0
	return nil
}

func (rs *RollbackSystem) postMatchSetup() {
	rs.session.SaveReplay()
	if len(rs.session.watchers) > 0 || rs.session.spectating {
		// Host and watcher print the same line, so the two can be compared.
		platformWatchLog("match result: " + watchMatchSummary())
	}
	rs.session.watchEnd()
	if rs.session.spectating {
		sys.watch.running = false
	}

	// sys.esc = true

	sys.netConnection = rs.netConnection
	rs.netConnection = nil
	rs.session.backend.Close()

	// Prep for the next match.
	if sys.netConnection != nil {
		newSession := NewRollbackSession(sys.cfg.Netplay.Rollback)
		host := rs.session.host
		remoteIp := rs.session.remoteIp

		rs.session = &newSession
		rs.session.host = host
		rs.session.remoteIp = remoteIp
	} else {
		rs.session = nil
	}
}

// Called once per frame by the main game loop
// Responsible for collecting local inputs and driving the GGPO backend forward
func (rs *RollbackSystem) runFrame() bool {
	var buffer []byte
	var ggpoerr error
	rs.advanced = false

	if sys.esc {
		if rs.session.spectating && sys.watch.escKey {
			sys.watch.leaveLocal()
		}
		return false
	}
	// A collection a round transition or match load asked for (browser build).
	platformPendingGC()

	if rs.session.spectating {
		return rs.runWatchFrame()
	}
	rs.session.watchCheckGone()

	// Surface the connection quality once a second (browser overlay ping
	// counter; no-op on desktop).
	if rs.session != nil && rs.session.backend != nil && sys.gameTime()%60 == 0 {
		if stats, err := rs.session.backend.GetNetworkStats(rs.session.remotePlayerHandle); err == nil {
			platformNetStats(int(stats.Network.Ping))
		}
	}

	if !rs.inputSubmitted && rs.session.syncTest && rs.session.netTime == 0 {
		if rs.session.config.DesyncTestAI {
			buffer = getAIInputs(0)
			ggpoerr = rs.session.backend.AddLocalInput(ggpo.PlayerHandle(0), buffer, len(buffer))
			buffer = getAIInputs(1)
			ggpoerr = rs.session.backend.AddLocalInput(ggpo.PlayerHandle(1), buffer, len(buffer))
		} else {
			buffer = rs.getInputs(0)
			ggpoerr = rs.session.backend.AddLocalInput(ggpo.PlayerHandle(0), buffer, len(buffer))
			buffer = rs.getInputs(1)
			ggpoerr = rs.session.backend.AddLocalInput(ggpo.PlayerHandle(1), buffer, len(buffer))
		}
	} else if !rs.inputSubmitted {
		buffer = rs.getInputs(0)
		ggpoerr = rs.session.backend.AddLocalInput(rs.session.currentPlayerHandle, buffer, len(buffer))
	}

	if ggpoerr == nil {
		rs.inputSubmitted = true
		// Interactive motif screens wait for both players; gameplay may predict.
		var values [][]byte
		disconnectFlags := 0
		if sys.motif.me.active || sys.motifOverlayActive() || sys.postMatchFlg {
			values, ggpoerr = rs.session.backend.SyncInputConfirmed(&disconnectFlags)
		} else {
			values, ggpoerr = rs.session.backend.SyncInput(&disconnectFlags)
		}

		if ggpoerr == nil {
			rs.advanced = true
			rs.ggpoInputs = decodeInputs(values)
			if len(rs.session.watchers) > 0 {
				if p, ok := rs.session.backend.(*ggpo.Peer); ok {
					rs.session.watchRecord(p.CurrentFrame(), values)
				}
			}

			// Record inputs against the rollback timeline. netTime is part of the saved state,
			// so any confirmed re-simulation overwrites earlier speculative data for the same frame.
			if rs.session.recording != nil {
				rs.session.RecordReplayFrame(rs.session.netTime, rs.ggpoInputs)
				rs.session.netTime++
			}

			// Commit this frame to GGPO even if this frame exits gameplay.
			keepRunning := rs.simulateFrame()

			defer func() {
				if re := recover(); re != nil {
					if rs.session.config.DesyncTest {
						rs.session.log.updateStateLogs()
						rs.session.log.saveStateLogs()
						panic("RaiseDesyncError")
					}
				}
			}()

			err := rs.session.backend.AdvanceFrame(rs.session.LiveChecksum())
			rs.inputSubmitted = false
			if err != nil {
				panic(err)
			}
			if sys.esc || !keepRunning {
				return false
			}
		}
	}

	if ggpoerr != nil {
		if err, ok := ggpoerr.(ggpo.Error); !ok || err.Code != ggpo.ErrorCodeNotSynchronized && err.Code != ggpo.ErrorCodePredictionThreshod {
			panic(ggpoerr)
		}
	}
	return true
}

// Contains the logic for a single frame of the game
// Called by both runFrame for speculative execution, and AdvanceFrame for confirmed execution
func (rs *RollbackSystem) simulateFrame() bool {
	sys.frameStepFlag = false

	// If next round
	if !sys.runNextRound() {
		return false
	}

	// Update game state
	sys.action()

	// Update motif state
	sys.uiAction()
	if sys.motif.me.active && !rs.session.inRollback {
		rs.menuUpdating = true
		state := sys.captureAspectState()
		if sys.motif.shouldScopeMotifAspect() {
			state = sys.motifAspectState()
		}
		sys.withAspectState(state, func() { sys.motif.me.runLua(&sys.motif) })
		rs.menuUpdating = false
	}

	// if rs.handleFlags() {
	//	return true
	// }

	if !rs.updateEvents() {
		return false
	}

	// Break if finished
	if sys.fightLoopEnd && !sys.postMatchFlg {
		return false
	}

	// Update system; break if update returns false (game ended)
	//if !sys.update() {
	//	return false
	//}

	// If end match selected from menu/end of attract mode match/etc
	if sys.endMatch {
		sys.esc = true
		return false
	} else if sys.esc {
		sys.endMatch = sys.netConnection != nil
		return false
	}
	return true
}

func (rs *RollbackSystem) updateEvents() bool {
	if !sys.addFrameTime(sys.turbo) {
		if !sys.eventUpdate() {
			return false
		}
		return false
	}
	return true
}

// AI players read their own inputs locally, so they send an idle controller.
func getAIInputs(player int) []byte {
	return VirtualFrame{}.encode()
}

func readI16(b []byte) int16 {
	if len(b) < 2 {
		return 0
	}
	return int16(b[0]) | int16(b[1])<<8
}

func readI32(b []byte) int32 {
	if len(b) < 4 {
		return 0
	}
	return int32(b[0]) | int32(b[1])<<8 | int32(b[2])<<16 | int32(b[3])<<24
}

func decodeInputs(buffer [][]byte) []VirtualFrame {
	frames := make([]VirtualFrame, len(buffer))
	for i, b := range buffer {
		frames[i] = decodeVirtualFrame(b)
	}
	return frames
}

func writeI16(i16 int16) []byte {
	b := []byte{byte(i16), byte((i16 >> 8) & 0xFF)}
	return b
}

func writeI32(i32 int32) []byte {
	b := []byte{byte(i32), byte(i32 >> 8), byte(i32 >> 16), byte(i32 >> 24)}
	return b
}

// Local virtual controller state. The sync test drives both sides locally.
func (rs *RollbackSystem) getInputs(player int) []byte {
	if !rs.session.syncTest {
		player = 0
	}
	return sys.participantFrame(player).encode()
}

func (rs *RollbackSystem) readRollbackInput(controller int) VirtualFrame {
	if controller < 0 || controller >= len(sys.inputRemap) {
		return VirtualFrame{}
	}
	remap := sys.inputRemap[controller]
	if remap < 0 || remap >= len(rs.ggpoInputs) {
		return VirtualFrame{}
	}
	return rs.ggpoInputs[remap]
}

func (rs *RollbackSystem) anyButton() bool {
	for _, f := range rs.ggpoInputs {
		if f.anyButton() {
			return true
		}
	}
	return false
}

// Logs are sized for the worst case scenario, where every frame of the buffer carries a max rollback
type RollbackLogger struct {
	filename   string
	currentLog strings.Builder
	logs       [(MaxSaveStates + 2) * (MaxSaveStates + 1)]string
}

func NewRollbackLogger(timestamp string) RollbackLogger {
	gl := RollbackLogger{filename: "save/logs/Rollback-State-" + timestamp + ".log"}
	return gl
}

func (g *RollbackLogger) logState(action string, stateIdx int, state *GameState) {
	//fmt.Fprintf(&g.currentLog, "State Logger: \n")
	fmt.Fprintf(&g.currentLog, "%s data for the state in slot %d\n", action, stateIdx)

	if state.isSpeculativeFrame {
		fmt.Fprintf(&g.currentLog, "Frame type: Speculative\n")
	} else {
		fmt.Fprintf(&g.currentLog, "Frame type: Confirmed\n")
	}

	fmt.Fprintf(&g.currentLog, "Checksum: %d\n", state.Checksum()) // Calls String() again. Minor issue
	fmt.Fprintf(&g.currentLog, "%s\n", state.String())
}

func (g *RollbackLogger) logRoundSkipCheck(fadeoutStart int32, anyButton, roundnotskip, skipEligible, matchEndDialogue bool) {
	if sys.rollback.session == nil || !sys.rollback.session.config.StateLogsEnabled {
		return
	}
	var inputs [2]uint16
	for i := 0; i < len(inputs) && i < len(sys.rollback.ggpoInputs); i++ {
		inputs[i] = sys.rollback.ggpoInputs[i].Buttons
	}
	fadeoutActive := false
	fadeoutRemain := int32(0)
	if sys.fightScreen.round != nil && sys.fightScreen.round.fadeOut != nil {
		fadeoutActive = sys.fightScreen.round.fadeOut.isActive()
		fadeoutRemain = sys.fightScreen.round.fadeOut.timeRemaining
	}
	fmt.Fprintf(&g.currentLog,
		"RoundSkipCheck MatchTime:%d InRollback:%t RoundState:%d Inputs:[%d/0x%04x %d/0x%04x] "+
			"Intro:%d WinPoseTime:%d WinWaitTime:%d WinSkipped:%t AnyButton:%t "+
			"RoundNotSkip:%t RoundNotOver:%t MatchEndDialogue:%t FadeoutStart:%d "+
			"FadeOutActive:%t FadeOutRemaining:%d SkipEligible:%t\n",
		sys.matchTime, sys.inRollback(), sys.roundState(),
		int16(inputs[0]), uint16(inputs[0]), int16(inputs[1]), uint16(inputs[1]),
		sys.intro, sys.winposetime, sys.winwaittime, sys.winskipped, anyButton,
		roundnotskip, sys.gsf(GSF_roundnotover), matchEndDialogue, fadeoutStart,
		fadeoutActive, fadeoutRemain, skipEligible)
}

func (g *RollbackLogger) logRoundAdvanceCheck(roundOver, tickFrame, motifEndActive, fightLoopEnd, holdPostMatch, canAdvance bool) {
	if sys.rollback.session == nil || !sys.rollback.session.config.StateLogsEnabled {
		return
	}
	fmt.Fprintf(&g.currentLog,
		"RoundAdvanceCheck MatchTime:%d InRollback:%t RoundState:%d Intro:%d "+
			"RoundOver:%t TickFrame:%t MotifEndActive:%t FightLoopEnd:%t "+
			"HoldPostMatch:%t CanAdvance:%t "+
			"TickCount:%d OldTickCount:%d TickCountF:%v NextAddTime:%v "+
			"FrameStep:%t Paused:%t RoundFreeze:%t DialogueActive:%t\n",
		sys.matchTime, sys.inRollback(), sys.roundState(), sys.intro,
		roundOver, tickFrame, motifEndActive, fightLoopEnd, holdPostMatch, canAdvance,
		sys.tickCount, sys.oldTickCount, sys.tickCountF, sys.nextAddTime,
		sys.frameStepFlag, sys.paused, sys.gsf(GSF_roundfreeze),
		sys.motif.di.active)
}

func (g *RollbackLogger) Write(p []byte) (n int, err error) {
	g.currentLog.WriteString(string(p))
	return len(p), nil
}

// Updates state logs using a ring buffer
func (g *RollbackLogger) updateStateLogs() {
	tmp := g.logs
	for i := 0; i < len(g.logs)-1; i++ {
		g.logs[i] = tmp[i+1]
	}
	g.logs[len(g.logs)-1] = g.currentLog.String()
	g.currentLog.Reset()
}

func (g *RollbackLogger) saveStateLogs() {
	// Header indicating the number of entries captured
	// Each save and each load writes one, so this is not quite a frame count
	fullLog := fmt.Sprintf("Writing save state data from the last %d entries.\n\n", len(g.logs))
	// Collect all the logs in the buffer
	for i := range g.logs {
		fullLog += g.logs[i]
	}
	// Save file
	err := os.WriteFile(g.filename, []byte(fullLog), 0666)
	if err != nil {
		fmt.Println(err)
	}
}

type RollbackSession struct {
	backend             ggpo.Backend
	saveStates          map[int]*GameState
	now                 int64
	next                int64
	players             []ggpo.Player
	handles             []ggpo.PlayerHandle
	recording           *os.File
	connected           bool
	host                string
	playerNo            int
	syncProgress        int
	synchronized        bool
	syncTest            bool
	run                 int
	remoteIp            string
	currentPlayer       int
	currentPlayerHandle ggpo.PlayerHandle
	remotePlayerHandle  ggpo.PlayerHandle
	loopTimer           LoopTimer
	config              RollbackProperties
	log                 RollbackLogger
	timestamp           string
	checksumHistory     []checksumSnap
	netTime             int32
	replayInputs        [][REPLAY_NUM_INPUTS]VirtualFrame
	inRollback          bool
	replaySaved         bool

	// Watching (watch.go). Host: the slots offered this match, the watchers
	// registered and the inputs of its last frames. Watcher: this is a
	// spectator session, and the next frame it simulates.
	watchSlots []int
	watchers   []watchReg
	watchTail  []watchTail
	spectating bool
	watchFrame int
}

func (rs *RollbackSession) ensureReplayFrame(frame int) {
	if frame < len(rs.replayInputs) {
		return
	}
	if frame > len(rs.replayInputs) {
		log.Printf("Rollback replay frame gap detected: expected frame %d, got %d; padding missing frames with zeros", len(rs.replayInputs), frame)
	}
	missing := frame - len(rs.replayInputs) + 1
	rs.replayInputs = append(rs.replayInputs, make([][REPLAY_NUM_INPUTS]VirtualFrame, missing)...)
}

func (rs *RollbackSession) RecordReplayFrame(time int32, frames []VirtualFrame) {
	if rs == nil || time < 0 {
		return
	}
	frame := int(time)
	rs.ensureReplayFrame(frame)

	// Always clear the frame first so any overwritten rollback frame keeps zeroes in slots not present in the current input payload.
	rs.replayInputs[frame] = [REPLAY_NUM_INPUTS]VirtualFrame{}
	for i := 0; i < REPLAY_NUM_INPUTS && i < len(frames); i++ {
		rs.replayInputs[frame][i] = frames[i]
	}
}

func (rs *RollbackSession) TruncateReplayFrom(time int32) {
	if rs == nil {
		return
	}
	frame := int(time)
	if frame < 0 {
		frame = 0
	}
	if frame < len(rs.replayInputs) {
		rs.replayInputs = rs.replayInputs[:frame]
	}
}

func (rs *RollbackSession) SaveReplay() {
	if rs == nil || rs.recording == nil || rs.replaySaved {
		return
	}

	// Never append the same rollback replay twice.
	rs.replaySaved = true

	frameCount := int(rs.netTime)
	if frameCount <= 0 {
		return
	}

	if len(rs.replayInputs) < frameCount {
		frameCount = len(rs.replayInputs)
		log.Printf("Missing rollback replay frames after frame %d; truncating replay at this point", frameCount)
	}

	for frame := 0; frame < frameCount; frame++ {
		inputFrame := rs.replayInputs[frame]
		for i := 0; i < REPLAY_NUM_INPUTS; i++ {
			if err := writeReplayInput(rs.recording, inputFrame[i]); err != nil {
				log.Printf("Error while writing rollback replay frame %d controller %d: %v", frame, i, err)
				return
			}
		}
	}
}

type LoopTimer struct {
	lastAdvantage      float32
	usPergameLoop      time.Duration
	usExtraToWait      int
	framesToSpreadWait int
	waitCount          time.Duration
	timeWait           time.Duration
	waitTotal          time.Duration
}

func NewLoopTimer(fps uint32, framesToSpread uint32) LoopTimer {
	return LoopTimer{
		framesToSpreadWait: int(framesToSpread),
		usPergameLoop:      time.Second / time.Duration(fps),
	}
}

func (lt *LoopTimer) OnGGPOTimeSyncEvent(framesAhead float32) {
	lt.waitTotal = time.Duration(float32(time.Second/60) * framesAhead)
	lt.lastAdvantage = float32(time.Second/60) * framesAhead

	if sys.intro > 0 && sys.tickCount == 0 {
		// Wait longer during the start of each round, allowing both players to load assets
		if lt.lastAdvantage < 0 {
			lt.timeWait = time.Duration(lt.lastAdvantage) / time.Duration(lt.framesToSpreadWait)
			lt.waitCount = time.Duration(lt.framesToSpreadWait)
		}
	} else {
		// Normal waiting time
		lt.lastAdvantage /= 4
		lt.timeWait = time.Duration(lt.lastAdvantage) / time.Duration(lt.framesToSpreadWait)
		lt.waitCount = time.Duration(lt.framesToSpreadWait)
	}
}

func (lt *LoopTimer) usToWaitThisLoop() time.Duration {
	if lt.waitCount > 0 {
		lt.waitCount--
		return lt.usPergameLoop + lt.timeWait
	}
	return lt.usPergameLoop
}

func (r *RollbackSession) Close() {
	if r.backend != nil {
		r.backend.Close()
	}
}

func (r *RollbackSession) IsConnected() bool {
	return r.connected
}

func (r *RollbackSession) SaveGameState(stateIdx int) int {
	sys.savePool.curStateID = stateIdx
	sys.rollbackStateID = stateIdx
	oldest := (stateIdx + 1) % (MaxSaveStates + 2)
	// These slots are no longer restorable; drop their snapshots before freeing pooled data.
	// The dropped GameStates go back to the pool (like LoadGameState does), so the next save
	// reuses one instead of allocating a new ~170 KB snapshot every frame: that garbage set
	// the Go GC rate online, and on js/wasm each GC is one long main-thread stall.
	for _, idx := range [2]int{oldest, stateIdx} {
		if gs, ok := r.saveStates[idx]; ok {
			delete(r.saveStates, idx)
			if gs != nil {
				sys.statePool.gameStatePool.Put(gs)
			}
		}
	}
	sys.savePool.Free(oldest)
	sys.savePool.Free(stateIdx)

	r.saveStates[stateIdx] = sys.statePool.gameStatePool.Get().(*GameState)
	r.saveStates[stateIdx].SaveState(stateIdx)

	if r.config.DesyncTest {
		if r.config.StateLogsEnabled {
			r.log.logState("Saving", stateIdx, r.saveStates[stateIdx])
			r.log.updateStateLogs()
		}
		return r.saveStates[stateIdx].Checksum()
	} else {
		if r.config.StateLogsEnabled {
			r.log.logState("Saving", stateIdx, r.saveStates[stateIdx])
			r.log.updateStateLogs()
		}
		return ggpo.DefaultChecksum
	}
}

func (r *RollbackSession) LoadGameState(stateIdx int) {
	sys.loadPool.curStateID = stateIdx
	sys.loadPool.Free(stateIdx)

	r.saveStates[stateIdx].LoadState(stateIdx)

	if r.config.DesyncTest && r.config.StateLogsEnabled {
		r.log.logState("Loading", stateIdx, r.saveStates[stateIdx])
	}

	// Drop the entry alongside the Put, or a later Get can hand out a GameState this map still points at
	sys.statePool.gameStatePool.Put(r.saveStates[stateIdx])
	delete(r.saveStates, stateIdx)
}

// Called when the GGPO backend needs the game to simulate a single frame
// This can happen multiple times per displayed frame during a rollback
func (r *RollbackSession) AdvanceFrame(flags int) {
	// This flag allows the game logic to re-run while knowing it's in a rollback
	// Will be useful later
	r.inRollback = true
	defer func() {
		r.inRollback = false
	}()

	// Make sure we fetch the inputs from GGPO and use these to update
	// the game state instead of reading from the keyboard.
	// Get the confirmed inputs from the GGPO backend for the frame being simulated
	var disconnectFlags int
	inputs, ggpoerr := r.backend.SyncInput(&disconnectFlags)

	// Run frame again using confirmed inputs
	if ggpoerr == nil {
		sys.rollback.ggpoInputs = decodeInputs(inputs)
		if len(r.watchers) > 0 {
			if p, ok := r.backend.(*ggpo.Peer); ok {
				r.watchRecord(p.CurrentFrame(), inputs)
			}
		}
		if r.recording != nil {
			r.RecordReplayFrame(r.netTime, sys.rollback.ggpoInputs)
			r.netTime++
		}
		// As in runFrame(): commit rollback re-simulated frames to GGPO even if this frame exits gameplay.
		_ = sys.rollback.simulateFrame()
		defer func() {
			if re := recover(); re != nil {
				if r.config.DesyncTest {
					r.log.updateStateLogs()
					r.log.saveStateLogs()
					panic("RaiseDesyncError")
				}
			}
		}()

		// Notify GGPO that frame has advanced
		err := r.backend.AdvanceFrame(r.LiveChecksum())
		if err != nil {
			panic(err)
		}
	}
}

func (r *RollbackSession) OnEvent(info *ggpo.Event) {
	// A watcher's events (handles from 1000 up) never touch the players'
	// match: before this a watcher that disconnected ended it, through the
	// EventCodeDisconnectedFromPeer case below.
	if int(info.Player) >= 1000 {
		r.onWatcherEvent(info)
		return
	}
	if r.spectating {
		r.onWatchEvent(info)
		return
	}
	switch info.Code {
	case ggpo.EventCodeConnectedToPeer:
		r.connected = true
	case ggpo.EventCodeSynchronizingWithPeer:
		r.syncProgress = 100 * (info.Count / info.Total)
	case ggpo.EventCodeSynchronizedWithPeer:
		r.syncProgress = 100
		r.synchronized = true
	case ggpo.EventCodeRunning:
		fmt.Println("EventCodeRunning")
	case ggpo.EventCodeDisconnectedFromPeer:
		if sys.postMatchFlg {
			return
		}
		if r.config.StateLogsEnabled {
			r.log.saveStateLogs()
		}
		fmt.Println("EventCodeDisconnectedFromPeer")
		sys.endMatch = true
		r.SaveReplay()
		if sys.sessionWarning == "" {
			sys.sessionWarning = fmt.Sprintf(sys.motif.WarningInfo.Text.Text["disconnect"], int(info.Player))
		}
	case ggpo.EventCodeTimeSync:
		fmt.Printf("EventCodeTimeSync: FramesAhead %f TimeSyncPeriodInFrames: %d\n", info.FramesAhead, info.TimeSyncPeriodInFrames)
		r.loopTimer.OnGGPOTimeSyncEvent(info.FramesAhead)
	case ggpo.EventCodeDesync:
		if r.config.StateLogsEnabled {
			r.log.saveStateLogs()
		}
		fmt.Println("EventCodeDesync")
		log.Printf("Rollback desync detected")
		sys.esc = true
		r.SaveReplay()
		// Itemise the mismatching frame from the snapshot ring. Each player's
		// screen shows their OWN side's raw values; comparing the two screens
		// line by line identifies which hashed field diverged. The prev-frame
		// seed line shows whether the RNG was churning at the boundary.
		detail := fmt.Sprintf("frame %d", info.NumFrameOfDesync)
		detail += fmt.Sprintf("\ncrcL=%d", info.LocalChecksum)
		detail += fmt.Sprintf("\ncrcR=%d", info.RemoteChecksum)
		if sn := r.checksumSnapFor(info.NumFrameOfDesync); sn != nil {
			detail += "\n" + formatChecksumSnap(sn)
			if prev := r.checksumSnapFor(info.NumFrameOfDesync - 1); prev != nil {
				detail += fmt.Sprintf("\nprev seed=%d", prev.randseed)
			}
		} else {
			detail += "\n(state history expired)"
		}
		fmt.Println("DESYNC DETAIL\n" + detail)
		// The input and menu layer goes to the console only: the warning
		// screen clips at the bottom (see formatChecksumSnap).
		if sn := r.checksumSnapFor(info.NumFrameOfDesync); sn != nil {
			fmt.Println("DESYNC INPUT\n" + formatInputSnap(sn))
		}
		// LATCH THE FIRST MISMATCH, NOT THE LAST. EventCodeDesync fires once per
		// mismatching frame, so an unguarded assignment here overwrites the
		// message every time and the player ends up reading the LAST divergent
		// frame - by which point the two simulations can be arbitrarily far
		// apart. The 2026-08-28 Kira dump showed the two peers a WHOLE ROUND
		// apart (rs4 vs rs0, one with a freshly reset round timer), which
		// confirmed a desync happened and could say nothing at all about what
		// caused it. The first divergent frame is the only one where the fields
		// still differ by a little, and it is the only one where `rngm` names a
		// statedef block worth reading.
		//
		// Same guard the disconnect handler ~30 lines above already uses. The
		// console Println stays unconditional: the full per-frame sequence is
		// still worth having in the browser log.
		if sys.sessionWarning == "" {
			sys.sessionWarning = sys.motif.WarningInfo.Text.Text["desync"] + "\n" + detail
		}
	case ggpo.EventCodeConnectionInterrupted:
		fmt.Println("EventCodeconnectionInterrupted")
	case ggpo.EventCodeConnectionResumed:
		fmt.Println("EventCodeconnectionInterrupted")
	}
}

// matchGone ends a match whose other player is gone for good (the page said
// so, see platformMatchGone). Not part of the simulation: the peer is gone,
// so nothing is compared any more.
func (r *RollbackSession) matchGone() {
	if r.config.StateLogsEnabled {
		r.log.saveStateLogs()
	}
	fmt.Println("MatchGone: the page dropped the match")
	log.Printf("Rollback: the other player left; the match ends here")
	sys.endMatch = true
}

func NewRollbackSession(config RollbackProperties) RollbackSession {
	r := RollbackSession{}
	r.saveStates = make(map[int]*GameState)
	r.players = make([]ggpo.Player, 2)       // MaxPlayerNo
	r.handles = make([]ggpo.PlayerHandle, 2) // MaxPlayerNo
	r.config = config
	r.loopTimer = NewLoopTimer(60, 100)
	r.timestamp = time.Now().Format("2006-01-02_15-04-05")
	r.log = NewRollbackLogger(r.timestamp)
	r.replayInputs = make([][REPLAY_NUM_INPUTS]VirtualFrame, 0)
	// Frame -1 marks "never recorded" so ring slot 0 can't false-match frame 0.
	r.checksumHistory = make([]checksumSnap, checksumHistorySize)
	for i := range r.checksumHistory {
		r.checksumHistory[i].frame = -1
	}
	return r

}

// --- Desync diagnostics ----------------------------------------------------
//
// LiveChecksum folds a handful of state fields into ONE crc per frame, and the
// peers exchange only that crc - so when they disagree, the error can't say
// WHICH field diverged. This ring keeps the raw components for recent frames,
// keyed by the same frame numbers the checksum exchange uses, so the desync
// event (which carries NumFrameOfDesync) can print the actual numbers on each
// player's error screen. Two screenshots, compared line by line, identify the
// diverging field - which is the difference between a diagnosis and a shrug.
//
// Fixed-size structs in a preallocated ring: no per-frame allocation, so this
// adds no GC pressure to the netplay path. Overwrites per frame mirror the
// pendingChecksums.Set(frame, ...) semantics in ggpo's AdvanceFrame - during a
// rollback re-simulation the corrected values replace the speculative ones,
// same as the checksum itself.
//
// NOTE: if LiveChecksum ever hashes a new field, mirror it here and in
// recordChecksumSnap, or the dump will say "everything matches" while the crc
// disagrees. Selection-phase hashes (roundState 1: stage/char names) are
// deliberately not itemised - a selection mismatch identifies itself.

const checksumHistorySize = 1024 // ~17s at 60fps; detection lag is far below this

type checksumSnap struct {
	frame        int32
	roundState   int32
	randseed     int32
	matchTime    int32
	curRoundTime int32
	score        [2]float32
	combo        [2]int32
	present      [MaxPlayerNo]bool
	life         [MaxPlayerNo]int32
	redLife      [MaxPlayerNo]int32
	dizzy        [MaxPlayerNo]int32
	guard        [MaxPlayerNo]int32
	power        [MaxPlayerNo]int32
	animNo       [MaxPlayerNo]int32
	stateNo      [MaxPlayerNo]int32
	// Live helper count per slot. LiveChecksum hashes sys.chars[i][0] and
	// nothing else, so every helper a character spawns is invisible to it -
	// and characters branch on NumHelper() constantly. A helper divergence
	// therefore stays silent until it changes which triggers evaluate, which
	// changes how many times Random() is called, which finally shows up as a
	// seed mismatch. By then the seed is the messenger and the helper is the
	// culprit. Recording the count is cheap and names it directly.
	helpers [MaxPlayerNo]int32
	// Round timeline. NEITHER sys.intro NOR sys.winposetime is hashed by
	// LiveChecksum at any round state, yet system.go's roundState-4 readiness
	// freeze writes sys.intro from character state that is ALSO unhashed
	// (ss.no, SCF_over_ko - c.ss.no is hashed at NO round state). That freeze is
	// the only character-gated write to the round timeline, so a one-frame skew
	// in it is permanent and completely silent. winposetime then drives
	// inputWait(), which drives BufReset, which re-arms every press-edge test in
	// the engine - including the fast-recovery RandI at char.go:12604, which
	// stays permanently armed on a KO'd char lying in 5150 because
	// down_recovertime only decrements in state 5110 and the dead leave it at
	// once.
	intro       int32
	winposetime int32
	// Round-transition CONSTANTS, not live state. fadeoutStart is what the
	// RoundNotOver pin compares sys.intro against with an EQUALITY, and it is
	// derived from fadeOut.duration() - which returns the fade ANIM's length
	// when the anim is loaded and falls back to fa.time when it is not
	// (rect.go: duration/animLength/colorFadeTime). If two peers disagree on
	// that, fadeoutStart differs by a CONSTANT, the pin engages on one peer and
	// not the other, and sys.intro carries a fixed offset for the rest of the
	// session - which is exactly the field pattern: a constant 20-frame delta
	// that persists, in sessions that desync from the start while others never
	// do at all. Diagnostic only; neither value is hashed.
	fadeoutStart int32
	fadeDuration int32
	// Per-frame Random() attribution, drained from sys each snap.
	//
	// "The peers are one draw apart" never said WHO drew. These do: total draws
	// that frame, split by player, and split by statedef block (-4/-3/-2/-1/0).
	// Comparing two dumps of the same desync localises the extra draw directly
	// instead of inferring it from the seed.
	rngDraws    int32
	rngByPlayer [MaxPlayerNo]int32
	rngByMinus  [6]int32
	// The input and menu layer (modern-input). Peers exchange virtual
	// controller frames and turn them into buttons with each player's
	// Button Setup; pause, rematch and setup edits are synchronized menu
	// frames. A divergence here (a different setup, a menu one peer opened and
	// the other did not) shows first in these fields, frames before it moves
	// a character. Diagnostic only, except what LiveChecksum folds in.
	in            [2]VirtualFrame // the frame's raw GGPO input per channel
	proj          [2]uint16       // its buttons through the channel's setup
	setups        uint32          // sessionSetupsHash
	menuPlayer    int32
	meActive      bool
	meState       int32
	meNetDelay    int32
	meRematch     bool
	paused        bool
	postMatch     bool
	menuEsc       [2]int32 // menuChannels: frames Esc held
	menuEscTaken  [2]bool
	menuMouseGen  [2]uint16 // generation, received
	menuMouseRecv [2]uint16
	menuMouseTgt  [2]uint32
	vsMatches     int32
	vsWins        [2]int32
	uiFrame       int32
}

// sessionSetupsHash hashes the Button Setup of every netplay channel by
// what decides the buttons (the virtual controls behind each engine button),
// in channel order, so both peers get the same number for the same setups.
func sessionSetupsHash() uint32 {
	if len(sys.sessionSetups) == 0 {
		return 0
	}
	var crc uint32
	var b [4]byte
	put := func(v uint32) {
		binary.BigEndian.PutUint32(b[:], v)
		crc = crc32.Update(crc, crc32.IEEETable, b[:])
	}
	for ch := 0; ch < MaxPlayerNo; ch++ {
		setup, ok := sys.sessionSetups[ch]
		if !ok {
			continue
		}
		put(uint32(ch))
		for i, controls := range setup.controls {
			put(uint32(i)<<16 | uint32(len(controls)))
			for _, vc := range controls {
				put(uint32(vc))
			}
		}
	}
	return crc
}

// Buttons of a projected frame as a bit mask (up, down, ... menu).
func projectedMask(buttons [14]bool) uint16 {
	var m uint16
	for i, on := range buttons {
		if on {
			m |= 1 << uint(i)
		}
	}
	return m
}

func (rs *RollbackSession) recordChecksumSnap() {
	if rs.checksumHistory == nil {
		return
	}
	// SyncTest sessions have their own frame-by-frame comparison machinery;
	// only a real peer session exposes the frame the checksum is keyed to.
	frame := -1
	if p, ok := rs.backend.(*ggpo.Peer); ok {
		frame = p.CurrentFrame()
	}
	if frame < 0 {
		return
	}
	sn := &rs.checksumHistory[frame%checksumHistorySize]
	*sn = checksumSnap{} // clear stale slots from the frame this overwrites
	sn.frame = int32(frame)
	state := sys.roundState()
	sn.roundState = state
	sn.randseed = sys.randseed
	sn.matchTime = sys.matchTime
	sn.curRoundTime = sys.curRoundTime
	for i := range sys.scorePoints {
		sn.score[i] = sys.scorePoints[i]
		sn.combo[i] = sys.comboCount[i]
	}
	// RECORD CHARACTER STATE AT EVERY ROUND STATE, not only 2 and 3.
	//
	// LiveChecksum itself only HASHES character data during 2/3, and this ring
	// originally mirrored that. The consequence showed up the first time a real
	// desync was captured: it fired at roundState 4, where the crc covers just
	// five system fields, so the dump carried no per-character lines at all and
	// could only say "the RNG seed diverged" - true, but the seed was the
	// messenger, not the culprit.
	//
	// The ring is a diagnostic, not the checksum: it is free to record MORE
	// than is hashed, and recording character state at 1 and 4 costs a handful
	// of array writes into an already-allocated slot. When a divergence shows
	// up in the seed at round-over, the accompanying state and anim numbers are
	// what turn "some RNG call differed" into "this character was in a
	// different state on the two machines".
	//
	// stateNo is new here for the same reason: animNo alone says what is being
	// drawn, while ss.no says which state the character is actually IN, which
	// is what decides whether a `random` trigger gets evaluated.
	for i := range sys.chars {
		if len(sys.chars[i]) == 0 {
			continue
		}
		c := sys.chars[i][0]
		sn.present[i] = true
		sn.life[i] = c.life
		sn.redLife[i] = c.redLife
		sn.dizzy[i] = c.dizzyPoints
		sn.guard[i] = c.guardPoints
		sn.power[i] = c.power
		sn.animNo[i] = c.animNo
		sn.stateNo[i] = c.ss.no
		// index 0 is the root; everything after it is a helper
		sn.helpers[i] = int32(len(sys.chars[i]) - 1)
	}
	sn.intro = sys.intro
	sn.winposetime = sys.winposetime
	sn.fadeDuration = sys.fightScreen.round.fadeOut.duration()
	sn.fadeoutStart = -sys.fightScreen.round.over_waittime - 2 -
		sys.fightScreen.round.overTime() + sn.fadeDuration
	// Drain the draw counters into this frame's slot. A rollback re-simulates
	// these frames and overwrites the slot, so what survives is the FINAL
	// accepted simulation - which is exactly what the peer compared against.
	sn.rngDraws = sys.rngDraws
	sn.rngByPlayer = sys.rngByPlayer
	sn.rngByMinus = sys.rngByMinus
	for ch := range sn.in {
		if ch < len(sys.rollback.ggpoInputs) {
			f := sys.rollback.ggpoInputs[ch]
			sn.in[ch] = f
			sn.proj[ch] = projectedMask(sys.channelSetup(ch).project(f.controller()))
		}
		mc := &sys.menuChannels[ch]
		sn.menuEsc[ch], sn.menuEscTaken[ch] = mc.escape, mc.escapeTaken
		sn.menuMouseGen[ch], sn.menuMouseRecv[ch] = mc.mouse.generation, mc.mouse.received
		sn.menuMouseTgt[ch] = mc.mouse.target
	}
	sn.setups = sessionSetupsHash()
	sn.menuPlayer = int32(sys.menuPlayer)
	me := &sys.motif.me
	sn.meActive, sn.meState, sn.meNetDelay, sn.meRematch = me.active, int32(me.state), int32(me.netDelay), me.rematch
	sn.paused, sn.postMatch = sys.paused, sys.postMatchFlg
	sn.vsMatches, sn.vsWins = sys.versusStats.Matches, sys.versusStats.Wins
	sn.uiFrame = sys.uiFrameCounter
	sys.rngDraws = 0
	sys.rngByPlayer = [MaxPlayerNo]int32{}
	sys.rngByMinus = [6]int32{}
}

func (r *RollbackSession) checksumSnapFor(frame int) *checksumSnap {
	if r.checksumHistory == nil || frame < 0 {
		return nil
	}
	sn := &r.checksumHistory[frame%checksumHistorySize]
	if int(sn.frame) != frame {
		return nil // ring has wrapped past it, or it was never recorded
	}
	return sn
}

// SHORT LINES ON PURPOSE. This is read off a screenshot of the warning screen,
// which does not wrap - the first field-captured dump ran off the right edge
// and lost a value. Nothing here should exceed roughly 40 characters.
func formatChecksumSnap(sn *checksumSnap) string {
	s := fmt.Sprintf("seed=%d rs%d", sn.randseed, sn.roundState)
	s += fmt.Sprintf("\nmt=%d crt=%d", sn.matchTime, sn.curRoundTime)
	s += fmt.Sprintf("\nsp=%.0f/%.0f cc=%d/%d",
		sn.score[0], sn.score[1], sn.combo[0], sn.combo[1])
	for i := 0; i < MaxPlayerNo; i++ {
		if !sn.present[i] {
			continue
		}
		// Two short lines per player rather than one long one, same reason.
		s += fmt.Sprintf("\np%d st=%d anim=%d life=%d",
			i+1, sn.stateNo[i], sn.animNo[i], sn.life[i])
		s += fmt.Sprintf("\np%d rl=%d dz=%d pw=%d hlp=%d",
			i+1, sn.redLife[i], sn.dizzy[i], sn.power[i], sn.helpers[i])
	}
	// MERGED onto the intro line, deliberately: the warning screen CLIPS at the
	// bottom and the rngm line below has never once been visible in a field
	// capture. A new line here would push this one off too. fs sits beside the
	// value it explains, and intro= has been fully visible in every dump so far.
	s += fmt.Sprintf("\nintro=%d wpt=%d fs=%d fd=%d",
		sn.intro, sn.winposetime, sn.fadeoutStart, sn.fadeDuration)
	s += fmt.Sprintf("\nrng=%d p=%d/%d",
		sn.rngDraws, sn.rngByPlayer[0], sn.rngByPlayer[1])
	// Draws split by statedef block: -4/-3/-2/-1/0, then draws with no working
	// char (engine-side, e.g. music or the round timeline).
	s += fmt.Sprintf("\nrngm %d/%d/%d/%d/%d eng=%d",
		sn.rngByMinus[0], sn.rngByMinus[1], sn.rngByMinus[2],
		sn.rngByMinus[3], sn.rngByMinus[4], sn.rngByMinus[5])
	return s
}

// The input and menu fields of a snapshot, one short line each.
func formatInputSnap(sn *checksumSnap) string {
	s := ""
	for ch := range sn.in {
		f := sn.in[ch]
		s += fmt.Sprintf("in%d b=%04x ax=%v pj=%04x\n", ch, f.Buttons, f.Axes, sn.proj[ch])
		s += fmt.Sprintf("mc%d esc=%d tk=%v gen=%d/%d tgt=%d\n", ch, sn.menuEsc[ch], sn.menuEscTaken[ch],
			sn.menuMouseGen[ch], sn.menuMouseRecv[ch], sn.menuMouseTgt[ch])
	}
	s += fmt.Sprintf("setups=%08x mp=%d ui=%d\n", sn.setups, sn.menuPlayer, sn.uiFrame)
	s += fmt.Sprintf("me=%v st=%d nd=%d rm=%v\n", sn.meActive, sn.meState, sn.meNetDelay, sn.meRematch)
	s += fmt.Sprintf("paused=%v post=%v vs=%d %d/%d", sn.paused, sn.postMatch, sn.vsMatches, sn.vsWins[0], sn.vsWins[1])
	return s
}

func (rs *RollbackSession) LiveChecksum() uint32 {
	// System variables. Check always
	buf := writeI32(sys.randseed)
	buf = append(buf, writeI32(sys.matchTime)...)
	buf = append(buf, writeI32(sys.curRoundTime)...)
	// THE ROUND TIMELINE. sys.intro drives roundState() outright - roundState is
	// a pure function of it (system.go) - and therefore decides when the round
	// ends and runNextRound()/resetRound() fire. Leaving it out meant the two
	// peers' timelines could drift apart SILENTLY, frame after frame, and the
	// checksum only noticed once one peer reset the round and moved
	// curRoundTime and scorePoints, which ARE hashed.
	//
	// That is why four captured desyncs all reported the identical values
	// (intro=-327/wpt=-332 against intro=80/wpt=45): those are fixed points in
	// the reset, not the moment of divergence. Even with the first-mismatch
	// guard, the dumps showed the two peers a whole ROUND apart and could
	// therefore name a symptom but never a cause.
	//
	// Hashing them makes the desync fire on the frame the timelines actually
	// part, where the two dumps differ by one frame instead of by a round.
	// Deliberate trade: a match now dies at the cause instead of surviving to
	// the end of the round - but it was already doomed at that point, since an
	// intro drift never converges.
	buf = append(buf, writeI32(sys.intro)...)
	buf = append(buf, writeI32(sys.winposetime)...)
	for i := range sys.scorePoints {
		buf = binary.BigEndian.AppendUint32(buf, math.Float32bits(sys.scorePoints[i]))
		buf = binary.BigEndian.AppendUint32(buf, uint32(sys.comboCount[i]))
	}
	// The input layer (modern-input): both peers turn every virtual frame into
	// buttons with the same Button Setups, and a pause or rematch menu is
	// open on both or on neither, for the same player. A setup edit in the
	// pause menu that reached one peer only, or a menu one peer opened alone,
	// would otherwise stay silent until it moved a character.
	buf = binary.BigEndian.AppendUint32(buf, sessionSetupsHash())
	buf = binary.BigEndian.AppendUint32(buf, uint32(sys.menuPlayer))
	if sys.motif.me.active {
		buf = append(buf, 1)
	} else {
		buf = append(buf, 0)
	}

	// Round start checks. Ensure both players have the same selection
	if sys.roundState() == 1 {
		// Stage
		stageHash := crc32.ChecksumIEEE([]byte(sys.stage.name))
		buf = binary.BigEndian.AppendUint32(buf, stageHash)

		// Characters
		for i := range sys.chars {
			if len(sys.chars[i]) == 0 {
				continue
			}
			c := sys.chars[i][0]
			nameHash := crc32.ChecksumIEEE([]byte(c.name))
			buf = binary.BigEndian.AppendUint32(buf, nameHash)
		}

		// CharGlobalInfo
		// Checking selected palette is a little overzealous and makes palette modules desync
		//for i := range sys.cgi {
		//	buf = binary.BigEndian.AppendUint32(buf, uint32(sys.cgi[i].palno))
		//}
	}

	// During fight checks
	// Checking life during intros may cause trouble with Turns mode life refill
	if sys.roundState() == 2 || sys.roundState() == 3 {
		// Character data
		for i := range sys.chars {
			if len(sys.chars[i]) == 0 {
				continue
			}
			c := sys.chars[i][0]
			buf = binary.BigEndian.AppendUint32(buf, uint32(c.life))
			buf = binary.BigEndian.AppendUint32(buf, uint32(c.redLife))
			buf = binary.BigEndian.AppendUint32(buf, uint32(c.dizzyPoints))
			buf = binary.BigEndian.AppendUint32(buf, uint32(c.guardPoints))
			buf = binary.BigEndian.AppendUint32(buf, uint32(c.power))
			buf = binary.BigEndian.AppendUint32(buf, uint32(c.animNo))
			//buf = binary.BigEndian.AppendUint32(buf, math.Float32bits(cc.pos[0])) // These might add float operation errors
			//buf = binary.BigEndian.AppendUint32(buf, math.Float32bits(cc.pos[1]))
			//buf = binary.BigEndian.AppendUint32(buf, math.Float32bits(cc.pos[2]))
		}
	}

	// Record the raw components under the same frame number this crc will be
	// keyed to, so a later desync event can print them (see checksumSnap).
	rs.recordChecksumSnap()

	return crc32.ChecksumIEEE(buf)
}

// Both games on one PC can start in the same second, so name the logs after the player too.
func (rs *RollbackSession) setLogPlayer(player int) {
	rs.timestamp = fmt.Sprintf("%s_P%d", rs.timestamp, player)
	rs.log = NewRollbackLogger(rs.timestamp)
}

func (rs *RollbackSession) InitP1(numPlayers int, localPort int, remotePort int, remoteIp string) error {
	rs.setLogPlayer(1)
	if rs.config.GgpoLogsEnabled {
		logFileName := fmt.Sprintf("save/logs/Rollback-%s.log", rs.timestamp)
		f, err := os.OpenFile(logFileName, os.O_CREATE|os.O_RDWR, 0666)
		if err != nil {
			panic(err)
		}
		logger := log.New(f, "Logger:", log.Ldate|log.Ltime|log.Lshortfile)
		ggpo.EnableLogs()
		ggpo.SetLogger(logger)
	}

	var inputSize int = virtualFrameBytes

	player := ggpo.NewLocalPlayer(20, 1)
	player2 := ggpo.NewRemotePlayer(20, 2, remoteIp, remotePort)
	rs.players = append(rs.players, player)
	rs.players = append(rs.players, player2)

	peer := ggpo.NewPeer(rs, localPort, numPlayers, inputSize)
	rs.backend = &peer

	// With no watcher to register this is platformGGPOTransport() as it was.
	if err := peer.InitializeConnection(rs.watchTransport(platformGGPOTransport())...); err != nil {
		return err
	}

	var handle ggpo.PlayerHandle
	result := peer.AddPlayer(&player, &handle)
	if result != nil {
		panic("panic")
	}
	rs.handles = append(rs.handles, handle)
	var handle2 ggpo.PlayerHandle
	result = peer.AddPlayer(&player2, &handle2)
	if result != nil {
		panic("panic")
	}
	rs.handles = append(rs.handles, handle2)
	rs.currentPlayer = int(handle)
	rs.currentPlayerHandle = handle
	rs.remotePlayerHandle = handle2
	rs.addWatchers(&peer)

	peer.SetDisconnectTimeout(rs.config.DisconnectTimeout)
	peer.SetDisconnectNotifyStart(rs.config.DisconnectNotifyStart)
	peer.SetFrameDelay(handle, rs.config.FrameDelay)

	peer.Start()
	return nil
}

func (rs *RollbackSession) InitP2(numPlayers int, localPort int, remotePort int, remoteIp string) error {
	rs.setLogPlayer(2)
	if rs.config.GgpoLogsEnabled {
		logFileName := fmt.Sprintf("save/logs/Rollback-%s.log", rs.timestamp)
		f, err := os.OpenFile(logFileName, os.O_CREATE|os.O_RDWR, 0666)
		if err != nil {
			panic(err)
		}
		logger := log.New(f, "Logger:", log.Ldate|log.Ltime|log.Lshortfile)
		ggpo.EnableLogs()
		ggpo.SetLogger(logger)
	}

	var inputSize int = virtualFrameBytes

	player := ggpo.NewRemotePlayer(20, 1, remoteIp, remotePort)
	player2 := ggpo.NewLocalPlayer(20, 2)
	rs.players = append(rs.players, player)
	rs.players = append(rs.players, player2)

	peer := ggpo.NewPeer(rs, localPort, numPlayers, inputSize)
	rs.backend = &peer

	if err := peer.InitializeConnection(platformGGPOTransport()...); err != nil {
		return err
	}

	var handle ggpo.PlayerHandle
	result := peer.AddPlayer(&player, &handle)
	if result != nil {
		panic("panic")
	}
	rs.handles = append(rs.handles, handle)
	var handle2 ggpo.PlayerHandle
	result = peer.AddPlayer(&player2, &handle2)
	if result != nil {
		panic("panic")
	}
	rs.handles = append(rs.handles, handle2)
	rs.currentPlayer = int(handle2)
	rs.currentPlayerHandle = handle2
	rs.remotePlayerHandle = handle

	peer.SetDisconnectTimeout(rs.config.DisconnectTimeout)
	peer.SetDisconnectNotifyStart(rs.config.DisconnectNotifyStart)
	peer.SetFrameDelay(handle2, rs.config.FrameDelay)

	peer.Start()
	return nil
}

func (rs *RollbackSession) InitSyncTest(numPlayers int) {
	rs.syncTest = true
	if rs.config.GgpoLogsEnabled {
		logFileName := fmt.Sprintf("save/logs/Rollback-Desync-Test-%s.log", rs.timestamp)
		f, err := os.OpenFile(logFileName, os.O_CREATE|os.O_RDWR, 0666)
		if err != nil {
			panic(err)
		}
		logger := log.New(f, "Logger:", log.Ldate|log.Ltime|log.Lshortfile)
		ggpo.EnableLogs()
		ggpo.SetLogger(logger)
	}

	var inputSize int = virtualFrameBytes

	player := ggpo.NewLocalPlayer(20, 1)
	player2 := ggpo.NewLocalPlayer(20, 2)
	rs.players = append(rs.players, player)
	rs.players = append(rs.players, player2)

	//peer := ggpo.NewPeer(sys.rollbackNetwork, localPort, numPlayers, inputSize)
	peer := ggpo.NewSyncTest(rs, numPlayers, rs.config.DesyncTestFrames, inputSize, true)
	rs.backend = &peer

	//
	peer.InitializeConnection(platformGGPOTransport()...)
	peer.Start()

	var handle ggpo.PlayerHandle
	result := peer.AddPlayer(&player, &handle)
	if result != nil {
		panic("panic")
	}
	rs.handles = append(rs.handles, handle)
	var handle2 ggpo.PlayerHandle
	result = peer.AddPlayer(&player2, &handle2)
	if result != nil {
		panic("panic")
	}
	rs.handles = append(rs.handles, handle2)

	peer.SetDisconnectTimeout(3000)
	peer.SetDisconnectNotifyStart(1000)
}
