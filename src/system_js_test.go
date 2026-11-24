//go:build js

package main

import (
	"math"
	"testing"
)

// The requestAnimationFrame release interval follows Video.Framerate, with 60
// when the setting is not positive.
func TestFrameIntervalMs(t *testing.T) {
	for _, c := range []struct {
		fr   int
		want float64
	}{{60, 1000.0 / 60}, {120, 1000.0 / 120}, {30, 1000.0 / 30}, {0, 1000.0 / 60}, {-5, 1000.0 / 60}} {
		if got := frameIntervalMs(c.fr); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("frameIntervalMs(%d) = %v, want %v", c.fr, got, c.want)
		}
	}
	w := &Window{}
	w.setFrameRate(60)
	w.setFrameRate(144)
	if w.frameRate != 144 || math.Abs(w.frameIntMs-1000.0/144) > 1e-9 {
		t.Errorf("after a live change: frameRate %d, interval %v", w.frameRate, w.frameIntMs)
	}
}
