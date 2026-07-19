//go:build js

package main

// GL command buffer: batches the browser build's per-frame WebGL commands into
// a shared ArrayBuffer that a JS-side interpreter drains in ONE syscall/js
// crossing per flush, instead of one crossing per GL command.
//
// Why: profiling the shipped (v0.9) build showed hundreds of `gl.Call`
// crossings per frame; each crossing boxes its arguments through
// syscall/js.makeValue (~450MB/30s of JS garbage) and pays the wasm->JS context
// switch. The GL command *stream* itself was already minimal (state caches and
// uniform dedup live on the Go side) - the cost was the per-command crossing.
//
// How:
//   - Go encodes opcodes+args into a []byte staging buffer (zero allocations),
//     then flushes: one CopyBytesToJS into a shared ArrayBuffer + one Invoke of
//     the interpreter. The interpreter (built once at Init via the Function
//     constructor - no boot-page changes, so dist/kit pick it up automatically)
//     walks Int32Array/Float32Array views over that buffer and issues the real
//     GL calls.
//   - JS-held objects (textures, programs, uniform locations, VAOs, buffers)
//     are registered into a JS-side table at creation time (cold path) and
//     referenced by integer handle in the command stream.
//   - ORDERING INVARIANT: every GL call that does NOT go through the command
//     buffer must flush it first, so the true GL command order always matches
//     program order. r.glc() enforces this for direct calls; bindAndPrepare
//     covers the texture-upload paths.
//
// This changes how commands cross the boundary, never which commands are
// issued: the interpreter performs the same GL calls in the same order with
// the same arguments, so rendered output is identical by construction.

import (
	"encoding/binary"
	"math"
	"syscall/js"
)

// Opcodes. Must match the switch in wglInterpSrc.
const (
	wcUseProgram  = 1  // progID
	wcUniform1i   = 2  // locID, int
	wcUniform1f   = 3  // locID, f
	wcUniform2f   = 4  // locID, f, f
	wcUniform3f   = 5  // locID, f, f, f
	wcUniform4f   = 6  // locID, f, f, f, f
	wcUniformMat4 = 7  // locID, 16 f
	wcActiveBind  = 8  // unit, texID     (activeTexture(TEXTURE0+unit) + bindTexture)
	wcVertexData  = 9  // bufID, n, n f   (bindBuffer + bufferData STATIC_DRAW)
	wcDrawArrays  = 10 // mode, first, count
	wcEnable      = 11 // cap
	wcDisable     = 12 // cap
	wcBlendEq     = 13 // eq
	wcBlendFunc   = 14 // src, dst
	wcScissor     = 15 // x, y, w, h
	wcDepthMask   = 16 // 0/1
	wcDepthFunc   = 17 // func
	wcBindVAO     = 18 // vaoID
	wcSubData     = 19 // bufID, n, n f   (bindBuffer + bufferSubData at offset 0)
	wcTexParam    = 20 // pname, value    (on TEXTURE_2D of the bound texture)
	wcActiveTex   = 21 // unit
	wcUniform4fv8 = 22 // locID, 8 f      (uniform4fv with two vec4s)
)

// The interpreter. Wrapped by the Function constructor at Init:
//
//	new Function("gl", "buffer", "T", wglInterpSrc) -> factory
//	run = factory(gl, sharedArrayBuffer, tableArray)
//
// Argument evaluation order in JS is left-to-right, so `T[i32[p++]], f32[p++]`
// sequences read the stream in encoding order.
const wglInterpSrc = `
var i32 = new Int32Array(buffer);
var f32 = new Float32Array(buffer);
return function (n) {
	var p = 0;
	while (p < n) {
		switch (i32[p++]) {
		case 1: gl.useProgram(T[i32[p++]]); break;
		case 2: gl.uniform1i(T[i32[p++]], i32[p++]); break;
		case 3: gl.uniform1f(T[i32[p++]], f32[p++]); break;
		case 4: gl.uniform2f(T[i32[p++]], f32[p++], f32[p++]); break;
		case 5: gl.uniform3f(T[i32[p++]], f32[p++], f32[p++], f32[p++]); break;
		case 6: gl.uniform4f(T[i32[p++]], f32[p++], f32[p++], f32[p++], f32[p++]); break;
		case 7: gl.uniformMatrix4fv(T[i32[p++]], false, f32, p, 16); p += 16; break;
		case 8: gl.activeTexture(0x84C0 + i32[p++]); gl.bindTexture(0x0DE1, T[i32[p++]]); break;
		case 9: {
			var b = i32[p++], n9 = i32[p++];
			gl.bindBuffer(0x8892, T[b]);
			gl.bufferData(0x8892, f32, 0x88E4, p, n9);
			p += n9;
			break;
		}
		case 10: gl.drawArrays(i32[p++], i32[p++], i32[p++]); break;
		case 11: gl.enable(i32[p++]); break;
		case 12: gl.disable(i32[p++]); break;
		case 13: gl.blendEquation(i32[p++]); break;
		case 14: gl.blendFunc(i32[p++], i32[p++]); break;
		case 15: gl.scissor(i32[p++], i32[p++], i32[p++], i32[p++]); break;
		case 16: gl.depthMask(i32[p++] !== 0); break;
		case 17: gl.depthFunc(i32[p++]); break;
		case 18: gl.bindVertexArray(T[i32[p++]]); break;
		case 19: {
			var b2 = i32[p++], n19 = i32[p++];
			gl.bindBuffer(0x8892, T[b2]);
			gl.bufferSubData(0x8892, 0, f32, p, n19);
			p += n19;
			break;
		}
		case 20: gl.texParameteri(0x0DE1, i32[p++], i32[p++]); break;
		case 21: gl.activeTexture(0x84C0 + i32[p++]); break;
		case 22: gl.uniform4fv(T[i32[p++]], f32, p, 8); p += 8; break;
		default: throw new Error("wglCmdBuf: bad opcode at " + (p - 1));
		}
	}
};
`

const wglCmdBufBytes = 1 << 18 // 256KB = 64K words; a heavy frame fits in one flush

type wglCmdBuf struct {
	stage  []byte   // Go-side staging (little-endian 4-byte words)
	pos    int      // bytes used
	jsU8   js.Value // Uint8Array over the shared ArrayBuffer (CopyBytesToJS dst)
	runFn  js.Value // the interpreter
	tables js.Value // JS array of registered GL objects
	nextID int      // next table slot
}

func (r *Renderer_WebGL) initCmdBuf() {
	cb := &r.cmd
	cb.stage = make([]byte, wglCmdBufBytes)
	buf := js.Global().Get("ArrayBuffer").New(wglCmdBufBytes)
	cb.jsU8 = js.Global().Get("Uint8Array").New(buf)
	cb.tables = js.Global().Get("Array").New()
	factory := js.Global().Get("Function").New("gl", "buffer", "T", wglInterpSrc)
	cb.runFn = factory.Invoke(r.gl, buf, cb.tables)
}

// regJS registers a JS-held GL object into the interpreter's table and returns
// its integer handle. Cold path only (object creation).
func (r *Renderer_WebGL) regJS(v js.Value) int32 {
	id := r.cmd.nextID
	r.cmd.nextID++
	r.cmd.tables.SetIndex(id, v)
	return int32(id)
}

// regLoc registers a uniform location, or returns -1 for an invalid one
// (mirrors the !loc.Truthy() guards in the uniform setters).
func (r *Renderer_WebGL) regLoc(loc js.Value) int32 {
	if !loc.Truthy() {
		return -1
	}
	return r.regJS(loc)
}

func (r *Renderer_WebGL) flushCmds() {
	if r.cmd.pos == 0 {
		return
	}
	js.CopyBytesToJS(r.cmd.jsU8, r.cmd.stage[:r.cmd.pos])
	r.cmd.runFn.Invoke(r.cmd.pos / 4)
	r.cmd.pos = 0
}

// glc is the direct-call escape hatch: flush pending commands first so the GL
// stream stays in program order, then issue the call normally. Every gl.Call
// outside the emitters below MUST go through this (or through a path that has
// already flushed, like bindAndPrepare).
func (r *Renderer_WebGL) glc(name string, args ...interface{}) js.Value {
	r.flushCmds()
	return r.gl.Call(name, args...)
}

// need ensures capacity for n more words, flushing if the buffer is near full.
func (r *Renderer_WebGL) need(nWords int) {
	if r.cmd.pos+nWords*4 > wglCmdBufBytes {
		r.flushCmds()
	}
}

func (r *Renderer_WebGL) emitI(v int32) {
	binary.LittleEndian.PutUint32(r.cmd.stage[r.cmd.pos:], uint32(v))
	r.cmd.pos += 4
}

func (r *Renderer_WebGL) emitF(v float32) {
	binary.LittleEndian.PutUint32(r.cmd.stage[r.cmd.pos:], math.Float32bits(v))
	r.cmd.pos += 4
}
