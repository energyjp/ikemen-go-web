//go:build js

package main

import (
	"errors"
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
		for !bridge.Call("connected").Bool() {
			if bridge.Call("failed").Bool() || nc.isClosing() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		conn := &webrtcConn{bridge: bridge}
		// IKEMENGO handshake, byte-compatible with the TCP path.
		if host {
			if sys.cfg.Netplay.RollbackNetcode {
				sys.rollback.session.remoteIp = webrtcPeerIP
			}
			if _, err := conn.Write([]byte("IKEMENGO")); err != nil {
				return
			}
			ack := make([]byte, 8)
			if _, err := ioReadFull(conn, ack); err != nil || string(ack) != "IKEMENGO" {
				return
			}
		} else {
			buf := make([]byte, 8)
			if _, err := ioReadFull(conn, buf); err != nil || string(buf) != "IKEMENGO" {
				return
			}
			if _, err := conn.Write([]byte("IKEMENGO")); err != nil {
				return
			}
		}
		if nc.isClosing() {
			conn.Close()
			return
		}
		nc.conn = conn
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

func platformGGPOTransport() []transport.Connection {
	bridge := js.Global().Get("ikemenNet")
	// The port this side expects the REMOTE player's packets to come from:
	// must match the port registered via NewRemotePlayer (host registers
	// the guest as 7550, guest registers the host as 7600).
	remotePort := 7600
	if sys.netConnection != nil && sys.netConnection.host {
		remotePort = 7550
	}
	return []transport.Connection{transport.NewWebRTC(bridge, webrtcPeerIP, remotePort)}
}
