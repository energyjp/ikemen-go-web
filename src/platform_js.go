//go:build js

package main

import (
	"runtime"
	"runtime/debug"
	"strings"
	"syscall/js"
)

// Platform stubs for the browser build.

// GC headroom: field-tuned on the V2 build. Mark cost scales with live
// heap and steals the single wasm thread (~35-40ms), so collections are
// forced at gameplay blind spots (platformIdleGC) and the automatic GC
// gets enough headroom (~a full round of garbage) to rarely fire
// mid-action. A ?gogc= URL parameter still overrides via the boot page.
func init() {
	search := js.Global().Get("location").Get("search").String()
	if !strings.Contains(search, "gogc=") {
		debug.SetGCPercent(500)
	}
}

// Android-only init paths (never taken on js; needed for compilation).
func platformAndroidGLInit() {}
func platformCoreInit() error {
	return nil
}
func platformInitJoysticks() {}

// osPreferredLanguage: the browser knows this natively.
func osPreferredLanguage() string {
	lang := js.Global().Get("navigator").Get("language")
	if lang.Truthy() {
		return lang.String()
	}
	return "en-US"
}

// bgVideo: stage background videos are ffmpeg-decoded on desktop; the
// browser build ships without video support. Stages that reference a
// video simply show nothing in that layer (texture stays nil, which
// stage.go treats as "no frame yet").
type bgVideo struct {
	texture Texture
}

func (bgv *bgVideo) Open(filename string, volume int, sm BgVideoScaleMode, sf BgVideoScaleFilter, loop bool) error {
	return Error("stage videos are not supported in the browser build")
}

func (bgv *bgVideo) Tick() error { return nil }

func (bgv *bgVideo) SetPlaying(on bool) {}

func (bgv *bgVideo) SetVisible(on bool) {}

func (bgv *bgVideo) Reset() {}

func (bgv *bgVideo) Close() {}

func (bgv *bgVideo) MixerCleared() bool { return true }

// platformIdleGC forces a garbage collection at moments where a pause is
// invisible (pauses, between rounds). On single-threaded wasm the GC mark
// phase freezes the game thread for ~35-40ms (a visible 2-3 frame hitch);
// collecting at blind spots keeps the automatic mid-round GC from firing.
// Desktop builds don't need this (no-op there).
func platformIdleGC() {
	// Not while a session is live. Rollback re-simulates frames (including
	// round transitions) and a forced GC inside that loop stalls long enough
	// to trip the disconnect timeout. Delay netcode doesn't re-simulate, but
	// these blind spots are only a few frames wide and both peers are
	// lockstepped on them, so a collection here is a stall the other side
	// waits out. Match loads are wide enough to hide one: platformLoadGC.
	if sys.netConnection != nil || sys.rollback.session != nil {
		return
	}
	runtime.GC()
}

// platformLoadGC forces a garbage collection at a match load, the widest blind
// spot there is: the loader already runs for seconds and a peer tolerates
// minutes of it, so a ~40ms mark is invisible even online. This is the one
// place a session can afford to collect, and it means a delay netcode match
// starts on a fresh heap instead of carrying the previous match's garbage into
// the fight, where the automatic GC would eventually fire mid-round.
//
// Rollback still opts out: it re-simulates frames, and stalling that loop is
// what trips the disconnect timeout.
func platformLoadGC() {
	if sys.rollback.session != nil {
		return
	}
	runtime.GC()
}
