package main

import "github.com/gopxl/beep/v2"

// AudioSink is the platform audio output: SDL on desktop (audio_sdl.go),
// beep's oto/WebAudio speaker in the browser (audio_js.go).
type AudioSink interface {
	Init(sr beep.SampleRate, bufferSize int) error
	Play(s beep.Streamer)
	Lock()
	Unlock()
	Close()
	FillAudio()
}

var speaker AudioSink
