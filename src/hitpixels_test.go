package main

import "testing"

// Mouse hits test menu art only, so a character's or stage's full SFF keeps
// no pixels for them (sffKeepsHitPixels); other SFFs keep them.
func TestSffHitPixelsByType(t *testing.T) {
	const file = "../font/default-3x5.sff"
	for _, c := range []struct {
		name string
		typ  SffType
		keep bool
	}{
		{"char", SffTypeChar, false},
		{"stage", SffTypeStage, false},
		{"other (motif, storyboard, Lua)", SffTypeOther, true},
		{"font", SffTypeFont, true},
		{"lifebar", SffTypeLifeBar, true},
	} {
		sff, err := loadSff(file, c.typ, false, false)
		if err != nil {
			t.Fatalf("%s: loadSff: %v", c.name, err)
		}
		kept, drawn := 0, 0
		for _, spr := range sff.sprites {
			if spr == nil || spr.Size[0] == 0 || spr.Size[1] == 0 {
				continue
			}
			drawn++
			if len(spr.hitPixels) > 0 {
				kept++
			}
		}
		if drawn == 0 {
			t.Fatalf("%s: no sprites with pixels in %s", c.name, file)
		}
		if c.keep && kept != drawn {
			t.Errorf("%s: %d of %d sprites keep hit pixels, want all", c.name, kept, drawn)
		}
		if !c.keep && kept != 0 {
			t.Errorf("%s: %d of %d sprites keep hit pixels, want none", c.name, kept, drawn)
		}
	}
	// The SetPxl texture uploads queued by these loads are never run here.
	for len(sys.mainThreadTask) > 0 {
		<-sys.mainThreadTask
	}
}
