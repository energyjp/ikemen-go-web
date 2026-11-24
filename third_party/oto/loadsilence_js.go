// Ikemen fork addition (not in upstream oto): silence while the page's thread
// is blocked.
//
// The ScriptProcessorNode's audioprocess callback runs on the page's main
// thread. When that thread is blocked (the engine loading a character, a
// stage or a screenpack, a garbage collection), the callbacks stop, and
// Chrome's ScriptProcessor keeps playing its two output buffers in turn: the
// last 2 x 46 ms of sound repeat until the thread is free again, then a burst
// of late callbacks overwrites buffers while they play. That is the loud
// stutter before a load.
//
// The fix is a GainNode between the ScriptProcessor and the destination whose
// automation is scheduled on the audio context's clock, so it runs on the
// audio thread whether or not the page's thread is free. Every callback
// covers the next buffer window (the one its own buffer plays in) with gain 1
// and schedules a 5 ms fade to 0 at that window's end. The next callback, if
// it comes in time, takes the fade back, so nothing changes while the page
// keeps up. If it does not come, the fade runs exactly where the old buffers
// would start repeating, and the output stays silent until a callback runs
// again; the first window it fills fades in over 15 ms. A backlog of late
// callbacks after a block all fall in the same period and cover the same next
// window, written by the last of them.
//
// Measured on Chrome: one audioprocess event per buffer, dispatched 0-4
// render quanta (128 frames each) after its buffer boundary on the context
// clock, never before; the buffer reaches the next node one quantum after its
// boundary; the boundaries start on multiples of the buffer size (the node is
// made in the same task as its context) and move one quantum later whenever
// the audio thread finds the listener busy (it then plays a quantum of
// zeros). The guard follows them from the callbacks that keep the buffer's
// pace. globalThis.ikemenLoadSilenceOff = true set before the engine starts
// leaves the plain connection; the state is globalThis.ikemenLoadSilence.

//go:build js

package oto

import (
	"syscall/js"
)

const loadSilenceJS = `
const Q = 128, sr = ctx.sampleRate, tm = (x) => x / sr, mod = (x) => ((x % n) + n) % n;
const FO = Math.round(0.005 * sr), FI = Math.round(0.015 * sr), TOL = 4 * Q;
const st = { on: false, why: '', ph: 0, winEnd: -1, follow: 0, gaps: 0, late: 0, moves: 0 };
globalThis.ikemenLoadSilence = st;
if (globalThis.ikemenLoadSilenceOff === true) { st.why = 'turned off by the page'; return false; }
let gain;
try {
  gain = ctx.createGain();
  sp.connect(gain);
  gain.connect(ctx.destination);
} catch (e) {
  try { if (gain) gain.disconnect(); } catch (e2) {}
  st.why = 'no gain node: ' + e;
  return false;
}
const g = gain.gain;
const per = n / sr * 1000, ring = [];
let last = 0;
st.on = true;
sp.addEventListener('audioprocess', () => {
  if (!st.on) return;
  try {
    const now = performance.now(), gap = now - last; last = now;
    const f = Math.round(ctx.currentTime * sr);
    if (gap > 0.75 * per && gap < 1.25 * per) {
      // Where the buffer boundaries are. An on-pace callback runs 0-4
      // quanta after its boundary, never before; the boundaries move later
      // by a quantum whenever the audio thread finds this listener busy and
      // plays a quantum of zeros instead (measured on Chrome).
      let L = mod(f - st.ph);
      if (L >= n - TOL) { st.ph = mod(st.ph - (n - L)); ring.length = 0; L = 0; st.moves++; }
      ring.push(L); if (ring.length > 12) ring.shift();
      if (ring.length === 12) {
        const mn = Math.min.apply(null, ring);
        if (mn >= 2 * Q) { const d = mn - Q; st.ph = mod(st.ph + d); for (let i = 0; i < ring.length; i++) ring[i] -= d; st.moves++; }
      }
    }
    // this callback's buffer plays in the next period, one quantum after its boundary
    const ws = st.ph + (Math.floor((f - st.ph) / n) + 1) * n + Q, we = ws + n;
    if (st.winEnd >= 0 && ws <= st.winEnd - n + TOL) return;
    if (st.winEnd >= 0 && Math.abs(ws - st.winEnd) <= TOL) {
      // on time: take back the fade waiting at the end of the window before
      if (f + Q >= st.winEnd - FO) st.late++;
      g.cancelScheduledValues(tm(st.winEnd - FO));
      st.follow++;
    } else {
      // the windows before this one went by without a callback: fade in
      g.setValueAtTime(0, tm(ws));
      g.linearRampToValueAtTime(1, tm(ws + FI));
      st.gaps++;
    }
    g.setValueAtTime(1, tm(we - FO));
    g.linearRampToValueAtTime(0, tm(we));
    st.winEnd = we;
  } catch (e) {
    st.on = false; st.why = 'error: ' + e;
    try { g.cancelScheduledValues(0); g.setValueAtTime(1, ctx.currentTime); } catch (e2) {}
    try { console.warn('audio: load silence off (' + st.why + ')'); } catch (e2) {}
  }
});
return true;
`

// installLoadSilence puts the guarded gain node between sp and the
// destination. False when it could not (a content security policy that
// forbids new Function, no GainNode, turned off): the caller then connects sp
// directly, as upstream oto does.
func installLoadSilence(ctx, sp js.Value, frames int) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return js.Global().Get("Function").New("ctx", "sp", "n", loadSilenceJS).Invoke(ctx, sp, frames).Truthy()
}
