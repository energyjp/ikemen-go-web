//go:build js

package main

import "syscall/js"

// Platform stubs for the browser build.

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
