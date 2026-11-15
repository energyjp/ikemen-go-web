//go:build js

package main

import (
	"syscall/js"

	"github.com/ikemen-engine/ggpo/transport"
)

// The page side of watching: globalThis.ikemenWatch (public/ikemen/webrtc.js).
// It exists only on a host page that allows watchers and on a watcher's page,
// so on every other page each function here returns at the first line, and a
// players' match makes no call into the page that it did not make before.
//
// Host (role "host"):
//	offer(match, json)    the select (or nowatch) for the next match; the page
//	                      sends it to every verified watcher, now and later
//	start(match) -> [slot] the watchers to register, closing the door on this
//	                      match (a watcher verified after it waits for the next)
//	ctl(slot, json)       one message to one watcher
//	sendTo / readAny      the watchers' GGPO datagrams (transport.WebRTCHub)
// Watcher (role "watcher"):
//	next() -> json|""|null the host's next message; null once disconnected
//	sendGGPO / readGGPO   the GGPO datagrams to and from the host
//	trail(frame, crc)     test hook: present only when the page asks for it
// Both: log(text) onto the netplay overlay.

func watchBridge(role string) js.Value {
	b := js.Global().Get("ikemenWatch")
	if !b.Truthy() || b.Get("role").String() != role {
		return js.Undefined()
	}
	return b
}

func platformWatchHosting() bool {
	return watchBridge("host").Truthy()
}

func platformWatchOffer(match int, raw string) {
	if b := watchBridge("host"); b.Truthy() {
		b.Call("offer", match, raw)
	}
}

func platformWatchStart(match int) []int {
	b := watchBridge("host")
	if !b.Truthy() {
		return nil
	}
	v := b.Call("start", match)
	if !v.Truthy() {
		return nil
	}
	var slots []int
	for i := 0; i < v.Length() && i < transport.MaxWatcherSlots; i++ {
		if s := v.Index(i).Int(); s >= 0 && s < transport.MaxWatcherSlots {
			slots = append(slots, s)
		}
	}
	return slots
}

func platformWatchSend(slot int, raw string) {
	if b := watchBridge("host"); b.Truthy() {
		b.Call("ctl", slot, raw)
	}
}

// The host's hub for the current match, so the next match can stop its reader
// (the players' transport is stopped by platformGGPOTransport as before).
var activeWatchHub *transport.WebRTCHub

func platformWatchHub(conns []transport.Connection) []transport.Connection {
	if activeWatchHub != nil {
		activeWatchHub.Close()
		activeWatchHub = nil
	}
	b := watchBridge("host")
	if !b.Truthy() || len(conns) != 1 {
		return conns
	}
	player, ok := conns[0].(*transport.WebRTC)
	if !ok {
		return conns
	}
	activeWatchHub = transport.NewWebRTCHub(player, b)
	return []transport.Connection{activeWatchHub}
}

// platformWatchHubClose stops the watched match's hub, so its reader of the
// page ends with the match (the next match builds its own, or none).
func platformWatchHubClose() {
	if activeWatchHub != nil {
		activeWatchHub.Close()
		activeWatchHub = nil
	}
}

// platformWatchAvailable: this engine can watch (the page has the watch flow).
func platformWatchAvailable() bool {
	return js.Global().Get("ikemenNet").Truthy() && js.Global().Get("ikemenNet").Get("watchSupported").Truthy()
}

// platformWatchBegin opens the page's "Watch a match" panel and installs the
// watcher bridge; false when the page cannot.
func platformWatchBegin() bool {
	n := js.Global().Get("ikemenNet")
	if !n.Truthy() || n.Get("start").Type() != js.TypeFunction || !platformWatchAvailable() {
		return false
	}
	n.Call("start", "watch")
	return watchBridge("watcher").Truthy()
}

// platformWatchLeave closes the watcher's connection (Esc on the waiting
// screen); the page says so on its panel.
func platformWatchLeave() {
	if b := watchBridge("watcher"); b.Truthy() && b.Get("leave").Type() == js.TypeFunction {
		b.Call("leave")
	}
}

func platformWatchSlotGone(slot int) bool {
	return activeWatchHub != nil && activeWatchHub.Gone(slot)
}

func platformWatchLog(msg string) {
	b := js.Global().Get("ikemenWatch")
	if b.Truthy() && b.Get("log").Type() == js.TypeFunction {
		b.Call("log", msg)
	}
}

// ---- watcher

func platformWatchNext() (string, bool) {
	b := watchBridge("watcher")
	if !b.Truthy() {
		return "", false
	}
	v := b.Call("next")
	if v.IsNull() {
		return "", false
	}
	if v.Type() != js.TypeString {
		return "", true
	}
	return v.String(), true
}

var activeWatchTransport *transport.WebRTC

func platformWatchTransport() []transport.Connection {
	if activeWatchTransport != nil {
		activeWatchTransport.Close()
	}
	activeWatchTransport = transport.NewWebRTC(watchBridge("watcher"), webrtcPeerIPForWatch, watchHostPort)
	return []transport.Connection{activeWatchTransport}
}

func platformWatchTrail(frame int, crc uint32) {
	b := watchBridge("watcher")
	if b.Truthy() && b.Get("trail").Type() == js.TypeFunction {
		b.Call("trail", frame, crc)
	}
}
