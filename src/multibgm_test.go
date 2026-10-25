package main

import "testing"

// A stage with SEVERAL songs, authored from the Mod Studio.
//
// The Studio writes a stage's music as select.def params, one `music=` per
// track: "stages/x/x.def, music=a.mp3, music=b.mp3, music=c.mp3". These tests
// pin the two properties that feature depends on, because both live in engine
// code the Studio does not own:
//
//   1. every `music=` param becomes ANOTHER candidate, not an overwrite;
//   2. two peers that agreed on a seed pick the SAME candidate.
//
// (2) is what makes randomised stage BGM safe in netplay at all. It holds
// because Srand derives the music stream from the gameplay seed both peers
// already agree on - see common.go - and never the other way around.

func TestMusicParamsAppendCandidates(t *testing.T) {
	m := make(Music)
	m.AppendParams([]string{
		"music=sound/one.mp3",
		"music=sound/two.mp3",
		"music=sound/three.mp3",
	})
	got := m[""]
	if len(got) != 3 {
		t.Fatalf("three music= params should give 3 candidates, got %d", len(got))
	}
	for i, want := range []string{"sound/one.mp3", "sound/two.mp3", "sound/three.mp3"} {
		if got[i].bgmusic != want {
			t.Errorf("candidate %d = %q, want %q", i, got[i].bgmusic, want)
		}
	}
	// A single track must still behave exactly as it always has.
	one := make(Music)
	one.AppendParams([]string{"music=sound/solo.mp3"})
	if len(one[""]) != 1 || one[""][0].bgmusic != "sound/solo.mp3" {
		t.Fatalf("single music= param regressed: %+v", one[""])
	}
}

func TestPeersPickTheSameStageTrack(t *testing.T) {
	build := func() Music {
		m := make(Music)
		m.AppendParams([]string{
			"music=sound/one.mp3", "music=sound/two.mp3",
			"music=sound/three.mp3", "music=sound/four.mp3",
		})
		return m
	}
	// Same synced seed on both sides, as netplay.go arranges.
	for _, seed := range []int32{1, 7, 12345, 2147483646, -99} {
		Srand(seed)
		sys.matchMusicSel = nil
		a := build().matchSelection("")
		Srand(seed)
		sys.matchMusicSel = nil
		b := build().matchSelection("")
		if a == nil || b == nil {
			t.Fatalf("seed %d: no candidate chosen", seed)
		}
		if a.bgmusic != b.bgmusic {
			t.Fatalf("seed %d: peers disagree - %q vs %q", seed, a.bgmusic, b.bgmusic)
		}
	}
	// NEGATIVE CONTROL: different seeds must be able to disagree, or the test
	// above would pass just as well on a picker that always returns index 0.
	//
	// The seeds here are DRAWN FROM THE ENGINE RNG, which is how real ones arise:
	// netplay.go has the host do `seed = Random()` and send it. That matters. A
	// counter (1, 2, 3...) makes this assertion FAIL, because Srand derives the
	// music stream as (s ^ 0x5f3759df) and one Park-Miller step does not diffuse
	// the low bits of adjacent seeds into the high bits the index division reads -
	// every seed from 1 to 200 picks the same track. Measured, not assumed.
	// Harmless as shipped (no counter ever reaches Srand) but it is a real edge:
	// anyone who later seeds matches from a counter or a coarse timestamp will get
	// the same song every time and think the randomiser is broken.
	Srand(20240901)
	seeds := make([]int32, 0, 200)
	for i := 0; i < 200; i++ {
		seeds = append(seeds, Random())
	}
	seen := map[string]bool{}
	for _, s := range seeds {
		Srand(s)
		sys.matchMusicSel = nil
		if bg := build().matchSelection(""); bg != nil {
			seen[bg.bgmusic] = true
		}
	}
	if len(seen) < 3 {
		t.Fatalf("picker barely varied across 200 engine-drawn seeds (%d of 4 tracks)", len(seen))
	}
	t.Logf("distinct tracks chosen across 200 seeds: %d of 4", len(seen))
}

func TestStageTrackChoiceLeavesGameplaySeedAlone(t *testing.T) {
	m := make(Music)
	m.AppendParams([]string{"music=sound/one.mp3", "music=sound/two.mp3"})
	Srand(4242)
	before := sys.randseed
	sys.matchMusicSel = nil
	for i := 0; i < 50; i++ {
		m.matchSelection("")
		sys.matchMusicSel = nil // force a fresh draw every time
	}
	if sys.randseed != before {
		t.Fatalf("choosing a stage track advanced the GAMEPLAY seed: %d -> %d", before, sys.randseed)
	}
}
