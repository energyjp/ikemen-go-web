//go:build js

package main

import (
	"io"
	"os"
	"runtime/pprof"
	"strings"
	"syscall/js"
	"time"
)

// Diagnostic kill-switch: load the page with ?nodedup=1 to disable the
// uniform de-duplication optimization when hunting rendering bugs.
func init() {
	search := js.Global().Get("location").Get("search").String()
	if strings.Contains(search, "nodedup=1") {
		uniformDedupDisabled = true
	}
}

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

// TTF font loading stub
func LoadFntTtf(f *Fnt, fontfile string, filename string, height int32) {
	panic(Error("TrueType fonts are not supported on this platform"))
}

// Heap profiling support for the browser build: when enabled (boot page
// sets IKEMEN_PROFILE via ?profile=1), a pprof heap profile is written to
// the virtual filesystem every 30 seconds, where the boot page's debug
// hook can read it out for analysis with `go tool pprof`.
func init() {
	if os.Getenv("IKEMEN_PROFILE") == "" {
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
