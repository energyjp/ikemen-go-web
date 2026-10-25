package main

import (
	"reflect"
	"testing"
)

// The round fade length is host-authoritative in netplay.
//
// WHY THIS EXISTS. Demo mode copies its own fade template into the SHARED
// fight-screen round fade (motif.go:3919 fadeIn, motif.go:3929 fadeOut, via
// Fade.init at rect.go:70/84-85), and the demo teardown calls only
// Fade.reset() (motif.go:3937), which never touches .time (rect.go:27-38).
// A peer that idled ~600 frames on the title screen therefore carries the
// [Demo Mode] fade time (50) into every later match, while a peer that went
// straight to netplay carries the fight.def [Round] value (30).
//
// fadeoutStart = -over_waittime - 2 - overTime() + fadeOut.duration()
// With the shipped fight.def values (over.waittime=45, over.time=300) that is
// -347 + fade, so the two peers pin sys.intro at -317 and -297 and every later
// `intro == fadeoutStart` equality runs out of step for the rest of the match.
//
// Field-reproduced 2026-09-01, 5/5, with a decisive control: when BOTH peers
// had run demo mode there was NO desync. The bug is disagreement, not
// corruption -- which is exactly what pinning to one value removes.
func withRoundFade(t *testing.T, localOut, localIn int32, f func()) {
	t.Helper()
	oldRound := sys.fightScreen.round
	oldOv := sys.netplayOverride
	defer func() { sys.fightScreen.round = oldRound; sys.netplayOverride = oldOv }()
	sys.fightScreen.round = &FightScreenRound{over_waittime: 45, over_time: 300}
	sys.fightScreen.round.fadeOut = newFade()
	sys.fightScreen.round.fadeOut.time = localOut
	sys.fightScreen.round.fadeIn = newFade()
	sys.fightScreen.round.fadeIn.time = localIn
	f()
}

func fadeoutStartNow() int32 {
	ro := sys.fightScreen.round
	return -ro.over_waittime - 2 - ro.overTime() + ro.fadeOut.duration()
}

// NEGATIVE CONTROL, and the most important test here: it proves the harness
// actually reproduces the captured dump. Without it, TestPinMakesPeersAgree
// could pass vacuously against a harness that never diverged in the first
// place. -317/-297 are the real numbers off the author's two screens.
func TestUnpinnedRoundFadeStillDiverges(t *testing.T) {
	var a, b int32
	withRoundFade(t, 30, 30, func() { a = fadeoutStartNow() })
	withRoundFade(t, 50, 50, func() { b = fadeoutStartNow() })
	if a == b {
		t.Fatal("negative control failed: this harness does not reproduce the bug, " +
			"so the positive test below proves nothing")
	}
	if a != -317 || b != -297 {
		t.Fatalf("got %d/%d, want -317/-297 (the captured field dump)", a, b)
	}
}

func TestPinMakesPeersAgree(t *testing.T) {
	var a, b int32
	// Host is the clean peer (30). Guest is "Infected" (50) and must be pulled
	// to the host's value.
	withRoundFade(t, 30, 30, func() {
		sys.netplayOverride = SessionConfigOverride{Active: true, RoundFadeTime: 30, RoundFadeInTime: 30}
		sys.applySyncedRoundFade()
		a = fadeoutStartNow()
	})
	withRoundFade(t, 50, 50, func() {
		sys.netplayOverride = SessionConfigOverride{Active: true, RoundFadeTime: 30, RoundFadeInTime: 30}
		sys.applySyncedRoundFade()
		b = fadeoutStartNow()
	})
	if a != b {
		t.Fatalf("peers still disagree after the pin: %d vs %d", a, b)
	}
	if a != -317 {
		t.Fatalf("fadeoutStart = %d, want -317", a)
	}
}

// The host may itself be Infected. Lockstep is the objective, not cosmetics:
// both peers must land on 50 and agree.
func TestInfectedHostPinsGuestToItsValue(t *testing.T) {
	var a, b int32
	withRoundFade(t, 50, 50, func() {
		sys.netplayOverride = SessionConfigOverride{Active: true, RoundFadeTime: 50, RoundFadeInTime: 50}
		sys.applySyncedRoundFade()
		a = fadeoutStartNow()
	})
	withRoundFade(t, 30, 30, func() {
		sys.netplayOverride = SessionConfigOverride{Active: true, RoundFadeTime: 50, RoundFadeInTime: 50}
		sys.applySyncedRoundFade()
		b = fadeoutStartNow()
	})
	if a != b {
		t.Fatalf("peers disagree with an infected host: %d vs %d", a, b)
	}
}

// fadeIn is clobbered by the same demo write and gates intro stepping, so
// pinning only fadeOut would leave half the divergence live.
func TestFadeInIsPinnedToo(t *testing.T) {
	withRoundFade(t, 30, 50, func() {
		sys.netplayOverride = SessionConfigOverride{Active: true, RoundFadeTime: 30, RoundFadeInTime: 30}
		sys.applySyncedRoundFade()
		if got := sys.fightScreen.round.fadeIn.time; got != 30 {
			t.Fatalf("fadeIn = %d after pin, want 30", got)
		}
	})
}

// A peer that predates the field sends nothing, which JSON decodes to 0.
// Pinning to 0 would turn an intermittent desync into a guaranteed one.
func TestZeroMeansNoPin(t *testing.T) {
	withRoundFade(t, 50, 50, func() {
		sys.netplayOverride = SessionConfigOverride{Active: true, RoundFadeTime: 0, RoundFadeInTime: 0}
		sys.applySyncedRoundFade()
		if got := sys.fightScreen.round.fadeOut.time; got != 50 {
			t.Fatalf("an absent field pinned fadeOut to %d; it must be a no-op", got)
		}
		if got := sys.fightScreen.round.fadeIn.time; got != 50 {
			t.Fatalf("an absent field pinned fadeIn to %d; it must be a no-op", got)
		}
	})
}

// POSITIVE CONTROL for the guard itself: offline must be untouched. Without
// this, a predicate that always returned early would pass every test above.
func TestOfflineIsUntouched(t *testing.T) {
	withRoundFade(t, 50, 50, func() {
		sys.netplayOverride = SessionConfigOverride{} // Active = false
		sys.applySyncedRoundFade()
		if got := sys.fightScreen.round.fadeOut.time; got != 50 {
			t.Fatalf("offline fadeOut was changed to %d", got)
		}
	})
}

// The pinned value is a SESSION CONSTANT and must never be rolled back. It
// rides the snapshot already via FightScreen.Clone -> Fade.Clone; putting it in
// SystemStateVars would let a snapshot taken before the pin restore a stale
// value. Mirrors the inverse assertion style of roundnotover_cap_test.go.
func TestRoundFadeIsNotInSystemStateVars(t *testing.T) {
	ty := reflect.TypeOf(SystemStateVars{})
	for _, name := range []string{"RoundFadeTime", "RoundFadeInTime"} {
		if _, ok := ty.FieldByName(name); ok {
			t.Fatalf("%s must NOT be in SystemStateVars: it is a session constant, "+
				"and a pre-pin snapshot would restore a stale value on load", name)
		}
	}
}
