package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// A projectile's animation is simulation state, so it steps in the simulation.
//
// WHY THIS EXISTS. Projectile collision reads the animation's CURRENT element
// (projClsnCheckSingle and tradeDetection in char.go, ProjVar AnimElem in
// bytecode.go), but the element was advanced only in Projectile.cueDraw(),
// the draw path. Two things never draw:
//
//   - rollback re-simulation: RollbackSession.AdvanceFrame runs only
//     simulateFrame(); sys.cueDraw() is called outside it (rollback.go);
//   - a skipped render frame: System.cueDraw() returns early on sys.frameSkip,
//     which await() sets from the wall clock (system.go).
//
// So after a rollback, or on a machine that stalled, a projectile tested an
// older Clsn box than on the peer: a hit on one side, a miss on the other.
// The step now lives in Projectile.tick().
//
// HOW THE ENGINE IS DRIVEN. System.action() needs a loaded stage, fight screen
// and characters, which a unit test does not have. projAnimRun.action() below
// is that function reduced to the calls a projectile takes part in, in the
// same order and under the same gates, and every one of those calls is the
// real one: sys.charUpdate(), sys.globalCollision(), sys.globalTick(),
// sys.addFrameTime(), sys.cueDraw(). Projectiles are made with the real
// spawnProjectile()/commitProjectile() from animations read by the real AIR
// parser, they trade through the real tradeDetection(), and the saved state
// in T1 goes through the real GameState.SaveState()/LoadState(), the pair
// RollbackSession.SaveGameState()/LoadGameState() call.
//
// THE GOLDEN SEQUENCES at the end of this file were GENERATED, not written by
// hand: this same file was copied into a checkout of the engine as it was
// before the change (commit f76237c) and run there with PROJ_ANIM_PRINT=1,
// which prints every sequence as Go source. The "pre-split" sequences come
// from the same run with cueDraw() called where it was before upstream
// c3ac399 "split cueDraw from action" (inside action(), after the tick);
// Projectile.cueDraw/tick/update are byte-identical between c3ac399^ and
// f76237c, so that reproduces the old placement exactly. Only the "Now"
// sequences of T4 were printed by the engine with the change: they pin what
// it draws in slow motion.

// projAnimTex is a texture that is never drawn: it only makes a sprite count as
// not blank, so that DrawList.add() keeps it.
type projAnimTex struct{}

func (*projAnimTex) SetData(data []byte)                                         {}
func (*projAnimTex) SetSubData(data []byte, x, y, width, height, stride int32)   {}
func (*projAnimTex) SetDataG(data []byte, mag, min, ws, wt TextureSamplingParam) {}
func (*projAnimTex) SetPixelData(data []float32)                                 {}
func (*projAnimTex) IsValid() bool                                               { return true }
func (*projAnimTex) GetWidth() int32                                             { return 4 }
func (*projAnimTex) GetHeight() int32                                            { return 4 }
func (*projAnimTex) CopyData(src *Texture)                                       {}

// Action 100 is the projectile under test: 5 elements with different times
// (3, 2, 4, 1, 2), a loop start at element 2, and a different Clsn1 and Clsn2
// on every element. Only element 4 reaches the other projectile (Clsn2 right
// edge 120 against a box at 90..110), and only elements 3, 4 and 5 reach the
// target character (Clsn1 right edge 28, 98, 38 against a box at 20..40).
// Element 4 is interpolated into, so the drawn offset changes every tick of
// element 3.
const projAnimAir = `
[Begin Action 100]
Clsn1: 1
 Clsn1[0] = -8,-8,8,8
Clsn2: 1
 Clsn2[0] = -10,-10,10,10
10,0, 0,0, 3
Loopstart
Clsn1: 1
 Clsn1[0] = -8,-8,18,8
Clsn2: 1
 Clsn2[0] = -10,-10,20,10
10,1, 0,0, 2
Clsn1: 1
 Clsn1[0] = -8,-8,28,8
Clsn2: 1
 Clsn2[0] = -10,-10,30,10
10,2, 0,0, 4
Interpolate Offset
Clsn1: 1
 Clsn1[0] = -8,-8,98,8
Clsn2: 1
 Clsn2[0] = -10,-10,120,10
10,3, 8,0, 1
Clsn1: 1
 Clsn1[0] = -8,-8,38,8
Clsn2: 1
 Clsn2[0] = -10,-10,40,10
10,4, 0,0, 2

[Begin Action 110]
11,0, 0,0, 2
11,1, 0,0, 1
11,2, 0,0, 2

[Begin Action 200]
Clsn2Default: 1
 Clsn2[0] = -10,-10,10,10
20,0, 0,0, 3
20,1, 0,0, 3

[Begin Action 210]
21,0, 0,0, 2
21,1, 0,0, 2
21,2, 0,0, 2
`

// projAnimRun drives the fight loop and records, pass by pass, what collision
// read and what was cued for drawing.
type projAnimRun struct {
	script   func(n int) // character code of logic frame n (sys.matchTime)
	preSplit bool        // cue sprites inside action(), where c3ac399^ did
	col      []string    // one entry per collision pass
	colElem  []string    // the same for projectile 1 only: "e<elem>.<time>"
	drw      []string    // one entry per cued frame
	drwElem  []string    // the same for projectile 1 only: "d<elem>"
	gates    []string    // tickFrame/tickNextFrame of every pass
}

// withProjAnimWorld builds the smallest match a projectile can live in: two
// root characters that never run (the script stands in for their code), one
// empty stage, and the animations above. Everything it or a state load writes
// is put back afterwards.
func withProjAnimWorld(t *testing.T, f func(r *projAnimRun)) {
	t.Helper()
	oldVars, oldFrameCounter := sys.SystemStateVars, sys.frameCounter
	oldChars, oldProjs, oldCgi := sys.chars, sys.projs, sys.cgi
	oldExplods, oldTexts := sys.explods, sys.chartexts
	oldStage, oldStageList := sys.stage, sys.stageList
	oldFightScreen, oldMotif, oldStoryboard := sys.fightScreen, sys.motif, sys.storyboard
	oldCharList := sys.charList
	oldWC, oldWS, oldDebugWC := sys.workingChar, sys.workingState, sys.debugWC
	oldMax, oldClsn := sys.cfg.Config.ProjectileMax, sys.clsnDisplay
	oldBcStack, oldBcVarStack, oldBcVar := sys.bcStack, sys.bcVarStack, sys.bcVar
	oldWorkBe, oldWorkpal := sys.workBe, sys.workpal
	oldAllPalFX, oldBgPalFX := sys.allPalFX, sys.bgPalFX
	oldTimerRounds, oldScoreRounds, oldTimerCount := sys.timerRounds, sys.scoreRounds, sys.timerCount
	oldMusicSel, oldConsole := sys.matchMusicSel, sys.consoleText
	defer func() {
		sys.SystemStateVars, sys.frameCounter = oldVars, oldFrameCounter
		sys.chars, sys.projs, sys.cgi = oldChars, oldProjs, oldCgi
		sys.explods, sys.chartexts = oldExplods, oldTexts
		sys.stage, sys.stageList = oldStage, oldStageList
		sys.fightScreen, sys.motif, sys.storyboard = oldFightScreen, oldMotif, oldStoryboard
		sys.charList = oldCharList
		sys.workingChar, sys.workingState, sys.debugWC = oldWC, oldWS, oldDebugWC
		sys.cfg.Config.ProjectileMax, sys.clsnDisplay = oldMax, oldClsn
		sys.bcStack, sys.bcVarStack, sys.bcVar = oldBcStack, oldBcVarStack, oldBcVar
		sys.workBe, sys.workpal = oldWorkBe, oldWorkpal
		sys.allPalFX, sys.bgPalFX = oldAllPalFX, oldBgPalFX
		sys.timerRounds, sys.scoreRounds, sys.timerCount = oldTimerRounds, oldScoreRounds, oldTimerCount
		sys.matchMusicSel, sys.consoleText = oldMusicSel, oldConsole
		sys.clearSpriteData()
	}()

	sys.resetFrameTime()
	sys.turbo = 1
	sys.paused, sys.frameStepFlag, sys.frameSkip = false, false, false
	sys.supertime, sys.pausetime, sys.supertimebuffer, sys.pausetimebuffer = 0, 0, 0, 0
	sys.matchTime = 0
	sys.motif.me.active, sys.motif.ch.active = false, false
	sys.stage = newStage("")
	sys.stageList = map[int32]*Stage{}
	sys.xmin, sys.xmax, sys.zmin, sys.zmax = -1000, 1000, 0, 0
	sys.cam.XMin, sys.cam.XMax, sys.cam.Scale = -1000, 1000, 1
	sys.cam.Pos = [2]float32{}
	sys.gameWidth = 320
	sys.cfg.Config.ProjectileMax = 8
	sys.clsnDisplay = true
	sys.charList = CharList{}
	for i := range sys.chars {
		sys.chars[i], sys.projs[i], sys.explods[i], sys.chartexts[i] = nil, nil, nil, nil
	}

	sff := newSff()
	for _, gn := range [][2]uint16{{10, 0}, {10, 1}, {10, 2}, {10, 3}, {10, 4},
		{11, 0}, {11, 1}, {11, 2}, {20, 0}, {20, 1}, {21, 0}, {21, 1}, {21, 2}} {
		sff.sprites[gn] = &Sprite{Group: gn[0], Number: gn[1], Size: [2]uint16{4, 4},
			Tex: &projAnimTex{}, palidx: -1}
	}
	lines := strings.Split(projAnimAir, "\n")
	for pn := 0; pn < 2; pn++ {
		i := 0
		sys.cgi[pn] = CharGlobalInfo{}
		sys.cgi[pn].sff = sff
		sys.cgi[pn].palettedata = newPaldata()
		sys.cgi[pn].animTable = ReadAnimationTable("projectile_anim_sim_test.air", sff, &sff.palList, lines, &i, false)
		c := &Char{}
		c.playerNo, c.id, c.teamside = pn, int32(pn+1), pn
		c.localscl, c.localcoord, c.facing = 1, 320, 1
		c.clsnBaseScale, c.clsnScale = [2]float32{1, 1}, [2]float32{1, 1}
		c.palfx = newPalFX()
		c.minus = 3 // all tasks finished: getClsn() uses curFrame
		sys.chars[pn] = []*Char{c}
	}
	// The second root is also the target a projectile is checked against.
	target := sys.chars[1][0]
	target.pos = [3]float32{30, 0, 0}
	target.curFrame = &AnimFrame{Clsn2: [][4]float32{{-10, -40, 10, 0}}}

	f(&projAnimRun{})
}

// projAnimSpawn does what the Projectile state controller does (bytecode.go):
// spawnProjectile(), the parameters, then commitProjectile().
func projAnimSpawn(pn int, id, anim int32, offx float32, set func(p *Projectile)) {
	c := sys.chars[pn][0]
	p := c.spawnProjectile()
	p.id = id
	p.animNo = anim
	if set != nil {
		set(p)
	}
	c.commitProjectile(p, PT_P1, offx, 0, 0, false, -1, 0, false)
}

// projAnimScenario is the match T1, T2 and T3 play.
//
//	frame  1  both projectiles are created (the creation frame)
//	frame 10  projectile 1 reaches element 4 and trades with projectile 2:
//	          it keeps 2 of 3 hits and gets a hitpause of 5, the other one is
//	          canceled and changes to its cancel animation (an animation change)
//	frame 26  a SuperPause of 6 is requested (the pause stretch)
//	later     projectile 1 runs out of removetime and changes to its remove
//	          animation, then is removed
func projAnimScenario(n int) {
	switch n {
	case 1:
		projAnimSpawn(0, 1, 100, 0, func(p *Projectile) {
			p.hits = 3
			p.hitdef.pausetime = [2]int32{5, 0}
			p.remanim = 110
			p.removetime = 46
		})
		projAnimSpawn(1, 2, 200, 70, func(p *Projectile) {
			p.cancelanim = 210
		})
	case 26:
		// Char.setSuperPauseTime()
		sys.supertimebuffer = ^int32(6)
		sys.superplayerno = 0
	}
}

// action is System.action() (system.go) reduced to what a projectile takes
// part in. Order and gates are the engine's.
func (r *projAnimRun) action() {
	if sys.matchPaused() {
		return
	}

	// Run "tick frame"
	if sys.tickFrame() {
		// Step pause timers
		if sys.supertime > 0 {
			sys.supertime--
		} else if sys.pausetime > 0 {
			sys.pausetime--
		}
		// Start pause timers
		if sys.supertimebuffer < 0 {
			sys.supertimebuffer = ^sys.supertimebuffer
			sys.supertime = sys.supertimebuffer
		}
		if sys.pausetimebuffer < 0 {
			sys.pausetimebuffer = ^sys.pausetimebuffer
			sys.pausetime = sys.pausetimebuffer
		}
		// s.charList.action(): character code runs here
		if r.script != nil {
			r.script(int(sys.matchTime))
		}
	}

	// This function runs every tick
	sys.charUpdate()

	if sys.tickNextFrame() {
		before := r.readCollision()
		sys.globalCollision()
		r.noteCollision(before)
		sys.globalTick()
	}
}

// simulate is what RollbackSystem.simulateFrame() does, and the first half of
// a pass through the offline loop in System.runMatch(): the game logic, then
// addFrameTime(). In the old placement the sprites were cued inside action().
func (r *projAnimRun) simulate() bool {
	gate := [2]byte{'-', '-'}
	if sys.tickFrame() {
		gate[0] = 'F'
	}
	if sys.tickNextFrame() {
		gate[1] = 'N'
	}
	r.gates = append(r.gates, string(gate[:]))
	collided := len(r.colElem)
	r.action()
	if len(r.colElem) == collided {
		r.colElem = append(r.colElem, "-") // no collision on this pass
	}
	if r.preSplit {
		sys.cueDraw()
		r.noteDrawn()
	}
	return sys.addFrameTime(sys.turbo)
}

// draw is the second half of a pass: sys.cueDraw(), then the render (which
// only reads). cueDraw() itself returns early on sys.frameSkip.
func (r *projAnimRun) draw() {
	if r.preSplit {
		return
	}
	sys.cueDraw()
	if sys.frameSkip {
		r.drw = append(r.drw, "skipped")
		r.drwElem = append(r.drwElem, "skip")
		return
	}
	r.noteDrawn()
}

// readCollision is taken right before sys.globalCollision(): the element and
// time collision is about to read, its first Clsn1 box, and the result of the
// real check against the target (the call hitDetectionProjectile makes).
func (r *projAnimRun) readCollision() []string {
	var out []string
	target := sys.chars[1][0]
	for pn := range sys.projs {
		for _, p := range sys.projs[pn] {
			s := fmt.Sprintf("P%d#%d", pn+1, p.id)
			if p.anim == nil || p.anim.CurrentFrame() == nil {
				out = append(out, s+" noanim")
				continue
			}
			frm := p.anim.CurrentFrame()
			s += fmt.Sprintf(" e%d.%d", p.anim.curelem+1, p.anim.curelemtime)
			if len(frm.Clsn1) > 0 {
				s += fmt.Sprintf(" c1[%g,%g,%g,%g]", frm.Clsn1[0][0], frm.Clsn1[0][1], frm.Clsn1[0][2], frm.Clsn1[0][3])
			} else {
				s += " c1[]"
			}
			s += fmt.Sprintf(" hit=%v", target.projClsnCheckSingle(p, 2, 1))
			out = append(out, s)
		}
	}
	return out
}

// noteCollision adds what the collision pass left behind: hits, hitpause and
// status. The projectile list does not change inside globalCollision().
func (r *projAnimRun) noteCollision(before []string) {
	elem := "x"
	k := 0
	for pn := range sys.projs {
		for _, p := range sys.projs[pn] {
			if p.id == 1 && p.anim != nil && p.anim.CurrentFrame() != nil {
				elem = fmt.Sprintf("e%d.%d", p.anim.curelem+1, p.anim.curelemtime)
			}
			before[k] += fmt.Sprintf(" > hits=%d hitpause=%d status=%d", p.hits, p.hitpause, p.status)
			k++
		}
	}
	r.col = append(r.col, fmt.Sprintf("f%d ", sys.matchTime)+strings.Join(before, "; "))
	r.colElem = append(r.colElem, elem)
}

// noteDrawn is taken right after sys.cueDraw(): for every projectile, the
// element the sprite shows (drawidx), the sprite, the interpolated offset and
// whether it reached the sprite list; then the boxes Clsn display would draw
// (they come from drawFrame()).
func (r *projAnimRun) noteDrawn() {
	var out []string
	elem := "-"
	for pn := range sys.projs {
		for _, p := range sys.projs[pn] {
			s := fmt.Sprintf("P%d#%d", pn+1, p.id)
			if p.anim == nil {
				out = append(out, s+" noanim")
				continue
			}
			cued := false
			for _, sd := range sys.spriteList {
				if sd.anim == p.anim {
					cued = true
				}
			}
			spr := "nil"
			if p.anim.spr != nil {
				spr = fmt.Sprintf("%d,%d", p.anim.spr.Group, p.anim.spr.Number)
			}
			s += fmt.Sprintf(" d%d spr=%s io=%g cued=%v", p.anim.drawidx+1, spr, p.anim.interpolate_offset_x, cued)
			if p.id == 1 && cued {
				elem = fmt.Sprintf("d%d", p.anim.drawidx+1)
			}
			out = append(out, s)
		}
	}
	box := func(name string, rects [][7]float32) {
		for _, b := range rects {
			out = append(out, fmt.Sprintf("%s[%g,%g,%g,%g@%g]", name, b[0], b[1], b[2], b[3], b[4]))
		}
	}
	box("clsn1", sys.debugc1hit.rects)
	box("clsn2", sys.debugc2hb.rects)
	r.drw = append(r.drw, strings.Join(out, "; "))
	r.drwElem = append(r.drwElem, elem)
}

// projAnimCompare reports every index where two sequences differ.
func projAnimCompare(t *testing.T, what, nameA, nameB string, a, b []string) int {
	t.Helper()
	diff := 0
	if len(a) != len(b) {
		t.Errorf("%s: %d entries in %s, %d in %s", what, len(a), nameA, len(b), nameB)
		diff++
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			diff++
			t.Errorf("%s, entry %d differs\n  %-10s %s\n  %-10s %s", what, i, nameA+":", a[i], nameB+":", b[i])
		}
	}
	return diff
}

func projAnimPrint(t *testing.T, name string, lines []string) {
	if os.Getenv("PROJ_ANIM_PRINT") == "" {
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "GOLDEN-BEGIN %s\nvar %s = []string{\n", name, name)
	for _, l := range lines {
		fmt.Fprintf(&sb, "\t%q,\n", l)
	}
	fmt.Fprintf(&sb, "}\nGOLDEN-END %s", name)
	t.Log(sb.String())
}

// T1. Re-simulation. Rollback saves right after a simulated frame (GGPO calls
// SaveGameState from backend.AdvanceFrame, before the frame is cued or
// rendered), later loads it and re-runs the frames with simulateFrame() only.
// Collision must read the same element on the re-run as it did live.
func TestProjectileAnimSurvivesResimulation(t *testing.T) {
	const saveAt, k = 4, 7
	withProjAnimWorld(t, func(r *projAnimRun) {
		r.script = projAnimScenario
		for n := 0; n < saveAt; n++ {
			r.simulate()
			r.draw()
		}
		r.simulate()
		gs := NewGameState()
		gs.SaveState(0)
		r.draw()

		mark := len(r.col)
		for i := 0; i < k; i++ {
			r.simulate()
			r.draw()
		}
		live := append([]string(nil), r.col[mark:]...)

		gs.LoadState(0)
		if got := int(sys.matchTime); got != saveAt+1 {
			t.Fatalf("the load did not restore the clock: matchTime %d, want %d", got, saveAt+1)
		}
		r.col = r.col[:mark]
		for i := 0; i < k; i++ {
			r.simulate() // no draw: this is all AdvanceFrame runs
		}
		resim := r.col[mark:]

		if len(live) != k || len(resim) != k {
			t.Fatalf("expected %d collision passes each, got %d live and %d re-simulated", k, len(live), len(resim))
		}
		// The control: the window must contain the trade, or equal sequences
		// would prove nothing.
		if !strings.Contains(strings.Join(live, "\n"), "P1#1 e4.0") || !strings.Contains(live[k-1], "hits=2 hitpause=") {
			t.Fatalf("the live run never reached the trade on element 4:\n%s", strings.Join(live, "\n"))
		}
		for _, l := range live {
			t.Log("live   " + l)
		}
		for _, l := range resim {
			t.Log("re-sim " + l)
		}
		if d := projAnimCompare(t, "collision read", "live", "re-sim", live, resim); d > 0 {
			t.Errorf("%d of %d re-simulated frames read a different projectile state than the live run: this is the desync", d, k)
		}
	})
}

// T2. A skipped render frame. await() sets sys.frameSkip from the wall clock
// at the end of a loop pass and the next pass does not cue. The simulation
// must not notice.
func TestProjectileAnimIgnoresFrameSkip(t *testing.T) {
	const frames = 12
	run := func(skip map[int]bool) []string {
		var col []string
		withProjAnimWorld(t, func(r *projAnimRun) {
			r.script = projAnimScenario
			for n := 0; n < frames; n++ {
				sys.frameSkip = skip[n]
				r.simulate()
				r.draw()
			}
			col = r.col
		})
		return col
	}
	never := run(nil)
	skipped := run(map[int]bool{5: true, 6: true, 8: true})
	if len(never) != frames {
		t.Fatalf("expected %d collision passes, got %d", frames, len(never))
	}
	if !strings.Contains(strings.Join(never, "\n"), "P1#1 e4.0") {
		t.Fatalf("the run never reached the trade on element 4:\n%s", strings.Join(never, "\n"))
	}
	for i := range never {
		t.Log("drawn every frame  " + never[i])
		if i < len(skipped) {
			t.Log("3 frames skipped   " + skipped[i])
		}
	}
	if d := projAnimCompare(t, "collision read", "all drawn", "3 skipped", never, skipped); d > 0 {
		t.Errorf("%d of %d frames read a different projectile state when 3 render frames were skipped", d, frames)
	}
}

// projAnimOfflineRecord plays the scenario at normal speed with every frame
// drawn and returns one line per frame: what collision read and what was cued.
func projAnimOfflineRecord(t *testing.T) []string {
	const frames = 72
	var out []string
	withProjAnimWorld(t, func(r *projAnimRun) {
		r.script = projAnimScenario
		for n := 0; n < frames; n++ {
			r.simulate()
			r.draw()
		}
		if len(r.col) != frames || len(r.drw) != frames {
			t.Fatalf("expected %d collision passes and cued frames, got %d and %d", frames, len(r.col), len(r.drw))
		}
		for n := 0; n < frames; n++ {
			out = append(out, r.col[n]+" || drawn "+r.drw[n])
		}
	})
	return out
}

// T3. Offline, at normal speed, nothing changes: 72 frames covering the
// creation frame, a hitpause of 5, a SuperPause, loop wraps and two animation
// changes must equal, frame by frame, what the engine did before the change,
// both in what collision reads and in what is drawn (sprite, drawn element,
// interpolated offset, Clsn display boxes).
func TestProjectileAnimOfflineEquivalence(t *testing.T) {
	got := projAnimOfflineRecord(t)
	projAnimPrint(t, "projAnimOfflineGolden", got)
	all := strings.Join(got, "\n")
	for _, must := range []string{
		"f1 P1#1 e1.0",            // the creation frame
		"hitpause=5",              // the trade
		"hitpause=1",              // the last frame of that hitpause
		"P2#2 d1 spr=21,0",        // the cancel animation
		"P1#1 d1 spr=11,0",        // the remove animation
		"P1#1 e5.1 c1[-8,-8,38,8", // the last tick before a loop wrap
		"io=6",                    // interpolation at the last tick of element 3
	} {
		if !strings.Contains(all, must) {
			t.Errorf("the recording does not cover %q", must)
		}
	}
	if n := strings.Count(all, "P1#1 e2.0 "); n < 3 {
		t.Errorf("the recording wraps back to element 2 only %d times, want at least 2 wraps", n-1)
	}
	if d := projAnimCompare(t, "frame", "before", "now", projAnimOfflineGolden, got); d > 0 {
		t.Errorf("%d of %d frames differ from the engine before the change", d, len(projAnimOfflineGolden))
	}
}

// projAnimSlowRecord runs one projectile for a number of loop passes at a
// fixed sys.turbo and returns one token per pass:
// <gates>:<what collision read>:<what was cued>.
func projAnimSlowRecord(t *testing.T, turbo float32, passes, bornAt int, preSplit bool) (tokens, col []string) {
	withProjAnimWorld(t, func(r *projAnimRun) {
		r.preSplit = preSplit
		r.script = func(n int) {
			if n == bornAt {
				projAnimSpawn(0, 1, 100, 0, func(p *Projectile) { p.hits = 3 })
			}
		}
		sys.turbo = turbo
		for i := 0; i < passes; i++ {
			if r.simulate() {
				r.draw()
			} else {
				// runMatch `continue`s here: a catch-up pass is not drawn
				r.drw = append(r.drw, "not drawn")
				r.drwElem = append(r.drwElem, "none")
			}
		}
		for i := 0; i < passes; i++ {
			tokens = append(tokens, r.gates[i]+":"+r.colElem[i]+":"+r.drwElem[i])
		}
		col = r.col
	})
	return
}

// T4. Slow motion (sys.turbo 0.25, the KO slowdown). A logic frame is spread
// over 4 loop passes: character code on the first, collision and tick on the
// last. Before the change the step was taken while cueing, one pass BEFORE the
// collision pass, so a projectile BORN in slow motion was tested one tick
// ahead of the same projectile at normal speed, and of the engine before
// upstream c3ac399. One already in flight when the slowdown started was not.
// With the step in tick() the element collision reads on logic frame n is the
// same at every speed. The sprite now changes on the tick pass, the last of
// the 4, where c3ac399^ changed it on the first. This test pins all of that
// and prints the three engines side by side.
func TestProjectileAnimSlowMotion(t *testing.T) {
	const passes = 40
	for _, v := range []struct {
		name, id            string
		bornAt              int
		before, split, want []string
	}{
		{"born in slow motion", "BornSlow", 1, projAnimSlowBornSlowBefore, projAnimSlowBornSlowPreSplit, projAnimSlowBornSlowNow},
		{"born at normal speed", "BornFast", 0, projAnimSlowBornFastBefore, projAnimSlowBornFastPreSplit, projAnimSlowBornFastNow},
	} {
		t.Run(v.name, func(t *testing.T) {
			got, slowCol := projAnimSlowRecord(t, 0.25, passes, v.bornAt, false)
			projAnimPrint(t, "projAnimSlow"+v.id+"Run", got)
			if os.Getenv("PROJ_ANIM_PRINT") != "" {
				split, _ := projAnimSlowRecord(t, 0.25, passes, v.bornAt, true)
				projAnimPrint(t, "projAnimSlow"+v.id+"PreSplitRun", split)
			}

			part := func(tok string, i int) string { return strings.Split(tok, ":")[i] }
			t.Logf("%-5s %-6s | %-14s | %-14s | %-14s", "pass", "gates", "before (f76237c)", "pre-split", "now")
			colDiffBefore, drwDiffBefore, colDiffSplit, drwDiffSplit := 0, 0, 0, 0
			for i := 0; i < passes && i < len(got) && i < len(v.before) && i < len(v.split); i++ {
				b, s, g := v.before[i], v.split[i], got[i]
				mark := ""
				if part(b, 1) != part(g, 1) {
					colDiffBefore++
					mark += " collision differs from before;"
				}
				if part(b, 2) != part(g, 2) {
					drwDiffBefore++
					mark += " drawn differs from before;"
				}
				if part(s, 1) != part(g, 1) {
					colDiffSplit++
					mark += " collision differs from pre-split;"
				}
				if part(s, 2) != part(g, 2) {
					drwDiffSplit++
					mark += " drawn differs from pre-split;"
				}
				t.Logf("%-5d %-6s | %-6s %-7s | %-6s %-7s | %-6s %-7s |%s", i, part(g, 0),
					part(b, 1), part(b, 2), part(s, 1), part(s, 2), part(g, 1), part(g, 2), mark)
			}
			t.Logf("over %d passes: collision differs from before on %d, drawn on %d; collision differs from pre-split on %d, drawn on %d",
				passes, colDiffBefore, drwDiffBefore, colDiffSplit, drwDiffSplit)

			// The simulation must be the pre-c3ac399 one, pass for pass.
			if colDiffSplit != 0 {
				t.Errorf("collision read a different element than the pre-split engine on %d passes", colDiffSplit)
			}
			// And it must not depend on the game speed: logic frame n reads
			// the same at 0.25 as at 1.
			_, fastCol := projAnimSlowRecord(t, 1, 10, v.bornAt, false)
			if len(slowCol) < 9 || len(fastCol) < len(slowCol) {
				t.Fatalf("expected at least 9 collision passes in slow motion and as many at normal speed, got %d and %d", len(slowCol), len(fastCol))
			}
			if d := projAnimCompare(t, "collision read by logic frame", "speed 1", "speed 0.25", fastCol[:len(slowCol)], slowCol); d > 0 {
				t.Errorf("%d of %d logic frames read a different element in slow motion than at normal speed", d, len(slowCol))
			}
			// Pin the whole slow-motion recording, drawn element included.
			projAnimCompare(t, "slow-motion pass", "pinned", "now", v.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// Generated sequences. See THE GOLDEN SEQUENCES at the top for how.
// ---------------------------------------------------------------------------

// What the engine before the change (f76237c) records for T3.
var projAnimOfflineGolden = []string{
	"f0  || drawn ",
	"f1 P1#1 e1.0 c1[-8,-8,8,8] hit=false > hits=3 hitpause=0 status=0; P2#2 e1.0 c1[] hit=false > hits=1 hitpause=0 status=0 || drawn P1#1 d1 spr=10,0 io=0 cued=true; P2#2 d1 spr=20,0 io=0 cued=true; clsn1[-8,-8,16,16@0]; clsn2[-10,-10,20,20@0]; clsn2[-10,-10,20,20@100]",
	"f2 P1#1 e1.1 c1[-8,-8,8,8] hit=false > hits=3 hitpause=0 status=0; P2#2 e1.1 c1[] hit=false > hits=1 hitpause=0 status=0 || drawn P1#1 d1 spr=10,0 io=0 cued=true; P2#2 d1 spr=20,0 io=0 cued=true; clsn1[-8,-8,16,16@0]; clsn2[-10,-10,20,20@0]; clsn2[-10,-10,20,20@100]",
	"f3 P1#1 e1.2 c1[-8,-8,8,8] hit=false > hits=3 hitpause=0 status=0; P2#2 e1.2 c1[] hit=false > hits=1 hitpause=0 status=0 || drawn P1#1 d1 spr=10,0 io=0 cued=true; P2#2 d1 spr=20,0 io=0 cued=true; clsn1[-8,-8,16,16@0]; clsn2[-10,-10,20,20@0]; clsn2[-10,-10,20,20@100]",
	"f4 P1#1 e2.0 c1[-8,-8,18,8] hit=false > hits=3 hitpause=0 status=0; P2#2 e2.0 c1[] hit=false > hits=1 hitpause=0 status=0 || drawn P1#1 d2 spr=10,1 io=0 cued=true; P2#2 d2 spr=20,1 io=0 cued=true; clsn1[-8,-8,26,16@0]; clsn2[-10,-10,30,20@0]; clsn2[-10,-10,20,20@100]",
	"f5 P1#1 e2.1 c1[-8,-8,18,8] hit=false > hits=3 hitpause=0 status=0; P2#2 e2.1 c1[] hit=false > hits=1 hitpause=0 status=0 || drawn P1#1 d2 spr=10,1 io=0 cued=true; P2#2 d2 spr=20,1 io=0 cued=true; clsn1[-8,-8,26,16@0]; clsn2[-10,-10,30,20@0]; clsn2[-10,-10,20,20@100]",
	"f6 P1#1 e3.0 c1[-8,-8,28,8] hit=true > hits=3 hitpause=0 status=0; P2#2 e2.2 c1[] hit=false > hits=1 hitpause=0 status=0 || drawn P1#1 d3 spr=10,2 io=0 cued=true; P2#2 d2 spr=20,1 io=0 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]; clsn2[-10,-10,20,20@100]",
	"f7 P1#1 e3.1 c1[-8,-8,28,8] hit=true > hits=3 hitpause=0 status=0; P2#2 e1.0 c1[] hit=false > hits=1 hitpause=0 status=0 || drawn P1#1 d3 spr=10,2 io=2 cued=true; P2#2 d1 spr=20,0 io=0 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]; clsn2[-10,-10,20,20@100]",
	"f8 P1#1 e3.2 c1[-8,-8,28,8] hit=true > hits=3 hitpause=0 status=0; P2#2 e1.1 c1[] hit=false > hits=1 hitpause=0 status=0 || drawn P1#1 d3 spr=10,2 io=4 cued=true; P2#2 d1 spr=20,0 io=0 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]; clsn2[-10,-10,20,20@100]",
	"f9 P1#1 e3.3 c1[-8,-8,28,8] hit=true > hits=3 hitpause=0 status=0; P2#2 e1.2 c1[] hit=false > hits=1 hitpause=0 status=0 || drawn P1#1 d3 spr=10,2 io=6 cued=true; P2#2 d1 spr=20,0 io=0 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]; clsn2[-10,-10,20,20@100]",
	"f10 P1#1 e4.0 c1[-8,-8,98,8] hit=true > hits=2 hitpause=5 status=0; P2#2 e2.0 c1[] hit=false > hits=0 hitpause=0 status=2 || drawn P1#1 d3 spr=10,2 io=6 cued=true; P2#2 d2 spr=20,1 io=0 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]; clsn2[-10,-10,20,20@100]",
	"f11 P1#1 e4.0 c1[-8,-8,98,8] hit=true > hits=2 hitpause=4 status=0; P2#2 e1.0 c1[] hit=false > hits=0 hitpause=0 status=2 || drawn P1#1 d3 spr=10,2 io=6 cued=true; P2#2 d1 spr=21,0 io=0 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]",
	"f12 P1#1 e4.0 c1[-8,-8,98,8] hit=true > hits=2 hitpause=3 status=0; P2#2 e1.1 c1[] hit=false > hits=0 hitpause=0 status=2 || drawn P1#1 d3 spr=10,2 io=6 cued=true; P2#2 d1 spr=21,0 io=0 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]",
	"f13 P1#1 e4.0 c1[-8,-8,98,8] hit=true > hits=2 hitpause=2 status=0; P2#2 e2.0 c1[] hit=false > hits=0 hitpause=0 status=2 || drawn P1#1 d3 spr=10,2 io=6 cued=true; P2#2 d2 spr=21,1 io=0 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]",
	"f14 P1#1 e4.0 c1[-8,-8,98,8] hit=true > hits=2 hitpause=1 status=0; P2#2 e2.1 c1[] hit=false > hits=0 hitpause=0 status=2 || drawn P1#1 d4 spr=10,3 io=0 cued=true; P2#2 d2 spr=21,1 io=0 cued=true; clsn1[-8,-8,106,16@0]; clsn2[-10,-10,130,20@0]",
	"f15 P1#1 e5.0 c1[-8,-8,38,8] hit=true > hits=2 hitpause=0 status=0; P2#2 e3.0 c1[] hit=false > hits=0 hitpause=0 status=2 || drawn P1#1 d5 spr=10,4 io=0 cued=true; P2#2 d3 spr=21,2 io=0 cued=true; clsn1[-8,-8,46,16@0]; clsn2[-10,-10,50,20@0]",
	"f16 P1#1 e5.1 c1[-8,-8,38,8] hit=true > hits=2 hitpause=0 status=0; P2#2 e3.1 c1[] hit=false > hits=0 hitpause=0 status=2 || drawn P1#1 d5 spr=10,4 io=0 cued=true; P2#2 d3 spr=21,2 io=0 cued=true; clsn1[-8,-8,46,16@0]; clsn2[-10,-10,50,20@0]",
	"f17 P1#1 e2.0 c1[-8,-8,18,8] hit=false > hits=2 hitpause=0 status=0 || drawn P1#1 d2 spr=10,1 io=0 cued=true; clsn1[-8,-8,26,16@0]; clsn2[-10,-10,30,20@0]",
	"f18 P1#1 e2.1 c1[-8,-8,18,8] hit=false > hits=2 hitpause=0 status=0 || drawn P1#1 d2 spr=10,1 io=0 cued=true; clsn1[-8,-8,26,16@0]; clsn2[-10,-10,30,20@0]",
	"f19 P1#1 e3.0 c1[-8,-8,28,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d3 spr=10,2 io=0 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]",
	"f20 P1#1 e3.1 c1[-8,-8,28,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d3 spr=10,2 io=2 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]",
	"f21 P1#1 e3.2 c1[-8,-8,28,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d3 spr=10,2 io=4 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]",
	"f22 P1#1 e3.3 c1[-8,-8,28,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d3 spr=10,2 io=6 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]",
	"f23 P1#1 e4.0 c1[-8,-8,98,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d4 spr=10,3 io=0 cued=true; clsn1[-8,-8,106,16@0]; clsn2[-10,-10,130,20@0]",
	"f24 P1#1 e5.0 c1[-8,-8,38,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d5 spr=10,4 io=0 cued=true; clsn1[-8,-8,46,16@0]; clsn2[-10,-10,50,20@0]",
	"f25 P1#1 e5.1 c1[-8,-8,38,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d5 spr=10,4 io=0 cued=true; clsn1[-8,-8,46,16@0]; clsn2[-10,-10,50,20@0]",
	"f26 P1#1 e2.0 c1[-8,-8,18,8] hit=false > hits=2 hitpause=0 status=0 || drawn P1#1 d2 spr=10,1 io=0 cued=true; clsn1[-8,-8,26,16@0]; clsn2[-10,-10,30,20@0]",
	"f27 P1#1 e2.1 c1[-8,-8,18,8] hit=false > hits=2 hitpause=0 status=0 || drawn P1#1 d2 spr=10,1 io=0 cued=true; clsn1[-8,-8,26,16@0]; clsn2[-10,-10,30,20@0]",
	"f28 P1#1 e2.1 c1[-8,-8,18,8] hit=false > hits=2 hitpause=0 status=0 || drawn P1#1 d2 spr=10,1 io=0 cued=true; clsn1[-8,-8,26,16@0]; clsn2[-10,-10,30,20@0]",
	"f29 P1#1 e2.1 c1[-8,-8,18,8] hit=false > hits=2 hitpause=0 status=0 || drawn P1#1 d2 spr=10,1 io=0 cued=true; clsn1[-8,-8,26,16@0]; clsn2[-10,-10,30,20@0]",
	"f30 P1#1 e2.1 c1[-8,-8,18,8] hit=false > hits=2 hitpause=0 status=0 || drawn P1#1 d2 spr=10,1 io=0 cued=true; clsn1[-8,-8,26,16@0]; clsn2[-10,-10,30,20@0]",
	"f31 P1#1 e2.1 c1[-8,-8,18,8] hit=false > hits=2 hitpause=0 status=0 || drawn P1#1 d2 spr=10,1 io=0 cued=true; clsn1[-8,-8,26,16@0]; clsn2[-10,-10,30,20@0]",
	"f32 P1#1 e2.1 c1[-8,-8,18,8] hit=false > hits=2 hitpause=0 status=0 || drawn P1#1 d2 spr=10,1 io=0 cued=true; clsn1[-8,-8,26,16@0]; clsn2[-10,-10,30,20@0]",
	"f33 P1#1 e2.1 c1[-8,-8,18,8] hit=false > hits=2 hitpause=0 status=0 || drawn P1#1 d2 spr=10,1 io=0 cued=true; clsn1[-8,-8,26,16@0]; clsn2[-10,-10,30,20@0]",
	"f34 P1#1 e3.0 c1[-8,-8,28,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d3 spr=10,2 io=0 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]",
	"f35 P1#1 e3.1 c1[-8,-8,28,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d3 spr=10,2 io=2 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]",
	"f36 P1#1 e3.2 c1[-8,-8,28,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d3 spr=10,2 io=4 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]",
	"f37 P1#1 e3.3 c1[-8,-8,28,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d3 spr=10,2 io=6 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]",
	"f38 P1#1 e4.0 c1[-8,-8,98,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d4 spr=10,3 io=0 cued=true; clsn1[-8,-8,106,16@0]; clsn2[-10,-10,130,20@0]",
	"f39 P1#1 e5.0 c1[-8,-8,38,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d5 spr=10,4 io=0 cued=true; clsn1[-8,-8,46,16@0]; clsn2[-10,-10,50,20@0]",
	"f40 P1#1 e5.1 c1[-8,-8,38,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d5 spr=10,4 io=0 cued=true; clsn1[-8,-8,46,16@0]; clsn2[-10,-10,50,20@0]",
	"f41 P1#1 e2.0 c1[-8,-8,18,8] hit=false > hits=2 hitpause=0 status=0 || drawn P1#1 d2 spr=10,1 io=0 cued=true; clsn1[-8,-8,26,16@0]; clsn2[-10,-10,30,20@0]",
	"f42 P1#1 e2.1 c1[-8,-8,18,8] hit=false > hits=2 hitpause=0 status=0 || drawn P1#1 d2 spr=10,1 io=0 cued=true; clsn1[-8,-8,26,16@0]; clsn2[-10,-10,30,20@0]",
	"f43 P1#1 e3.0 c1[-8,-8,28,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d3 spr=10,2 io=0 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]",
	"f44 P1#1 e3.1 c1[-8,-8,28,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d3 spr=10,2 io=2 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]",
	"f45 P1#1 e3.2 c1[-8,-8,28,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d3 spr=10,2 io=4 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]",
	"f46 P1#1 e3.3 c1[-8,-8,28,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d3 spr=10,2 io=6 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]",
	"f47 P1#1 e4.0 c1[-8,-8,98,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d4 spr=10,3 io=0 cued=true; clsn1[-8,-8,106,16@0]; clsn2[-10,-10,130,20@0]",
	"f48 P1#1 e5.0 c1[-8,-8,38,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d5 spr=10,4 io=0 cued=true; clsn1[-8,-8,46,16@0]; clsn2[-10,-10,50,20@0]",
	"f49 P1#1 e5.1 c1[-8,-8,38,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d5 spr=10,4 io=0 cued=true; clsn1[-8,-8,46,16@0]; clsn2[-10,-10,50,20@0]",
	"f50 P1#1 e2.0 c1[-8,-8,18,8] hit=false > hits=2 hitpause=0 status=0 || drawn P1#1 d2 spr=10,1 io=0 cued=true; clsn1[-8,-8,26,16@0]; clsn2[-10,-10,30,20@0]",
	"f51 P1#1 e2.1 c1[-8,-8,18,8] hit=false > hits=2 hitpause=0 status=0 || drawn P1#1 d2 spr=10,1 io=0 cued=true; clsn1[-8,-8,26,16@0]; clsn2[-10,-10,30,20@0]",
	"f52 P1#1 e3.0 c1[-8,-8,28,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d3 spr=10,2 io=0 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]",
	"f53 P1#1 e3.1 c1[-8,-8,28,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d3 spr=10,2 io=2 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]",
	"f54 P1#1 e3.2 c1[-8,-8,28,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d3 spr=10,2 io=4 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]",
	"f55 P1#1 e3.3 c1[-8,-8,28,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d3 spr=10,2 io=6 cued=true; clsn1[-8,-8,36,16@0]; clsn2[-10,-10,40,20@0]",
	"f56 P1#1 e4.0 c1[-8,-8,98,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d4 spr=10,3 io=0 cued=true; clsn1[-8,-8,106,16@0]; clsn2[-10,-10,130,20@0]",
	"f57 P1#1 e5.0 c1[-8,-8,38,8] hit=true > hits=2 hitpause=0 status=0 || drawn P1#1 d5 spr=10,4 io=0 cued=true; clsn1[-8,-8,46,16@0]; clsn2[-10,-10,50,20@0]",
	"f58 P1#1 e1.0 c1[] hit=false > hits=2 hitpause=0 status=3 || drawn P1#1 d1 spr=11,0 io=0 cued=true",
	"f59 P1#1 e1.1 c1[] hit=false > hits=2 hitpause=0 status=3 || drawn P1#1 d1 spr=11,0 io=0 cued=true",
	"f60 P1#1 e2.0 c1[] hit=false > hits=2 hitpause=0 status=3 || drawn P1#1 d2 spr=11,1 io=0 cued=true",
	"f61 P1#1 e3.0 c1[] hit=false > hits=2 hitpause=0 status=3 || drawn P1#1 d3 spr=11,2 io=0 cued=true",
	"f62 P1#1 e3.1 c1[] hit=false > hits=2 hitpause=0 status=3 || drawn P1#1 d3 spr=11,2 io=0 cued=true",
	"f63  || drawn ",
	"f64  || drawn ",
	"f65  || drawn ",
	"f66  || drawn ",
	"f67  || drawn ",
	"f68  || drawn ",
	"f69  || drawn ",
	"f70  || drawn ",
	"f71  || drawn ",
}

// T4, born in slow motion: the engine before the change (f76237c).
var projAnimSlowBornSlowBefore = []string{
	"FN:x:-", "F-:-:-", "--:-:-", "--:-:d1",
	"-N:e1.1:d1", "F-:-:d1", "--:-:d1", "--:-:d1",
	"-N:e1.2:d1", "F-:-:d1", "--:-:d1", "--:-:d1",
	"-N:e2.0:d2", "F-:-:d2", "--:-:d2", "--:-:d2",
	"-N:e2.1:d2", "F-:-:d2", "--:-:d2", "--:-:d2",
	"-N:e3.0:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e3.1:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e3.2:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e3.3:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e4.0:d4", "F-:-:d4", "--:-:d4", "--:-:d4",
}

// T4, born in slow motion: f76237c with cueDraw() called where c3ac399^ called it.
var projAnimSlowBornSlowPreSplit = []string{
	"FN:x:-", "F-:-:d1", "--:-:d1", "--:-:d1",
	"-N:e1.0:d1", "F-:-:d1", "--:-:d1", "--:-:d1",
	"-N:e1.1:d1", "F-:-:d1", "--:-:d1", "--:-:d1",
	"-N:e1.2:d1", "F-:-:d2", "--:-:d2", "--:-:d2",
	"-N:e2.0:d2", "F-:-:d2", "--:-:d2", "--:-:d2",
	"-N:e2.1:d2", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e3.0:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e3.1:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e3.2:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e3.3:d3", "F-:-:d4", "--:-:d4", "--:-:d4",
}

// T4, born in slow motion: the engine with the step in tick().
var projAnimSlowBornSlowNow = []string{
	"FN:x:-", "F-:-:-", "--:-:-", "--:-:-",
	"-N:e1.0:d1", "F-:-:d1", "--:-:d1", "--:-:d1",
	"-N:e1.1:d1", "F-:-:d1", "--:-:d1", "--:-:d1",
	"-N:e1.2:d1", "F-:-:d1", "--:-:d1", "--:-:d1",
	"-N:e2.0:d2", "F-:-:d2", "--:-:d2", "--:-:d2",
	"-N:e2.1:d2", "F-:-:d2", "--:-:d2", "--:-:d2",
	"-N:e3.0:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e3.1:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e3.2:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e3.3:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
}

// T4, born at normal speed: the engine before the change (f76237c).
var projAnimSlowBornFastBefore = []string{
	"FN:e1.0:d1", "F-:-:d1", "--:-:d1", "--:-:d1",
	"-N:e1.1:d1", "F-:-:d1", "--:-:d1", "--:-:d1",
	"-N:e1.2:d1", "F-:-:d1", "--:-:d1", "--:-:d1",
	"-N:e2.0:d2", "F-:-:d2", "--:-:d2", "--:-:d2",
	"-N:e2.1:d2", "F-:-:d2", "--:-:d2", "--:-:d2",
	"-N:e3.0:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e3.1:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e3.2:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e3.3:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e4.0:d4", "F-:-:d4", "--:-:d4", "--:-:d4",
}

// T4, born at normal speed: f76237c with cueDraw() called where c3ac399^ called it.
var projAnimSlowBornFastPreSplit = []string{
	"FN:e1.0:d1", "F-:-:d1", "--:-:d1", "--:-:d1",
	"-N:e1.1:d1", "F-:-:d1", "--:-:d1", "--:-:d1",
	"-N:e1.2:d1", "F-:-:d2", "--:-:d2", "--:-:d2",
	"-N:e2.0:d2", "F-:-:d2", "--:-:d2", "--:-:d2",
	"-N:e2.1:d2", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e3.0:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e3.1:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e3.2:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e3.3:d3", "F-:-:d4", "--:-:d4", "--:-:d4",
	"-N:e4.0:d4", "F-:-:d5", "--:-:d5", "--:-:d5",
}

// T4, born at normal speed: the engine with the step in tick().
var projAnimSlowBornFastNow = []string{
	"FN:e1.0:d1", "F-:-:d1", "--:-:d1", "--:-:d1",
	"-N:e1.1:d1", "F-:-:d1", "--:-:d1", "--:-:d1",
	"-N:e1.2:d1", "F-:-:d1", "--:-:d1", "--:-:d1",
	"-N:e2.0:d2", "F-:-:d2", "--:-:d2", "--:-:d2",
	"-N:e2.1:d2", "F-:-:d2", "--:-:d2", "--:-:d2",
	"-N:e3.0:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e3.1:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e3.2:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e3.3:d3", "F-:-:d3", "--:-:d3", "--:-:d3",
	"-N:e4.0:d4", "F-:-:d4", "--:-:d4", "--:-:d4",
}
