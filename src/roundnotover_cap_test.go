package main

import (
	"reflect"
	"testing"
)

// The online RoundNotOver watchdog. These tests cover the three properties the
// change was asked to guarantee: online-only, no false trigger on a normal
// round end, and desync safety.

func TestWatchdogNeverFiresOffline(t *testing.T) {
	var pin int32
	// Far past the cap. Offline must never count and never release, or an
	// H-scene/finisher would be truncated in single player.
	for i := 0; i < int(RoundNotOverOnlineCap)*3; i++ {
		var release bool
		pin, release = roundNotOverWatchdog(false, pin)
		if release {
			t.Fatalf("watchdog released OFFLINE at frame %d - offline must be untouched", i)
		}
		if pin != 0 {
			t.Fatalf("offline counter advanced to %d at frame %d - must stay 0", pin, i)
		}
	}
}

func TestWatchdogFiresOnlineAtCap(t *testing.T) {
	var pin int32
	released := -1
	for i := 1; i <= int(RoundNotOverOnlineCap)+10; i++ {
		var release bool
		pin, release = roundNotOverWatchdog(true, pin)
		if release {
			released = i
			break
		}
	}
	if released == -1 {
		t.Fatal("watchdog never released online - the hang would persist")
	}
	want := int(RoundNotOverOnlineCap) + 1
	if released != want {
		t.Fatalf("released on frame %d, want %d", released, want)
	}
	// 1200 frames == 20s at 60fps, the agreed budget.
	if RoundNotOverOnlineCap != 1200 {
		t.Fatalf("cap is %d frames, expected 1200 (20s @60fps)", RoundNotOverOnlineCap)
	}
}

func TestWatchdogDoesNotFireBeforeCap(t *testing.T) {
	var pin int32
	// The longest LEGITIMATE win-pose hold measured on the roster is ~1015
	// ticks, which is fewer pin frames than that once the pre-pin countdown is
	// accounted for. Nothing at that length may be clipped.
	for i := 1; i <= 1015; i++ {
		var release bool
		pin, release = roundNotOverWatchdog(true, pin)
		if release {
			t.Fatalf("released at frame %d - clips a legitimate long win pose", i)
		}
	}
}

// A normal round end never engages the pin at all, so the caller zeroes the
// counter every frame. Even in an online match that must never accumulate
// toward a release.
func TestNormalRoundEndNeverAccumulates(t *testing.T) {
	var pin int32
	for i := 0; i < int(RoundNotOverOnlineCap)*2; i++ {
		// Caller's else-branch: not pinned this frame.
		pin = 0
		// A brief, non-continuous assert (one frame every so often).
		if i%50 == 0 {
			var release bool
			pin, release = roundNotOverWatchdog(true, pin)
			if release {
				t.Fatalf("intermittent one-frame asserts released at %d - must require a CONTINUOUS hold", i)
			}
		}
	}
}

func TestIntermittentHoldsResetTheCounter(t *testing.T) {
	var pin int32
	// Hold for most of the budget, drop the pin, then hold again. The second
	// hold must start from scratch.
	for i := 0; i < int(RoundNotOverOnlineCap)-1; i++ {
		pin, _ = roundNotOverWatchdog(true, pin)
	}
	pin = 0 // pin released for a frame -> caller resets
	for i := 1; i <= int(RoundNotOverOnlineCap); i++ {
		var release bool
		pin, release = roundNotOverWatchdog(true, pin)
		if release {
			t.Fatalf("released after only %d frames of the SECOND hold - counter did not reset", i)
		}
	}
}

// DESYNC SAFETY: the counter must live inside SystemStateVars, which rollback
// copies wholesale on save/load. If it were a plain System field it would not
// be restored on a rollback re-simulation, so the two peers would diverge.
func TestPinCounterIsRollbackSnapshotted(t *testing.T) {
	tv := reflect.TypeOf(SystemStateVars{})
	f, ok := tv.FieldByName("roundNotOverPin")
	if !ok {
		t.Fatal("roundNotOverPin is NOT in SystemStateVars - rollback would not restore it, and the peers would diverge")
	}
	if f.Type.Kind() != reflect.Int32 {
		t.Fatalf("roundNotOverPin is %s, expected int32 (fixed width, deterministic across peers)", f.Type)
	}
	// Sanity: the field the watchdog mirrors is snapshotted the same way.
	if _, ok := tv.FieldByName("winskipped"); !ok {
		t.Fatal("winskipped missing from SystemStateVars - the release flag must be snapshotted too")
	}
}

// The watchdog must be a pure function of (online, pin): no clock, no local
// input, no config. Same inputs must always give the same output, or the two
// peers could release on different frames.
func TestWatchdogIsDeterministic(t *testing.T) {
	for _, online := range []bool{true, false} {
		for _, pin := range []int32{0, 1, 599, 1199, 1200, 1201, 5000} {
			gotPin, gotRel := roundNotOverWatchdog(online, pin)
			for i := 0; i < 200; i++ {
				p, r := roundNotOverWatchdog(online, pin)
				if p != gotPin || r != gotRel {
					t.Fatalf("non-deterministic for (online=%v, pin=%d): (%d,%v) then (%d,%v)",
						online, pin, gotPin, gotRel, p, r)
				}
			}
		}
	}
}
