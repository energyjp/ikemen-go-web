package ggpo_test

// The reasons a watcher is dropped with, and the late start a watcher is
// allowed. These use the API added with the hardening (SpectatorDropReason,
// FailReason), so unlike spectator_hardening_test.go they do not build against
// the older library.

import (
	"strings"
	"testing"
	"time"

	"github.com/ikemen-engine/ggpo/internal/protocol"
)

func TestWatcherDropReasons(t *testing.T) {
	silent := newTrio(true)
	silent.run(30)
	silent.board.cut[boardKey(tIP, tSpecPort)] = true
	silent.run(600)
	if r := silent.host.SpectatorDropReason(silent.specHandle); !strings.Contains(r, "behind") {
		t.Errorf("silent watcher: reason %q", r)
	}

	absent := newTrio(false)
	absent.run(600)
	if r := absent.host.SpectatorDropReason(absent.specHandle); !strings.Contains(r, "not connected") {
		t.Errorf("absent watcher: reason %q", r)
	}

	lagging := newTrio(true)
	lagging.run(10)
	lagging.specConsume = false
	lagging.run(800)
	if r := lagging.host.SpectatorDropReason(lagging.specHandle); !strings.Contains(r, "behind") {
		t.Errorf("lagging watcher: reason %q", r)
	}
	if lagging.spec.FailReason() == "" {
		t.Errorf("the lagging watcher was not told why it stopped")
	}
	if lagging.host.SpectatorDropReason(1) != "" || lagging.host.SpectatorDropReason(1000+5) != "" {
		t.Errorf("a drop reason for a handle that is not a registered watcher")
	}
}

// A watcher that finishes connecting after the players have started, but
// within the frames the host keeps for it, still sees the match from frame 0.
func TestWatcherSyncingLateStillGetsFrameZero(t *testing.T) {
	tr := newTrio(false)
	tr.run(60) // the players are 60 frames in; the watcher is not there yet
	tr.spec.Start()
	tr.specStarted = true
	advance := func() int64 { return time.Now().Add(2000 * time.Millisecond).UnixMilli() }
	for i := 0; i < protocol.NumSyncPackets; i++ {
		tr.host.Idle(0, advance)
		tr.spec.Idle(0, advance)
	}
	if err := tr.run(200); err != nil {
		t.Fatalf("%v", err)
	}
	if len(tr.specFrames) < 200 {
		t.Fatalf("the late watcher simulated only %d frames", len(tr.specFrames))
	}
	if err := tr.specFramesContiguous(); err != nil {
		t.Fatalf("%v", err)
	}
	if r := tr.host.SpectatorDropReason(tr.specHandle); r != "" {
		t.Fatalf("the late watcher was dropped: %s", r)
	}
}

// Dropping one watcher by hand (its page went away) leaves the players and
// any other bookkeeping alone.
func TestDropSpectatorByHand(t *testing.T) {
	tr := newTrio(true)
	tr.run(30)
	if err := tr.host.DropSpectator(tr.specHandle, "the watcher's page closed"); err != nil {
		t.Fatalf("%v", err)
	}
	before := tr.hostFrames
	if err := tr.run(100); err != nil {
		t.Fatalf("%v", err)
	}
	if tr.hostFrames-before < 95 {
		t.Fatalf("the players stalled after a watcher was dropped")
	}
	if r := tr.host.SpectatorDropReason(tr.specHandle); r != "the watcher's page closed" {
		t.Fatalf("reason %q", r)
	}
	if tr.host.DropSpectator(1, "x") == nil {
		t.Fatalf("a player's handle was accepted as a watcher's")
	}
	if !onlySpectatorDisconnects(tr.hostS) {
		t.Fatalf("a player was disconnected")
	}
}
