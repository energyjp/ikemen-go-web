//go:build js

package main

import (
	"errors"
	"fmt"
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

func (c *webrtcConn) Read(p []byte) (int, error) {
	for {
		r := c.bridge.Call("read", len(p))
		if r.IsNull() {
			return 0, errors.New("webrtc: connection closed")
		}
		if n := r.Get("length").Int(); n > 0 {
			js.CopyBytesToGo(p[:n], r)
			return n, nil
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
	buf := js.Global().Get("Uint8Array").New(len(p))
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
	if host {
		nc.locIn, nc.remIn = nc.GetHostGuestRemap()
	} else {
		nc.remIn, nc.locIn = nc.GetHostGuestRemap()
	}
	mode := "join"
	if host {
		mode = "host"
	}
	bridge.Call("start", mode)
	SafeGo(func() {
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
		nc.conn = conn
		hlog("conn established; entering synchronize")
	})
	return nil
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

func (nc *NetConnection) Accept(port string) error {
	return nc.webrtcStart(true)
}

func (nc *NetConnection) Connect(server, port string) {
	nc.webrtcStart(false)
}

// ---------------------------------------------------------------------------
// GGPO rollback transport

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
	remotePort := 7600
	if sys.netConnection != nil && sys.netConnection.host {
		remotePort = 7550
	}
	activeGGPOTransport = transport.NewWebRTC(bridge, webrtcPeerIP, remotePort)
	return []transport.Connection{activeGGPOTransport}
}

// platformNetStats feeds connection quality to the page overlay.
func platformNetStats(pingMs int) {
	bridge := js.Global().Get("ikemenNet")
	if bridge.Truthy() && !bridge.Get("setPing").IsUndefined() {
		bridge.Call("setPing", pingMs)
	}
}
