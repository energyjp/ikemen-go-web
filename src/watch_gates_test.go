package main

import "testing"

// A watcher runs the players' match from their confirmed inputs (watch.go).
// Its engine must read them as the players' engines do: synchronized (their
// Esc and menu frames step its menu channels), with no local slot and no local
// participant (its own keys reach nothing in the match), and its own Esc ends
// watching the way it ends a replay.
func TestWatcherInputGates(t *testing.T) {
	oldSession, oldNC, oldRNC, oldReplay, oldEsc := sys.rollback.session, sys.netConnection, sys.rollback.netConnection, sys.replayFile, sys.esc
	defer func() {
		sys.rollback.session, sys.netConnection, sys.rollback.netConnection, sys.replayFile, sys.esc = oldSession, oldNC, oldRNC, oldReplay, oldEsc
	}()
	sys.netConnection, sys.rollback.netConnection, sys.replayFile, sys.esc = nil, nil, nil, false

	// offline: nothing is synchronized and the keyboard drives slot 1
	sys.rollback.session = nil
	if sys.spectating() || sys.synchronizedInput() {
		t.Fatal("offline: spectating or synchronized")
	}

	session := RollbackSession{spectating: true}
	sys.rollback.session = &session
	if !sys.spectating() {
		t.Fatal("a spectator session is not spectating()")
	}
	if !sys.synchronizedInput() {
		t.Fatal("a watcher's input is not synchronized: the players' Esc and menu frames would not step its menus")
	}
	for slot := -1; slot < 4; slot++ {
		if sys.localSlot(slot) {
			t.Fatalf("slot %d reads the watcher's own keyboard", slot)
		}
	}
	for ch := 0; ch < 4; ch++ {
		if p := sys.localInputParticipant(ch); p != -1 {
			t.Fatalf("channel %d is the watcher's own participant %d", ch, p)
		}
	}
	if sys.escExit() {
		t.Fatal("escExit without any Esc")
	}
	sys.esc = true
	if !sys.escExit() {
		t.Fatal("the watcher's own Esc does not end watching")
	}
}
