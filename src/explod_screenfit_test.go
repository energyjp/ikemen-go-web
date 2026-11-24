package main

import "testing"

// Screen-space explods of a pre-1.1 MUGEN character in a fight wider than 4:3
// keep the vertical share of the screen they had on the 4:3 screen those
// engines always gave them (a CvS groove meter at y = 230 of 240).

func TestScreenYFit43(t *testing.T) {
	for _, tc := range []struct {
		w, h, want float32
	}{
		{320, 240, 1},         // 4:3 fight: unchanged
		{426.6667, 240, 0.75}, // 16:9 fight
		{426, 240, 240 / (426 * 0.75)},
		{384, 240, 240 / (384 * 0.75)}, // 16:10
		{320, 256, 1},                  // taller than 4:3: unchanged
		{320, 0, 1},                    // no game size yet
	} {
		got := screenYFit43(tc.w, tc.h)
		if d := got - tc.want; d > 1e-4 || d < -1e-4 {
			t.Errorf("screenYFit43(%v, %v) = %v, want %v", tc.w, tc.h, got, tc.want)
		}
	}
}

func TestExplodScreenYFitByVersion(t *testing.T) {
	oldW, oldH := sys.gameWidth, sys.gameHeight
	oldChars, oldCgi := sys.chars[0], sys.cgi[0]
	defer func() {
		sys.gameWidth, sys.gameHeight = oldW, oldH
		sys.chars[0], sys.cgi[0] = oldChars, oldCgi
	}()

	c := &Char{}
	c.playerNo = 0
	sys.chars[0] = []*Char{c}
	e := &Explod{playerno: 0}

	for _, tc := range []struct {
		name     string
		mugen    [2]uint16
		ikemen   [3]uint16
		w        float32
		wantFit  float32
		wantNote string
	}{
		{"WinMUGEN char, 16:9", [2]uint16{0, 0}, [3]uint16{0, 0, 0}, 426.6667, 0.75, "no mugenversion"},
		{"MUGEN 1.0 char, 16:9", [2]uint16{1, 0}, [3]uint16{0, 0, 0}, 426.6667, 0.75, "Cvs_Mai's case"},
		{"MUGEN 1.0 char, 4:3", [2]uint16{1, 0}, [3]uint16{0, 0, 0}, 320, 1, "4:3 stage untouched"},
		{"MUGEN 1.1 char, 16:9", [2]uint16{1, 1}, [3]uint16{0, 0, 0}, 426.6667, 1, "1.1 keeps the engine's screen"},
		{"Ikemen char, 16:9", [2]uint16{1, 0}, [3]uint16{1, 0, 0}, 426.6667, 1, "Ikemen chars use ScreenHeight"},
	} {
		sys.gameWidth, sys.gameHeight = tc.w, 240
		sys.cgi[0] = CharGlobalInfo{mugenver: tc.mugen, ikemenver: tc.ikemen}
		got := e.screenYFit()
		if d := got - tc.wantFit; d > 1e-4 || d < -1e-4 {
			t.Errorf("%s (%s): screenYFit = %v, want %v", tc.name, tc.wantNote, got, tc.wantFit)
		}
	}

	// The CvS meter: y = 230 at the 16:9 character scale lands inside the screen.
	sys.gameWidth, sys.gameHeight = 426, 240
	sys.cgi[0] = CharGlobalInfo{mugenver: [2]uint16{1, 0}}
	localscl := sys.gameWidth / 320
	if y := 230 * localscl * e.screenYFit(); y >= sys.gameHeight || y < 220 {
		t.Errorf("meter at y=230 draws at %v of %v", y, sys.gameHeight)
	}
}
