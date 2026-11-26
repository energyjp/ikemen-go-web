package ggpo_test

import (
	"testing"
	"time"

	"github.com/ikemen-engine/ggpo"
	"github.com/ikemen-engine/ggpo/internal/protocol"
)

// A Turns match plays every turn as a NEW host session (and a rematch is a
// new one too). The host builds the next turn's session, and registers the
// watcher with it, while the watcher's session for the turn before may still
// be simulating its last frames a few frames behind. Both sessions talk over
// the same line to the watcher. The old one must not answer the new one's
// sync requests: if it did, the host's endpoint for the watcher would sync
// with it and pin its magic, then reject every acknowledgment from the
// watcher's real next session and drop it SpectatorMaxPendingFrames in.
func TestNextSessionIgnoresTheWatchersOldSession(t *testing.T) {
	tr := newTrio(true)
	tr.board.delay = 2 // the watcher a few frames away
	if err := tr.run(120); err != nil {
		t.Fatal(err)
	}
	if len(tr.specFrames) < 60 {
		t.Fatalf("the watcher took only %d frames of the first turn", len(tr.specFrames))
	}
	old := tr.spec

	// The next turn: the players' new sessions on the same ports, the
	// watcher registered again.
	nx := &trio{board: tr.board, hostS: &recSession{}, guestS: &recSession{}, specS: &recSession{}}
	host := ggpo.NewPeer(nx.hostS, tHostPort, 2, tInputSize)
	guest := ggpo.NewPeer(nx.guestS, tGuestPort, 2, tInputSize)
	nx.host, nx.guest = &host, &guest
	nx.hostS.peer, nx.guestS.peer = nx.host, nx.guest
	tr.board.handlers[boardKey(tIP, tHostPort)] = nx.host
	tr.board.handlers[boardKey(tIP, tGuestPort)] = nx.guest
	nx.host.InitializeConnection(&boardConn{tr.board, tIP, tHostPort})
	nx.guest.InitializeConnection(&boardConn{tr.board, tIP, tGuestPort})
	var ignore ggpo.PlayerHandle
	g1 := ggpo.NewRemotePlayer(20, 1, tIP, tHostPort)
	g2 := ggpo.NewLocalPlayer(20, 2)
	nx.guest.AddPlayer(&g1, &ignore)
	nx.guest.AddPlayer(&g2, &nx.g2)
	p1 := ggpo.NewLocalPlayer(20, 1)
	p2 := ggpo.NewRemotePlayer(20, 2, tIP, tGuestPort)
	sp := ggpo.NewSpectatorPlayer(20, tIP, tSpecPort)
	nx.host.AddPlayer(&p1, &nx.h1)
	nx.host.AddPlayer(&p2, &ignore)
	nx.host.AddPlayer(&sp, &nx.specHandle)

	// Half a second of frames in which the old session still runs: the new
	// host session's sync requests reach it, every leg 2 frames late.
	advance := func() int64 { return time.Now().Add(2500 * time.Millisecond).UnixMilli() }
	for i := 0; i < 30; i++ {
		tr.board.tick()
		nx.host.Idle(0)
		nx.guest.Idle(0, advance) // its first request went to a host not set up yet
		old.Idle(0)
	}
	if !nx.hostS.has(ggpo.EventCodeRunning, 0) {
		t.Fatalf("the players' next session did not start")
	}
	if nx.hostS.has(ggpo.EventCodeSynchronizedWithPeer, 1000) {
		// Go on: what it costs shows below.
		t.Errorf("the next session's endpoint for the watcher synced with the watcher's OLD session")
	}

	// The old session ends; the watcher's next one starts and answers.
	old.Close()
	spec := ggpo.NewSpectator(nx.specS, tSpecPort, 2, tInputSize, tIP, tHostPort)
	nx.spec = &spec
	tr.board.handlers[boardKey(tIP, tSpecPort)] = nx.spec
	nx.spec.InitializeConnection(&boardConn{tr.board, tIP, tSpecPort})
	nx.spec.Start()
	nx.specStarted, nx.specConsume = true, true
	retried := false
	for i := 0; i < 60 && !nx.hostS.has(ggpo.EventCodeSynchronizedWithPeer, 1000); i++ {
		tr.board.tick()
		if !retried {
			nx.host.Idle(0, advance) // its retry, once
			retried = true
		} else {
			nx.host.Idle(0)
		}
		nx.spec.Idle(0)
	}
	if !nx.hostS.has(ggpo.EventCodeSynchronizedWithPeer, 1000) {
		t.Fatalf("the next session's endpoint never synced with the watcher's next session")
	}

	// The whole next turn and more: the watcher is never dropped and takes
	// every frame, in order.
	n := protocol.SpectatorMaxPendingFrames + 240
	if err := nx.run(n); err != nil {
		t.Fatal(err)
	}
	if nx.hostS.has(ggpo.EventCodeDisconnectedFromPeer, 1000) {
		t.Fatalf("the host dropped the watcher's next session (its acknowledgments were rejected)")
	}
	if nx.hostFrames < n-20 || len(nx.specFrames) < nx.hostFrames-60 {
		t.Fatalf("host at %d frames, the watcher's next session took %d", nx.hostFrames, len(nx.specFrames))
	}
	if err := nx.specFramesContiguous(); err != nil {
		t.Fatal(err)
	}
}
