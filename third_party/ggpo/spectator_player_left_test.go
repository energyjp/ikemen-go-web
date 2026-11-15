package ggpo_test

// A player leaving mid-match must not crash the watchers, and nothing the
// match host sends may panic a watcher's engine (a lobby's match host is
// another member, so a stranger). Found in a four-engine lobby run: a player
// closed her tab mid-match and a watcher's engine panicked with "slice bounds
// out of range [:32] with capacity 24" in UdpProtocol.OnInput. The host sent
// the frames past the leaver's last one with an empty input for her (GGPO's
// erased input) under a full frame size, and the watcher sliced a whole
// frame out of the short tail.

import (
	"strings"
	"testing"

	"github.com/ikemen-engine/ggpo"
	"github.com/ikemen-engine/ggpo/internal/messages"
)

// hostTap records the last message the spectator got from the host.
type hostTap struct {
	inner interface {
		HandleMessage(string, int, messages.UDPMessage, int)
	}
	last messages.UDPMessage
}

func (h *hostTap) HandleMessage(ip string, port int, msg messages.UDPMessage, n int) {
	if port == tHostPort {
		h.last = msg
	}
	h.inner.HandleMessage(ip, port, msg, n)
}

// consume hands out every frame the spectator has, recording both players'
// inputs per frame.
func consume(tr *trio, got *[][2][]byte) error {
	var flags int
	for {
		vals, e := tr.spec.SyncInput(&flags)
		if e != nil {
			return nil
		}
		if len(vals) != 2 {
			return nil
		}
		*got = append(*got, [2][]byte{append([]byte(nil), vals[0]...), append([]byte(nil), vals[1]...)})
		tr.spec.AdvanceFrame(0)
	}
}

func TestWatcherKeepsWatchingWhenAPlayerLeaves(t *testing.T) {
	tr := newTrio(true)
	tr.specConsume = false
	var got [][2][]byte
	for i := 0; i < 40; i++ {
		if err := tr.step(); err != nil {
			t.Fatalf("before the guest left: %v", err)
		}
		consume(tr, &got)
	}
	// The guest goes (its tab closed): nothing more from it, and the host
	// drops it and plays on alone, as GGPO does.
	tr.board.cut[boardKey(tIP, tGuestPort)] = true
	if err := tr.host.DisconnectPlayer(ggpo.PlayerHandle(2)); err != nil {
		t.Fatalf("DisconnectPlayer: %v", err)
	}
	left := tr.hostFrames
	for i := 0; i < 120; i++ {
		if err := tr.step(); err != nil {
			t.Fatalf("after the guest left (display frame %d): %v", i, err)
		}
		consume(tr, &got)
	}
	if why := tr.spec.FailReason(); why != "" {
		t.Fatalf("the watcher stopped watching: %s", why)
	}
	if len(got) < left+60 {
		t.Fatalf("the watcher has %d frames; the guest left at %d and the host is at %d", len(got), left, tr.hostFrames)
	}
	zeros := 0
	for f, in := range got {
		if len(in[0]) != tInputSize || len(in[1]) != tInputSize {
			t.Fatalf("frame %d: inputs of %d and %d bytes, want %d each", f, len(in[0]), len(in[1]), tInputSize)
		}
		if p1 := int(in[0][0]) | int(in[0][1])<<8; p1 != f {
			t.Fatalf("frame %d carried the host's frame %d", f, p1)
		}
		if f > left && string(in[1]) == string(make([]byte, tInputSize)) {
			zeros++
		}
	}
	if zeros < 60 {
		t.Fatalf("only %d frames after the guest left read as zeros for it", zeros)
	}
	t.Logf("guest left at host frame %d; the watcher played %d frames, %d with the guest's input zeroed", left, len(got), zeros)
}

func TestWatcherRefusesMalformedFramesFromTheHost(t *testing.T) {
	cases := []struct {
		name     string
		size     uint8
		bits     int
		statuses int
	}{
		// the lobby run's packet: two frames of 16 announced, 24 bytes sent
		{"a short last frame", 2 * tInputSize, 3 * tInputSize, messages.UDPMsgMaxPlayers},
		{"input size 0 with input", 0, 2 * tInputSize, messages.UDPMsgMaxPlayers},
		{"one connect status", 2 * tInputSize, 2 * tInputSize, 1},
	}
	for _, c := range cases {
		tr := newTrio(true)
		tap := &hostTap{inner: tr.spec}
		tr.board.handlers[boardKey(tIP, tSpecPort)] = tap
		if err := tr.run(30); err != nil {
			t.Fatalf("%s: before the bad packet: %v", c.name, err)
		}
		if tap.last == nil {
			t.Fatalf("%s: the host never sent the watcher anything", c.name)
		}
		bad := messages.NewUDPMessage(messages.InputMsg).(*messages.InputPacket)
		bad.PeerConnectStatus = make([]messages.UdpConnectStatus, c.statuses)
		for i := range bad.PeerConnectStatus {
			bad.PeerConnectStatus[i].LastFrame = 1 << 20
		}
		bad.StartFrame = uint32(len(tr.specFrames))
		bad.InputSize = c.size
		bad.NumBits = uint16(c.size)
		bad.Bits = make([]byte, c.bits)
		bad.SetHeader(tap.last.Header().Magic, tap.last.Header().SequenceNumber+1)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s: a malformed packet from the host panicked the watcher: %v", c.name, r)
				}
			}()
			tr.spec.HandleMessage(tIP, tHostPort, bad, bad.PacketSize())
			tr.spec.Idle(0)
		}()
		why := tr.spec.FailReason()
		if !strings.Contains(why, "match host") {
			t.Fatalf("%s: the watcher was not stopped with a reason (reason %q)", c.name, why)
		}
		if !tr.specS.has(ggpo.EventCodeDisconnectedFromPeer, 0) {
			t.Fatalf("%s: the watcher's game was not told the watch ended", c.name)
		}
		// the players carry on
		before := tr.hostFrames
		if err := tr.run(30); err != nil {
			t.Fatalf("%s: after the bad packet: %v", c.name, err)
		}
		if tr.hostFrames-before < 25 {
			t.Fatalf("%s: the players stalled after the watcher stopped", c.name)
		}
		t.Logf("%-26s watcher stopped: %s", c.name, why)
	}
}
