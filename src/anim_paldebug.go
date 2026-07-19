package main

// Palette-resolution debug tap, enabled with IKEMEN_PALDEBUG=1 (browser:
// ?paldebug=1). For sprites in the scene/overlay bands it logs, once per
// (group,image,remap-target), exactly how the palette resolves: the sprite's
// palidx, what the active PalFX remap maps it to, and whether a palettedata
// override is in play. This exists because three consecutive palette-bug
// models (act stomp, forceRemapPal breadth, giver-air group refs) each looked
// airtight and each failed live testing - the draw path itself is the only
// witness worth trusting.

import (
	"fmt"
	"os"
)

var palDebugEnabled = os.Getenv("IKEMEN_PALDEBUG") != ""

var palDebugSeen = map[[3]int]bool{}

func palDebugGroupWanted(g uint16) bool {
	// Sakura scene/overlay bands + protected registry bands under study
	return (g >= 7000 && g <= 7999) ||
		(g >= 19230 && g <= 19239) || (g >= 12249 && g <= 12259) ||
		(g >= 17230 && g <= 17239) || (g >= 18300 && g <= 18309)
}

func logPalDebug(a *Animation, pfx *PalFX) {
	spr := a.spr
	mapped := spr.palidx
	nRemap := 0
	if pfx != nil {
		nRemap = len(pfx.remap)
		if spr.palidx >= 0 && spr.palidx < len(pfx.remap) {
			mapped = pfx.remap[spr.palidx]
		}
	}
	// Log scene-band sprites, PLUS any sprite anywhere whose palette gets
	// remapped away from identity - that set contains whichever sprite is
	// drawing with the player's selected act during the scene.
	if !palDebugGroupWanted(spr.Group) && mapped == spr.palidx {
		return
	}
	key := [3]int{int(spr.Group)<<16 | int(spr.Number), spr.palidx, mapped}
	if palDebugSeen[key] {
		return
	}
	palDebugSeen[key] = true
	src := "sff.palList"
	if a.palettedata != nil {
		src = "palettedata(override)"
	}
	fmt.Printf("[paldebug] spr %d,%d palidx=%d remap->%d (remapLen=%d, src=%s)\n",
		spr.Group, spr.Number, spr.palidx, mapped, nRemap, src)
}
