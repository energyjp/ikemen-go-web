package main

import "testing"

// A bitmap font with the given characters, each w pixels wide.
func testBitmapFont(w uint16, chars ...rune) *Fnt {
	f := newFnt()
	f.Type = "bitmap"
	f.Size = [2]uint16{4, 8}
	f.images[0] = make(map[rune]*FntCharImage)
	for _, c := range chars {
		f.images[0][c] = &FntCharImage{w: w}
	}
	return f
}

func TestFontFallbackFor(t *testing.T) {
	pack := testBitmapFont(5, 'A', 'B', 'é')
	jp := testBitmapFont(9, 'A', 'ア', 'イ')
	if got := pack.fallbackFor("アイ", 0, false); got != pack {
		t.Fatal("no fallback set: the font itself")
	}
	pack.fallback = jp
	cases := []struct {
		text    string
		literal bool
		want    *Fnt
		why     string
	}{
		{"AB", false, pack, "ASCII stays in the pack's font"},
		{"AZ", false, pack, "a missing ASCII character never switches"},
		{"Aé", false, pack, "a character above ASCII the pack has"},
		{"アイA", false, jp, "kana the pack lacks: the whole text in the fallback"},
		{"アウ", false, pack, "the fallback lacks one too: the pack's font, as before"},
		{"<glyph:ア>A", false, pack, "a tag is not text"},
		{"<#ff0000>アイ</>", false, jp, "text between color tags"},
		{"<ア>", true, jp, "literal text has no tags"},
	}
	for _, c := range cases {
		for pass := 0; pass < 2; pass++ { // the second answer comes from the per-text cache
			if got := pack.fallbackFor(c.text, 0, c.literal); got != c.want {
				t.Errorf("%q (pass %d): %s", c.text, pass, c.why)
			}
		}
	}
	jp.fallback = jp
	if got := jp.fallbackFor("ウ", 0, false); got != jp {
		t.Error("a font that is its own fallback stays itself")
	}
}

func TestFontFallbackWidth(t *testing.T) {
	pack := testBitmapFont(5, 'A')
	jp := testBitmapFont(9, 'ア')
	ts := NewTextSprite()
	ts.fnt = pack
	ts.yscl = 1
	before := ts.TextWidth("ア")
	pack.fallback = jp
	if got := ts.TextWidth("ア"); got != 9 || got == before {
		t.Fatalf("width measured in the fallback: got %v (without it %v), want 9", got, before)
	}
	if got := ts.TextWidth("A"); got != 5 {
		t.Fatalf("ASCII width in the pack's font: got %v, want 5", got)
	}
	if ts.fnt != pack {
		t.Fatal("the sprite keeps its own font")
	}
}

func TestLinkFontFallbacks(t *testing.T) {
	m := &Motif{Fnt: map[int]*Fnt{2: newFnt(), 5: newFnt(), 7: newFnt()}}
	m.Files.Font = map[string]*FontProperties{
		"font2": {Fallback: 5},
		"Font7": {Fallback: 7},
		"font5": {Fallback: -1},
		"font9": {Fallback: 5},
	}
	m.linkFontFallbacks()
	if m.Fnt[2].fallback != m.Fnt[5] {
		t.Error("font2.fallback = 5 links font 2 to font 5")
	}
	if m.Fnt[7].fallback != nil || m.Fnt[5].fallback != nil {
		t.Error("no link for a font's own index or -1")
	}
}
