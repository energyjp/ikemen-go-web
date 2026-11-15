//go:build js

package main

import (
	"sync/atomic"
	"syscall/js"
	"testing"
	"time"

	"github.com/ikemen-engine/ggpo/transport"
)

// The host's watcher hub reads the page (ikemenWatch.readAny) on its own loop
// for as long as it is open. A watched match's hub must stop at the end of
// that match even when the next match has no watcher: before, it was closed
// only when the NEXT watched match built a new one, so a session that went
// on without watchers kept a dead hub polling the page every millisecond.
func TestWatchHubStopsWhenTheMatchEnds(t *testing.T) {
	var reads atomic.Int64
	empty := js.FuncOf(func(js.Value, []js.Value) any { return js.Global().Get("Uint8Array").New(0) })
	readAny := js.FuncOf(func(js.Value, []js.Value) any {
		reads.Add(1)
		return js.Global().Get("Uint8Array").New(0)
	})
	// Never released: the readers this test leaves behind on a failure keep
	// calling them until the test binary exits.
	watch := js.Global().Get("Object").New()
	watch.Set("role", "host")
	watch.Set("readAny", readAny)
	net := js.Global().Get("Object").New()
	net.Set("readGGPO", empty)
	js.Global().Set("ikemenWatch", watch)
	defer js.Global().Delete("ikemenWatch")

	player := transport.NewWebRTC(net, webrtcPeerIPForWatch, 7550)
	conns := platformWatchHub([]transport.Connection{player})
	if activeWatchHub == nil || len(conns) != 1 {
		t.Fatal("no hub was built for a watched match")
	}
	go conns[0].Read(make(chan transport.MessageChannelItem, 8))
	time.Sleep(60 * time.Millisecond)
	if reads.Load() == 0 {
		t.Fatal("the hub never read the page")
	}

	// The match ends (postMatchSetup); the next match has no watcher.
	var rs RollbackSession
	rs.watchEnd()
	time.Sleep(30 * time.Millisecond)
	after := reads.Load()
	time.Sleep(100 * time.Millisecond)
	if n := reads.Load(); n != after {
		t.Fatalf("the hub kept polling the page after its match ended (%d more reads in 100 ms)", n-after)
	}
	if activeWatchHub != nil {
		t.Fatal("the ended match's hub is still the active one")
	}

	// A watched match followed by one with no watcher to register: the
	// transport for the new match never hands out (or keeps) the old hub.
	conns = platformWatchHub([]transport.Connection{transport.NewWebRTC(net, webrtcPeerIPForWatch, 7550)})
	go conns[0].Read(make(chan transport.MessageChannelItem, 8))
	time.Sleep(30 * time.Millisecond)
	var next RollbackSession
	plain := []transport.Connection{transport.NewWebRTC(net, webrtcPeerIPForWatch, 7550)}
	js.Global().Delete("ikemenWatch") // this page no longer hosts watchers
	got := next.watchTransport(plain)
	if len(got) != 1 || got[0] != plain[0] {
		t.Fatal("a match with no watcher did not get the players' transport unchanged")
	}
	time.Sleep(30 * time.Millisecond)
	after = reads.Load()
	time.Sleep(100 * time.Millisecond)
	if n := reads.Load(); n != after {
		t.Fatalf("the previous match's hub kept polling into a match with no watcher (%d more reads)", n-after)
	}
}
