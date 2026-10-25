# Ikemen GO — WebAssembly / browser fork

This is a fork of [Ikemen GO](https://github.com/ikemen-engine/Ikemen-GO) that
runs in a web browser. It is based on Ikemen GO v1.0.0. The engine compiles to
WebAssembly and renders through WebGL 2; no plugins, no server-side game logic.
Upstream's own `README.md` still describes the engine itself — this file covers
only what the fork adds.

The content pipeline is **Mugen Studio**, a free Windows app that builds a
roster and exports it as a website running this engine (or as a desktop
Ikemen GO build). It's on [itch.io](https://energyjp.itch.io/energy-webmugen),
along with Energy's WebMUGEN, the game it makes. This repo is just the engine.

## Building

```sh
bash tools/build-engine.sh
```

That runs

```sh
GOEXPERIMENT=arenas GOOS=js GOARCH=wasm CGO_ENABLED=0 \
  go build -trimpath -buildvcs=false -o bin/ikemen-v2.wasm ./src
```

and writes a build stamp beside the engine, `bin/ikemen-v2.wasm.build.txt`: the
engine's SHA-256, the exact command, the Go version, the commit, and a hash of
every Go source that went in. The same source and Go version rebuild the same
engine byte for byte, so the stamp answers "was this engine built from this
tree?" for any engine you are handed.

Serve `bin/ikemen-v2.wasm` alongside Go's `wasm_exec.js` and a page that mounts
a virtual filesystem for the engine to read. Mugen Studio's Website export does
all of that for you.

## What the fork changes

**A WebGL 2 renderer** (`src/render_webgl.go`) replacing the GL33 path: the same
2D sprite and post-process pipeline, ported to the browser's GL bindings.

**3D model stages** (`src/render_webgl_model.go`). Stages built around a glTF or
GLB model draw in the browser. Model shadows stay off. Textures larger than the
GPU's `MAX_TEXTURE_SIZE` are scaled to fit and uploaded in bands, decoded
textures are premultiplied in place rather than copied, the per-draw uniform and
morph-target paths do not allocate, and a skin's joint matrices are uploaded only
when they change. A stage whose model or environment fails to load is played
without it instead of stopping the game.

**A GL command buffer** (`src/render_webgl_cmdbuf.go`). WebAssembly cannot call
WebGL directly; every call crosses into JavaScript through `syscall/js`, and
that crossing boxes its arguments. Issuing hundreds of GL commands per frame
that way is the single biggest cost in a browser build. Commands are instead
encoded into a shared `ArrayBuffer` and replayed by a small JS interpreter in
**one crossing per frame**, the model pipeline included. Ordering is preserved:
any direct GL call flushes the buffer first.

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
- Stages: `[Music]` is read from its own lines and never fails a stage; a
  `[BGCtrl]` above every `[BGCtrlDef]` controls the whole background, as MUGEN
  reads it; a `[Shadow]` color applies only to stages made for MUGEN before 1.1;
  a model stage without its model keeps its ground line.
- Round fades are pinned so both netplay peers agree on them
  (`src/roundfade_sync_test.go`).

## Known constraints

Go's `js/wasm` target is **single-threaded**. GC pauses freeze the whole game
briefly and scale with loaded content; the optimizations above make them rare,
not absent. `SharedArrayBuffer` does not help — Go's browser target cannot use
OS threads at all. Rollback is the default netcode in Energy's WebMUGEN; a GC
pause still forces a deeper catch-up rollback, so Delay stays available as the
fallback for weak machines and choppy connections.

## License

MIT, same as upstream — see `LICENCE.txt`.
