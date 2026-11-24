package main

import "testing"

// A 32-bit glyph keeps copies at 1/2, 1/4 and 1/8 of its size, each pixel the
// average of the 2 x 2 above it, with the axis scaled too; drawGlyph takes the
// smallest copy still as tall as the glyph on screen.
func TestGlyphMipLevels(t *testing.T) {
	const w, h = 64, 64
	data := make([]byte, w*h*4)
	// Left half opaque white, right half clear: every copy keeps the edge in
	// the middle, and a 2 x 2 block across it never happens at these sizes.
	for y := 0; y < h; y++ {
		for x := 0; x < w/2; x++ {
			copy(data[(y*w+x)*4:], []byte{255, 255, 255, 255})
		}
	}
	// One grey 2 x 2 block averages to its mean.
	copy(data[(0*w+40)*4:], []byte{100, 100, 100, 100})
	copy(data[(0*w+41)*4:], []byte{0, 0, 0, 0})
	copy(data[(1*w+40)*4:], []byte{200, 200, 200, 200})
	copy(data[(1*w+41)*4:], []byte{102, 102, 102, 102})
	s := &Sprite{Size: [2]uint16{w, h}, Offset: [2]int16{0, 64}}
	mips := glyphMipLevels(s, data, w, h)
	if len(mips) != 3 {
		t.Fatalf("levels: got %d, want 3 (32, 16, 8)", len(mips))
	}
	for i, want := range []uint16{32, 16, 8} {
		m := mips[i]
		if m.Size != [2]uint16{want, want} || m.Offset != [2]int16{0, int16(want)} || m.coldepth != 32 {
			t.Fatalf("level %d: size %v offset %v depth %d", i+1, m.Size, m.Offset, m.coldepth)
		}
	}
	// glyphMip: never a copy shorter than the glyph on screen.
	for i := range mips {
		mips[i].Tex = glyphMipTestTex{} // stands in for the uploaded texture
	}
	s.mips = mips
	for _, c := range []struct {
		px   float32
		want uint16
	}{{70, 64}, {64, 64}, {33, 64}, {32, 32}, {22, 32}, {15, 16}, {9, 16}, {8, 8}, {4, 8}} {
		if got := glyphMip(s, c.px).Size[1]; got != c.want {
			t.Fatalf("glyphMip(%v px): got %d, want %d", c.px, got, c.want)
		}
	}
	// A copy whose texture has not arrived yet is not used.
	mips[1].Tex = nil
	if got := glyphMip(s, 15).Size[1]; got != 32 {
		t.Fatalf("glyphMip with level 2 pending: got %d, want 32", got)
	}
}

// Stands in for an uploaded texture; no method is called.
type glyphMipTestTex struct{ Texture }

func TestGlyphHalve(t *testing.T) {
	const w, h = 4, 3
	data := make([]byte, w*h*4)
	set := func(x, y int, v byte) { copy(data[(y*w+x)*4:], []byte{v, v, v, v}) }
	set(0, 0, 100)
	set(1, 0, 0)
	set(0, 1, 200)
	set(1, 1, 102)
	set(2, 2, 40) // odd last row: repeated, so the block is 40, 0, 40, 0
	next, nw, nh := glyphHalve(data, w, h)
	if nw != 2 || nh != 2 {
		t.Fatalf("size: got %dx%d, want 2x2", nw, nh)
	}
	// (100 + 0 + 200 + 102 + 2) / 4 = 101; (40 + 0 + 40 + 0 + 2) / 4 = 20
	if next[0] != 101 || next[3] != 101 || next[(1*2+1)*4] != 20 || next[(0*2+1)*4] != 0 {
		t.Fatalf("averages: got %v", next)
	}
}
