package main

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"log"
	"math"
	"os"
	"strings"
	"time"

	ggpo "github.com/ikemen-engine/ggpo"
)

type RollbackSystem struct {
	session          *RollbackSession
	netConnection    *NetConnection
	ggpoInputs       []InputBits
	ggpoAnalogInputs [][6]int8
}

type RollbackProperties struct {
	FrameDelay            int  `ini:"FrameDelay" sync:"host"`
	DisconnectNotifyStart int  `ini:"DisconnectNotifyStart" sync:"host"`
	DisconnectTimeout     int  `ini:"DisconnectTimeout" sync:"host"`
	LogsEnabled           bool `ini:"LogsEnabled" sync:"host"`
	SaveStageData         bool `ini:"SaveStageData" sync:"host"`
	DesyncTest            bool `ini:"DesyncTest" sync:"host"`
	DesyncTestFrames      int  `ini:"DesyncTestFrames" sync:"host"`
	DesyncTestAI          bool `ini:"DesyncTestAI" sync:"host"`
}

// TODO: Merge with system.go
func (rs *RollbackSystem) hijackRunMatch() bool {
	// Shared part up until this point already handled by sys.runMatch()

	// Reset variables
	rs.ggpoInputs = make([]InputBits, 2)
	rs.ggpoAnalogInputs = make([][6]int8, 2)

	// Initialize rollback network session and synchronize state
	rs.preMatchSetup()

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

		// These are outside runFrame(), so they won't be needlessly executed during a rollback
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

func (rs *RollbackSystem) preMatchSetup() {
	if rs.session != nil && sys.netConnection != nil {
		if rs.session.host != "" {
			// Initialize client as P2
			rs.session.InitP2(2, 7550, 7600, rs.session.host)
			rs.session.playerNo = 2
		} else {
			// Initialize host as P1
			rs.session.InitP1(2, 7600, 7550, rs.session.remoteIp)
			rs.session.playerNo = 1
		}

		// Synchronize matchTime at match start
		sys.matchTime = rs.session.netTime //s.time = rs.session.netTime // Old typo?
		sys.preMatchTime = sys.netConnection.preMatchTime

		// Wait until both peers have fully synchronized?
		//if !rs.session.IsConnected() {
		//	for !rs.session.synchronized {
		//		rs.session.backend.Idle(0)
		//	}
		//}
		//sys.netConnection.Close()

		// Borrow netConnection replay recording
		rs.session.recording = sys.netConnection.recording

		// Transfer the active netConnection to the rollback system
		rs.netConnection = sys.netConnection
		sys.netConnection = nil

	} else if sys.netConnection == nil && rs.session == nil {
		// If offline, initialize a local rollback sync test session
		session := NewRollbackSession(sys.cfg.Netplay.Rollback)
		rs.session = &session
		rs.session.InitSyncTest(2)
	}

	// Reset rollback session timer
	rs.session.netTime = 0
}

func (rs *RollbackSystem) postMatchSetup() {
	rs.session.SaveReplay()

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

	if sys.esc {
		return false
	}

	// Surface the connection quality once a second (browser overlay ping
	// counter; no-op on desktop).
	if rs.session != nil && rs.session.backend != nil && sys.gameTime()%60 == 0 {
		if stats, err := rs.session.backend.GetNetworkStats(rs.session.remotePlayerHandle); err == nil {
			platformNetStats(int(stats.Network.Ping))
		}
	}

	if rs.session.syncTest && rs.session.netTime == 0 {
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
	} else {
		buffer = rs.getInputs(0)
		ggpoerr = rs.session.backend.AddLocalInput(rs.session.currentPlayerHandle, buffer, len(buffer))
	}

	if ggpoerr == nil {
		// Get speculative inputs for the local player for this frame
		var values [][]byte
		disconnectFlags := 0
		values, ggpoerr = rs.session.backend.SyncInput(&disconnectFlags)

		if ggpoerr == nil {
			rs.ggpoInputs, rs.ggpoAnalogInputs = decodeInputs(values)

			// Record inputs against the rollback timeline. netTime is part of the saved state,
			// so any confirmed re-simulation overwrites earlier speculative data for the same frame.
			if rs.session.recording != nil {
				rs.session.RecordReplayFrame(rs.session.netTime, rs.ggpoInputs, rs.ggpoAnalogInputs)
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
			if err != nil {
				panic(err)
			}
			if sys.esc || !keepRunning {
				return false
			}
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

func getAIInputs(player int) []byte {
	var ib InputBits
	ib.SetInputAI(player)
	return append(encodeInputs(ib), make([]byte, 6)...)
}

func (ib *InputBits) SetInputAI(in int) {
	*ib = InputBits(Btoi(sys.aiInput[in].U()) |
		Btoi(sys.aiInput[in].D())<<1 |
		Btoi(sys.aiInput[in].L())<<2 |
		Btoi(sys.aiInput[in].R())<<3 |
		Btoi(sys.aiInput[in].a())<<4 |
		Btoi(sys.aiInput[in].b())<<5 |
		Btoi(sys.aiInput[in].c())<<6 |
		Btoi(sys.aiInput[in].x())<<7 |
		Btoi(sys.aiInput[in].y())<<8 |
		Btoi(sys.aiInput[in].z())<<9 |
		Btoi(sys.aiInput[in].s())<<10 |
		Btoi(sys.aiInput[in].d())<<11 |
		Btoi(sys.aiInput[in].w())<<12 |
		Btoi(sys.aiInput[in].m())<<13)
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

func decodeInputs(buffer [][]byte) ([]InputBits, [][6]int8) {
	var inputs = make([]InputBits, len(buffer))
	var analogInputs = make([][6]int8, len(buffer))
	for i, b := range buffer {
		inputs[i] = InputBits(readI16(b))
		for j := 0; j < len(analogInputs[i]); j++ {
			if len(b) < 8 {
				analogInputs[i][j] = 0
			} else {
				analogInputs[i][j] = int8(b[2+j])
			}
		}
	}
	return inputs, analogInputs
}

func writeI16(i16 int16) []byte {
	b := []byte{byte(i16), byte((i16 >> 8) & 0xFF)}
	return b
}

func writeI32(i32 int32) []byte {
	b := []byte{byte(i32), byte(i32 >> 8), byte(i32 >> 16), byte(i32 >> 24)}
	return b
}

func (rs *RollbackSystem) getInputs(player int) []byte {
	// Digital inputs
	var ib InputBits
	ib.KeysToBits(rs.netConnection.buf[player].InputReader.LocalInput(0))
	bytes := writeI16(int16(ib))

	// Analog inputs
	sbyteAxes := rs.netConnection.buf[player].InputReader.LocalAnalogInput(0)
	for i := 0; i < len(sbyteAxes); i++ {
		bytes = append(bytes, byte(sbyteAxes[i]))
	}

	return bytes
}

func (rs *RollbackSystem) readRollbackInput(controller int) [14]bool {
	if controller < 0 || controller >= len(sys.inputRemap) {
		return [14]bool{}
	}

	remap := sys.inputRemap[controller]
	if remap < 0 || remap >= len(rs.ggpoInputs) {
		return [14]bool{}
	}

	return rs.ggpoInputs[remap].BitsToKeys()
}

func (rs *RollbackSystem) readRollbackInputAnalog(controller int) [6]int8 {
	if controller < 0 || controller >= len(sys.inputRemap) {
		return [6]int8{}
	}

	remap := sys.inputRemap[controller]
	if remap < 0 || remap >= len(rs.ggpoInputs) {
		return [6]int8{}
	}

	return rs.ggpoAnalogInputs[remap]
}

func (rs *RollbackSystem) anyButton() bool {
	for _, b := range rs.ggpoInputs {
		if b&IB_anybutton != 0 {
			return true
		}
	}
	return false
}

type RollbackLogger struct {
	filename   string
	currentLog strings.Builder
	logs       [(MaxSaveStates * 2) + 3]string
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
	if sys.rollback.session == nil || !sys.rollback.session.config.LogsEnabled {
		return
	}
	var inputs [2]InputBits
	copy(inputs[:], sys.rollback.ggpoInputs)
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
	if sys.rollback.session == nil || !sys.rollback.session.config.LogsEnabled {
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
	// Header indicating the number of frames captured
	fullLog := fmt.Sprintf("Writing save state data from the last %d frames.\n\n", len(g.logs))
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
	inputs              map[int][MaxPlayerNo]InputBits
	analogInputs        map[int][MaxPlayerNo][6]int8
	config              RollbackProperties
	log                 RollbackLogger
	timestamp           string
	checksumHistory     []checksumSnap
	netTime             int32
	replayInputs        [][REPLAY_NUM_INPUTS]InputBits
	replayAnalogInputs  [][REPLAY_NUM_INPUTS][6]int8
	lastConfirmedInput  [MaxPlayerNo]InputBits
	inputBits           []InputBits
	inRollback          bool
	replaySaved         bool
}

func (rs *RollbackSession) SetInput(time int32, player int, input InputBits, axes [6]int8) {
	if _, ok := rs.inputs[int(time)]; !ok {
		rs.inputs[int(time)] = [MaxPlayerNo]InputBits{}
		rs.analogInputs[int(time)] = [MaxPlayerNo][6]int8{}
	}
	inputArr := rs.inputs[int(time)]
	analogInputs := rs.analogInputs[int(time)]
	inputArr[player] = input
	analogInputs[player] = axes
	rs.inputs[int(time)] = inputArr
	rs.analogInputs[int(time)] = analogInputs
}

func (rs *RollbackSession) ensureReplayFrame(frame int) {
	if frame < len(rs.replayInputs) {
		return
	}
	if frame > len(rs.replayInputs) {
		log.Printf("Rollback replay frame gap detected: expected frame %d, got %d; padding missing frames with zeros", len(rs.replayInputs), frame)
	}
	missing := frame - len(rs.replayInputs) + 1
	rs.replayInputs = append(rs.replayInputs, make([][REPLAY_NUM_INPUTS]InputBits, missing)...)
	rs.replayAnalogInputs = append(rs.replayAnalogInputs, make([][REPLAY_NUM_INPUTS][6]int8, missing)...)
}

func (rs *RollbackSession) RecordReplayFrame(time int32, inputs []InputBits, axes [][6]int8) {
	if rs == nil || time < 0 {
		return
	}
	frame := int(time)
	rs.ensureReplayFrame(frame)

	// Always clear the frame first so any overwritten rollback frame keeps zeroes in slots not present in the current input payload.
	rs.replayInputs[frame] = [REPLAY_NUM_INPUTS]InputBits{}
	rs.replayAnalogInputs[frame] = [REPLAY_NUM_INPUTS][6]int8{}

	for i := 0; i < REPLAY_NUM_INPUTS; i++ {
		if i < len(inputs) {
			rs.replayInputs[frame][i] = inputs[i]
		}

		if i < len(axes) {
			rs.replayAnalogInputs[frame][i] = axes[i]
		}
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
	if frame < len(rs.replayAnalogInputs) {
		rs.replayAnalogInputs = rs.replayAnalogInputs[:frame]
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

	if len(rs.replayInputs) < frameCount || len(rs.replayAnalogInputs) < frameCount {
		frameCount = len(rs.replayInputs)
		if len(rs.replayAnalogInputs) < frameCount {
			frameCount = len(rs.replayAnalogInputs)
		}
		log.Printf("Missing rollback replay frames after frame %d; truncating replay at this point", frameCount)
	}

	for frame := 0; frame < frameCount; frame++ {
		inputFrame := rs.replayInputs[frame]
		axisFrame := rs.replayAnalogInputs[frame]
		for i := 0; i < REPLAY_NUM_INPUTS; i++ {
			if err := writeReplayInput(rs.recording, inputFrame[i], axisFrame[i]); err != nil {
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
	if _, ok := sys.arenaSaveMap[oldest]; ok {
		sys.arenaSaveMap[oldest].Free()
		sys.arenaSaveMap[oldest] = nil
		delete(sys.arenaSaveMap, oldest)
	}
	sys.savePool.Free(oldest)
	if _, ok := sys.arenaSaveMap[stateIdx]; ok {
		sys.arenaSaveMap[stateIdx].Free()
		sys.arenaSaveMap[stateIdx] = nil
		delete(sys.arenaSaveMap, stateIdx)
	}
	sys.savePool.Free(stateIdx)

	r.saveStates[stateIdx] = sys.statePool.gameStatePool.Get().(*GameState)
	r.saveStates[stateIdx].SaveState(stateIdx)

	if r.config.DesyncTest {
		if r.config.LogsEnabled {
			r.log.logState("Saving", stateIdx, r.saveStates[stateIdx])
			r.log.updateStateLogs()
		}
		return r.saveStates[stateIdx].Checksum()
	} else {
		if r.config.LogsEnabled {
			r.log.logState("Saving", stateIdx, r.saveStates[stateIdx])
			r.log.updateStateLogs()
		}
		return ggpo.DefaultChecksum
	}
}

var lastLoadedFrame int = -1

func (r *RollbackSession) LoadGameState(stateIdx int) {
	sys.loadPool.curStateID = stateIdx
	if _, ok := sys.arenaLoadMap[stateIdx]; ok {
		sys.arenaLoadMap[stateIdx].Free()
		sys.arenaLoadMap[stateIdx] = nil
		delete(sys.arenaLoadMap, stateIdx)
	}
	for sid := range sys.arenaLoadMap {
		if sid != lastLoadedFrame {
			sys.arenaLoadMap[sid].Free()
			sys.arenaLoadMap[sid] = nil
			delete(sys.arenaLoadMap, sid)
		}
	}
	sys.loadPool.Free(stateIdx)

	r.saveStates[stateIdx].LoadState(stateIdx)

	if r.config.DesyncTest && r.config.LogsEnabled {
		r.log.logState("Loading", stateIdx, r.saveStates[stateIdx])
	}

	sys.statePool.gameStatePool.Put(r.saveStates[stateIdx])

	lastLoadedFrame = stateIdx
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
		sys.rollback.ggpoInputs, sys.rollback.ggpoAnalogInputs = decodeInputs(inputs)
		if r.recording != nil {
			r.RecordReplayFrame(r.netTime, sys.rollback.ggpoInputs, sys.rollback.ggpoAnalogInputs)
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
		if r.config.LogsEnabled {
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
		if r.config.LogsEnabled {
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

func NewRollbackSession(config RollbackProperties) RollbackSession {
	r := RollbackSession{}
	r.saveStates = make(map[int]*GameState)
	r.players = make([]ggpo.Player, 2)       // MaxPlayerNo
	r.handles = make([]ggpo.PlayerHandle, 2) // MaxPlayerNo
	r.config = config
	r.loopTimer = NewLoopTimer(60, 100)
	r.timestamp = time.Now().Format("2006-01-02_15-04-05")
	r.log = NewRollbackLogger(r.timestamp)
	r.replayInputs = make([][REPLAY_NUM_INPUTS]InputBits, 0)
	r.replayAnalogInputs = make([][REPLAY_NUM_INPUTS][6]int8, 0)
	r.inputs = make(map[int][MaxPlayerNo]InputBits)
	r.analogInputs = make(map[int][MaxPlayerNo][6]int8)
	// Frame -1 marks "never recorded" so ring slot 0 can't false-match frame 0.
	r.checksumHistory = make([]checksumSnap, checksumHistorySize)
	for i := range r.checksumHistory {
		r.checksumHistory[i].frame = -1
	}
	return r

}

func encodeInputs(inputs InputBits) []byte {
	return writeI16(int16(inputs))
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

func (rs *RollbackSession) Input(time int32, player int) (input InputBits) {
	inputs := rs.inputs[int(time)]
	input = inputs[player]
	return
}

func (rs *RollbackSession) AnyButton() bool {
	for i := 0; i < len(rs.inputs[len(rs.inputs)-1]); i++ {
		if rs.Input(sys.matchTime, i)&IB_anybutton != 0 {
			return true
		}
	}
	return false
}

func (rs *RollbackSession) InitP1(numPlayers int, localPort int, remotePort int, remoteIp string) {
	if rs.config.LogsEnabled {
		logFileName := fmt.Sprintf("save/logs/Rollback-%s.log", rs.timestamp)
		f, err := os.OpenFile(logFileName, os.O_CREATE|os.O_RDWR, 0666)
		if err != nil {
			panic(err)
		}
		logger := log.New(f, "Logger:", log.Ldate|log.Ltime|log.Lshortfile)
		ggpo.EnableLogs()
		ggpo.SetLogger(logger)
	}

	var inputBits InputBits = 0
	var inputAxes [6]int8 = [6]int8{}
	var inputSize int = len(encodeInputs(inputBits)) + len(inputAxes)

	player := ggpo.NewLocalPlayer(20, 1)
	player2 := ggpo.NewRemotePlayer(20, 2, remoteIp, remotePort)
	rs.players = append(rs.players, player)
	rs.players = append(rs.players, player2)

	peer := ggpo.NewPeer(rs, localPort, numPlayers, inputSize)
	rs.backend = &peer

	peer.InitializeConnection(platformGGPOTransport()...)

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

	peer.SetDisconnectTimeout(rs.config.DisconnectTimeout)
	peer.SetDisconnectNotifyStart(rs.config.DisconnectNotifyStart)
	peer.SetFrameDelay(handle, rs.config.FrameDelay)

	peer.Start()
}

func (rs *RollbackSession) InitP2(numPlayers int, localPort int, remotePort int, remoteIp string) {
	if rs.config.LogsEnabled {
		logFileName := fmt.Sprintf("save/logs/Rollback-%s.log", rs.timestamp)
		f, err := os.OpenFile(logFileName, os.O_CREATE|os.O_RDWR, 0666)
		if err != nil {
			panic(err)
		}
		logger := log.New(f, "Logger:", log.Ldate|log.Ltime|log.Lshortfile)
		ggpo.EnableLogs()
		ggpo.SetLogger(logger)
	}

	var inputBits InputBits = 0
	var inputAxes [6]int8 = [6]int8{}
	var inputSize int = len(encodeInputs(inputBits)) + len(inputAxes)

	player := ggpo.NewRemotePlayer(20, 1, remoteIp, remotePort)
	player2 := ggpo.NewLocalPlayer(20, 2)
	rs.players = append(rs.players, player)
	rs.players = append(rs.players, player2)

	peer := ggpo.NewPeer(rs, localPort, numPlayers, inputSize)
	rs.backend = &peer

	peer.InitializeConnection(platformGGPOTransport()...)

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
}

func (rs *RollbackSession) InitSyncTest(numPlayers int) {
	rs.syncTest = true
	if rs.config.LogsEnabled {
		logFileName := fmt.Sprintf("save/logs/Rollback-Desync-Test-%s.log", rs.timestamp)
		f, err := os.OpenFile(logFileName, os.O_CREATE|os.O_RDWR, 0666)
		if err != nil {
			panic(err)
		}
		logger := log.New(f, "Logger:", log.Ldate|log.Ltime|log.Lshortfile)
		ggpo.EnableLogs()
		ggpo.SetLogger(logger)
	}

	var inputBits InputBits = 0
	var inputAxes [6]int8 = [6]int8{}
	var inputSize int = len(encodeInputs(inputBits)) + len(inputAxes)

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
