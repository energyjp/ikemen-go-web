//go:build js

package main

import (
	"io"
	"math"
	"sync"

	"github.com/ebitengine/oto/v3"
	"github.com/gopxl/beep/v2"
)

// Browser audio sink: beep.Mixer pulled by an oto/v3 player (WebAudio).
//
// beep's own speaker package is NOT used: it converts its buffer size
// through a time.Duration round-trip that lands on 1023 samples at
// 44100Hz, and the browser's createScriptProcessor only accepts
// power-of-two buffer sizes - a guaranteed init crash on wasm. Driving
// oto directly with BufferSize 0 uses oto's browser-safe default.

type BeepSpeaker struct {
	mu     sync.Mutex
	mixer  *beep.Mixer
	sr     beep.SampleRate
	ctx    *oto.Context
	player *oto.Player
}

// beepReader adapts the mixer to oto's io.Reader (float32 LE stereo).
type beepReader struct {
	s   *BeepSpeaker
	buf [][2]float64
}

func (r *beepReader) Read(p []byte) (int, error) {
	frames := len(p) / 8 // 2 channels x 4 bytes
	if frames == 0 {
		return 0, nil
	}
	if len(r.buf) < frames {
		r.buf = make([][2]float64, frames)
	}
	buf := r.buf[:frames]
	r.s.mu.Lock()
	r.s.mixer.Stream(buf)
	r.s.mu.Unlock()
	for i, smp := range buf {
		writeF32LE(p[i*8:], float32(smp[0]))
		writeF32LE(p[i*8+4:], float32(smp[1]))
	}
	return frames * 8, nil
}

func writeF32LE(b []byte, f float32) {
	bits := math.Float32bits(f)
	b[0] = byte(bits)
	b[1] = byte(bits >> 8)
	b[2] = byte(bits >> 16)
	b[3] = byte(bits >> 24)
}

func (s *BeepSpeaker) Init(sampleRate beep.SampleRate, bufferSize int) error {
	s.sr = sampleRate
	s.mixer = &beep.Mixer{}
	ctx, _, err := oto.NewContext(&oto.NewContextOptions{
		SampleRate:   int(sampleRate),
		ChannelCount: 2,
		Format:       oto.FormatFloat32LE,
		// BufferSize 0 = oto's platform default; the browser driver picks
		// a valid power-of-two ScriptProcessor size.
		BufferSize: 0,
	})
	if err != nil {
		return err
	}
	// Note: the ready channel is intentionally NOT awaited - browsers gate
	// audio behind a user gesture, and blocking here would hang boot.
	s.ctx = ctx
	var reader io.Reader = &beepReader{s: s}
	s.player = ctx.NewPlayer(reader)
	// oto's default player buffer is very deep (it read-ahead-buffers on
	// the order of a second, which players hear as sound lagging the
	// action). ~100ms keeps hits and sounds in sync while leaving enough
	// slack for browser scheduling jitter.
	s.player.SetBufferSize(int(sampleRate) / 10 * 8) // 100ms of stereo f32
	s.player.Play()
	return nil
}

func (s *BeepSpeaker) Play(st beep.Streamer) {
	s.mu.Lock()
	s.mixer.Add(st)
	s.mu.Unlock()
}

func (s *BeepSpeaker) Lock()   { s.mu.Lock() }
func (s *BeepSpeaker) Unlock() { s.mu.Unlock() }

func (s *BeepSpeaker) Close() {
	if s.player != nil {
		s.player.Close()
	}
}

// FillAudio is the SDL pull-model hook; oto's player drives itself.
func (s *BeepSpeaker) FillAudio() {}

func newSpeaker() AudioSink {
	return &BeepSpeaker{}
}

// xmpDecode stub: .xm module music needs libxmp (cgo); unavailable in the
// browser build.
func xmpDecode(f io.ReadSeekCloser) (beep.StreamSeekCloser, beep.Format, error) {
	return nil, beep.Format{}, Error("xm module music is not supported in the browser build")
}
