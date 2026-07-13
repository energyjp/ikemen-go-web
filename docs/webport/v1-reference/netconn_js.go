//go:build js

package main

import (
	"errors"
	"syscall/js"
	"time"
)

// Browser netplay transport: tunnels NetInput's byte stream through a
// WebRTC DataChannel managed by the page (public/ikemen/webrtc.js, exposed
// as globalThis.ikemenNet). The DataChannel is ordered+reliable, so it
// behaves like the TCP stream the engine's lockstep protocol expects.
// Signaling (offer/answer exchange) happens in the page UI; the in-game
// NETWORK menu just waits for the connection like it would for a TCP peer.
//
// Bridge contract (all synchronous JS calls):
//   start(mode)     mode = "host" | "join"; shows the signaling panel
//   connected()     -> bool, true once the DataChannel is open
//   failed()        -> bool, true if signaling/connection failed or closed
//   read(max)       -> Uint8Array (possibly empty) | null when closed
//   send(u8)        -> bool, false when closed
//   close()

type webrtcConn struct {
	bridge js.Value
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
		// Esc aborts a blocking wait. Synchronize() reads on the main
		// goroutine; if the peer's engine never speaks (e.g. the other
		// player backed out of the connecting screen while the WebRTC
		// panel kept negotiating), this read would otherwise block the
		// whole game forever with no way out.
		if sys.escPending {
			return 0, errors.New("webrtc: canceled")
		}
		// No data buffered yet. Sleeping yields to the JS event loop so
		// the DataChannel's onmessage can run.
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

// webrtcCloser tears down the page-side WebRTC session (and its signaling
// panel) when the engine abandons netplay. Stored in ni.ln so NetInput.Close
// reaches it even when ni.conn was never established - without this, a
// player backing out of the connecting screen leaves the panel negotiating,
// and the OTHER side then connects to an engine that will never speak.
type webrtcCloser struct {
	bridge js.Value
}

func (c *webrtcCloser) Close() error {
	c.bridge.Call("close")
	return nil
}

func (ni *NetInput) webrtcStart(host bool) error {
	bridge := js.Global().Get("ikemenNet")
	if bridge.IsUndefined() {
		return Error("webrtc bridge (ikemenNet) missing from page")
	}
	ni.ln = &webrtcCloser{bridge: bridge}
	ni.host = host
	if host {
		ni.locIn, ni.remIn = ni.GetHostGuestRemap()
	} else {
		ni.remIn, ni.locIn = ni.GetHostGuestRemap()
	}
	mode := "join"
	if host {
		mode = "host"
	}
	bridge.Call("start", mode)
	go func() {
		for !bridge.Call("connected").Bool() {
			if bridge.Call("failed").Bool() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		ni.conn = &webrtcConn{bridge: bridge}
	}()
	return nil
}

// The host/join split comes from the in-game NETWORK menu; port and server
// address are meaningless over WebRTC and ignored.

func (ni *NetInput) Accept(port string) error {
	return ni.webrtcStart(true)
}

func (ni *NetInput) Connect(server, port string) {
	ni.webrtcStart(false)
}
