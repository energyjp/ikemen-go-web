package ggpo

import (
	"github.com/ikemen-engine/ggpo/internal/input"
	"github.com/ikemen-engine/ggpo/internal/messages"
	"github.com/ikemen-engine/ggpo/internal/polling"
	"github.com/ikemen-engine/ggpo/internal/protocol"
	"github.com/ikemen-engine/ggpo/internal/util"
	"github.com/ikemen-engine/ggpo/transport"
)

// How many frames of the host's input a watcher holds. A watcher takes in
// (and acknowledges) only frames that fit, so the host keeps the rest and a
// watcher can never lose a frame to its own buffer; one that stays behind for
// longer than the host keeps frames for it is dropped from the match with a
// reason (protocol.SpectatorMaxPendingFrames). 256 frames is about 4 s.
const SpectatorFrameBufferSize int = 256
const DefaultMaxFramesBehind int = 10
const DefaultCatchupSpeed int = 1

type Spectator struct {
	session         Session
	poll            polling.Poller
	connection      transport.Connection
	host            protocol.UdpProtocol
	synchonizing    bool
	inputSize       int
	numPlayers      int
	nextInputToSend int
	inputs          []input.GameInput
	hostIp          string
	hostPort        int
	framesBehind    int
	localPort       int
	currentFrame    int
	messageChannel  chan transport.MessageChannelItem
	failReason      string
	disconnectSent  bool
}

func NewSpectator(cb Session, localPort int, numPlayers int, inputSize int, hostIp string, hostPort int) Spectator {
	s := Spectator{}
	s.numPlayers = numPlayers
	s.inputSize = inputSize
	s.nextInputToSend = 0

	s.session = cb
	s.synchonizing = true

	// Every slot starts at frame -1, "nothing here yet". This must index the
	// slice: `for _, i := range inputs { i.Frame = -1 }` set a copy and left
	// every slot at frame 0, so slot 0 passed for the host's frame 0, with no
	// input in it, before frame 0 had arrived.
	inputs := make([]input.GameInput, SpectatorFrameBufferSize)
	for i := range inputs {
		inputs[i].Frame = -1
	}
	s.inputs = inputs
	//port := strconv.Itoa(hostPort)
	//s.udp = NewUdp(&s, localPort)
	s.hostIp = hostIp
	s.hostPort = hostPort
	s.localPort = localPort
	var poll polling.Poll = polling.NewPoll()
	s.poll = &poll
	s.messageChannel = make(chan transport.MessageChannelItem, 200)
	//go s.udp.Read()
	return s
}

func (s *Spectator) Idle(timeout int, timeFunc ...polling.FuncTimeType) error {
	// Take in only what the buffer can hold without overwriting a frame not
	// yet simulated.
	s.host.SetRecvLimit(s.nextInputToSend + SpectatorFrameBufferSize - 1)
	s.HandleMessages()
	if len(timeFunc) == 0 {
		s.poll.Pump()
	} else {
		s.poll.Pump(timeFunc[0])
	}
	s.PollUdpProtocolEvents()

	// No catch-up by skipping. The old loop here stepped over frames whose
	// input was gone, calling AdvanceFrame with none: the watcher carried on
	// simulating a different match from the players, silently. A watcher
	// cannot lose a frame any more (see SpectatorFrameBufferSize); if one is
	// ever missing, SyncInput ends the watch with a reason instead.
	return nil
}

func (s *Spectator) SyncInput(disconnectFlags *int) ([][]byte, error) {
	// Wait until we've started to return inputs
	if s.synchonizing {
		return nil, Error{Code: ErrorCodeNotSynchronized, Name: "ErrorCodeNotSynchronized"}
	}

	input := s.inputs[s.nextInputToSend%SpectatorFrameBufferSize]
	s.currentFrame = input.Frame
	if input.Frame < s.nextInputToSend {
		// Haved recieved input from the host yet. Wait
		return nil, Error{Code: ErrorCodePredictionThreshod, Name: "ErrorCodePredictionThreshod"}

	}
	if input.Frame > s.nextInputToSend {
		s.framesBehind = input.Frame - s.nextInputToSend
		// The input for this frame is gone: never guess, stop watching.
		s.fail("this watcher fell behind the match and lost frame " + itoa(s.nextInputToSend))
		return nil, Error{Code: ErrorCodeGeneralFailure, Name: "ErrorCodeGeneralFailure"}

	}
	if len(input.Bits) != s.inputSize*s.numPlayers {
		s.fail("the host sent a frame of the wrong size")
		return nil, Error{Code: ErrorCodeGeneralFailure, Name: "ErrorCodeGeneralFailure"}
	}
	//s.framesBehind = 0

	//Assert(size >= s.inputSize*s.numPlayers)
	values := make([][]byte, s.numPlayers)
	offset := 0
	counter := 0
	for offset < len(input.Bits) {
		values[counter] = input.Bits[offset : s.inputSize+offset]
		offset += s.inputSize
		counter++
	}

	if disconnectFlags != nil {
		*disconnectFlags = 0 // xxx: we should get them from the host! -pond3r
	}
	s.nextInputToSend++
	return values, nil
}

func (s *Spectator) AdvanceFrame(checksum uint32) error {
	util.Log.Printf("End of frame (%d)...\n", s.nextInputToSend-1)
	s.Idle(0)
	s.PollUdpProtocolEvents()

	return nil
}

func (s *Spectator) PollUdpProtocolEvents() {
	for {
		evt, ok := s.host.GetEvent()
		if ok != nil {
			break
		} else {
			s.OnUdpProtocolEvent(evt)
		}
	}
}

func (s *Spectator) OnUdpProtocolEvent(evt *protocol.UdpProtocolEvent) {
	var info Event
	switch evt.Type() {
	case protocol.ConnectedEvent:
		info.Code = EventCodeConnectedToPeer
		info.Player = 0
		s.session.OnEvent(&info)

	case protocol.SynchronizingEvent:
		info.Code = EventCodeSynchronizingWithPeer
		info.Player = 0
		info.Count = evt.Count
		info.Total = evt.Total
		s.session.OnEvent(&info)

	case protocol.SynchronziedEvent:
		if s.synchonizing {
			info.Code = EventCodeSynchronizedWithPeer
			info.Player = 0
			s.session.OnEvent(&info)

			info.Code = EventCodeRunning
			s.session.OnEvent(&info)
			s.synchonizing = false
		}

	case protocol.NetworkInterruptedEvent:
		info.Code = EventCodeConnectionInterrupted
		info.Player = 0
		info.DisconnectTimeout = evt.DisconnectTimeout
		s.session.OnEvent(&info)

	case protocol.NetworkResumedEvent:
		info.Code = EventCodeConnectionResumed
		info.Player = 0
		s.session.OnEvent(&info)

	case protocol.DisconnectedEvent:
		if s.disconnectSent {
			return
		}
		s.disconnectSent = true
		if s.failReason == "" {
			// the endpoint's own reason (a malformed packet) when it has one
			s.failReason = s.host.FailReason()
		}
		if s.failReason == "" {
			s.failReason = "the host ended the connection"
		}
		info.Code = EventCodeDisconnectedFromPeer
		info.Player = 0
		s.session.OnEvent(&info)

	case protocol.InputEvent:
		input := evt.Input

		s.host.SetLocalFrameNumber(input.Frame)
		s.host.SendInputAck()
		s.inputs[input.Frame%SpectatorFrameBufferSize] = input
	}
}

func (s *Spectator) HandleMessage(ipAddress string, port int, msg messages.UDPMessage, len int) {
	if s.host.HandlesMsg(ipAddress, port) {
		s.host.OnMsg(msg, len)
	}
}

func (p *Spectator) AddLocalInput(player PlayerHandle, values []byte, size int) error {
	return nil
}

func (s *Spectator) AddPlayer(player *Player, handle *PlayerHandle) error {
	return Error{Code: ErrorCodeInvalidRequest, Name: "ErrorCodeInvalidRequest"}
}

// We must 'impliment' these for this to be a true Session
func (s *Spectator) DisconnectPlayer(handle PlayerHandle) error {
	return Error{Code: ErrorCodeInvalidRequest, Name: "ErrorCodeInvalidRequest"}
}
func (s *Spectator) GetNetworkStats(handle PlayerHandle) (protocol.NetworkStats, error) {
	return protocol.NetworkStats{}, Error{Code: ErrorCodeInvalidRequest, Name: "ErrorCodeInvalidRequest"}
}
func (s *Spectator) SetFrameDelay(player PlayerHandle, delay int) error {
	return Error{Code: ErrorCodeInvalidRequest, Name: "ErrorCodeInvalidRequest"}
}
func (s *Spectator) SetDisconnectTimeout(timeout int) error {
	return Error{Code: ErrorCodeInvalidRequest, Name: "ErrorCodeInvalidRequest"}
}
func (s *Spectator) SetDisconnectNotifyStart(timeout int) error {
	return Error{Code: ErrorCodeInvalidRequest, Name: "ErrorCodeInvalidRequest"}
}

// Close stops this watcher's endpoint and its transport reader; the next
// match builds a new Spectator over the same page connection.
func (s *Spectator) Close() error {
	if s.host.IsInitialized() {
		s.host.Close()
	} else if s.connection != nil {
		s.connection.Close()
	}
	return nil
}
func (s *Spectator) InitializeConnection(c ...transport.Connection) error {
	if len(c) == 0 {
		s.connection = transport.NewUdp(s, s.localPort)
		return nil
	}
	s.connection = c[0]
	return nil
}

func (s *Spectator) HandleMessages() {
	for i := 0; i < len(s.messageChannel); i++ {
		mi := <-s.messageChannel
		s.HandleMessage(mi.Peer.Ip, mi.Peer.Port, mi.Message, mi.Length)
	}
}

func (s *Spectator) Start() {
	go s.connection.Read(s.messageChannel)

	s.host = protocol.NewUdpProtocol(s.connection, 0, s.hostIp, s.hostPort, nil)
	s.host.SetWatcherLine()
	// A host that vanishes ends the watch instead of leaving it waiting forever
	// (the endpoint had no timeout at all before).
	s.host.SetDisconnectTimeout(DefaultDisconnectTimeout)
	s.host.SetDisconnectNotifyStart(DefaultDisconnectNotifyStart)
	s.poll.RegisterLoop(&s.host, nil)
	s.host.Synchronize()

}

// fail ends this watch with a reason: one EventCodeDisconnectedFromPeer to the
// session, and the host is told, best effort.
func (s *Spectator) fail(reason string) {
	if s.failReason == "" {
		s.failReason = reason
	}
	if s.host.IsInitialized() && !s.host.IsDisconnected() {
		s.host.Fail(reason)
	}
	if !s.disconnectSent {
		s.disconnectSent = true
		info := Event{Code: EventCodeDisconnectedFromPeer}
		s.session.OnEvent(&info)
	}
}

// FailReason says why this watch ended ("" while it is running).
func (s *Spectator) FailReason() string {
	if s.failReason != "" {
		return s.failReason
	}
	return s.host.FailReason()
}

// Frame is the next frame this watcher will simulate.
func (s *Spectator) Frame() int {
	return s.nextInputToSend
}

// Buffered is how many frames have arrived and wait to be simulated.
func (s *Spectator) Buffered() int {
	n := s.host.LastReceivedFrame() - s.nextInputToSend + 1
	if n < 0 {
		return 0
	}
	return n
}

// Running reports whether the watcher has synced with the host and not ended.
func (s *Spectator) Running() bool {
	return !s.synchonizing && !s.disconnectSent
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
