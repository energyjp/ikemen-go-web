//go:build js

package main

import (
	"fmt"
	"image"
	"syscall/js"
)

// Browser window backend: replaces system_sdl.go for wasm builds.
//
// The "window" is a <canvas> (reused if the page provides one with
// id="ikemen-canvas", else created). Frame pacing rides
// requestAnimationFrame: SwapBuffers parks the main goroutine until the
// browser is ready for the next frame, which both paces the game and
// yields to the JS event loop (input events, network callbacks, audio).

type Window struct {
	canvas     js.Value
	title      string
	w, h       int
	fullscreen bool
	closeflag  bool
	frameCh    chan struct{}
	rafCb      js.Func
	frameIntMs float64 // target ms between engine frames (from Video.Framerate)
	nextFrame  float64 // rAF timestamp at which the next frame may be released
	frames     int     // released-frame counter (exposed for perf diagnostics)
}

func (s *System) newWindow(w, h int) (*Window, error) {
	doc := js.Global().Get("document")
	if !doc.Truthy() {
		return nil, fmt.Errorf("no DOM document (not running in a browser?)")
	}
	canvas := doc.Call("getElementById", "ikemen-canvas")
	if !canvas.Truthy() {
		canvas = doc.Call("createElement", "canvas")
		canvas.Set("id", "ikemen-canvas")
		doc.Get("body").Call("appendChild", canvas)
	}
	canvas.Set("width", w)
	canvas.Set("height", h)

	win := &Window{
		canvas:  canvas,
		title:   s.cfg.Config.WindowTitle,
		w:       w,
		h:       h,
		frameCh: make(chan struct{}, 1),
	}
	doc.Set("title", win.title)

	// requestAnimationFrame pump, capped to the configured framerate.
	//
	// Browsers fire rAF at the DISPLAY's refresh rate - 120/144/240Hz on
	// high-refresh screens. The engine runs one full game+render iteration per
	// released frame, and every render marshals GL calls through scratch typed
	// arrays (garbage). Releasing a frame on every rAF therefore runs the whole
	// loop 2-4x more often than the game is authored for, multiplying
	// allocation and forcing far more frequent GC pauses (the 200-500ms mark
	// stalls high-refresh users saw) for zero visual benefit. So release a
	// frame at most once per Video.Framerate interval; extra rAF callbacks on a
	// fast display just re-arm. On a 60Hz display this is a no-op.
	win.frameIntMs = 1000.0 / float64(s.cfg.Video.Framerate)
	if !(win.frameIntMs > 0) {
		win.frameIntMs = 1000.0 / 60.0
	}
	win.rafCb = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		now := 0.0
		if len(args) > 0 {
			now = args[0].Float() // DOMHighResTimeStamp, ms
		}
		if win.nextFrame == 0 {
			win.nextFrame = now
		}
		if now >= win.nextFrame {
			win.nextFrame += win.frameIntMs
			if win.nextFrame <= now { // fell behind (tab was hidden etc.) - don't spiral
				win.nextFrame = now + win.frameIntMs
			}
			win.frames++
			js.Global().Set("__ikemenEngineFrames", win.frames)
			select {
			case win.frameCh <- struct{}{}:
			default:
			}
		}
		js.Global().Call("requestAnimationFrame", win.rafCb)
		return nil
	})
	js.Global().Call("requestAnimationFrame", win.rafCb)

	// Keyboard: document-level so no element focus is required.
	keydown := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		ev := args[0]
		code := ev.Get("code").String()
		if key, ok := jsCodeToKey[code]; ok {
			OnKeyPressed(key, jsEventModifiers(ev))
			// Printable characters also feed text input (menus, IP entry).
			k := ev.Get("key").String()
			if len(k) == 1 {
				OnTextEntered(k)
			}
			ev.Call("preventDefault")
		}
		return nil
	})
	keyup := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		ev := args[0]
		code := ev.Get("code").String()
		if key, ok := jsCodeToKey[code]; ok {
			OnKeyReleased(key, jsEventModifiers(ev))
			ev.Call("preventDefault")
		}
		return nil
	})
	doc.Call("addEventListener", "keydown", keydown)
	doc.Call("addEventListener", "keyup", keyup)

	return win, nil
}

// SwapBuffers waits for the next animation frame. The browser presents
// the WebGL drawing buffer automatically when the callback yields.
func (w *Window) SwapBuffers() {
	<-w.frameCh
}

func (w *Window) SetIcon(icon []image.Image) {
	// Browser tabs use the page favicon; nothing to do.
}

func (w *Window) SetSwapInterval(interval int) {
	gfx.SetVSync(interval)
}

func (w *Window) GetSize() (int, int) {
	return w.canvas.Get("width").Int(), w.canvas.Get("height").Int()
}

// Calculates a position and size for the viewport to fill the window
// while centered. Same math as the SDL backend.
func (w *Window) GetScaledViewportSize() (int32, int32, int32, int32) {
	winWidth, winHeight := w.GetSize()

	if !sys.cfg.Video.KeepAspect {
		return 0, 0, int32(winWidth), int32(winHeight)
	}

	var x, y, resizedWidth, resizedHeight int32 = 0, 0, int32(winWidth), int32(winHeight)

	aspectGame := sys.getCurrentAspect()
	aspectWindow := float32(winWidth) / float32(winHeight)

	if aspectWindow > aspectGame {
		resizedHeight = int32(winHeight)
		resizedWidth = int32(float32(resizedHeight) * aspectGame)
		x = (int32(winWidth) - resizedWidth) / 2
		y = 0
	} else {
		resizedWidth = int32(winWidth)
		resizedHeight = int32(float32(resizedWidth) / aspectGame)
		x = 0
		y = (int32(winHeight) - resizedHeight) / 2
	}

	return x, y, resizedWidth, resizedHeight
}

func (w *Window) GetClipboardString() string {
	// navigator.clipboard is async-only; a blocking read isn't possible.
	return ""
}

func (w *Window) toggleFullscreen() {
	doc := js.Global().Get("document")
	if doc.Get("fullscreenElement").Truthy() {
		doc.Call("exitFullscreen")
		w.fullscreen = false
	} else if w.canvas.Get("requestFullscreen").Truthy() {
		w.canvas.Call("requestFullscreen")
		w.fullscreen = true
	}
}

func (w *Window) UpdateDebugFPS() {
	now := uint64(js.Global().Get("performance").Call("now").Float() * 1000) // us
	diff := float32(now - sys.gameFPSprevcount)
	if diff > 0 {
		instantFPS := 1e6 / diff
		sys.gameFPS = (sys.gameFPS * 0.95) + (instantFPS * 0.05)
	}
	sys.gameFPSprevcount = now
}

// pollEvents: keyboard is event-driven (listeners above); gamepads are
// polled because the browser API is poll-based.
func (w *Window) pollEvents() {
	pollGamepads()
}

// GL context creation is handled inside the WebGL renderer (it grabs the
// canvas's webgl2 context in Init); these exist because shared system.go
// calls them for renderers whose name starts with "OpenGL". The WebGL
// renderer's name doesn't, so they are never invoked at runtime.
func (w *Window) GLCreateContext() (js.Value, error) {
	return js.Undefined(), nil
}

func (w *Window) GLMakeCurrent(ctx js.Value) error {
	return nil
}

func (w *Window) shouldClose() bool {
	return w.closeflag
}

func (w *Window) Close() {
}
