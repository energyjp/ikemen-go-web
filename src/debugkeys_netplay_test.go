package main

import "testing"

// Debug hotkeys must not fire during ANY netplay session.
//
// WHY THIS EXISTS. Every script registered in external/script/debug.lua mutates
// game state on the LOCAL peer only. F4 is `resetRound()`, which resets the
// round timer to maxRoundTime, restores both players' life, sends both
// characters to state 5900 and zeroes the score - while the opponent, who did
// not press it, keeps playing the round out. That is an instant, total,
// unrecoverable desync.
//
// The original guard was `sys.netConnection == nil`, which is the DELAY
// netcode's connection. Under ROLLBACK - the DEFAULT netcode - netConnection is
// nil, so every one of these keys was live in a ranked match. Two Kira-vs-Kira
// desyncs on 2026-08-28 were captured from both peers simultaneously and showed
// exactly that signature: one peer at roundstate 0 with crt=5940 (a full,
// freshly reset timer) and sp=0/0, the other still at roundstate 4 in the win
// pose, with randseed IDENTICAL on both (so nothing random had diverged).
//
// This is the same shape as the win-pose skip bug: upstream gates a
// local-input feature on netConnection, which silently means "delay only".
// If a third netcode is ever added, this predicate is the place to update.

func withSys(t *testing.T, f func()) {
	t.Helper()
	// The tests below only touch the handful of fields allowShortcut reads.
	oldNet, oldRb := sys.netConnection, sys.rollback.session
	oldReplay, oldAllow := sys.replayFile, sys.cfg.Debug.AllowDebugKeys
	oldActive := sys.motif.me.active
	defer func() {
		sys.netConnection, sys.rollback.session = oldNet, oldRb
		sys.replayFile, sys.cfg.Debug.AllowDebugKeys = oldReplay, oldAllow
		sys.motif.me.active = oldActive
	}()
	sys.netConnection, sys.rollback.session = nil, nil
	sys.replayFile = nil
	sys.motif.me.active = false
	sys.cfg.Debug.AllowDebugKeys = true
	f()
}

func TestDebugKeysBlockedDuringRollbackNetplay(t *testing.T) {
	debugKey := &ShortcutScript{DebugKey: true, Script: "resetRound()"}
	plainKey := &ShortcutScript{DebugKey: false, Script: "togglePause()"}

	withSys(t, func() {
		// Offline: both kinds work. This is the positive control - without it,
		// a predicate that always returned false would pass every other case.
		if !sys.allowShortcut(debugKey) {
			t.Fatal("offline: debug hotkey must work (AllowDebugKeys is on)")
		}
		if !sys.allowShortcut(plainKey) {
			t.Fatal("offline: non-debug hotkey must work")
		}

		// THE REGRESSION. rollback.session is non-nil while netConnection stays
		// nil - exactly the state a ranked match runs in.
		sys.rollback.session = &RollbackSession{}
		if sys.allowShortcut(debugKey) {
			t.Error("ROLLBACK netplay: debug hotkey fired - this is the resetRound desync")
		}
		if sys.allowShortcut(plainKey) {
			t.Error("ROLLBACK netplay: non-debug hotkey fired (F9/F10/PAUSE/SCROLLLOCK " +
				"are DebugKey=false, so AllowDebugKeys does not gate them at all)")
		}
	})
}

func TestDebugKeysBlockedDuringDelayNetplay(t *testing.T) {
	debugKey := &ShortcutScript{DebugKey: true}
	plainKey := &ShortcutScript{DebugKey: false}
	withSys(t, func() {
		sys.netConnection = &NetConnection{}
		if sys.allowShortcut(debugKey) || sys.allowShortcut(plainKey) {
			t.Error("DELAY netplay: hotkeys must stay blocked (this half already worked)")
		}
	})
}

func TestAllowDebugKeysOffDoesNotProtectPlainHotkeys(t *testing.T) {
	// Documents WHY the config switch is not a substitute for the netplay gate:
	// turning AllowDebugKeys off leaves loadState/saveState/togglePause/
	// frameStep fully live, because those are registered with DebugKey=false.
	withSys(t, func() {
		sys.cfg.Debug.AllowDebugKeys = false
		if sys.allowShortcut(&ShortcutScript{DebugKey: true}) {
			t.Error("AllowDebugKeys=false must block DebugKey hotkeys")
		}
		if !sys.allowShortcut(&ShortcutScript{DebugKey: false}) {
			t.Error("AllowDebugKeys=false is not meant to block non-debug hotkeys - " +
				"which is exactly why the netplay gate has to do that job")
		}
	})
}

func TestReplayKeepsPauseAndFrameStep(t *testing.T) {
	// A replay viewer has no peer to desync from and legitimately wants pause
	// and frame-step. Behaviour here must be unchanged by the rollback fix.
	withSys(t, func() {
		sys.replayFile = &ReplayFile{}
		if sys.allowShortcut(&ShortcutScript{DebugKey: true}) {
			t.Error("replay: debug hotkeys stay blocked, as before")
		}
		if !sys.allowShortcut(&ShortcutScript{DebugKey: false}) {
			t.Error("replay: pause/frame-step must still work")
		}
	})
}
