//go:build js

package main

import (
	"fmt"
	"image"
	"math"
	"strconv"
	"syscall/js"
)

// Browser window backend: replaces system_sdl.go for wasm builds.
//
// The "window" is a <canvas> (reused if the page provides one with
// id="ikemen-canvas", else created). Frame pacing rides
// requestAnimationFrame: SwapBuffers parks the main goroutine until the
// browser is ready for the next frame, which both paces the game and
// yields to the JS event loop (input events, network callbacks, audio).
//
// Input events: the DOM listeners below only copy each event into a queue
// (and preventDefault what the game owns). pollEvents applies the queue in
// order once per frame, as system_sdl.go drains SDL's event queue, so key,
// mouse, focus and text events reach shared code at the same point of the
// frame on both platforms.

type Window struct {
	canvas     js.Value
	title      string
	w, h       int
	fullscreen bool
	closeflag  bool
	frameCh    chan struct{}
	rafCb      js.Func
	rafPump    js.Value // the JavaScript pacing loop's state (iv = frameIntMs), or undefined
	frameIntMs float64  // target ms between engine frames (from Video.Framerate)
	frameRate  int      // the Video.Framerate frameIntMs was computed from
	nextFrame  float64  // rAF timestamp at which the next frame may be released
	frames     int      // released-frame counter (exposed for perf diagnostics)

	// renderingPaused is shared code's switch for skipping presentation
	// (Android activity pause, Vulkan surface loss). The page never pauses
	// rendering on its own: a hidden tab simply stops firing rAF.
	renderingPaused bool
	// focused tracks the page's real focus (window focus/blur events), for
	// Sound.FocusLossVolume. It must never be a constant false: shared code
	// reads it to decide focus-loss behavior.
	focused bool
	// Canvas backing size, read once per frame in pollEvents (shared code asks
	// for it several times per frame; each read is a JS call).
	drawW, drawH int32

	// DOM events waiting for the next pollEvents, and the spare buffer the
	// two swap with (no allocation per frame).
	events, spare []jsEvent
	wheel         wheelAccum
	cursor        string // canvas.style.cursor as last set

	// The hidden text field for IME and phone keyboards (SetTextInputRect).
	textField   js.Value
	textOpen    bool
	textRect    [4]int32
	textRectAge int

	// Clipboard: the last text pasted into the page or read with
	// navigator.clipboard.readText(), and the paste action waiting for it.
	clipText    string
	clipReading bool
	clipReady   bool
	clipPending string
	clipOK      js.Func
	clipFail    js.Func

	fsLock js.Func // requestFullscreen resolved: lock Esc
	fsFail js.Func // requestFullscreen refused
	noop   js.Func
}

func (w *Window) queue(e jsEvent) {
	w.events = append(w.events, e)
}

// The page's performance object, looked up once: each lookup of a JS object
// is a new reference, a finalizer for the next garbage collection to run.
var jsPerformance = js.Global().Get("performance")

// jsTry runs a DOM call that may throw (a JS exception is a Go panic in
// syscall/js) and reports whether it returned.
func jsTry(f func()) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	f()
	return true
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
		focused: true, // as the SDL window starts; focus/blur below keep it real
		drawW:   int32(w),
		drawH:   int32(h),
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
	// The interval follows Video.Framerate live: modern-input applies video
	// settings without a restart.
	win.setFrameRate(s.cfg.Video.Framerate)
	//
	// The pacing itself runs in JavaScript (startRafPump): calling into Go hands
	// it two new JS references per call (the event and its arguments), each one a
	// finalizer the next garbage collection must run, and on a 240Hz display the
	// Go-side pacing below was called 240 times a second just to re-arm. The
	// finalizers pile up between collections and lengthen each collection's
	// freeze. The Go-side pacing stays as the fallback if the pump can't start.
	win.rafCb = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if win.rafPump.Truthy() {
			// The pump calls only when a frame is due.
			if fr := sys.cfg.Video.Framerate; fr != win.frameRate {
				win.setFrameRate(fr)
				win.rafPump.Set("iv", win.frameIntMs)
			}
			win.frames++
			js.Global().Set("__ikemenEngineFrames", win.frames)
			select {
			case win.frameCh <- struct{}{}:
			default:
			}
			return nil
		}
		now := 0.0
		if len(args) > 0 {
			now = args[0].Float() // DOMHighResTimeStamp, ms
		}
		if fr := sys.cfg.Video.Framerate; fr != win.frameRate {
			win.setFrameRate(fr)
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
	if win.rafPump = startRafPump(win.rafCb, win.frameIntMs); !win.rafPump.Truthy() {
		js.Global().Call("requestAnimationFrame", win.rafCb)
	}

	win.listen(doc)
	return win, nil
}

// rafPumpJS: a requestAnimationFrame loop that calls cb(now) at most once per
// st.iv ms, with the same release rule as the Go-side fallback in newWindow.
const rafPumpJS = `const st = { iv: iv, next: 0 };
const loop = (now) => {
  if (st.next === 0) st.next = now;
  if (now >= st.next) {
    st.next += st.iv;
    if (st.next <= now) st.next = now + st.iv;
    cb(now);
  }
  requestAnimationFrame(loop);
};
requestAnimationFrame(loop);
return st;`

// startRafPump starts the JavaScript pacing loop; undefined if it can't (a page
// whose content security policy forbids new Function keeps the Go-side loop).
func startRafPump(cb js.Func, iv float64) (st js.Value) {
	st = js.Undefined()
	jsTry(func() {
		st = js.Global().Get("Function").New("cb", "iv", rafPumpJS).Invoke(cb, iv)
	})
	return st
}

// setFrameRate sets the rAF release interval from a Video.Framerate value
// (60 when it is not positive).
func (w *Window) setFrameRate(fr int) {
	w.frameRate = fr
	w.frameIntMs = frameIntervalMs(fr)
}

func frameIntervalMs(fr int) float64 {
	if fr <= 0 {
		return 1000.0 / 60.0
	}
	return 1000.0 / float64(fr)
}

// listen registers the DOM listeners. Each one copies its event into the
// queue; the decisions that must be made while the event is live
// (preventDefault, pointer capture) are made here.
func (w *Window) listen(doc js.Value) {
	on := func(target js.Value, name string, opts interface{}, f func(ev js.Value)) {
		fn := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
			f(args[0])
			return nil
		})
		if opts != nil {
			target.Call("addEventListener", name, fn, opts)
		} else {
			target.Call("addEventListener", name, fn)
		}
	}
	w.noop = js.FuncOf(func(this js.Value, args []js.Value) interface{} { return nil })

	// Keyboard: document-level so no element focus is required.
	on(doc, "keydown", nil, func(ev js.Value) {
		code := ev.Get("code").String()
		ctrl, alt, shift, meta := ev.Get("ctrlKey").Bool(), ev.Get("altKey").Bool(), ev.Get("shiftKey").Bool(), ev.Get("metaKey").Bool()
		if sys.textInput.active {
			// An IME is composing: its keys are its own, the result comes
			// back as text (the hidden field's composition events).
			if ev.Get("isComposing").Bool() || ev.Get("keyCode").Int() == 229 {
				return
			}
			// Left to the browser, which answers with a paste event.
			if isPasteShortcut(code, ctrl, shift, meta) {
				return
			}
		}
		key, ok := jsCodeToKey[code]
		if !ok {
			return
		}
		e := jsEvent{kind: jsEvKeyDown, key: key, mod: jsEventModifiers(ev)}
		// A printable character also types into an open text input (name
		// entry, IP entry), as SDL's TEXTINPUT event does on desktop.
		e.text, e.insert = keyText(ev.Get("key").String(), ctrl, alt, meta)
		w.queue(e)
		ev.Call("preventDefault")
	})
	on(doc, "keyup", nil, func(ev js.Value) {
		if key, ok := jsCodeToKey[ev.Get("code").String()]; ok {
			w.queue(jsEvent{kind: jsEvKeyUp, key: key, mod: jsEventModifiers(ev)})
			ev.Call("preventDefault")
		}
	})
	// Ctrl+V, Cmd+V, Shift+Insert while a text input is open.
	on(doc, "paste", nil, func(ev js.Value) {
		if !sys.textInput.active {
			return
		}
		text := ""
		if cd := ev.Get("clipboardData"); cd.Truthy() {
			text = cd.Call("getData", "text").String()
		}
		ev.Call("preventDefault")
		w.clipText = text
		w.queue(jsEvent{kind: jsEvText, text: text})
	})

	// Mouse, touch and pen on the canvas only, so clicks on the page's own
	// panels never reach the game.
	pointer := func(ev js.Value) {
		kind := ev.Get("type").String()
		buttons := ev.Get("buttons").Int()
		if kind == "pointercancel" {
			buttons = 0
		}
		if kind == "pointerdown" {
			// Keep the release when it happens off the canvas.
			id := ev.Get("pointerId")
			jsTry(func() { w.canvas.Call("setPointerCapture", id) })
		}
		w.queue(jsEvent{kind: jsEvPointer, clientX: ev.Get("clientX").Float(), clientY: ev.Get("clientY").Float(),
			buttons: buttons, pointerType: ev.Get("pointerType").String()})
	}
	for _, name := range []string{"pointerdown", "pointermove", "pointerup", "pointercancel"} {
		on(w.canvas, name, nil, pointer)
	}
	// The browser's own uses of these buttons: middle-button autoscroll, the
	// context menu, and back/forward navigation on the side buttons.
	on(w.canvas, "mousedown", nil, func(ev js.Value) {
		if ev.Get("button").Int() == 1 {
			ev.Call("preventDefault")
		}
	})
	sideButtons := func(ev js.Value) {
		if b := ev.Get("button").Int(); b == 3 || b == 4 {
			ev.Call("preventDefault")
		}
	}
	on(w.canvas, "mouseup", nil, sideButtons)
	on(w.canvas, "auxclick", nil, sideButtons)
	on(w.canvas, "contextmenu", nil, func(ev js.Value) { ev.Call("preventDefault") })
	wheelOpts := js.Global().Get("Object").New()
	wheelOpts.Set("passive", false)
	on(w.canvas, "wheel", wheelOpts, func(ev js.Value) {
		// deltaMode first: Firefox then reports a wheel in lines (3 per notch).
		mode := ev.Get("deltaMode").Int()
		e := jsEvent{kind: jsEvWheel, deltaMode: mode, deltaX: ev.Get("deltaX").Float(), deltaY: ev.Get("deltaY").Float(),
			clientX: ev.Get("clientX").Float(), clientY: ev.Get("clientY").Float()}
		// The wheel scrolls the game's menus, not the page, while the game reads the mouse.
		if sys.mouse.enabled() {
			ev.Call("preventDefault")
		}
		w.queue(e)
	})

	// Page focus (window, not the canvas, so a click elsewhere on the page
	// does not count as losing focus), and a hidden tab.
	on(js.Global(), "focus", nil, func(ev js.Value) {
		if ev.Get("target").Equal(js.Global()) {
			w.queue(jsEvent{kind: jsEvFocus, on: true})
		}
	})
	on(js.Global(), "blur", nil, func(ev js.Value) {
		if ev.Get("target").Equal(js.Global()) {
			w.queue(jsEvent{kind: jsEvBlur})
		}
	})
	on(doc, "visibilitychange", nil, func(ev js.Value) {
		if doc.Get("hidden").Bool() {
			w.queue(jsEvent{kind: jsEvBlur})
		} else {
			w.queue(jsEvent{kind: jsEvFocus, on: doc.Call("hasFocus").Bool()})
		}
	})
	on(doc, "fullscreenchange", nil, func(ev js.Value) {
		w.queue(jsEvent{kind: jsEvFullscreen, on: doc.Get("fullscreenElement").Equal(w.canvas)})
	})

	w.clipOK = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		text := ""
		if len(args) > 0 && args[0].Type() == js.TypeString {
			text = args[0].String()
		}
		w.queue(jsEvent{kind: jsEvClipboard, text: text, on: true})
		return nil
	})
	w.clipFail = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		w.queue(jsEvent{kind: jsEvClipboard})
		return nil
	})
	// Esc in fullscreen (Decision 19): with the Keyboard Lock API (Chromium) a
	// press of Esc reaches the game and holding it leaves fullscreen.
	w.fsLock = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		kb := js.Global().Get("navigator").Get("keyboard")
		if kb.Truthy() && kb.Get("lock").Type() == js.TypeFunction {
			keys := js.Global().Get("Array").New("Escape")
			jsTry(func() { kb.Call("lock", keys).Call("catch", w.noop) })
		}
		return nil
	})
	w.fsFail = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		w.queue(jsEvent{kind: jsEvFullscreen, on: false})
		return nil
	})
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

// GetDrawablePixelSize is the canvas backing size in pixels (the drawable;
// CSS scaling is the page's). Cached per frame, see pollEvents.
func (w *Window) GetDrawablePixelSize() (int32, int32) {
	return w.drawW, w.drawH
}

// ResizeWindowed is modern-input's instant window size. In the browser the
// page owns the canvas's on-screen size (its letterboxing), and the canvas
// backing is the game resolution (Decision 21, Option A), so the requested
// window size does not size anything here: the backing follows the game
// resolution, which applyVideoSettings has already set when it calls this.
// The page is asked to fit the canvas again right away instead of on its
// next timer.
func (w *Window) ResizeWindowed(width, height int) {
	gw, gh := int(sys.scrrect[2]), int(sys.scrrect[3])
	if gw <= 0 || gh <= 0 {
		return
	}
	w.w, w.h = gw, gh
	if w.canvas.Get("width").Int() == gw && w.canvas.Get("height").Int() == gh {
		return
	}
	w.canvas.Set("width", gw)
	w.canvas.Set("height", gh)
	w.drawW, w.drawH = int32(gw), int32(gh)
	if fit := js.Global().Get("ikemenFitCanvas"); fit.Type() == js.TypeFunction {
		jsTry(func() { fit.Invoke() })
	}
}

// setBordered: a browser canvas has no window border.
func (w *Window) setBordered(bordered bool) {}

// The canvas's content box on the page, and whether its picture is contained
// (letterboxed) inside it: the browser's fullscreen style does that.
func (w *Window) canvasBox() canvasBox {
	c := w.canvas
	r := c.Call("getBoundingClientRect")
	bl, bt := c.Get("clientLeft").Float(), c.Get("clientTop").Float()
	br := c.Get("offsetWidth").Float() - c.Get("clientWidth").Float() - bl
	bb := c.Get("offsetHeight").Float() - c.Get("clientHeight").Float() - bt
	b := canvasBox{left: r.Get("left").Float() + bl, top: r.Get("top").Float() + bt,
		width: r.Get("width").Float() - bl - br, height: r.Get("height").Float() - bt - bb}
	fit := js.Global().Call("getComputedStyle", c).Get("objectFit").String()
	b.contain = fit == "contain" || fit == "scale-down"
	return b
}

// Text entry hooks for shared input.go (TextInput open/finish): a hidden text
// field takes the focus while the game's text input is open, so an IME and a
// phone's on-screen keyboard have somewhere to type. Physical keys still go
// through the keydown listener; the field only delivers what has no keydown
// (IME commits, phone keyboards, keys the key table does not name).
func platformStartTextInput() {
	if sys.window != nil {
		sys.window.openTextField()
	}
}

func platformStopTextInput() {
	if sys.window != nil {
		sys.window.closeTextField()
	}
}

func (w *Window) openTextField() {
	doc := js.Global().Get("document")
	if w.textField.IsUndefined() {
		f := doc.Call("createElement", "input")
		f.Set("type", "text")
		f.Set("id", "ikemen-textinput")
		for k, v := range map[string]string{"autocomplete": "off", "autocorrect": "off", "autocapitalize": "off",
			"spellcheck": "false", "aria-hidden": "true", "tabindex": "-1"} {
			f.Call("setAttribute", k, v)
		}
		// Invisible, but a real box over the game's text field: an IME places
		// its candidate window next to it. 16px keeps iOS from zooming in.
		st := f.Get("style")
		st.Set("cssText", "position:fixed;left:0;top:0;width:1px;height:1px;opacity:0;border:0;padding:0;margin:0;"+
			"background:transparent;color:transparent;caret-color:transparent;font-size:16px;pointer-events:none;z-index:2147483647;display:none")
		on := func(name string, fn func(ev js.Value)) {
			f.Call("addEventListener", name, js.FuncOf(func(this js.Value, args []js.Value) interface{} {
				fn(args[0])
				return nil
			}))
		}
		on("compositionupdate", func(ev js.Value) {
			w.queue(jsEvent{kind: jsEvComposition, text: jsString(ev.Get("data"))})
		})
		on("compositionend", func(ev js.Value) {
			w.queue(jsEvent{kind: jsEvComposition})
			if text := jsString(ev.Get("data")); text != "" {
				w.queue(jsEvent{kind: jsEvText, text: text})
			}
			f.Set("value", "")
		})
		on("input", func(ev js.Value) {
			if ev.Get("isComposing").Bool() {
				return
			}
			switch ev.Get("inputType").String() {
			case "insertCompositionText": // the commit came with compositionend
			case "deleteContentBackward":
				w.queue(jsEvent{kind: jsEvBackspace})
			default:
				if text := f.Get("value").String(); text != "" {
					w.queue(jsEvent{kind: jsEvText, text: text})
				}
			}
			f.Set("value", "")
		})
		doc.Get("body").Call("appendChild", f)
		w.textField = f
	}
	w.textOpen = true
	w.textRectAge = 1 << 20 // place it on the next SetTextInputRect
	w.textField.Set("value", "")
	w.textField.Get("style").Set("display", "block")
	opts := js.Global().Get("Object").New()
	opts.Set("preventScroll", true)
	jsTry(func() { w.textField.Call("focus", opts) })
}

func (w *Window) closeTextField() {
	w.textOpen = false
	if w.textField.IsUndefined() {
		return
	}
	jsTry(func() { w.textField.Call("blur") })
	w.textField.Set("value", "")
	w.textField.Get("style").Set("display", "none")
}

func jsString(v js.Value) string {
	if v.Type() == js.TypeString {
		return v.String()
	}
	return ""
}

// SetTextInputRect is where the text box is on screen (game draw pixels):
// the hidden text field is laid over it, so an IME's candidate window opens
// next to it. Same math as system_sdl.go, then canvas pixels to page pixels.
func (w *Window) SetTextInputRect(bounds [4]int32) {
	if !w.textOpen || w.textField.IsUndefined() {
		return
	}
	// Called every frame while the keyboard is open; the page layout is
	// re-read once a second, or when the box moves.
	if bounds == w.textRect && w.textRectAge < 60 {
		w.textRectAge++
		return
	}
	w.textRect, w.textRectAge = bounds, 0
	vx, vy, vw, vh := w.GetScaledViewportSize()
	rt := sys.renderTargetRect
	if !(vw > 0 && vh > 0) || rt[2] <= 0 || rt[3] <= 0 {
		return
	}
	b := sys.viewportScissor(bounds)
	sx, sy := float64(vw)/float64(rt[2]), float64(vh)/float64(rt[3])
	x, y := float64(vx)+float64(b[0])*sx, float64(vy)+float64(b[1])*sy
	px, py, cx, cy := canvasToPage(x, y, w.canvasBox(), w.drawW, w.drawH)
	st := w.textField.Get("style")
	st.Set("left", strconv.FormatFloat(px, 'f', 1, 64)+"px")
	st.Set("top", strconv.FormatFloat(py, 'f', 1, 64)+"px")
	st.Set("width", strconv.FormatFloat(max(1, float64(b[2])*sx*cx), 'f', 1, 64)+"px")
	st.Set("height", strconv.FormatFloat(max(1, float64(b[3])*sy*cy), 'f', 1, 64)+"px")
}

// Where the render target appears on the canvas, scaled to fit (system_sdl.go's
// math). The fight and menu aspects are already placed inside the render
// target by the draw viewport (sys.currentDrawViewport), so this only fits the
// whole target: letterboxing here by the current aspect, as 2.4 did, would
// box a 4:3 fight a second time and widen it whenever a menu opens.
// With the canvas backing at the game resolution (Option A) this is the whole
// canvas.
func (w *Window) GetScaledViewportSize() (float32, float32, float32, float32) {
	winWidth, winHeight := w.GetDrawablePixelSize()
	if !sys.cfg.Video.KeepAspect || winWidth <= 0 || winHeight <= 0 {
		return 0, 0, float32(winWidth), float32(winHeight)
	}
	target := sys.renderTargetRect
	view := sys.renderTargetView
	scale := math.Min(float64(winWidth)/view[0], float64(winHeight)/view[1])
	width, height := float64(target[2])*scale, float64(target[3])*scale
	// Keep fractions: rounding would break exact pixel scaling (e.g. 3x at 720p).
	return float32((float64(winWidth) - width) / 2), float32((float64(winHeight) - height) / 2),
		float32(width), float32(height)
}

// The final image is clipped to this area so shaders and fades leave the black
// bars alone (system_sdl.go's version).
func (w *Window) GetPresentationScissor() [4]int32 {
	x, y, width, height := w.GetScaledViewportSize()
	rect := sys.compositionViewport(true)
	target := sys.renderTargetRect
	sx, sy := float64(width)/float64(target[2]), float64(height)/float64(target[3])
	x1 := int32(math.Round(float64(x) + float64(rect[0])*sx))
	y1 := int32(math.Round(float64(y) + float64(rect[1])*sy))
	x2 := int32(math.Round(float64(x) + float64(rect[0]+rect[2])*sx))
	y2 := int32(math.Round(float64(y) + float64(rect[1]+rect[3])*sy))
	dw, dh := w.GetDrawablePixelSize()
	return intersectRect([4]int32{x1, y1, x2 - x1, y2 - y1}, [4]int32{0, 0, dw, dh})
}

// GetClipboardString is the last text the page saw on the clipboard (pasted
// into it, or read for a paste action). navigator.clipboard is async-only,
// so a blocking read is not possible.
func (w *Window) GetClipboardString() string {
	return w.clipText
}

// The text input's paste action (a pad or mouse button, or Insert). The
// clipboard is read asynchronously: the first call starts the read and
// reports not ready; when the text arrives, pollEvents runs the paste action
// again and this call hands it over.
func (w *Window) pasteText() (string, bool) {
	if w.clipReady {
		w.clipReady = false
		return w.clipPending, true
	}
	w.readClipboard()
	return "", false
}

func (w *Window) readClipboard() {
	if w.clipReading {
		return
	}
	w.clipReading = true
	clip := js.Global().Get("navigator").Get("clipboard")
	if !clip.Truthy() || clip.Get("readText").Type() != js.TypeFunction ||
		!jsTry(func() { clip.Call("readText").Call("then", w.clipOK, w.clipFail) }) {
		w.queue(jsEvent{kind: jsEvClipboard})
	}
}

func (w *Window) toggleFullscreen() {
	doc := js.Global().Get("document")
	if doc.Get("fullscreenElement").Truthy() {
		w.unlockEscape()
		jsTry(func() {
			if p := doc.Call("exitFullscreen"); p.Type() == js.TypeObject {
				p.Call("catch", w.noop)
			}
		})
		w.fullscreen = false
	} else if w.canvas.Get("requestFullscreen").Truthy() {
		w.fullscreen = true
		ok := jsTry(func() {
			if p := w.canvas.Call("requestFullscreen"); p.Type() == js.TypeObject {
				p.Call("then", w.fsLock, w.fsFail)
			}
		})
		if !ok {
			w.fullscreen = false
		}
	}
}

func (w *Window) unlockEscape() {
	kb := js.Global().Get("navigator").Get("keyboard")
	if kb.Truthy() && kb.Get("unlock").Type() == js.TypeFunction {
		jsTry(func() { kb.Call("unlock") })
	}
}

func (w *Window) UpdateDebugFPS() {
	now := uint64(jsPerformance.Call("now").Float() * 1000) // us
	diff := float32(now - sys.gameFPSprevcount)
	if diff > 0 {
		instantFPS := 1e6 / diff
		sys.gameFPS = (sys.gameFPS * 0.95) + (instantFPS * 0.05)
	}
	sys.gameFPSprevcount = now
}

// pollEvents is the browser's event pump, in system_sdl.go's order: the DOM
// events queued since the last call, then the gamepads (the browser API is
// poll-based), then the end-of-pump device steps.
func (w *Window) pollEvents() {
	w.drawW, w.drawH = int32(w.canvas.Get("width").Int()), int32(w.canvas.Get("height").Int())
	if len(w.events) > 0 {
		evs := w.events
		w.events = w.spare[:0]
		w.apply(evs)
		w.spare = evs[:0]
	}
	pollGamepads()
	// End of the event pump, as system_sdl.go: menu press/hold state of every
	// device, and the text input's closing-press guard.
	sys.pollDevices()
	sys.textInput.releaseInputs()
	// Fullscreen menus need a pointer too. Gameplay keeps its hidden cursor.
	cursor := ""
	if w.fullscreen && !(sys.mouse.enabled() &&
		(!sys.gameRunning || sys.motifOverlayActive() || sys.postMatchFlg || sys.textInput.active)) {
		cursor = "none"
	}
	if cursor != w.cursor {
		w.cursor = cursor
		w.canvas.Get("style").Set("cursor", cursor)
	}
}

func (w *Window) apply(evs []jsEvent) {
	var box canvasBox
	haveBox := false
	for i := range evs {
		e := &evs[i]
		switch e.kind {
		case jsEvKeyDown:
			OnKeyPressed(e.key, e.mod)
			// The key press above already ran the editing actions; Enter may
			// have closed the input.
			if e.insert && sys.textInput.active {
				sys.textInput.keyboard = true
				sys.textInput.composition = ""
				sys.textInput.insert(e.text)
			}
		case jsEvKeyUp:
			OnKeyReleased(e.key, e.mod)
		case jsEvPointer, jsEvWheel:
			if !haveBox {
				box, haveBox = w.canvasBox(), true
			}
			x, y := canvasPoint(e.clientX, e.clientY, box, w.drawW, w.drawH)
			if e.kind == jsEvWheel {
				sys.mouse.x, sys.mouse.y = x, y
				dx, dy := w.wheel.add(e.deltaX, e.deltaY, e.deltaMode)
				if dx != 0 || dy != 0 {
					sys.mouse.active = true
				}
				sys.mouse.wheel[0] += dx
				sys.mouse.wheel[1] += dy
				continue
			}
			if x != sys.mouse.x || y != sys.mouse.y {
				sys.mouse.active, sys.mouse.motion = true, true
			}
			sys.mouse.x, sys.mouse.y = x, y
			down := pointerButtons(e.buttons, e.pointerType)
			for b := range down {
				if down[b] && !sys.mouse.down[b] {
					sys.mouse.clicked[b] = true
					sys.mouse.active = true
				}
				sys.mouse.down[b] = down[b]
			}
		case jsEvFocus:
			if e.on {
				w.focused = true
			}
		case jsEvBlur:
			w.loseFocus()
		case jsEvText:
			if sys.textInput.active {
				sys.textInput.keyboard = true
				sys.textInput.composition = ""
				sys.textInput.insert(e.text)
			}
		case jsEvComposition:
			if sys.textInput.active {
				sys.textInput.keyboard = true
				sys.textInput.composition = e.text
			}
		case jsEvBackspace:
			if sys.textInput.active && sys.textInput.composition == "" {
				sys.textInput.keyboard = true
				sys.textInput.action("backspace", false)
			}
		case jsEvClipboard:
			w.clipReading = false
			text := ""
			if e.on {
				w.clipText, text = e.text, e.text
			}
			// The paste action that started the read gets its text now (an
			// empty text, when the read was refused, plays the failure sound).
			if sys.textInput.active {
				w.clipReady, w.clipPending = true, text
				sys.textInput.action("paste", !sys.textInput.keyboard)
				w.clipReady, w.clipPending = false, ""
			}
		case jsEvFullscreen:
			w.fullscreen = e.on
			if !e.on {
				w.unlockEscape()
			}
		}
	}
}

// loseFocus: the page lost focus (another window, another tab). As
// system_sdl.go on FOCUS_LOST, and like SDL's own keyboard reset: every held
// key and button is released, because their release events go to whatever
// has the focus now. A lost Esc release would otherwise keep every frame a
// menu frame (and, online, the player's character dead for the rest of the
// match on both peers).
func (w *Window) loseFocus() {
	w.focused = false
	sys.netMenuEscapeHeld = false
	sys.mouse.release()
	w.wheel.reset()
	var held []Key
	for key, down := range sys.keyState {
		if down {
			held = append(held, key)
		}
	}
	for _, key := range held {
		OnKeyReleased(key, KMOD_NONE)
	}
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

// Focused reports whether the page has focus (window focus/blur).
func (w *Window) Focused() bool {
	return w.focused
}

func (w *Window) shouldClose() bool {
	return w.closeflag
}

func (w *Window) Close() {
}
