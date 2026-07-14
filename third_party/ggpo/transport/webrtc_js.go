//go:build js

package transport

import (
	"sync/atomic"
	"syscall/js"
	"time"

	"github.com/ikemen-engine/ggpo/internal/messages"
	"github.com/ikemen-engine/ggpo/internal/util"
)

// WebRTC transport for browser builds (Ikemen GO web port).
//
// Rollback wants UDP semantics; the page bridge (globalThis.ikemenNet)
// provides an unordered/unreliable RTCDataChannel ("ggpo") whose whole
// datagrams are exchanged via sendGGPO/readGGPO. There is exactly one
// remote peer, identified to GGPO's endpoint matching by the (peerIp,
// peerPort) this transport stamps on every incoming message.

type WebRTC struct {
	bridge   js.Value
	peerIp   string
	peerPort int
	closed   atomic.Bool
}

// NewWebRTC wraps the page bridge as a GGPO transport. peerIp/peerPort
// must match the values the engine registered for the remote player.
func NewWebRTC(bridge js.Value, peerIp string, peerPort int) *WebRTC {
	return &WebRTC{bridge: bridge, peerIp: peerIp, peerPort: peerPort}
}

func (w *WebRTC) SendTo(msg messages.UDPMessage, remoteIp string, remotePort int) {
	if msg == nil || !w.bridge.Truthy() {
		return
	}
	b := msg.ToBytes()
	buf := js.Global().Get("Uint8Array").New(len(b))
	js.CopyBytesToJS(buf, b)
	w.bridge.Call("sendGGPO", buf)
}

func (w *WebRTC) Read(messageChan chan MessageChannelItem) {
	recvBuf := make([]byte, MaxUDPPacketSize*2)
	for {
		// The engine builds a fresh GGPO session per match over the same
		// DataChannel; the previous session's reader MUST stop or the two
		// readers steal each other's datagrams (observed as a
		// FindSavedFrameIndex panic when starting a rematch).
		if w.closed.Load() {
			return
		}
		v := w.bridge.Call("readGGPO")
		if v.IsNull() {
			return // channel closed
		}
		n := v.Get("length").Int()
		if n == 0 {
			time.Sleep(time.Millisecond)
			continue
		}
		if n > len(recvBuf) {
			recvBuf = make([]byte, n)
		}
		js.CopyBytesToGo(recvBuf[:n], v)
		msg, err := messages.DecodeMessageBinary(recvBuf[:n])
		if err != nil {
			util.Log.Printf("Error decoding message: %s", err)
			continue
		}
		messageChan <- MessageChannelItem{
			Peer:    peerAddress{Ip: w.peerIp, Port: w.peerPort},
			Message: msg,
			Length:  n,
		}
	}
}

func (w *WebRTC) Close() {
	// Stops this transport's Read loop. The RTCPeerConnection itself is
	// owned by the lobby layer and survives across matches.
	w.closed.Store(true)
}

func (w *WebRTC) IsInitialized() bool {
	return w.bridge.Truthy()
}
