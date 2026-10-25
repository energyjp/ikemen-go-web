//go:build js

package main

import (
	"runtime"
	"syscall/js"
)

// Platform stubs for the browser build.

// GC headroom is set by the boot page via the GOGC env, not here. This used to
// call SetGCPercent(500) from init, which runs after the runtime has read GOGC
// and therefore silently overrode it - so the boot page's measured value never
// took effect and every collection ran at 500's headroom instead.
//
// That matters because more headroom is not free: mark cost scales with the
// live heap and steals the single wasm thread, so a bigger multiplier trades
// frequent short pauses for rare long ones. The boot page settled on GOGC=200
// after measuring stalls up to ~700ms at higher values, which is the number
// that should win. ?gogc= overrides it there.

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
//
// THAT COMMENT DESCRIBED AN INTENTION THE CODE DID NOT IMPLEMENT. Open()
// returned an error, and nothing above it treats a failed video layer as a
// skippable layer:
//
//	stage.go:272        bg.video.Open(...) err -> return nil, err
//	bgdef.go:101-103    readBackGround err     -> return nil, err, ABANDONING
//	stage.go:1461-1463  the rest of the BG sections in that group
//
// So one video layer discarded every OTHER layer beside it. For a screenpack
// that is survivable - motif.go:2399-2406 logs it and substitutes an empty
// BGDef, so the screen boots with no background at all - but for a STAGE it is
// fatal: loadStage returns the error to system.go:6398 and the match never
// loads. ICA_GO declares seven `type = video` sections.
//
// Returning nil makes the failure mean what the comment always said: this one
// layer has no frames. The sprite and anim layers beside it survive, and a
// video stage loads and plays with a hole where the video would be. That is a
// strictly better outcome than the group vanishing or the match refusing to
// start, and it costs nothing - Tick() is already a no-op and the draw path
// already treats a nil texture as "no frame yet" (stage.go:677-679).
//
// The message is still logged, so this is not silent.
type bgVideo struct {
	texture Texture
	muted   bool // written by System.tickSound for every stage video; nothing here reads it
}

func (bgv *bgVideo) Open(filename string, volume int, sm BgVideoScaleMode, sf BgVideoScaleFilter, loop bool) error {
	LogMessage("Video backgrounds are not supported in the browser build; "+
		"this layer will be empty: %v", filename)
	return nil
}

func (bgv *bgVideo) Tick() error { return nil }

func (bgv *bgVideo) SetPlaying(on bool) {}

func (bgv *bgVideo) SetVisible(on bool) {}

func (bgv *bgVideo) Reset() {}

func (bgv *bgVideo) Close() {}

func (bgv *bgVideo) MixerCleared() {}

// No audio chain on the browser build; tickSound calls this after setting muted.
func (bgv *bgVideo) updateAudioVolume() {}

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
