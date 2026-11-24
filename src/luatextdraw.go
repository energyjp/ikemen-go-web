package main

// textImgDraw's queued draws, reused frame after frame.
//
// textImgDraw snapshots the sprite (its text, position and colors as they are
// when Lua calls it) and queues the draw for the Lua draw queue's flush. It
// used to make the snapshot with Copy and queue two closures (the draw and
// the aspect-state wrapper of luaQueueLayerDraw): four heap objects per call,
// and menus call it for every row and column of every frame (main.lua
// f_menuItemTextDraw). Here each queued draw is a slot that keeps its sprite,
// its PalFX and its two closures; luaFlushDrawQueue runs the queued draws and
// then frees every slot for the next frame (luaTextDrawsUsed = 0).
type luaTextDraw struct {
	ts    TextSprite
	layer int16
	state drawAspectState
	run   func() // what the queue runs: the aspect state, then draw
	draw  func() // ts.Draw(layer)
}

var (
	luaTextDraws     []*luaTextDraw
	luaTextDrawsUsed int
)

// luaQueueTextDraw queues ts as textImgDraw did with
// luaQueueLayerDraw(layer, func() { ts.Copy().Draw(layer) }).
func (s *System) luaQueueTextDraw(ts *TextSprite, layer int16) {
	if luaTextDrawsUsed == len(luaTextDraws) {
		d := &luaTextDraw{}
		d.draw = func() { d.ts.Draw(d.layer) }
		// wrapDrawWithAspectState's closure, with the captured state in d.
		d.run = func() {
			st := d.state
			if sys.shouldComposeFullResolution() {
				st = sys.motifAspectState()
			}
			sys.withAspectState(st, d.draw)
		}
		luaTextDraws = append(luaTextDraws, d)
	}
	d := luaTextDraws[luaTextDrawsUsed]
	luaTextDrawsUsed++
	d.ts.refreshFrom(ts)
	d.layer = layer
	d.state = s.captureAspectState()
	// luaQueueLayerDraw's queues, the draw already wrapped.
	if layer < 0 {
		s.luaDrawPreOps = append(s.luaDrawPreOps, d.run)
		return
	}
	l := int(layer)
	if l >= len(s.luaDrawLayerOps) {
		l = len(s.luaDrawLayerOps) - 1
	}
	s.luaDrawLayerOps[l] = append(s.luaDrawLayerOps[l], d.run)
}
