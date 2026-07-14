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

// TTF font loading stub - no FreeType on wasm.
func LoadFntTtf(f *Fnt, fontfile string, filename string, height int32) {
	panic(Error("TrueType fonts are not supported on this platform"))
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
