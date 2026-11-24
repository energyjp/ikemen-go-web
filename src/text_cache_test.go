package main

import (
	"math"
	"testing"
)

// The text path's caches (font.go cachedRuns and textCache, glyphgen.go,
// luatextdraw.go): what they key on, and that a cache hit allocates nothing.

func TestRunePrefix(t *testing.T) {
	for _, c := range []struct {
		s    string
		n    int
		want string
		cnt  int
	}{
		{"abc", math.MaxInt, "abc", 3}, {"abc", 2, "ab", 2}, {"abc", 0, "", 0},
		{"aÜb", 2, "aÜ", 2}, {"aÜb", 3, "aÜb", 3}, {"日本語", 1, "日", 1}, {"", 5, "", 0},
	} {
		got, cnt := runePrefix(c.s, c.n)
		if got != c.want || cnt != c.cnt {
			t.Errorf("runePrefix(%q, %d) = %q, %d; want %q, %d", c.s, c.n, got, cnt, c.want, c.cnt)
		}
	}
}

// The processed-text cache is keyed on ts.literal too: literal text (typed
// by a player) keeps a written \n as two characters.
func TestProcessedTextKeysOnLiteral(t *testing.T) {
	ts := &TextSprite{text: `one\ntwo`}
	ts.ensureProcessed()
	if len(ts.procLines) != 2 {
		t.Fatalf("escaped text: %d lines, want 2", len(ts.procLines))
	}
	ts.literal = true
	ts.ensureProcessed()
	if len(ts.procLines) != 1 || ts.procLines[0] != `one\ntwo` {
		t.Fatalf("literal text: %q, want one line kept as written", ts.procLines)
	}
	ts.literal = false
	ts.ensureProcessed()
	if len(ts.procLines) != 2 {
		t.Fatalf("escaped again: %d lines, want 2", len(ts.procLines))
	}
}

// Runs are reused while the text stays, rebuilt when it changes, and for
// tagged text rebuilt when the glyph generation moves.
func TestCachedRunsReuseAndInvalidate(t *testing.T) {
	ts := &TextSprite{text: "plain\tline"}
	a := ts.cachedRuns()
	if len(a) != 1 || len(a[0]) != 1 || a[0][0].text != "plain    line" {
		t.Fatalf("runs of plain text = %+v", a)
	}
	if b := ts.cachedRuns(); &b[0] != &a[0] {
		t.Fatal("same text: runs were rebuilt")
	}
	// A second sprite drawing the same text gets the same runs (textCache).
	other := &TextSprite{text: "plain\tline"}
	if b := other.cachedRuns(); &b[0] != &a[0] {
		t.Fatal("another sprite with the same text: runs were rebuilt")
	}
	ts.text = "changed"
	if b := ts.cachedRuns(); b[0][0].text != "changed" {
		t.Fatalf("changed text: runs %+v", b)
	}

	saved := sys.menuPlayer
	defer func() { sys.menuPlayer = saved }()
	tagged := &TextSprite{text: "press <unknown.tag> now"}
	r1 := tagged.cachedRuns()
	if r2 := tagged.cachedRuns(); &r2[0] != &r1[0] {
		t.Fatal("tagged text, nothing changed: runs were rebuilt")
	}
	sys.menuPlayer = saved + 1
	if r3 := tagged.cachedRuns(); &r3[0] == &r1[0] {
		t.Fatal("tagged text after a glyph state change: runs were not rebuilt")
	}
}

func TestGlyphGenerationFollowsState(t *testing.T) {
	saved := sys.uiGlyphDevice
	defer func() { sys.uiGlyphDevice = saved }()
	g1 := sys.glyphGeneration()
	if g1 == 0 {
		t.Fatal("generation 0 is reserved for text without tags")
	}
	if g2 := sys.glyphGeneration(); g2 != g1 {
		t.Fatalf("no change: generation moved %d -> %d", g1, g2)
	}
	sys.uiGlyphDevice = saved + 7
	g3 := sys.glyphGeneration()
	if g3 == g1 {
		t.Fatal("the hint device changed: generation did not move")
	}

	// Controller Mapping > Glyphs edits the chosen-pack map in place.
	savedPacks := sys.cfg.Input.ControllerGlyphs
	defer func() { sys.cfg.Input.ControllerGlyphs = savedPacks }()
	packs := map[string]string{}
	for k, v := range savedPacks {
		packs[k] = v
	}
	sys.cfg.Input.ControllerGlyphs = packs
	g4 := sys.glyphGeneration()
	if g4 != g3 {
		t.Fatalf("same chosen packs in a new map: generation moved %d -> %d", g3, g4)
	}
	const pad = "test pad 045e 0b13"
	steps := []struct {
		name string
		edit func()
	}{
		{"choose a pack", func() { packs[pad] = "ps2" }},
		{"choose another pack", func() { packs[pad] = "ps5" }},
		{"back to automatic", func() { delete(packs, pad) }},
	}
	last := g4
	for _, step := range steps {
		step.edit()
		g := sys.glyphGeneration()
		if g == last {
			t.Fatalf("%s: generation did not move", step.name)
		}
		if again := sys.glyphGeneration(); again != g {
			t.Fatalf("%s: generation moved again without a change", step.name)
		}
		last = g
	}
}

// refreshFrom copies the template but keeps the sprite's own PalFX and text
// caches.
func TestRefreshFromKeepsCaches(t *testing.T) {
	template := NewTextSprite()
	template.text = "template"
	template.x = 5
	ts := &TextSprite{text: "label"}
	runs := ts.cachedRuns()
	ts.refreshFrom(template)
	if ts.x != 5 || ts.palfx == nil || ts.palfx == template.palfx {
		t.Fatalf("refreshFrom: x=%v palfx=%p template palfx=%p", ts.x, ts.palfx, template.palfx)
	}
	pfx := ts.palfx
	ts.text = "label"
	if again := ts.cachedRuns(); &again[0] != &runs[0] {
		t.Fatal("refreshFrom lost the runs of the sprite's own text")
	}
	ts.refreshFrom(template)
	if ts.palfx != pfx {
		t.Fatal("refreshFrom replaced the sprite's PalFX")
	}
}

// Cache hits allocate nothing: this is what keeps menus, hints and the
// movelist from making garbage every frame.
func TestTextCacheHitsDoNotAllocate(t *testing.T) {
	ts := &TextSprite{text: "Command List\nController Setup"}
	ts.cachedRuns()
	if n := testing.AllocsPerRun(100, func() { ts.cachedRuns() }); n != 0 {
		t.Errorf("cachedRuns hit: %v allocations", n)
	}
	// One sprite drawing two texts in turn, as a menu's rows do.
	if n := testing.AllocsPerRun(100, func() {
		ts.text = "Command List"
		ts.cachedRuns()
		ts.text = "Controller Setup"
		ts.cachedRuns()
	}); n != 0 {
		t.Errorf("rows through one sprite: %v allocations", n)
	}
	if n := testing.AllocsPerRun(100, func() { sys.glyphGeneration() }); n != 0 {
		t.Errorf("glyphGeneration: %v allocations", n)
	}
	if n := testing.AllocsPerRun(100, func() { runePrefix("Controller Setup", 5) }); n != 0 {
		t.Errorf("runePrefix: %v allocations", n)
	}
	template := NewTextSprite()
	label := &TextSprite{}
	label.refreshFrom(template)
	if n := testing.AllocsPerRun(100, func() { label.refreshFrom(template); label.text = "q"; label.cachedRuns() }); n != 0 {
		t.Errorf("refreshFrom + cachedRuns: %v allocations", n)
	}
}
