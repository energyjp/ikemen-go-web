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

// The screen a pre-1.1 character was made for is the shape of its own localcoord, between 2:1
// and 4:3; anything else is 4:3.
func TestScreenYFitShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		lc   [2]int32
		want float32
	}{
		{"no localcoord (default 320x240)", [2]int32{320, 240}, 0.75},
		{"Cvs_Mai: localcoord = 320 (read as 320x240)", [2]int32{320, 240}, 0.75},
		{"640x480", [2]int32{640, 480}, 0.75},
		{"REV2 character 427x240", [2]int32{427, 240}, 240.0 / 427},
		{"Dhalsim 428x240", [2]int32{428, 240}, 240.0 / 428},
		{"1280x720", [2]int32{1280, 720}, 0.5625},
		{"taller than 4:3", [2]int32{320, 256}, 0.75},
		{"width only: localcoord = 640 (read as 640x240)", [2]int32{640, 240}, 0.75},
		{"zero", [2]int32{0, 0}, 0.75},
	} {
		if got := screenYFitShape(tc.lc); got-tc.want > 1e-6 || tc.want-got > 1e-6 {
			t.Errorf("%s: screenYFitShape = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The reference shape is the widest of 4:3, the character's localcoord and a MUGEN 1.0
// screenpack's localcoord. A 1.1 or Ikemen screenpack never widens it.
func TestScreenYFitAspect(t *testing.T) {
	rev2 := InfoProperties{MugenVersion: "1.0", Localcoord: [2]int32{1280, 720}}
	ilx := InfoProperties{MugenVersion: "1.1", Localcoord: [2]int32{1280, 720}}
	ikemen1 := InfoProperties{IkemenVersion: "1.0", Localcoord: [2]int32{1280, 720}}
	cvs2 := InfoProperties{MugenVersion: "1.0", Localcoord: [2]int32{640, 480}}
	mk2 := InfoProperties{MugenVersion: "1.0", Localcoord: [2]int32{452, 254}}
	c320, c427 := [2]int32{320, 240}, [2]int32{427, 240}
	for _, tc := range []struct {
		name string
		lc   [2]int32
		info InfoProperties
		want float32
	}{
		{"320x240 under REV2 (MUGEN 1.0, 1280x720)", c320, rev2, 0.5625},
		{"427x240 under REV2", c427, rev2, 240.0 / 427},
		{"320x240 under Infinite Legacy X (mugenversion 1.1, 1280x720)", c320, ilx, 0.75},
		{"427x240 under Infinite Legacy X", c427, ilx, 240.0 / 427},
		{"320x240 under ikemen1", c320, ikemen1, 0.75},
		{"427x240 under ikemen1", c427, ikemen1, 240.0 / 427},
		{"320x240 under CvS2 (MUGEN 1.0, 640x480)", c320, cvs2, 0.75},
		{"320x240 under Mortal Kombat II Remix (MUGEN 1.0, 452x254)", c320, mk2, 254.0 / 452},
		{"320x240, MUGEN 1.0 pack with ikemenversion", c320, InfoProperties{MugenVersion: "1.0", IkemenVersion: "0.99", Localcoord: [2]int32{1280, 720}}, 0.75},
		{"320x240, no screenpack read", c320, InfoProperties{}, 0.75},
	} {
		if got := screenYFitAspect(tc.lc, tc.info); got-tc.want > 1e-6 || tc.want-got > 1e-6 {
			t.Errorf("%s: screenYFitAspect = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Capcom VS SNK EVOLUTION REV 2 (427x240 MUGEN 1.0 characters, a 1280x720 MUGEN 1.0 screenpack):
// add004 places its power bars at the bottom of the 16:9 screen; no fit may lift them, for its own
// 427-wide characters or for a 320x240 one added to it. Cvs_Mai (320x240 MUGEN 1.0) keeps the 4:3
// fit under ikemen1 and under Infinite Legacy X (mugenversion 1.1, no ikemenversion, 1280x720).
func TestExplodScreenYFitByLocalcoordAndScreenpack(t *testing.T) {
	oldW, oldH := sys.gameWidth, sys.gameHeight
	oldChars, oldCgi := sys.chars[0], sys.cgi[0]
	oldInfo := sys.motif.Info
	defer func() {
		sys.gameWidth, sys.gameHeight = oldW, oldH
		sys.chars[0], sys.cgi[0] = oldChars, oldCgi
		sys.motif.Info = oldInfo
	}()

	c := &Char{}
	c.playerNo = 0
	sys.chars[0] = []*Char{c}
	e := &Explod{playerno: 0}
	for _, tc := range []struct {
		name    string
		info    InfoProperties
		lc      [2]int32
		w       float32
		wantFit float32
	}{
		{"REV2, 427x240, 16:9", InfoProperties{MugenVersion: "1.0", Localcoord: [2]int32{1280, 720}}, [2]int32{427, 240}, 426.6667, 1},
		{"REV2, 320x240 added, 16:9", InfoProperties{MugenVersion: "1.0", Localcoord: [2]int32{1280, 720}}, [2]int32{320, 240}, 426.6667, 1},
		{"ikemen1, Cvs_Mai, 16:9", InfoProperties{IkemenVersion: "1.0", Localcoord: [2]int32{1280, 720}}, [2]int32{320, 240}, 426.6667, 0.75},
		{"ikemen1, 427x240, 16:9", InfoProperties{IkemenVersion: "1.0", Localcoord: [2]int32{1280, 720}}, [2]int32{427, 240}, 426.6667, 1},
		{"Infinite Legacy X, Cvs_Mai, 16:9", InfoProperties{MugenVersion: "1.1", Localcoord: [2]int32{1280, 720}}, [2]int32{320, 240}, 426.6667, 0.75},
		{"Infinite Legacy X, 427x240, 16:9", InfoProperties{MugenVersion: "1.1", Localcoord: [2]int32{1280, 720}}, [2]int32{427, 240}, 426.6667, 1},
		{"CvS2, Cvs_Mai, 16:9", InfoProperties{MugenVersion: "1.0", Localcoord: [2]int32{640, 480}}, [2]int32{320, 240}, 426.6667, 0.75},
		{"REV2, 320x240, 4:3", InfoProperties{MugenVersion: "1.0", Localcoord: [2]int32{1280, 720}}, [2]int32{320, 240}, 320, 1},
		{"ikemen1, Cvs_Mai, 4:3", InfoProperties{IkemenVersion: "1.0", Localcoord: [2]int32{1280, 720}}, [2]int32{320, 240}, 320, 1},
		// A fight wider than 16:9 (21:9): a 427x240 character's 240 maps onto the real height.
		{"ikemen1, 427x240, 21:9", InfoProperties{IkemenVersion: "1.0", Localcoord: [2]int32{1280, 720}}, [2]int32{427, 240}, 560, 240 / (560 * 240.0 / 427)},
	} {
		sys.motif.Info = tc.info
		sys.gameWidth, sys.gameHeight = tc.w, 240
		sys.cgi[0] = CharGlobalInfo{mugenver: [2]uint16{1, 0}, localcoord: tc.lc}
		got := e.screenYFit()
		if d := got - tc.wantFit; d > 1e-4 || d < -1e-4 {
			t.Errorf("%s: screenYFit = %v, want %v", tc.name, got, tc.wantFit)
		}
	}

	// Cvs_Mai's meter at y = 230 stays on screen under Infinite Legacy X in a 16:9 fight.
	sys.motif.Info = InfoProperties{MugenVersion: "1.1", Localcoord: [2]int32{1280, 720}}
	sys.gameWidth, sys.gameHeight = 426.6667, 240
	sys.cgi[0] = CharGlobalInfo{mugenver: [2]uint16{1, 0}, localcoord: [2]int32{320, 240}}
	if y := 230 * (sys.gameWidth / 320) * e.screenYFit(); y >= sys.gameHeight || y < 220 {
		t.Errorf("Infinite Legacy X: Cvs_Mai's meter at y=230 draws at %v of %v", y, sys.gameHeight)
	}
}
