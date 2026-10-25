package main

import (
	"reflect"
	"testing"
)

// The round-transition desyncs documented in mugen-web/NETPLAY.md were caused by
// background-music selection drawing from the GAMEPLAY rng (sys.randseed).
//
// Music.act() is reached from Stage.action(), which System.action() calls ABOVE
// the sys.tickFrame() gate, so it runs once per pass through the main loop, not
// once per simulated frame. A machine that spends more passes at tickCount == 0
// therefore drew more randoms, and sys.randseed - the first field LiveChecksum
// hashes - walked out from under the peer.
//
// The invariant these tests protect: NOTHING in music selection may advance
// sys.randseed.

func musicFixture() Music {
	m := make(Music)
	for _, k := range []string{"", "victory", "life", "round1"} {
		a, b := newBgMusic(), newBgMusic()
		a.bgmusic = "sound/does-not-exist-a.mp3"
		b.bgmusic = "sound/does-not-exist-b.mp3"
		m[k] = []*bgMusic{a, b} // two candidates: the branch that used to draw
	}
	return m
}

func TestMusicSelectionNeverTouchesGameplaySeed(t *testing.T) {
	const seed int32 = 123456789
	cases := []struct {
		name string
		run  func(m Music)
	}{
		{"matchSelection", func(m Music) { m.matchSelection("") }},
		{"matchSelection victory", func(m Music) { m.matchSelection("victory") }},
		{"Read", func(m Music) { m.Read("", "") }},
		{"tryPlay", func(m Music) { m.tryPlay("victory", "") }},
		{"tryPlay empty prefix", func(m Music) { m.tryPlay("", "") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := musicFixture()
			sys.matchMusicSel = sys.matchMusicSel[:0]
			Srand(seed)
			before := sys.randseed
			for i := 0; i < 50; i++ { // the real bug was repetition across frames
				tc.run(m)
			}
			if sys.randseed != before {
				t.Fatalf("%s advanced the gameplay seed: %d -> %d (%s)",
					tc.name, before, sys.randseed,
					"this is the round-transition desync; music must use MusicRandom")
			}
		})
	}
}

func TestMusicStreamIsSeededAndValid(t *testing.T) {
	for _, s := range []int32{1, -1, 0, 123456789, IMax, -IMax, 0x5f3759df} {
		Srand(s)
		if sys.musicRandseed <= 0 {
			t.Fatalf("Srand(%d) left musicRandseed = %d; Park-Miller state must be > 0",
				s, sys.musicRandseed)
		}
		for i := 0; i < 10000; i++ {
			if v := MusicRandom(); v <= 0 {
				t.Fatalf("MusicRandom returned %d after Srand(%d) at draw %d", v, s, i)
			}
		}
	}
}

func TestSrandIsDeterministicAcrossPeers(t *testing.T) {
	// Both peers call Srand with the same synced value (netplay.go), so the
	// music stream must be a pure function of it - otherwise the track choice
	// diverges even though the checksum no longer would.
	Srand(987654321)
	a := sys.musicRandseed
	for i := 0; i < 100; i++ {
		MusicRandom()
	}
	Srand(987654321)
	if sys.musicRandseed != a {
		t.Fatalf("Srand is not pure: %d then %d", a, sys.musicRandseed)
	}
}

func TestMusicRandIStaysInRange(t *testing.T) {
	Srand(42)
	for _, n := range []int32{1, 2, 3, 7, 64} {
		for i := 0; i < 5000; i++ {
			v := MusicRandI(0, n-1)
			if v < 0 || v > n-1 {
				t.Fatalf("MusicRandI(0,%d) returned %d", n-1, v)
			}
		}
	}
}

func TestMusicRandseedIsRollbackSaved(t *testing.T) {
	// In SystemStateVars so a rollback restores it and the peers keep picking
	// the same track. It must NOT be in LiveChecksum - see the other test.
	f, ok := reflect.TypeOf(SystemStateVars{}).FieldByName("musicRandseed")
	if !ok {
		t.Fatal("musicRandseed is NOT in SystemStateVars - rollback would not restore it")
	}
	if f.Type.Kind() != reflect.Int32 {
		t.Fatalf("musicRandseed is %s, expected int32", f.Type)
	}
}
