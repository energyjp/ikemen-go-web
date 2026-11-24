package main

import "testing"

// [Def] bold = n is read with the font's other [Def] keys; a font without it is not bold.
func TestFontDefBold(t *testing.T) {
	read := func(lines ...string) *Fnt {
		f := newFnt()
		is := NewIniSection()
		i := 0
		is.Parse(lines, &i)
		loadDefInfo(f, "test.def", is, -1)
		return f
	}
	if f := read("Type = truetype", "Size = 16,16", "Bold = 1"); f.bold != 1 || f.Size[1] != 16 {
		t.Fatalf("Bold = 1: got bold %v, size %v", f.bold, f.Size)
	}
	if f := read("Type = truetype", "Size = 16,16"); f.bold != 0 {
		t.Fatalf("no Bold key: got bold %v, want 0", f.bold)
	}
}
