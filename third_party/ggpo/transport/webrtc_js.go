//go:build js

package transport

import (
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
	// The lobby layer owns the RTCPeerConnection lifecycle.
}

func (w *WebRTC) IsInitialized() bool {
	return w.bridge.Truthy()
}
