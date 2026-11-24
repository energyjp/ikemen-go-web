//go:build js

package main

import (
	"errors"
	"fmt"
	"net"
	"syscall/js"
	"time"

	"github.com/ikemen-engine/ggpo/transport"
)

// Browser netplay transport (V2): both netplay layers ride ONE WebRTC
// peer connection with TWO DataChannels, managed by the page bridge
// (public/ikemen/webrtc.js, globalThis.ikemenNet):
//
//   "ikemen" - ordered+reliable: carries NetConnection's byte stream
//              (lobby/handshake/delay-netcode), TCP semantics.
//   "ggpo"   - unordered+unreliable (maxRetransmits 0): carries GGPO
//              rollback datagrams, UDP semantics. Message boundaries are
//              preserved (bridge queues whole datagrams).
//
// Peer addressing: GGPO routes incoming messages by (ip, port) match
// against the registered remote player. Both sides use ip "webrtc" and
// the fixed engine ports (host registers remote 7550, guest 7600), and
// this transport stamps incoming datagrams accordingly.

const webrtcPeerIP = "webrtc"

// The fixed GGPO ports of the two sides (2.4's values). Each side registers
// its remote player at the OTHER side's port, and the transport stamps every
// incoming datagram with that same port, so the pair must agree. They are not
// sockets: the configured Netplay.Rollback.Port is still exchanged in the
// handshake (desktop needs it) and ignored here (platformRollbackPeer).
const (
	webrtcHostGGPOPort  = 7600
	webrtcGuestGGPOPort = 7550
)

// ---------------------------------------------------------------------------
// lobby stream (NetConnection)

type webrtcConn struct {
	bridge   js.Value
	deadline time.Time
}

// webrtcTimeout satisfies net.Error so shared polling code
// (tryReadU8) can distinguish timeouts from real failures.
type webrtcTimeout struct{}

func (webrtcTimeout) Error() string   { return "webrtc: read deadline exceeded" }
func (webrtcTimeout) Timeout() bool   { return true }
func (webrtcTimeout) Temporary() bool { return true }

func (c *webrtcConn) SetReadDeadline(t time.Time) error {
	c.deadline = t
	return nil
}

// SetDeadline: only reads can wait (send queues in the page bridge and never
// blocks), so the read deadline is the whole deadline.
func (c *webrtcConn) SetDeadline(t time.Time) error {
	c.deadline = t
	return nil
}

// SetWriteDeadline is a no-op for the same reason.
func (c *webrtcConn) SetWriteDeadline(t time.Time) error {
	return nil
}

// webrtcAddr is the address of either end of the page's DataChannel. It is
// deliberately not a *net.TCPAddr: shared code that needs a real IP (the
// guest's localhost rollback port pick) checks the type and skips.
type webrtcAddr struct{}

func (webrtcAddr) Network() string { return "webrtc" }
func (webrtcAddr) String() string  { return webrtcPeerIP }

func (c *webrtcConn) LocalAddr() net.Addr  { return webrtcAddr{} }
func (c *webrtcConn) RemoteAddr() net.Addr { return webrtcAddr{} }

func (c *webrtcConn) Read(p []byte) (int, error) {
	for {
		// true: a page that knows it answers "nothing yet" with 0, not an
		// empty array (one less JS object per poll for the GC to finalize).
		r := c.bridge.Call("read", len(p), true)
		if r.IsNull() {
			return 0, errors.New("webrtc: connection closed")
		}
		if r.Type() == js.TypeObject {
			if n := r.Get("length").Int(); n > 0 {
				js.CopyBytesToGo(p[:n], r)
				return n, nil
			}
		}
		if !c.deadline.IsZero() && time.Now().After(c.deadline) {
			return 0, webrtcTimeout{}
		}
		// Esc aborts a blocking wait (peer's engine may have left).
		if sys.esc {
			return 0, errors.New("webrtc: canceled")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (c *webrtcConn) Write(p []byte) (int, error) {
	buf := jsUint8ArrayCtor.New(len(p))
	js.CopyBytesToJS(buf, p)
	if !c.bridge.Call("send", buf).Bool() {
		return 0, errors.New("webrtc: send failed")
	}
	return len(p), nil
}

func (c *webrtcConn) Close() error {
	c.bridge.Call("close")
	return nil
}

// webrtcCloser tears down the page-side session when the engine abandons
// netplay before/without a live conn (stored in nc.ln).
type webrtcCloser struct {
	bridge js.Value
}

func (c *webrtcCloser) Close() error {
	c.bridge.Call("close")
	return nil
}

func (nc *NetConnection) webrtcStart(host bool) error {
	bridge := js.Global().Get("ikemenNet")
	if bridge.IsUndefined() {
		return Error("webrtc bridge (ikemenNet) missing from page")
	}
	nc.ln = &webrtcCloser{bridge: bridge}
	nc.host = host
	nc.conn = nil
	// Fixed input channels, as upstream eae619c3 (GetHostGuestRemap is gone).
	if host {
		nc.locIn, nc.remIn = 0, 1
	} else {
		nc.remIn, nc.locIn = 0, 1
	}
	mode := "join"
	if host {
		mode = "host"
	}
	bridge.Call("start", mode)
	nc.webrtcAwait(bridge, host)
	return nil
}

// Matchmaking queue: the server decides who hosts, so unlike webrtcStart the
// host/guest fields can only be filled in AFTER the bridge reports a pairing
// (queueRole()). Everything from the DataChannel wait onward is the shared
// path.
//
// `mode` selects WHICH queue - "queue" (ranked) or "casual". It is passed
// straight to the page bridge, which puts it on the matchmaking socket; the
// server buckets on it, so the two queues are separate pools that never pair
// with each other. Everything below this line is identical for both: the same
// role assignment, the same transport, and therefore the same desync
// behaviour, which is the point - casual gets the rollback fixes for free
// because they live in the engine and the netcode, not in ranked.
func (nc *NetConnection) webrtcStartQueue(mode string) error {
	bridge := js.Global().Get("ikemenNet")
	if bridge.IsUndefined() {
		return Error("webrtc bridge (ikemenNet) missing from page")
	}
	nc.ln = &webrtcCloser{bridge: bridge}
	nc.conn = nil
	bridge.Call("start", mode)
	SafeGo(func() {
		// Queue waits are open-ended by design (an empty queue sits at
		// "1 player waiting" until someone shows up); only a closing
		// engine (Esc) ends the wait early.
		deadline := time.Now().Add(2 * time.Hour)
		for {
			if nc.isClosing() || time.Now().After(deadline) {
				return
			}
			role := bridge.Call("queueRole").String()
			if role != "" {
				nc.host = role == "host"
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		// Fixed input channels, as upstream eae619c3.
		if nc.host {
			nc.locIn, nc.remIn = 0, 1
		} else {
			nc.remIn, nc.locIn = 0, 1
		}
		// enterNetPlay ran with the placeholder address 'webrtc-queue',
		// which preset the rollback session as guest-of-'webrtc-queue'.
		// Both halves of that are wrong once the real role arrives:
		//  - as host, session.host must be EMPTY (preMatchSetup branches
		//    on that to init as P1);
		//  - as guest, session.host is the REMOTE ADDRESS handed to
		//    InitP2, and the transport stamps every incoming datagram
		//    with webrtcPeerIP - leaving 'webrtc-queue' here made GGPO
		//    drop 100% of remote inputs (field: hard stutter for a few
		//    seconds, then the session dies).
		if sys.cfg.Netplay.RollbackNetcode && sys.rollback.session != nil {
			if nc.host {
				sys.rollback.session.host = ""
			} else {
				sys.rollback.session.host = webrtcPeerIP
			}
		}
		nc.webrtcAwaitBody(bridge, nc.host)
	})
	return nil
}

// webrtcAwait runs the DataChannel wait + IKEMENGO handshake in the
// background; webrtcAwaitBody is the same thing on the caller's goroutine
// (the queue path is already inside a SafeGo of its own).
func (nc *NetConnection) webrtcAwait(bridge js.Value, host bool) {
	SafeGo(func() { nc.webrtcAwaitBody(bridge, host) })
}

func (nc *NetConnection) webrtcAwaitBody(bridge js.Value, host bool) {
	{
		// Wait for the DataChannel to open. Transient ICE 'failed'/
		// 'disconnected' states are NORMAL during NAT traversal over the
		// real internet and often recover, so we do NOT abandon the
		// handshake on bridge.failed() (doing so killed this goroutine
		// mid-negotiation, so the peer's hello was received but never read).
		// Give up only if the engine is closing (user Esc'd) or the bridge
		// reports the session CONTINUOUSLY failed for over a minute (its own
		// state machine already grants transient 'failed'/'disconnected' blips
		// a 12s recovery grace and clears the flag on recovery, so a minute of
		// unbroken failure is a genuinely dead session, not NAT traversal).
		// Do NOT wall-clock the connect wait with a short deadline: a host in
		// the room-code lobby legitimately waits however long the friend takes
		// to download the build and join - a flat 5-minute cap here silently
		// killed this goroutine during such a wait, so when the guest finally
		// connected the transport was alive but the host engine never sent
		// IKEMENGO (guest hung at "waiting for IKEMENGO"). The remaining
		// absolute cap exists only to reap a true zombie goroutine.
		deadline := time.Now().Add(2 * time.Hour)
		var failedSince time.Time
		for !bridge.Call("connected").Bool() {
			if nc.isClosing() || time.Now().After(deadline) {
				return
			}
			if bridge.Call("failed").Bool() {
				if failedSince.IsZero() {
					failedSince = time.Now()
				} else if time.Since(failedSince) > time.Minute {
					return
				}
			} else {
				failedSince = time.Time{}
			}
			time.Sleep(100 * time.Millisecond)
		}
		conn := &webrtcConn{bridge: bridge}
		hlog := func(s string) {
			// Prefer the on-screen overlay (visible without F12); fall back
			// to the console if the bridge lacks olog.
			if !bridge.Get("olog").IsUndefined() {
				bridge.Call("olog", "HSHAKE "+s)
			} else {
				js.Global().Get("console").Call("log", "[HSHAKE] "+s)
			}
		}
		// IKEMENGO handshake, byte-compatible with the TCP path.
		if host {
			hlog("host: sending IKEMENGO")
			if sys.cfg.Netplay.RollbackNetcode {
				sys.rollback.session.remoteIp = webrtcPeerIP
			}
			if _, err := conn.Write([]byte("IKEMENGO")); err != nil {
				hlog("host: write failed: " + err.Error())
				return
			}
			hlog("host: waiting for ack")
			ack := make([]byte, 8)
			if n, err := ioReadFull(conn, ack); err != nil {
				hlog("host: ack read error: " + err.Error())
				return
			} else if string(ack) != "IKEMENGO" {
				hlog(fmt.Sprintf("host: bad ack (n=%d) %q", n, string(ack)))
				return
			}
			hlog("host: handshake complete")
		} else {
			hlog("guest: waiting for IKEMENGO")
			buf := make([]byte, 8)
			if n, err := ioReadFull(conn, buf); err != nil {
				hlog("guest: read error: " + err.Error())
				return
			} else if string(buf) != "IKEMENGO" {
				hlog(fmt.Sprintf("guest: bad hello (n=%d) %q", n, string(buf)))
				return
			}
			hlog("guest: got IKEMENGO, sending ack")
			if _, err := conn.Write([]byte("IKEMENGO")); err != nil {
				hlog("guest: ack write failed: " + err.Error())
				return
			}
			hlog("guest: handshake complete")
		}
		if nc.isClosing() {
			conn.Close()
			return
		}
		// Hand the stream to the main thread (upstream ea69d1f3): only the
		// main thread writes nc.conn, in IsConnected.
		hlog("conn established; entering synchronize")
		nc.publishConnection(conn)
	}
}

func ioReadFull(c *webrtcConn, buf []byte) (int, error) {
	got := 0
	for got < len(buf) {
		n, err := c.Read(buf[got:])
		if err != nil {
			return got, err
		}
		got += n
	}
	return got, nil
}

func (nc *NetConnection) Accept(port int) error {
	return nc.webrtcStart(true)
}

func (nc *NetConnection) Connect(server string, port int) {
	// Two matchmaking queues, selected by the placeholder address the Lua
	// passes to main.f_connect. The server assigns host/guest for both.
	//   'webrtc-queue'  -> RANKED: rated, reported, first-to-N set
	//   'webrtc-casual' -> CASUAL: endless, never reported, never rated
	// Anything else is the classic join flow: this side is the guest.
	if server == "webrtc-queue" {
		nc.webrtcStartQueue("queue")
		return
	}
	if server == "webrtc-casual" {
		nc.webrtcStartQueue("casual")
		return
	}
	// 'webrtc-lobby' -> a winner-stays lobby's match: the lobby server
	// assigned this page its role (host or guest) for the match, and the page
	// hands it over like a queue's (queueRole), so it shares the queue path.
	if server == "webrtc-lobby" {
		nc.webrtcStartQueue("lobby")
		return
	}
	nc.webrtcStart(false)
}

// platformLobby is the page's winner-stays lobby (webrtc.js ikemenNet.lobby):
// one command in, one line of text out, never blocking. Commands: 'open' (show
// the lobby sidebar to create or join one), 'next' (the next assignment once:
// empty while there is none, 'play:N', 'watch:N', 'gone' when this page is not in
// a lobby), 'text' (the lobby screen's lines), 'result' (arg: the winning side
// of the match just played, 1 or 2, 0 a draw, -1 none), 'leave', 'rounds' (a
// dev switch: rounds to win, or empty). Empty when the page has no lobby.
func platformLobby(cmd, arg string) string {
	bridge := js.Global().Get("ikemenNet")
	if !bridge.Truthy() || bridge.Get("lobby").Type() != js.TypeFunction {
		return ""
	}
	v := bridge.Call("lobby", cmd, arg)
	if v.Type() != js.TypeString {
		return ""
	}
	return v.String()
}

// platformOnlineNames feeds the in-match name plates (Lua binding
// netOnlineNames). Only a queue match carries verified names; the friend
// flows return "" and the plates fall back to their local-name logic.
// platformRankStatus feeds NETWORK > YOUR RANK (Lua binding netRankStatus).
// The page keeps the standing cached and returns it synchronously, so this
// never blocks a menu: an empty string simply means "nothing to show yet".
func platformRankStatus() string {
	bridge := js.Global().Get("ikemenNet")
	if bridge.IsUndefined() {
		return ""
	}
	fn := bridge.Get("rankStatus")
	if fn.Type() != js.TypeFunction {
		return "" // older page script: the menu says so rather than crashing
	}
	return bridge.Call("rankStatus").String()
}

func platformOnlineNames() string {
	bridge := js.Global().Get("ikemenNet")
	if bridge.Truthy() && !bridge.Get("queueNames").IsUndefined() {
		return bridge.Call("queueNames").String()
	}
	return ""
}

// platformReportResult hands a finished matchmade result to the page, which
// signs it with this player's identity and posts it to the leaderboard.
// The two character names are ENGINE SIDES 1 and 2 (identical on both peers);
// the page picks its own out of them by role.
// Fire-and-forget: a failed report must never disturb the game.
func platformReportResult(winSide int, p1Char, p2Char string) {
	bridge := js.Global().Get("ikemenNet")
	if bridge.Truthy() && !bridge.Get("reportResult").IsUndefined() {
		bridge.Call("reportResult", winSide, p1Char, p2Char)
	}
}

// ---------------------------------------------------------------------------
// GGPO rollback transport

// The browser build has no netplay deadlines: upstream's Netplay.SyncTimeout
// and Netplay.LoadingTimeout (4d009150) would cut off a slow page load, so
// the WebRTC path keeps waiting as 2.4 did (Decision 10; netDeadline in
// netplay.go).
const platformNetDeadlines = false

// The engine creates a fresh GGPO session (and transport) per match; the
// previous match's transport must be stopped so its reader releases the
// shared DataChannel queue.
var activeGGPOTransport *transport.WebRTC

func platformGGPOTransport() []transport.Connection {
	if activeGGPOTransport != nil {
		activeGGPOTransport.Close()
	}
	bridge := js.Global().Get("ikemenNet")
	// The port this side expects the REMOTE player's packets to come from:
	// must match the port registered via NewRemotePlayer (host registers
	// the guest as 7550, guest registers the host as 7600).
	remotePort := webrtcHostGGPOPort
	if sys.netConnection != nil && sys.netConnection.host {
		remotePort = webrtcGuestGGPOPort
	}
	activeGGPOTransport = transport.NewWebRTC(bridge, webrtcPeerIP, remotePort)
	return []transport.Connection{activeGGPOTransport}
}

// platformRollbackPeer gives preMatchSetup the rollback peer of a WebRTC
// match: the stream has no IP address (rollback.go would otherwise read a
// *net.TCPAddr from it), and the ports are the fixed pair the transport
// stamps, not the ones the handshake exchanged. remoteIp is webrtcPeerIP, the
// same value the handshake and the queue path put in session.remoteIp/host.
func platformRollbackPeer(host bool) (remoteIp string, localPort, remotePort int, ok bool) {
	if host {
		return webrtcPeerIP, webrtcHostGGPOPort, webrtcGuestGGPOPort, true
	}
	return webrtcPeerIP, webrtcGuestGGPOPort, webrtcHostGGPOPort, true
}

// platformMatchGone asks the page whether it dropped this match's connection
// because the other player is gone: the lobby server says so at once when a
// player leaves or is kicked (webrtc.js lobbyPlayDrop, lobbyGone), long before
// GGPO's DisconnectTimeout or the browser notices a closed tab. 2.4 ended the
// match through an Esc the page pressed; here Esc is synchronized input
// (OnKeyPressed), which a loop stalled on the gone player never confirms, so
// the rollback loop asks every turn instead (hijackRunMatch). The page also
// learns from this call that the engine handles the drop itself, and presses
// no Esc. An older page without the call: false, and GGPO's timeout ends it.
func platformMatchGone() bool {
	// Asked every turn: the bridge is looked up once (each lookup of a JS object
	// is a new reference for the garbage collector to finalize). The page sets
	// ikemenNet once and never replaces it.
	if !matchGoneBridge.Truthy() {
		bridge := js.Global().Get("ikemenNet")
		if !bridge.Truthy() || bridge.Get("matchGone").Type() != js.TypeFunction {
			return false
		}
		matchGoneBridge = bridge
	}
	return matchGoneBridge.Call("matchGone").Truthy()
}

// ikemenNet once platformMatchGone found its matchGone (undefined until then).
var matchGoneBridge js.Value

// The page's Uint8Array constructor, looked up once (each lookup is a new
// reference for the garbage collector to finalize).
var jsUint8ArrayCtor = js.Global().Get("Uint8Array")

// platformNetFail hands a failed connection's message to the page's netplay
// log (shown under the game), besides the engine's session warning.
func platformNetFail(msg string) {
	bridge := js.Global().Get("ikemenNet")
	if bridge.Truthy() && bridge.Get("olog").Type() == js.TypeFunction {
		bridge.Call("olog", "engine: "+msg)
	}
}

// platformNetStats feeds connection quality to the page overlay.
func platformNetStats(pingMs int) {
	bridge := js.Global().Get("ikemenNet")
	if bridge.Truthy() && !bridge.Get("setPing").IsUndefined() {
		bridge.Call("setPing", pingMs)
	}
}
