//go:build js

package main

import (
	"io"

	"github.com/gopxl/beep/v2"
	bspeaker "github.com/gopxl/beep/v2/speaker"
)

// Browser audio sink: replaces audio_sdl.go for wasm builds. Delegates to
// beep's own speaker package, whose oto/v3 backend targets WebAudio on
// wasm - the same pipeline the engine used before it switched the final
// mix to SDL, so behavior matches the desktop build closely.

type BeepSpeaker struct{}

func (s *BeepSpeaker) Init(sampleRate beep.SampleRate, bufferSize int) error {
	return bspeaker.Init(sampleRate, bufferSize)
}

func (s *BeepSpeaker) Play(st beep.Streamer) {
	bspeaker.Play(st)
}

func (s *BeepSpeaker) Lock()   { bspeaker.Lock() }
func (s *BeepSpeaker) Unlock() { bspeaker.Unlock() }

func (s *BeepSpeaker) Close() {
	bspeaker.Close()
}

// FillAudio is the SDL pull-model hook; beep's speaker drives itself.
func (s *BeepSpeaker) FillAudio() {}

func newSpeaker() AudioSink {
	return &BeepSpeaker{}
}

// xmpDecode stub: .xm module music needs libxmp (cgo); unavailable in the
// browser build.
func xmpDecode(f io.ReadSeekCloser) (beep.StreamSeekCloser, beep.Format, error) {
	return nil, beep.Format{}, Error("xm module music is not supported in the browser build")
}
