package main

import (
	"strings"
	"testing"

	"gopkg.in/ini.v1"
)

// WHERE THE SELECT SCREEN'S BIG PORTRAIT DEFAULT ACTUALLY LIVES.
//
// Read only the struct tags and the engine looks broken: AnimationProperties
// defaults Spr to -1,0 and Anim to -1, and start.lua's f_animGet walks
// {anim, spr} and draws nothing when both are unset. Screenpacks written
// against MUGEN routinely position the portrait and never name it - both of
// NoZ's LEGACY motifs give p1.face.offset, .scale and .facing and stop - so it
// looks like they must come up blank.
//
// They do not. resources/defaultMotif.ini carries the number, and loadMotif
// assigns the embedded defaults before the user's file. This test exists so the
// next person to read those struct tags does not "fix" it twice, which is
// exactly the mistake that produced this file.

func faceTestIni(t *testing.T, body string) *ini.File {
	t.Helper()
	f, err := ini.LoadSources(ini.LoadOptions{
		Insensitive:             false,
		InsensitiveSections:     true,
		InsensitiveKeys:         false,
		SkipUnrecognizableLines: true,
	}, []byte(body))
	if err != nil {
		t.Fatalf("could not parse the test ini: %v", err)
	}
	return f
}

// The embedded defaults are the single source of the 9000,1 big portrait. If
// this ever stops holding, screenpacks that name no sprite go blank - and the
// fix belongs in that ini, not in a second copy in applyCustomDefaults.
func TestEmbeddedDefaultMotifSuppliesTheBigPortrait(t *testing.T) {
	f := faceTestIni(t, string(defaultMotif))
	sec, err := f.GetSection("Select Info")
	if err != nil {
		t.Fatalf("the embedded default motif has no [Select Info]: %v", err)
	}
	for _, side := range []string{"p1", "p2"} {
		k, err := sec.GetKey(side + ".face.spr")
		if err != nil {
			t.Fatalf("%s.face.spr missing from the embedded defaults: a pack that names no sprite would draw no portrait", side)
		}
		got := strings.ReplaceAll(k.Value(), " ", "")
		if got != "9000,1" {
			t.Errorf("%s.face.spr = %q in the embedded defaults, want 9000,1", side, got)
		}
	}
}

// applyCustomDefaults used to `return` when [Select Info] or cell.spacing was
// absent from the USER's file, taking every default below it with it - all of
// Title Info's computed offsets included. One unrelated missing key must not
// disable the rest of the function.
func TestCustomDefaultsSurviveAMissingCellSpacing(t *testing.T) {
	m := &Motif{}
	m.Info.Localcoord = [2]int32{1280, 720}

	f := faceTestIni(t, `
[Info]
localcoord = 1280,720

[Select Info]
rows = 13
columns = 24
p1.face.offset = 45,68
`)
	applyCustomDefaults(m, f)

	if m.TitleInfo.Footer.Version.Text == "" {
		t.Error("title_info.footer.version.text is empty: an early return is eating the defaults below it")
	}
	if m.TitleInfo.Loading.Offset == [2]float32{} {
		t.Error("title_info.loading.offset was never computed, for the same reason")
	}
}

// And the mirroring itself still works when the key IS there, which is the
// behaviour the early return was guarding.
func TestSingleCellSpacingStillMirrors(t *testing.T) {
	m := &Motif{}
	m.Info.Localcoord = [2]int32{1280, 720}
	m.SelectInfo.Cell.Spacing = [2]float32{2, 0}

	f := faceTestIni(t, `
[Select Info]
cell.spacing = 2
`)
	applyCustomDefaults(m, f)

	if m.SelectInfo.Cell.Spacing[1] != 2 {
		t.Errorf("cell.spacing = %v, want the single value mirrored into both axes", m.SelectInfo.Cell.Spacing)
	}
}
