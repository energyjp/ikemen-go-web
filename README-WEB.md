# Ikemen GO — WebAssembly / browser fork

This is a fork of [Ikemen GO](https://github.com/ikemen-engine/Ikemen-GO) that
runs in a web browser. The engine compiles to WebAssembly and renders through
WebGL 2; no plugins, no server-side game logic. Upstream's own `README.md` still
describes the engine itself — this file covers only what the fork adds.

The companion repository, **webmugen-tools**, is the content pipeline: a
browser-based roster builder, an exporter, and repair tooling. This repo is just
the engine.

## Building

```sh
GOEXPERIMENT=arenas GOOS=js GOARCH=wasm CGO_ENABLED=0 \
  go build -trimpath -o bin/ikemen-v2.wasm ./src
```

Serve `bin/ikemen-v2.wasm` alongside Go's `wasm_exec.js` and a page that mounts
a virtual filesystem for the engine to read. webmugen-tools does all of that
for you.

## What the fork changes

**A WebGL 2 renderer** (`src/render_webgl.go`) replacing the GL33 path: the same
2D sprite and post-process pipeline, ported to the browser's GL bindings. The 3D
model and shadow pipelines are stubbed out — glTF stages won't render their
models; everything else is unaffected.

**A GL command buffer** (`src/render_webgl_cmdbuf.go`). WebAssembly cannot call
WebGL directly; every call crosses into JavaScript through `syscall/js`, and
that crossing boxes its arguments. Issuing hundreds of GL commands per frame
that way is the single biggest cost in a browser build. Commands are instead
encoded into a shared `ArrayBuffer` and replayed by a small JS interpreter in
**one crossing per frame**. Ordering is preserved: any direct GL call flushes
the buffer first.

**A frame cap.** `requestAnimationFrame` fires at the display's refresh rate, so
on a 144 or 240 Hz monitor the engine was running its whole loop 2–4× more often
than the 60 the game is authored for. The pump now releases at most one frame
per `Video.Framerate` interval.

**Allocation-free hot paths.** The sprite and text draw paths reuse buffers
rather than allocating per sprite/glyph — on a single-threaded runtime, garbage
directly becomes GC pauses. Text preprocessing is cached, uniform writes are
deduplicated across all shader programs.

**Browser I/O.** Networking is transport-agnostic: `src/netconn_js.go` tunnels
`NetInput` through a page-provided WebRTC bridge (`globalThis.ikemenNet`) so
netplay is peer-to-peer with no game server. Audio, input (keyboard + Gamepad
API) and the filesystem are likewise bridged to browser APIs.

**Engine fixes** that matter beyond the browser:

- `forceRemapPal` no longer repaints palettes outside the selectable family, so
  characters carrying private palettes for overlay art keep their authored
  colors when a palette is selected.
- Character sound volume is capped at full scale, so content that requests
  amplitudes above 100% no longer clips into distortion.
- `?paldebug=1` (env `IKEMEN_PALDEBUG`) logs how each sprite resolves its
  palette — the fastest way to diagnose a palette bug in real content.

## Known constraints

Go's `js/wasm` target is **single-threaded**. GC pauses freeze the whole game
briefly and scale with loaded content; the optimizations above make them rare,
not absent. `SharedArrayBuffer` does not help — Go's browser target cannot use
OS threads at all. Rollback netcode is present but experimental for the same
reason: a GC pause forces a deep catch-up rollback, which can desync. Delay
netcode is the default.

## License

MIT, same as upstream — see `LICENCE.txt`.
