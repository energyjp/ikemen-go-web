//go:build js

package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/pprof"
	"syscall/js"
	"time"
)

// Browser build utilities: replaces util_desktop.go for wasm builds.

// Log writer implementation
type JsLogWriter struct {
	console_log js.Value
}

func (l JsLogWriter) Write(p []byte) (n int, err error) {
	l.console_log.Invoke(string(p))
	return len(p), nil
}

func NewLogWriter() io.Writer {
	return JsLogWriter{js.Global().Get("console").Get("log")}
}

// Message box implementation using basic JavaScript alert()
var alert = js.Global().Get("alert")

func ShowInfoDialog(message, title string) {
	alert.Invoke(title + "\n\n" + message)
}

func ShowErrorDialog(message string) {
	alert.Invoke("I.K.E.M.E.N Error\n\n" + message)
}

// TTF font loading for the browser build.
//
// Fonts are read from the wasm virtual filesystem only (there is no system
// font directory to fall back to, unlike the desktop findfont path). If the
// file is missing or fails to parse we leave the font unloaded and keep going:
// that costs only the text drawn with this font, which keeps a screenpack the
// player can still use rather than taking the whole engine down.
func LoadFntTtf(f *Fnt, fontfile string, filename string, height int32) {
	fileDir := SearchFile(filename, []string{fontfile, sys.motif.Def, "", "data/"}, "font/")
	if len(FileExist(fileDir)) == 0 {
		LogMessage("WARNING: TrueType font not found, skipping %v", filename)
		return
	}
	if height == -1 {
		height = int32(f.Size[1])
	} else {
		f.Size[1] = uint16(height)
	}
	ttf, err := gfxFont.LoadFont(fileDir, height, int(sys.gameWidth), int(sys.gameHeight))
	if err != nil {
		LogMessage("WARNING: failed to load ttf font %v: %v", fileDir, err)
		return
	}
	f.ttf = ttf.(Font)

	// Create Ttf dummy palettes
	f.palettes = make([][256]uint32, 1)
	for i := 0; i < 256; i++ {
		f.palettes[0][i] = 0
	}
}

// The browser has exactly one viable backend.
func selectRenderer(cfgVal string) (Renderer, FontRenderer) {
	return &Renderer_WebGL{}, &FontRenderer_WebGL{}
}

func Logcat(s string) {
	fmt.Println(s)
}

// Heap profiling support: boot page sets IKEMEN_PROFILE via ?profile=1; a
// pprof heap profile is written to the virtual filesystem every 30s where
// the boot page's debug hook can read it out.
func init() {
	if os.Getenv("IKEMEN_PROFILE") == "" {
		// Disable heap-profile allocation sampling in normal play. Go's default
		// MemProfileRate (512 KiB) makes the allocator invoke profilealloc,
		// which unwinds the wasm call stack on every sampled allocation - a
		// fragile operation on js/wasm that appeared in a mid-round-load runtime
		// corruption crash ("g 0: unexpected return pc for runtime.fillAligned",
		// stack profilealloc -> mallocgc -> beforeIdle). We never read these
		// samples unless ?profile=1, so switch the sampling off entirely; this
		// also shaves per-allocation overhead. ?profile=1 keeps it on below.
		runtime.MemProfileRate = 0
		return
	}
	go func() {
		time.Sleep(60 * time.Second)
		for {
			f, err := os.Create("save/logs/heap.pprof")
			if err == nil {
				pprof.WriteHeapProfile(f)
				f.Close()
			}
			time.Sleep(30 * time.Second)
		}
	}()
}
