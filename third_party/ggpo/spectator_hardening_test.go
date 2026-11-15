package ggpo_test

// What a watcher (a GGPO spectator) must never be able to do to the players.
//
// These tests use only the library's public surface as it stood before the
// hardening, so the same file builds against the older library: each one
// failed there (a panic in SendInput, a host that never started, a decoder
// panic, a watcher that skipped frames and carried on) and passes here.
//
// The three parties talk over a switchboard (testBoard below) that delivers
// a message synchronously, like the existing mocks, and can cut one party off
// in both directions, which is how a watcher that stops acknowledging, never
// connects, or only partly connects is simulated.

import (
	"fmt"
	"math/rand"
	"strconv"
	"testing"
	"time"

	"github.com/ikemen-engine/ggpo"
	"github.com/ikemen-engine/ggpo/internal/messages"
	"github.com/ikemen-engine/ggpo/internal/protocol"
	"github.com/ikemen-engine/ggpo/transport"
)

type testBoard struct {
	handlers map[string]transport.MessageHandler
	cut      map[string]bool
	lastFrom map[string]messages.UDPMessage
	// A far watcher: messages to and from the spectator's port are held for
	// delay display frames (each way), and with shuffle a frame's batch is
	// delivered in a scrambled order, with a loss share of them lost. Zero
	// delay delivers at once, as before.
	delay   int
	shuffle *rand.Rand
	loss    float64
	now     int
	held    []heldMsg
	// ackDelay holds what the spectator sends (its acknowledgments) this many
	// more frames; toSpec counts the bytes sent to the spectator's port.
	ackDelay int
	toSpec   int
}

type heldMsg struct {
	at       int
	fromIP   string
	fromPort int
	to       string
	msg      messages.UDPMessage
}

// tick delivers the held messages that are due.
func (b *testBoard) tick() {
	b.now++
	var due, keep []heldMsg
	for _, m := range b.held {
		if m.at <= b.now {
			due = append(due, m)
		} else {
			keep = append(keep, m)
		}
	}
	b.held = keep
	if b.shuffle != nil {
		b.shuffle.Shuffle(len(due), func(i, j int) { due[i], due[j] = due[j], due[i] })
	}
	for _, m := range due {
		if b.shuffle != nil && b.shuffle.Float64() < b.loss {
			continue
		}
		if h := b.handlers[m.to]; h != nil {
			h.HandleMessage(m.fromIP, m.fromPort, m.msg, m.msg.PacketSize())
		}
	}
}

func newTestBoard() *testBoard {
	return &testBoard{
		handlers: map[string]transport.MessageHandler{},
		cut:      map[string]bool{},
		lastFrom: map[string]messages.UDPMessage{},
	}
}

func boardKey(ip string, port int) string { return ip + ":" + strconv.Itoa(port) }

type boardConn struct {
	b    *testBoard
	ip   string
	port int
}

func (c *boardConn) SendTo(msg messages.UDPMessage, ip string, port int) {
	from := boardKey(c.ip, c.port)
	to := boardKey(ip, port)
	if c.b.cut[from] || c.b.cut[to] {
		return
	}
	c.b.lastFrom[from] = msg
	spec := boardKey(tIP, tSpecPort)
	if to == spec {
		c.b.toSpec += msg.PacketSize()
	}
	wait := 0
	if from == spec || to == spec {
		wait = c.b.delay
	}
	if from == spec {
		wait += c.b.ackDelay
	}
	if wait > 0 {
		c.b.held = append(c.b.held, heldMsg{c.b.now + wait, c.ip, c.port, to, msg})
		return
	}
	if h := c.b.handlers[to]; h != nil {
		h.HandleMessage(c.ip, c.port, msg, msg.PacketSize())
	}
}
func (c *boardConn) Read(chan transport.MessageChannelItem) {}
func (c *boardConn) Close()                                 {}

// recSession records every event, so a test can ask what the library told
// the game. A player's session re-simulates a rolled-back frame the way a game
// does (inputs from SyncInput, then AdvanceFrame); a watcher's does nothing,
// so a watcher library that steps over frames shows up as a gap.
type recSession struct {
	events []ggpo.Event
	peer   *ggpo.Peer
}

func (s *recSession) SaveGameState(int) int { return 0 }
func (s *recSession) LoadGameState(int)     {}
func (s *recSession) AdvanceFrame(int) {
	if s.peer == nil {
		return
	}
	var flags int
	if _, err := s.peer.SyncInput(&flags); err == nil {
		s.peer.AdvanceFrame(0)
	}
}
func (s *recSession) OnEvent(info *ggpo.Event) {
	s.events = append(s.events, *info)
}
func (s *recSession) has(code ggpo.EventCode, minPlayer int) bool {
	for _, e := range s.events {
		if e.Code == code && int(e.Player) >= minPlayer {
			return true
		}
	}
	return false
}

const (
	tIP        = "127.3.0.1"
	tHostPort  = 7600
	tGuestPort = 7550
	tSpecPort  = 7650
	tInputSize = 4
)

type trio struct {
	board                    *testBoard
	host, guest              *ggpo.Peer
	spec                     *ggpo.Spectator
	hostS, guestS, specS     *recSession
	h1, g2, specHandle       ggpo.PlayerHandle
	hostFrames, guestFrames  int
	specFrames               []int // frame numbers the spectator handed out, in order
	specStarted, specConsume bool
}

func newTrio(startSpectator bool) *trio {
	t := &trio{board: newTestBoard(), hostS: &recSession{}, guestS: &recSession{}, specS: &recSession{}}
	host := ggpo.NewPeer(t.hostS, tHostPort, 2, tInputSize)
	guest := ggpo.NewPeer(t.guestS, tGuestPort, 2, tInputSize)
	spec := ggpo.NewSpectator(t.specS, tSpecPort, 2, tInputSize, tIP, tHostPort)
	t.host, t.guest, t.spec = &host, &guest, &spec
	t.hostS.peer, t.guestS.peer = t.host, t.guest
	t.board.handlers[boardKey(tIP, tHostPort)] = t.host
	t.board.handlers[boardKey(tIP, tGuestPort)] = t.guest
	t.board.handlers[boardKey(tIP, tSpecPort)] = t.spec
	t.host.InitializeConnection(&boardConn{t.board, tIP, tHostPort})
	t.guest.InitializeConnection(&boardConn{t.board, tIP, tGuestPort})
	t.spec.InitializeConnection(&boardConn{t.board, tIP, tSpecPort})

	p1 := ggpo.NewLocalPlayer(20, 1)
	p2 := ggpo.NewRemotePlayer(20, 2, tIP, tGuestPort)
	sp := ggpo.NewSpectatorPlayer(20, tIP, tSpecPort)
	var ignore ggpo.PlayerHandle
	t.host.AddPlayer(&p1, &t.h1)
	t.host.AddPlayer(&p2, &ignore)
	t.host.AddPlayer(&sp, &t.specHandle)
	g1 := ggpo.NewRemotePlayer(20, 1, tIP, tHostPort)
	g2 := ggpo.NewLocalPlayer(20, 2)
	t.guest.AddPlayer(&g1, &ignore)
	t.guest.AddPlayer(&g2, &t.g2)
	if startSpectator {
		t.spec.Start()
		t.specStarted = true
	}
	t.specConsume = true
	// Let every first sync request be retried (the first ones go to parties
	// that did not exist yet), the way the existing tests do.
	advance := func() int64 { return time.Now().Add(2000 * time.Millisecond).UnixMilli() }
	for i := 0; i < protocol.NumSyncPackets; i++ {
		t.host.Idle(0, advance)
		t.guest.Idle(0, advance)
		if t.specStarted {
			t.spec.Idle(0, advance)
		}
	}
	return t
}

// frameInput encodes the frame number so a spectator handing out the wrong
// frame is caught by value, not only by count.
func frameInput(f int, who byte) []byte {
	return []byte{byte(f), byte(f >> 8), who, who}
}

// step runs one display frame on all three. It reports a panic as an error
// instead of killing the test binary.
func (t *trio) step() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	var flags int
	t.board.tick()
	t.host.Idle(0)
	if t.host.AddLocalInput(t.h1, frameInput(t.hostFrames, 1), tInputSize) == nil {
		if _, e := t.host.SyncInput(&flags); e == nil {
			t.host.AdvanceFrame(0)
			t.hostFrames++
		}
	}
	t.guest.Idle(0)
	if t.guest.AddLocalInput(t.g2, frameInput(t.guestFrames, 2), tInputSize) == nil {
		if _, e := t.guest.SyncInput(&flags); e == nil {
			t.guest.AdvanceFrame(0)
			t.guestFrames++
		}
	}
	if t.specStarted {
		t.spec.Idle(0)
		for t.specConsume {
			vals, e := t.spec.SyncInput(&flags)
			if e != nil {
				break
			}
			if len(vals) != 2 || len(vals[0]) < 2 {
				return fmt.Errorf("spectator handed out a malformed frame %v", vals)
			}
			t.specFrames = append(t.specFrames, int(vals[0][0])|int(vals[0][1])<<8)
			t.spec.AdvanceFrame(0)
		}
	}
	return nil
}

func (t *trio) run(n int) error {
	for i := 0; i < n; i++ {
		if err := t.step(); err != nil {
			return fmt.Errorf("display frame %d: %w", i, err)
		}
	}
	return nil
}

// The frames a spectator hands out must be 0, 1, 2, ... with no gap: a gap is
// a frame the players simulated and the watcher did not, a desync nobody sees.
func (t *trio) specFramesContiguous() error {
	for i, f := range t.specFrames {
		if f != i&0xffff {
			return fmt.Errorf("spectator frame #%d was the players' frame %d", i, f)
		}
	}
	return nil
}

func TestWatcherThatStopsAcknowledgingIsDroppedNotFatal(t *testing.T) {
	tr := newTrio(true)
	if err := tr.run(30); err != nil {
		t.Fatalf("before the cut: %v", err)
	}
	if len(tr.specFrames) == 0 {
		t.Fatalf("the spectator never received a frame before the cut")
	}
	// The watcher goes silent: nothing reaches it, nothing comes back.
	tr.board.cut[boardKey(tIP, tSpecPort)] = true
	hostBefore := tr.hostFrames
	if err := tr.run(400); err != nil {
		t.Fatalf("the host did not survive a silent watcher: %v", err)
	}
	if tr.hostFrames-hostBefore < 390 || tr.guestFrames < tr.hostFrames-10 {
		t.Fatalf("the players stalled behind a silent watcher: host advanced %d of 400 frames, guest at %d",
			tr.hostFrames-hostBefore, tr.guestFrames)
	}
	if !tr.hostS.has(ggpo.EventCodeDisconnectedFromPeer, 1000) {
		t.Fatalf("the silent watcher was never dropped")
	}
	if tr.hostS.has(ggpo.EventCodeDisconnectedFromPeer, 1) && !onlySpectatorDisconnects(tr.hostS) {
		t.Fatalf("a player was disconnected because of a watcher")
	}
}

func onlySpectatorDisconnects(s *recSession) bool {
	for _, e := range s.events {
		if e.Code == ggpo.EventCodeDisconnectedFromPeer && int(e.Player) < 1000 {
			return false
		}
	}
	return true
}

func TestMatchStartDoesNotWaitForAWatcher(t *testing.T) {
	// The watcher is registered with the host but never answers: its page
	// is still loading, or it has gone.
	tr := newTrio(false)
	if !tr.hostS.has(ggpo.EventCodeRunning, 0) {
		// Give the players a few more polls to finish their own sync.
		advance := func() int64 { return time.Now().Add(2000 * time.Millisecond).UnixMilli() }
		for i := 0; i < 5; i++ {
			tr.host.Idle(0, advance)
			tr.guest.Idle(0, advance)
		}
	}
	if !tr.hostS.has(ggpo.EventCodeRunning, 0) {
		t.Fatalf("the host's match waits on a watcher that never connected")
	}
	if err := tr.run(300); err != nil {
		t.Fatalf("%v", err)
	}
	if tr.hostFrames < 290 {
		t.Fatalf("the host advanced only %d of 300 frames with an absent watcher", tr.hostFrames)
	}
	if !tr.hostS.has(ggpo.EventCodeDisconnectedFromPeer, 1000) {
		t.Fatalf("a watcher that never connected is still registered after %d frames", tr.hostFrames)
	}
	if !onlySpectatorDisconnects(tr.hostS) {
		t.Fatalf("a player was disconnected because of a watcher")
	}
}

func TestMalformedPacketsAreRejectedNotFatal(t *testing.T) {
	// Decoder: every one of these used to index past the end of the buffer.
	header := func(n int) []byte {
		b := make([]byte, n)
		b[4] = byte(messages.InputMsg)
		return b
	}
	zeroStatuses := header(24) // count 0: the decoder read status [0] anyway
	manyStatuses := header(24) // claims 200 statuses in 24 bytes
	manyStatuses[5] = 200
	longBits := header(43) // 4 statuses, then claims 200 bytes of input
	longBits[5] = 4
	longBits[6+4*5+4+1+4+4+2+1] = 200
	cases := []struct {
		name      string
		buf       []byte
		mustError bool // false: a short but complete packet may decode
	}{
		{"zero statuses", zeroStatuses, false},
		{"200 statuses", manyStatuses, true},
		{"long bits", longBits, true},
	}
	for _, c := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("decoding a packet with %s panicked: %v", c.name, r)
				}
			}()
			if _, err := messages.DecodeMessageBinary(c.buf); err == nil && c.mustError {
				t.Errorf("a packet with %s decoded without an error", c.name)
			}
		}()
	}

	// Protocol: a well-formed packet with hostile content, from the watcher's
	// own address and with its magic number, after it has synced.
	tr := newTrio(true)
	if err := tr.run(20); err != nil {
		t.Fatalf("before the bad packet: %v", err)
	}
	last := tr.board.lastFrom[boardKey(tIP, tSpecPort)]
	if last == nil {
		t.Fatalf("the spectator never sent anything")
	}
	bad := messages.NewUDPMessage(messages.InputMsg).(*messages.InputPacket)
	bad.PeerConnectStatus = []messages.UdpConnectStatus{{LastFrame: 5}} // one status, the host reads four
	bad.StartFrame = 9000
	bad.InputSize = 1
	bad.NumBits = 1
	bad.Bits = []byte{1, 2, 3}
	bad.SetHeader(last.Header().Magic, last.Header().SequenceNumber+1)
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("a hostile packet from a watcher panicked the host: %v", r)
			}
		}()
		tr.host.HandleMessage(tIP, tSpecPort, bad, bad.PacketSize())
	}()
	hostBefore := tr.hostFrames
	if err := tr.run(60); err != nil {
		t.Fatalf("after the bad packet: %v", err)
	}
	if tr.hostFrames-hostBefore < 55 {
		t.Fatalf("the host stalled after a bad watcher packet")
	}
	if !tr.hostS.has(ggpo.EventCodeDisconnectedFromPeer, 1000) {
		t.Fatalf("the watcher that sent a hostile packet was not dropped")
	}
	if !onlySpectatorDisconnects(tr.hostS) {
		t.Fatalf("a player was disconnected because of a watcher's packet")
	}
}

func TestWatcherPastItsBufferIsDroppedNotSkipped(t *testing.T) {
	tr := newTrio(true)
	if err := tr.run(10); err != nil {
		t.Fatalf("%v", err)
	}
	// The watcher still receives (and its network acknowledges) but stops
	// simulating: a frozen tab, a machine that cannot keep up.
	tr.specConsume = false
	if err := tr.run(800); err != nil {
		t.Fatalf("the host did not survive a watcher that fell behind: %v", err)
	}
	if tr.hostFrames < 790 {
		t.Fatalf("the players stalled behind a lagging watcher: host at %d", tr.hostFrames)
	}
	// It wakes up and tries to carry on.
	tr.specConsume = true
	if err := tr.run(20); err != nil {
		t.Fatalf("%v", err)
	}
	if err := tr.specFramesContiguous(); err != nil {
		t.Fatalf("the watcher skipped frames instead of stopping: %v", err)
	}
	if !tr.hostS.has(ggpo.EventCodeDisconnectedFromPeer, 1000) && !tr.specS.has(ggpo.EventCodeDisconnectedFromPeer, 0) {
		t.Fatalf("a watcher %d frames behind was neither dropped by the host nor told it was gone",
			tr.hostFrames-len(tr.specFrames))
	}
	if !onlySpectatorDisconnects(tr.hostS) {
		t.Fatalf("a player was disconnected because of a watcher")
	}
}

// A watcher a long way off still keeps up. The host used to send a watcher one
// packet of its oldest unacknowledged frames per send (31 frames here, 15 in
// the engine), so a watcher got at most that many frames per round trip and,
// past a round trip of that many frames, fell behind until it was dropped.
func TestFarWatcherKeepsUp(t *testing.T) {
	tr := newTrio(true)
	tr.board.delay = 30 // each way: a one-second round trip at 60 fps
	if err := tr.run(1500); err != nil {
		t.Fatal(err)
	}
	if why := tr.host.SpectatorDropReason(tr.specHandle); why != "" {
		t.Fatalf("the far watcher was dropped: %s (it had %d of the host's %d frames)", why, len(tr.specFrames), tr.hostFrames)
	}
	if err := tr.specFramesContiguous(); err != nil {
		t.Fatal(err)
	}
	if behind := tr.hostFrames - len(tr.specFrames); behind > 120 {
		t.Fatalf("the far watcher is %d frames behind the host (%d of %d)", behind, len(tr.specFrames), tr.hostFrames)
	}
}

// The packets of one send can arrive out of order, or not at all: a packet
// that starts past the watcher's next frame is left for the host to resend,
// never a panic and never a gap, and the watcher still keeps up. (It used to
// pass whenever the watcher was dropped, so it held on a library that could
// not keep a far watcher at all: the one before dd76bac sent a single packet
// a send, and that watcher fell 240 frames behind and was dropped.)
func TestWatcherWindowOutOfOrderOrLost(t *testing.T) {
	tr := newTrio(true)
	tr.board.delay = 30 // a one-second round trip: a window of several packets
	tr.board.shuffle = rand.New(rand.NewSource(7))
	tr.board.loss = 0.2
	if err := tr.run(900); err != nil {
		t.Fatal(err)
	}
	if err := tr.specFramesContiguous(); err != nil {
		t.Fatal(err)
	}
	if why := tr.host.SpectatorDropReason(tr.specHandle); why != "" {
		t.Fatalf("the watcher was dropped: %s (it had %d of the host's %d frames)", why, len(tr.specFrames), tr.hostFrames)
	}
	if behind := tr.hostFrames - len(tr.specFrames); behind > 150 {
		t.Fatalf("the watcher is %d frames behind the host (%d of %d)", behind, len(tr.specFrames), tr.hostFrames)
	}
	t.Logf("watcher frames %d of %d", len(tr.specFrames), tr.hostFrames)
}

// What one watcher costs the host to send, in bytes per second at 60 fps,
// whatever its round trip and however long it holds back its
// acknowledgments: GGPO's retry sent the whole unacknowledged window on every
// poll, so the cost grew with both (P0 measured about 14 KB/s for a watcher on
// the same machine, and a watcher delaying its acks could push it to about
// 400 KB/s). The upload is paced now (protocol.go spectatorResendFrames) and
// held to a ceiling (spectatorBudgetPerFrame, 24,000 B/s).
func TestWatcherUploadIsBounded(t *testing.T) {
	const frames = 1800
	const ceiling = 24000
	cases := []struct {
		name            string
		delay, ackDelay int
		loss            float64
		keepUp          bool
	}{
		{"same machine", 0, 0, 0, true},
		{"+350 ms round trip", 11, 0, 0, true},
		{"+500 ms round trip, 5% loss", 15, 0, 0.05, true},
		{"acks held back 2 s", 0, 120, 0, true},
		// Past the 240 frames (4 s) a host keeps for a watcher: dropped, with
		// the reason, and its cost stays under the ceiling until then.
		{"acks held back 3.5 s", 0, 210, 0, false},
		{"never acknowledges", 0, 100000, 0, false},
	}
	for _, c := range cases {
		tr := newTrio(true)
		tr.board.delay, tr.board.ackDelay = c.delay, c.ackDelay
		if c.loss > 0 {
			tr.board.shuffle = rand.New(rand.NewSource(3))
			tr.board.loss = c.loss
		}
		if err := tr.run(frames); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		bps := tr.board.toSpec * 60 / frames
		why := tr.host.SpectatorDropReason(tr.specHandle)
		t.Logf("%-28s %6d B/s to the watcher, %4d of %d frames, dropped: %q", c.name, bps, len(tr.specFrames), tr.hostFrames, why)
		if bps > ceiling {
			t.Errorf("%s: the host sent %d B/s to one watcher, over the %d B/s ceiling", c.name, bps, ceiling)
		}
		if c.keepUp && why != "" {
			t.Errorf("%s: dropped: %s", c.name, why)
		}
		if !c.keepUp && why == "" {
			t.Errorf("%s: a watcher that never acknowledges was not dropped", c.name)
		}
		if err := tr.specFramesContiguous(); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}
