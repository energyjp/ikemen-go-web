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

// The page's Uint8Array constructor, looked up once: every lookup of an object
// hands Go a new reference that its garbage collector must finalize later.
var jsUint8Array = js.Global().Get("Uint8Array")

// isDatagram: v is a datagram array. Asked with numEmpty = true, a page that
// knows the argument answers an empty read with the number 0 (no JS object,
// so nothing for the garbage collector to finalize); an older page still
// answers with an empty array, read the same way.
func isDatagram(v js.Value) bool {
	return v.Type() == js.TypeObject
}

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
	buf := jsUint8Array.New(len(b))
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
		v := w.bridge.Call("readGGPO", true)
		if v.IsNull() {
			return // channel closed
		}
		n := 0
		if isDatagram(v) {
			n = v.Get("length").Int()
		}
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

// ---------------------------------------------------------------------------
// The host's transport when watchers are registered for a match.
//
// A watcher has its own RTCPeerConnection on the host's page (one per slot),
// so its datagrams travel through a second page bridge, globalThis.ikemenWatch
// (public/ikemen/webrtc.js), addressed by slot. GGPO tells peers apart by
// (ip, port): the opponent stays at the port the engine registered for it, and
// watcher n is given WatcherBasePort+n.
//
// Without a watcher the engine never builds one of these: it hands GGPO the
// plain WebRTC transport above, so the players' path is the same object making
// the same calls into the page as before.

const (
	WatcherBasePort = 7650
	MaxWatcherSlots = 32
	// The most watcher datagrams the hub takes per wake-up before it yields:
	// the page already holds each watcher to a budget, and this keeps even a
	// full set of watchers from holding the engine's thread between frames.
	hubReadBudget = 64
)

type WebRTCHub struct {
	player *WebRTC
	watch  js.Value
	ip     string
	closed atomic.Bool
	gone   [MaxWatcherSlots]atomic.Bool
}

// NewWebRTCHub routes the opponent through player (unchanged) and watcher
// slots through watch, a bridge with sendTo(slot, Uint8Array) and readAny()
// (one datagram with its slot in byte 0; a single byte means the watcher this
// match registered in that slot has closed; empty when nothing is waiting;
// null once closed). The page takes turns between the watchers, and only the
// watchers registered for this match are read, so a slot reused by a newcomer
// never speaks for, or closes, the one the match registered.
func NewWebRTCHub(player *WebRTC, watch js.Value) *WebRTCHub {
	return &WebRTCHub{player: player, watch: watch, ip: player.peerIp}
}

func (h *WebRTCHub) SendTo(msg messages.UDPMessage, remoteIp string, remotePort int) {
	slot := remotePort - WatcherBasePort
	if slot < 0 || slot >= MaxWatcherSlots {
		h.player.SendTo(msg, remoteIp, remotePort)
		return
	}
	if msg == nil || h.gone[slot].Load() || !h.watch.Truthy() {
		return
	}
	b := msg.ToBytes()
	buf := jsUint8Array.New(len(b))
	js.CopyBytesToJS(buf, b)
	h.watch.Call("sendTo", slot, buf)
}

func (h *WebRTCHub) Read(messageChan chan MessageChannelItem) {
	go h.player.Read(messageChan)
	recvBuf := make([]byte, MaxUDPPacketSize*2)
	for {
		// At most hubReadBudget datagrams, then yield (or sleep when nothing
		// waits), so a burst from the watchers never runs the hub for long.
		for read := 0; read < hubReadBudget; read++ {
			if h.closed.Load() {
				return
			}
			v := h.watch.Call("readAny", true)
			if v.IsNull() || v.IsUndefined() {
				return
			}
			n := 0
			if isDatagram(v) {
				n = v.Get("length").Int()
			}
			if n == 0 {
				break
			}
			if n > len(recvBuf) {
				// A datagram bigger than any GGPO message is not one: drop it
				// rather than grow a buffer for a stranger.
				continue
			}
			js.CopyBytesToGo(recvBuf[:n], v)
			slot := int(recvBuf[0])
			if slot >= MaxWatcherSlots {
				continue
			}
			if n == 1 {
				h.gone[slot].Store(true)
				continue
			}
			if h.gone[slot].Load() {
				continue
			}
			msg, err := messages.DecodeMessageBinary(recvBuf[1:n])
			if err != nil {
				util.Log.Printf("watcher %d: bad datagram: %s", slot, err)
				continue
			}
			messageChan <- MessageChannelItem{
				Peer:    peerAddress{Ip: h.ip, Port: WatcherBasePort + slot},
				Message: msg,
				Length:  n - 1,
			}
		}
		time.Sleep(time.Millisecond)
	}
}

// Gone reports whether a watcher slot's page connection has closed.
func (h *WebRTCHub) Gone(slot int) bool {
	return slot >= 0 && slot < MaxWatcherSlots && h.gone[slot].Load()
}

func (h *WebRTCHub) Close() {
	h.closed.Store(true)
	h.player.Close()
}
