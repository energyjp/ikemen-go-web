//go:build js

package main

import (
	"fmt"
	"math"
	"strings"
	"syscall/js"

	mgl "github.com/go-gl/mathgl/mgl32"
)

// WebGL2 renderer for the browser build: a port of render_gl33.go's 2D
// sprite/postprocess pipeline onto WebGL2 via syscall/js.
//
// Deliberate differences from the GL33 renderer:
//   - 3D model/shadow pipelines are disabled (IsModelEnabled/IsShadowEnabled
//     return false; the engine skips those passes). glTF stages won't render
//     their models, everything else is unaffected.
//   - No MSAA (WebGL2 multisampled renderbuffers exist but the ping-pong
//     postprocess path doesn't need them; sys.msaa is forced to 0).
//   - Uniform locations are JS objects, not ints, so programs index them
//     through per-program tables.
//
// Marshalling: WebGL cannot read Go/wasm memory, so uploads go through
// reusable scratch typed arrays (grown on demand, copied per call).

const webglDebugName = "WebGL 2"

// ---------------------------------------------------------------------------
// scratch typed-array marshalling

type wglScratch struct {
	buf js.Value // ArrayBuffer
	u8  js.Value // Uint8Array view
	f32 js.Value // Float32Array view
	cap int      // bytes
}

func (s *wglScratch) ensure(nBytes int) {
	if s.cap >= nBytes {
		return
	}
	n := s.cap * 2
	if n < nBytes {
		n = nBytes
	}
	if n < 1<<16 {
		n = 1 << 16
	}
	s.buf = js.Global().Get("ArrayBuffer").New(n)
	s.u8 = js.Global().Get("Uint8Array").New(s.buf)
	s.f32 = js.Global().Get("Float32Array").New(s.buf)
	s.cap = n
}

// bytes returns a Uint8Array view holding data.
func (s *wglScratch) bytes(data []byte) js.Value {
	s.ensure(len(data))
	dst := s.u8.Call("subarray", 0, len(data))
	js.CopyBytesToJS(dst, data)
	return dst
}

// floats returns a Float32Array view holding values.
func (s *wglScratch) floats(values []float32) js.Value {
	s.ensure(len(values) * 4)
	b := f32SliceAsByteSlice(values)
	js.CopyBytesToJS(s.u8.Call("subarray", 0, len(b)), b)
	return s.f32.Call("subarray", 0, len(values))
}

// f32SliceAsByteSlice encodes []float32 as little-endian bytes into a
// reusable Go-side buffer (wasm is little-endian).
func f32SliceAsByteSlice(values []float32) []byte {
	n := len(values) * 4
	if cap(wglGoScratch) < n {
		wglGoScratch = make([]byte, n*2)
	}
	b := wglGoScratch[:n]
	for i, v := range values {
		bits := math.Float32bits(v)
		b[i*4] = byte(bits)
		b[i*4+1] = byte(bits >> 8)
		b[i*4+2] = byte(bits >> 16)
		b[i*4+3] = byte(bits >> 24)
	}
	return b
}

var wglGoScratch []byte

// ---------------------------------------------------------------------------
// shader programs

type ShaderProgram_WebGL struct {
	program       js.Value
	name          string
	attributes    map[string]int
	uniformIdx    map[string]int
	uniformLocs   []js.Value
	textures      map[string]int
	needsGrabPass bool
	id            uint32
}

func (s *ShaderProgram_WebGL) uniformIndex(name string) int {
	if idx, ok := s.uniformIdx[name]; ok {
		return idx
	}
	return -1
}

// ---------------------------------------------------------------------------
// textures

type Texture_WebGL struct {
	width  int32
	height int32
	depth  int32
	filter bool
	handle js.Value
	serial uint64
}

func (t *Texture_WebGL) mapFormat(i int32) (internal, format js.Value, ok bool) {
	r := gfx.(*Renderer_WebGL)
	switch i {
	case 8:
		return r.c("R8"), r.c("RED"), true
	case 24:
		return r.c("RGB"), r.c("RGB"), true
	case 32:
		return r.c("RGBA"), r.c("RGBA"), true
	case 96:
		return r.c("RGB32F"), r.c("RGB"), true
	case 128:
		return r.c("RGBA32F"), r.c("RGBA"), true
	}
	return js.Undefined(), js.Undefined(), false
}

func (t *Texture_WebGL) samplingParam(i TextureSamplingParam) js.Value {
	r := gfx.(*Renderer_WebGL)
	switch i {
	case TextureSamplingFilterNearest:
		return r.c("NEAREST")
	case TextureSamplingFilterLinear:
		return r.c("LINEAR")
	case TextureSamplingFilterNearestMipMapNearest:
		return r.c("NEAREST_MIPMAP_NEAREST")
	case TextureSamplingFilterLinearMipMapNearest:
		return r.c("LINEAR_MIPMAP_NEAREST")
	case TextureSamplingFilterNearestMipMapLinear:
		return r.c("NEAREST_MIPMAP_LINEAR")
	case TextureSamplingFilterLinearMipMapLinear:
		return r.c("LINEAR_MIPMAP_LINEAR")
	case TextureSamplingWrapClampToEdge:
		return r.c("CLAMP_TO_EDGE")
	case TextureSamplingWrapMirroredRepeat:
		return r.c("MIRRORED_REPEAT")
	case TextureSamplingWrapRepeat:
		return r.c("REPEAT")
	}
	return r.c("NEAREST")
}

func (t *Texture_WebGL) bindAndPrepare() js.Value {
	r := gfx.(*Renderer_WebGL)
	r.SetActiveTexture0()
	r.gl.Call("bindTexture", r.c("TEXTURE_2D"), t.handle)
	r.gl.Call("pixelStorei", r.c("UNPACK_ALIGNMENT"), 1)
	r.gl.Call("pixelStorei", r.c("UNPACK_ROW_LENGTH"), 0)
	return r.gl
}

func (t *Texture_WebGL) SetData(data []byte) {
	r := gfx.(*Renderer_WebGL)
	gl := t.bindAndPrepare()
	internal, format, _ := t.mapFormat(Max(t.depth, 8))
	if data != nil {
		gl.Call("texImage2D", r.c("TEXTURE_2D"), 0, internal, t.width, t.height, 0,
			format, r.c("UNSIGNED_BYTE"), r.scratch.bytes(data))
	} else {
		gl.Call("texImage2D", r.c("TEXTURE_2D"), 0, internal, t.width, t.height, 0,
			format, r.c("UNSIGNED_BYTE"), js.Null())
	}
	interp := r.c("NEAREST")
	if t.filter {
		interp = r.c("LINEAR")
	}
	gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MAG_FILTER"), interp)
	gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MIN_FILTER"), interp)
	gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_S"), r.c("CLAMP_TO_EDGE"))
	gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_T"), r.c("CLAMP_TO_EDGE"))
}

func (t *Texture_WebGL) SetSubData(data []byte, x, y, width, height, stride int32) {
	r := gfx.(*Renderer_WebGL)
	gl := t.bindAndPrepare()
	_, format, _ := t.mapFormat(Max(t.depth, 8))
	bytesPerPixel := t.depth / 8
	if bytesPerPixel < 1 {
		bytesPerPixel = 1
	}
	if stride > 0 && stride != width*bytesPerPixel {
		gl.Call("pixelStorei", r.c("UNPACK_ROW_LENGTH"), stride/bytesPerPixel)
	}
	if data != nil {
		gl.Call("texSubImage2D", r.c("TEXTURE_2D"), 0, x, y, width, height,
			format, r.c("UNSIGNED_BYTE"), r.scratch.bytes(data))
	}
	gl.Call("pixelStorei", r.c("UNPACK_ROW_LENGTH"), 0)
	interp := r.c("NEAREST")
	if t.filter {
		interp = r.c("LINEAR")
	}
	gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MAG_FILTER"), interp)
	gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MIN_FILTER"), interp)
	gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_S"), r.c("CLAMP_TO_EDGE"))
	gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_T"), r.c("CLAMP_TO_EDGE"))
}

func (t *Texture_WebGL) SetDataG(data []byte, mag, min, ws, wt TextureSamplingParam) {
	r := gfx.(*Renderer_WebGL)
	gl := t.bindAndPrepare()
	internal, format, _ := t.mapFormat(Max(t.depth, 8))
	gl.Call("texImage2D", r.c("TEXTURE_2D"), 0, internal, t.width, t.height, 0,
		format, r.c("UNSIGNED_BYTE"), r.scratch.bytes(data))
	gl.Call("generateMipmap", r.c("TEXTURE_2D"))
	gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MAG_FILTER"), t.samplingParam(mag))
	gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MIN_FILTER"), t.samplingParam(min))
	gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_S"), t.samplingParam(ws))
	gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_T"), t.samplingParam(wt))
}

func (t *Texture_WebGL) SetPixelData(data []float32) {
	r := gfx.(*Renderer_WebGL)
	gl := t.bindAndPrepare()
	internal, _, _ := t.mapFormat(Max(t.depth, 8))
	_, format, _ := t.mapFormat(Max(t.depth/4, 8))
	gl.Call("texImage2D", r.c("TEXTURE_2D"), 0, internal, t.width, t.height, 0,
		format, r.c("FLOAT"), r.scratch.floats(data))
}

func (t *Texture_WebGL) CopyData(src *Texture) {}

func (t *Texture_WebGL) IsValid() bool {
	return t.width != 0 && t.height != 0 && t.handle.Truthy()
}

func (t *Texture_WebGL) GetWidth() int32  { return t.width }
func (t *Texture_WebGL) GetHeight() int32 { return t.height }

// ---------------------------------------------------------------------------
// renderer

type Renderer_WebGL struct {
	gl      js.Value // WebGL2RenderingContext
	consts  map[string]js.Value
	scratch wglScratch

	fbo          js.Value
	fboTexture   js.Value
	rboDepth     js.Value
	fboPP        []js.Value
	fboPPTexture []js.Value

	postVertBuffer   js.Value
	postShaderSelect []*ShaderProgram_WebGL

	spriteShader *ShaderProgram_WebGL
	vertexBuffer js.Value

	customShaders   map[uint32]*ShaderProgram_WebGL
	customShaderMap map[string]uint32
	nextShaderID    uint32
	currentProgram  *ShaderProgram_WebGL

	spriteVAO js.Value
	postVAO   js.Value

	grabTexture *Texture_WebGL

	// state cache
	programID      uint32
	blendEnabled   bool
	blendEquation  BlendEquation
	blendSrc       BlendFunc
	blendDst       BlendFunc
	depthTest      bool
	depthMask      bool
	scissorEnabled bool
	scissorRect    [4]int32

	// uniform dedup caches (keyed by programID<<16 | uniformIdx)
	uniformICache  map[uint32]int32
	uniformF1Cache map[uint32]float32
	uniformF2Cache map[uint32][2]float32
	uniformF3Cache map[uint32][3]float32
	uniformF4Cache map[uint32][4]float32

	// sprite texture unit cache
	texCacheTexSerial []uint64
	texCacheLastUsed  []uint64
	texCacheTimer     uint64
}

// c resolves a WebGL enum constant by name, cached.
func (r *Renderer_WebGL) c(name string) js.Value {
	if v, ok := r.consts[name]; ok {
		return v
	}
	v := r.gl.Get(name)
	r.consts[name] = v
	return v
}

func (r *Renderer_WebGL) ci(name string) int {
	return r.c(name).Int()
}

func (r *Renderer_WebGL) GetName() string { return webglDebugName }

func (r *Renderer_WebGL) DebugInfo() string {
	if !r.gl.Truthy() {
		return webglDebugName
	}
	return fmt.Sprintf("%v (%v)",
		r.gl.Call("getParameter", r.c("VERSION")).String(),
		r.gl.Call("getParameter", r.c("RENDERER")).String())
}

const webglESHeader = "#version 300 es\nprecision highp float;\nprecision highp int;\n"

func (r *Renderer_WebGL) compileShader(shaderType js.Value, src string) (js.Value, error) {
	src = strings.TrimRight(src, "\x00")
	if !strings.HasPrefix(strings.TrimSpace(src), "#version") {
		src = webglESHeader + src
	}
	shader := r.gl.Call("createShader", shaderType)
	r.gl.Call("shaderSource", shader, src)
	r.gl.Call("compileShader", shader)
	if !r.gl.Call("getShaderParameter", shader, r.c("COMPILE_STATUS")).Bool() {
		log := r.gl.Call("getShaderInfoLog", shader).String()
		r.gl.Call("deleteShader", shader)
		return js.Null(), Error("shader compile error: " + log)
	}
	return shader, nil
}

func (r *Renderer_WebGL) newShaderProgram(vert, frag, geo, name string, crashWhenFail bool) (*ShaderProgram_WebGL, error) {
	if len(geo) > 0 {
		err := Error("geometry shaders are not supported on WebGL")
		chkEX(err, "Shader compilation error on "+name+"\n", crashWhenFail)
		return nil, err
	}
	vertObj, err := r.compileShader(r.c("VERTEX_SHADER"), vert)
	if chkEX(err, "Shader compilation error on "+name+"\n", crashWhenFail) {
		return nil, err
	}
	fragObj, err := r.compileShader(r.c("FRAGMENT_SHADER"), frag)
	if chkEX(err, "Shader compilation error on "+name+"\n", crashWhenFail) {
		return nil, err
	}
	prog := r.gl.Call("createProgram")
	r.gl.Call("attachShader", prog, vertObj)
	r.gl.Call("attachShader", prog, fragObj)
	r.gl.Call("linkProgram", prog)
	r.gl.Call("deleteShader", vertObj)
	r.gl.Call("deleteShader", fragObj)
	if !r.gl.Call("getProgramParameter", prog, r.c("LINK_STATUS")).Bool() {
		log := r.gl.Call("getProgramInfoLog", prog).String()
		r.gl.Call("deleteProgram", prog)
		err := Error("program link error: " + log)
		chkEX(err, "Link program error on "+name+"\n", crashWhenFail)
		return nil, err
	}
	r.nextShaderID++
	return &ShaderProgram_WebGL{
		program:    prog,
		name:       name,
		attributes: make(map[string]int),
		uniformIdx: make(map[string]int),
		textures:   make(map[string]int),
		id:         r.nextShaderID,
	}, nil
}

func (s *ShaderProgram_WebGL) RegisterAttributes(names ...string) {
	r := gfx.(*Renderer_WebGL)
	for _, name := range names {
		s.attributes[name] = r.gl.Call("getAttribLocation", s.program, name).Int()
	}
}

func (s *ShaderProgram_WebGL) RegisterUniforms(names ...string) {
	r := gfx.(*Renderer_WebGL)
	for _, name := range names {
		loc := r.gl.Call("getUniformLocation", s.program, name)
		s.uniformIdx[name] = len(s.uniformLocs)
		s.uniformLocs = append(s.uniformLocs, loc)
	}
}

func (s *ShaderProgram_WebGL) RegisterTextures(names ...string) {
	r := gfx.(*Renderer_WebGL)
	for _, name := range names {
		loc := r.gl.Call("getUniformLocation", s.program, name)
		s.uniformIdx[name] = len(s.uniformLocs)
		s.uniformLocs = append(s.uniformLocs, loc)
		s.textures[name] = len(s.textures)
	}
}

func (r *Renderer_WebGL) generateTexture(width, height, depth int32, filter bool) *Texture_WebGL {
	h := r.gl.Call("createTexture")
	textureSerialNumber++
	return &Texture_WebGL{
		width: width, height: height, depth: depth, filter: filter,
		handle: h, serial: textureSerialNumber,
	}
}

func (r *Renderer_WebGL) newTexture(width, height, depth int32, filter bool) Texture {
	r.SetActiveTexture0()
	t := r.generateTexture(width, height, depth, filter)
	internal, format, _ := t.mapFormat(Max(depth, 8))
	r.gl.Call("bindTexture", r.c("TEXTURE_2D"), t.handle)
	r.gl.Call("texImage2D", r.c("TEXTURE_2D"), 0, internal, width, height, 0,
		format, r.c("UNSIGNED_BYTE"), js.Null())
	// WebGL2 defaults MIN_FILTER to mipmapping; set safe params up front.
	r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MIN_FILTER"), r.c("NEAREST"))
	r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MAG_FILTER"), r.c("NEAREST"))
	r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_S"), r.c("CLAMP_TO_EDGE"))
	r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_T"), r.c("CLAMP_TO_EDGE"))
	r.gl.Call("bindTexture", r.c("TEXTURE_2D"), js.Null())
	return t
}

func (r *Renderer_WebGL) newPaletteTexture() Texture {
	return r.newTexture(256, 1, 32, false)
}

func (r *Renderer_WebGL) newModelTexture(width, height, depth int32, filter bool) Texture {
	return r.newTexture(width, height, depth, filter)
}

func (r *Renderer_WebGL) newDataTexture(width, height int32) Texture {
	r.SetActiveTexture0()
	t := r.generateTexture(width, height, 128, false)
	r.gl.Call("bindTexture", r.c("TEXTURE_2D"), t.handle)
	r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MAG_FILTER"), r.c("NEAREST"))
	r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MIN_FILTER"), r.c("NEAREST"))
	r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_S"), r.c("CLAMP_TO_EDGE"))
	r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_T"), r.c("CLAMP_TO_EDGE"))
	return t
}

func (r *Renderer_WebGL) newHDRTexture(width, height int32) Texture {
	r.SetActiveTexture0()
	t := r.generateTexture(width, height, 128, false)
	r.gl.Call("bindTexture", r.c("TEXTURE_2D"), t.handle)
	r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MIN_FILTER"), r.c("LINEAR"))
	r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MAG_FILTER"), r.c("LINEAR"))
	r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_S"), r.c("MIRRORED_REPEAT"))
	r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_T"), r.c("MIRRORED_REPEAT"))
	return t
}

func (r *Renderer_WebGL) newCubeMapTexture(widthHeight int32, mipmap bool, lowestMipLevel int32) Texture {
	// Only used by the 3D model pipeline (disabled on WebGL).
	return r.generateTexture(widthHeight, widthHeight, 24, false)
}

func (r *Renderer_WebGL) Init() {
	doc := js.Global().Get("document")
	canvas := doc.Call("getElementById", "ikemen-canvas")
	if !canvas.Truthy() {
		canvas = doc.Call("createElement", "canvas")
		canvas.Set("id", "ikemen-canvas")
		doc.Get("body").Call("appendChild", canvas)
	}
	attrs := js.Global().Get("Object").New()
	attrs.Set("alpha", false)
	attrs.Set("antialias", false)
	attrs.Set("depth", true)
	attrs.Set("premultipliedAlpha", false)
	attrs.Set("preserveDrawingBuffer", false)
	attrs.Set("powerPreference", "high-performance")
	r.gl = canvas.Call("getContext", "webgl2", attrs)
	if !r.gl.Truthy() {
		panic(Error("WebGL2 is not available in this browser"))
	}
	r.consts = make(map[string]js.Value)
	LogMessage("Using %v", r.DebugInfo())

	// No MSAA on the WebGL path.
	if sys.msaa > 0 {
		sys.msaa = 0
	}

	r.customShaders = make(map[uint32]*ShaderProgram_WebGL)
	r.customShaderMap = make(map[string]uint32)
	r.nextShaderID = 1
	r.currentProgram = nil
	r.uniformICache = make(map[uint32]int32)
	r.uniformF1Cache = make(map[uint32]float32)
	r.uniformF2Cache = make(map[uint32][2]float32)
	r.uniformF3Cache = make(map[uint32][3]float32)
	r.uniformF4Cache = make(map[uint32][4]float32)

	maxUnits := r.gl.Call("getParameter", r.c("MAX_TEXTURE_IMAGE_UNITS")).Int()
	if maxUnits > 16 {
		maxUnits = 16
	}
	r.texCacheTexSerial = make([]uint64, maxUnits)
	r.texCacheLastUsed = make([]uint64, maxUnits)
	r.texCacheTimer = 1

	// VAOs and buffers
	r.spriteVAO = r.gl.Call("createVertexArray")
	r.postVAO = r.gl.Call("createVertexArray")
	r.vertexBuffer = r.gl.Call("createBuffer")
	r.postVertBuffer = r.gl.Call("createBuffer")

	// Post-processing quad
	postVerts := []float32{-1, -1, 1, -1, -1, 1, 1, 1}
	r.gl.Call("bindBuffer", r.c("ARRAY_BUFFER"), r.postVertBuffer)
	r.gl.Call("bufferData", r.c("ARRAY_BUFFER"), r.scratch.floats(postVerts), r.c("STATIC_DRAW"))
	r.gl.Call("bindBuffer", r.c("ARRAY_BUFFER"), js.Null())

	// Sprite shader
	r.spriteShader, _ = r.newShaderProgram(vertShader, fragShader, "", "Main Shader", true)
	r.spriteShader.RegisterAttributes("position", "uv")
	r.spriteShader.RegisterUniforms("modelview", "projection", "x1x2x4x3",
		"alpha", "tint", "mask", "neg", "gray", "add", "mult", "isFlat", "isRgba", "isTrapez", "hue")
	r.spriteShader.RegisterTextures("pal", "tex")

	// Sprite VAO layout
	r.gl.Call("bindVertexArray", r.spriteVAO)
	r.gl.Call("bindBuffer", r.c("ARRAY_BUFFER"), r.vertexBuffer)
	locPos := r.spriteShader.attributes["position"]
	r.gl.Call("enableVertexAttribArray", locPos)
	r.gl.Call("vertexAttribPointer", locPos, 2, r.c("FLOAT"), false, 16, 0)
	locUV := r.spriteShader.attributes["uv"]
	r.gl.Call("enableVertexAttribArray", locUV)
	r.gl.Call("vertexAttribPointer", locUV, 2, r.c("FLOAT"), false, 16, 8)
	r.gl.Call("bindVertexArray", js.Null())

	// Post-processing shaders (external ones + trailing identity shader)
	r.postShaderSelect = make([]*ShaderProgram_WebGL, len(sys.cfg.Video.ExternalShaders)+1)
	r.gl.Call("bindVertexArray", r.postVAO)
	r.gl.Call("bindBuffer", r.c("ARRAY_BUFFER"), r.postVertBuffer)
	for i := 0; i < len(sys.cfg.Video.ExternalShaders); i++ {
		r.postShaderSelect[i], _ = r.newShaderProgram(string(sys.externalShaders[0][i]),
			string(sys.externalShaders[1][i]), "", fmt.Sprintf("Postprocess Shader #%v", i), true)
		r.postShaderSelect[i].RegisterAttributes("VertCoord")
		r.postShaderSelect[i].RegisterUniforms("Texture_GL33", "TextureSize", "CurrentTime")
		if loc, ok := r.postShaderSelect[i].attributes["VertCoord"]; ok && loc >= 0 {
			r.gl.Call("enableVertexAttribArray", loc)
			r.gl.Call("vertexAttribPointer", loc, 2, r.c("FLOAT"), false, 0, 0)
		}
	}
	identShader, _ := r.newShaderProgram(identVertShader, identFragShader, "", "Identity Postprocess", true)
	identShader.RegisterAttributes("VertCoord")
	if loc, ok := identShader.attributes["VertCoord"]; ok && loc >= 0 {
		r.gl.Call("enableVertexAttribArray", loc)
		r.gl.Call("vertexAttribPointer", loc, 2, r.c("FLOAT"), false, 0, 0)
	}
	r.postShaderSelect[len(r.postShaderSelect)-1] = identShader
	r.gl.Call("bindVertexArray", js.Null())
	r.gl.Call("bindBuffer", r.c("ARRAY_BUFFER"), js.Null())

	// Grab-pass texture
	r.SetActiveTexture0()
	r.grabTexture = r.newTexture(sys.scrrect[2], sys.scrrect[3], 32, true).(*Texture_WebGL)
	r.grabTexture.SetData(nil)

	// Main offscreen framebuffer: RGBA color texture + depth renderbuffer
	r.fboTexture = r.gl.Call("createTexture")
	textureSerialNumber++
	r.gl.Call("bindTexture", r.c("TEXTURE_2D"), r.fboTexture)
	r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MAG_FILTER"), r.c("NEAREST"))
	r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MIN_FILTER"), r.c("NEAREST"))
	r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_S"), r.c("CLAMP_TO_EDGE"))
	r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_T"), r.c("CLAMP_TO_EDGE"))
	r.gl.Call("texImage2D", r.c("TEXTURE_2D"), 0, r.c("RGBA"), sys.scrrect[2], sys.scrrect[3], 0,
		r.c("RGBA"), r.c("UNSIGNED_BYTE"), js.Null())

	r.rboDepth = r.gl.Call("createRenderbuffer")
	r.gl.Call("bindRenderbuffer", r.c("RENDERBUFFER"), r.rboDepth)
	r.gl.Call("renderbufferStorage", r.c("RENDERBUFFER"), r.c("DEPTH_COMPONENT24"), sys.scrrect[2], sys.scrrect[3])

	r.fbo = r.gl.Call("createFramebuffer")
	r.gl.Call("bindFramebuffer", r.c("FRAMEBUFFER"), r.fbo)
	r.gl.Call("framebufferTexture2D", r.c("FRAMEBUFFER"), r.c("COLOR_ATTACHMENT0"), r.c("TEXTURE_2D"), r.fboTexture, 0)
	r.gl.Call("framebufferRenderbuffer", r.c("FRAMEBUFFER"), r.c("DEPTH_ATTACHMENT"), r.c("RENDERBUFFER"), r.rboDepth)

	// Ping-pong post-processing framebuffers
	r.fboPP = make([]js.Value, 2)
	r.fboPPTexture = make([]js.Value, 2)
	for i := 0; i < 2; i++ {
		r.fboPPTexture[i] = r.gl.Call("createTexture")
		textureSerialNumber++
		r.gl.Call("bindTexture", r.c("TEXTURE_2D"), r.fboPPTexture[i])
		r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MAG_FILTER"), r.c("NEAREST"))
		r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MIN_FILTER"), r.c("NEAREST"))
		r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_S"), r.c("CLAMP_TO_EDGE"))
		r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_T"), r.c("CLAMP_TO_EDGE"))
		r.gl.Call("texImage2D", r.c("TEXTURE_2D"), 0, r.c("RGBA"), sys.scrrect[2], sys.scrrect[3], 0,
			r.c("RGBA"), r.c("UNSIGNED_BYTE"), js.Null())
		r.fboPP[i] = r.gl.Call("createFramebuffer")
		r.gl.Call("bindFramebuffer", r.c("FRAMEBUFFER"), r.fboPP[i])
		r.gl.Call("framebufferTexture2D", r.c("FRAMEBUFFER"), r.c("COLOR_ATTACHMENT0"), r.c("TEXTURE_2D"), r.fboPPTexture[i], 0)
	}

	if r.gl.Call("checkFramebufferStatus", r.c("FRAMEBUFFER")).Int() != r.ci("FRAMEBUFFER_COMPLETE") {
		LogMessage("WebGL: framebuffer incomplete")
	}
	r.gl.Call("bindFramebuffer", r.c("FRAMEBUFFER"), js.Null())
}

func (r *Renderer_WebGL) Close() {}

func (r *Renderer_WebGL) IsModelEnabled() bool  { return false }
func (r *Renderer_WebGL) IsShadowEnabled() bool { return false }

func (r *Renderer_WebGL) BeginFrame(clearColor bool) {
	r.gl.Call("bindFramebuffer", r.c("FRAMEBUFFER"), r.fbo)
	r.gl.Call("viewport", 0, 0, sys.scrrect[2], sys.scrrect[3])
	if clearColor {
		r.gl.Call("clear", r.ci("COLOR_BUFFER_BIT")|r.ci("DEPTH_BUFFER_BIT"))
	} else {
		r.gl.Call("clear", r.ci("DEPTH_BUFFER_BIT"))
	}
}

func (r *Renderer_WebGL) EndFrame() {
	if len(r.fboPP) == 0 {
		return
	}
	width, height := int32(sys.scrrect[2]), int32(sys.scrrect[3])
	now := js.Global().Get("performance").Call("now").Float()

	scaleMode := r.c("NEAREST")
	if sys.cfg.Video.WindowScaleMode {
		scaleMode = r.c("LINEAR")
	}

	r.gl.Call("viewport", 0, 0, width, height)
	for i := 0; i < 2; i++ {
		r.gl.Call("bindFramebuffer", r.c("FRAMEBUFFER"), r.fboPP[i])
		r.gl.Call("clear", r.ci("COLOR_BUFFER_BIT"))
	}
	r.SetActiveTexture0()

	r.DisableScissor()
	r.DisableBlending()
	r.SetDepthTest(false)
	r.SetDepthMask(false)

	for i := 0; i < len(r.postShaderSelect); i++ {
		postShader := r.postShaderSelect[i]
		r.ChangeProgram(postShader)
		r.gl.Call("bindVertexArray", r.postVAO)

		if i%2 == 0 {
			r.gl.Call("bindFramebuffer", r.c("FRAMEBUFFER"), r.fboPP[0])
			if i == 0 {
				r.gl.Call("bindTexture", r.c("TEXTURE_2D"), r.fboTexture)
			} else {
				r.gl.Call("bindTexture", r.c("TEXTURE_2D"), r.fboPPTexture[1])
			}
		} else {
			r.gl.Call("bindFramebuffer", r.c("FRAMEBUFFER"), r.fboPP[1])
			r.gl.Call("bindTexture", r.c("TEXTURE_2D"), r.fboPPTexture[0])
		}

		if i >= len(r.postShaderSelect)-1 {
			x, y, w, h := sys.window.GetScaledViewportSize()
			r.gl.Call("viewport", x, y, w, h)
			r.gl.Call("bindFramebuffer", r.c("FRAMEBUFFER"), js.Null())
			r.gl.Call("clear", r.ci("COLOR_BUFFER_BIT")|r.ci("DEPTH_BUFFER_BIT"))
		}

		if idx := postShader.uniformIndex("Texture_GL33"); idx >= 0 {
			r.SetUniformISub(postShader, idx, 0)
		}
		if idx := postShader.uniformIndex("TextureSize"); idx >= 0 {
			r.SetUniformFSub(postShader, idx, float32(width), float32(height))
		}
		if idx := postShader.uniformIndex("CurrentTime"); idx >= 0 {
			r.SetUniformFSub(postShader, idx, float32(now))
		}

		r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MAG_FILTER"), scaleMode)
		r.gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MIN_FILTER"), scaleMode)

		r.gl.Call("drawArrays", r.c("TRIANGLE_STRIP"), 0, 4)
	}
	// Unbind the post-process texture so it is not still the active sampler
	// when the NEXT frame renders into the scene FBO - that pairing is what
	// makes some drivers spam "Feedback loop formed between Framebuffer and
	// active Texture" every frame (harmless per the spec, but the driver-side
	// error handling is a real per-frame cost on the machines that report it).
	r.gl.Call("bindTexture", r.c("TEXTURE_2D"), js.Null())
}

func (r *Renderer_WebGL) Await() {
	// gl.finish() stalls the browser's pipeline for little benefit.
}

func (r *Renderer_WebGL) mapBlendEquation(i BlendEquation) js.Value {
	switch i {
	case BlendReverseSubtract:
		return r.c("FUNC_REVERSE_SUBTRACT")
	default:
		return r.c("FUNC_ADD")
	}
}

func (r *Renderer_WebGL) mapBlendFunction(i BlendFunc) js.Value {
	switch i {
	case BlendOne:
		return r.c("ONE")
	case BlendZero:
		return r.c("ZERO")
	case BlendSrcAlpha:
		return r.c("SRC_ALPHA")
	case BlendOneMinusSrcAlpha:
		return r.c("ONE_MINUS_SRC_ALPHA")
	case BlendOneMinusDstColor:
		return r.c("ONE_MINUS_DST_COLOR")
	case BlendDstColor:
		return r.c("DST_COLOR")
	}
	return r.c("ONE")
}

func (r *Renderer_WebGL) mapPrimitiveMode(i PrimitiveMode) js.Value {
	switch i {
	case LINES:
		return r.c("LINES")
	case LINE_LOOP:
		return r.c("LINE_LOOP")
	case LINE_STRIP:
		return r.c("LINE_STRIP")
	case TRIANGLES:
		return r.c("TRIANGLES")
	case TRIANGLE_STRIP:
		return r.c("TRIANGLE_STRIP")
	case TRIANGLE_FAN:
		return r.c("TRIANGLE_FAN")
	}
	return r.c("TRIANGLES")
}

func (r *Renderer_WebGL) SetDepthTest(depthTest bool) {
	if depthTest == r.depthTest {
		return
	}
	r.depthTest = depthTest
	if depthTest {
		r.gl.Call("enable", r.c("DEPTH_TEST"))
		r.gl.Call("depthFunc", r.c("LESS"))
	} else {
		r.gl.Call("disable", r.c("DEPTH_TEST"))
	}
}

func (r *Renderer_WebGL) SetDepthMask(depthMask bool) {
	if depthMask == r.depthMask {
		return
	}
	r.depthMask = depthMask
	r.gl.Call("depthMask", depthMask)
}

func (r *Renderer_WebGL) ChangeProgram(prog *ShaderProgram_WebGL) {
	if prog == nil || r.programID == prog.id {
		return
	}
	r.gl.Call("useProgram", prog.program)
	r.programID = prog.id
	for i := range r.texCacheTexSerial {
		r.texCacheTexSerial[i] = 0
		r.texCacheLastUsed[i] = 0
	}
	r.texCacheTimer = 1
}

func (r *Renderer_WebGL) EnableBlending(eq BlendEquation, src, dst BlendFunc) {
	if !r.blendEnabled {
		r.gl.Call("enable", r.c("BLEND"))
		r.blendEnabled = true
	}
	if eq != r.blendEquation {
		r.blendEquation = eq
		r.gl.Call("blendEquation", r.mapBlendEquation(eq))
	}
	if src != r.blendSrc || dst != r.blendDst {
		r.blendSrc = src
		r.blendDst = dst
		r.gl.Call("blendFunc", r.mapBlendFunction(src), r.mapBlendFunction(dst))
	}
}

func (r *Renderer_WebGL) DisableBlending() {
	if r.blendEnabled {
		r.gl.Call("disable", r.c("BLEND"))
		r.blendEnabled = false
	}
}

// 3D model/shadow pipeline: disabled on WebGL.
func (r *Renderer_WebGL) prepareShadowMapPipeline(bufferIndex uint32) {}
func (r *Renderer_WebGL) setShadowMapPipeline(doubleSided, invertFrontFace, useUV, useNormal, useTangent, useVertColor, useJoint0, useJoint1 bool, numVertices, vertAttrOffset uint32) {
}
func (r *Renderer_WebGL) ReleaseShadowPipeline()                                    {}
func (r *Renderer_WebGL) prepareModelPipeline(bufferIndex uint32, env *Environment) {}
func (r *Renderer_WebGL) SetModelPipeline(eq BlendEquation, src, dst BlendFunc, depthTest, depthMask, doubleSided, invertFrontFace, useUV, useNormal, useTangent, useVertColor, useJoint0, useJoint1, useOutlineAttribute bool, numVertices, vertAttrOffset uint32) {
}
func (r *Renderer_WebGL) SetMeshOutlinePipeline(invertFrontFace bool, meshOutline float32) {}
func (r *Renderer_WebGL) ReleaseModelPipeline()                                            {}

func (r *Renderer_WebGL) ReadPixels(data []uint8, width, height int) {
	r.gl.Call("bindFramebuffer", r.c("READ_FRAMEBUFFER"), js.Null())
	r.scratch.ensure(len(data))
	dst := r.scratch.u8.Call("subarray", 0, len(data))
	r.gl.Call("readPixels", 0, 0, width, height, r.c("RGBA"), r.c("UNSIGNED_BYTE"), dst)
	js.CopyBytesToGo(data, dst)
}

func (r *Renderer_WebGL) EnableScissor(x, y, width, height int32) {
	realY := sys.scrrect[3] - (y + height)
	if r.scissorEnabled &&
		r.scissorRect[0] == x && r.scissorRect[1] == realY &&
		r.scissorRect[2] == width && r.scissorRect[3] == height {
		return
	}
	if !r.scissorEnabled {
		r.gl.Call("enable", r.c("SCISSOR_TEST"))
		r.scissorEnabled = true
	}
	r.gl.Call("scissor", x, realY, width, height)
	r.scissorRect = [4]int32{x, realY, width, height}
}

func (r *Renderer_WebGL) DisableScissor() {
	if r.scissorEnabled {
		r.gl.Call("disable", r.c("SCISSOR_TEST"))
		r.scissorEnabled = false
	}
}

func (r *Renderer_WebGL) SetUniformISub(prog *ShaderProgram_WebGL, idx int, val int32) {
	if idx < 0 || idx >= len(prog.uniformLocs) {
		return
	}
	loc := prog.uniformLocs[idx]
	if !loc.Truthy() {
		return
	}
	if prog == r.spriteShader {
		key := (prog.id << 16) | uint32(idx)
		if old, exists := r.uniformICache[key]; exists && old == val {
			return
		}
		r.uniformICache[key] = val
	}
	r.gl.Call("uniform1i", loc, val)
}

func (r *Renderer_WebGL) SetUniformFSub(prog *ShaderProgram_WebGL, idx int, values ...float32) {
	if idx < 0 || idx >= len(prog.uniformLocs) || len(values) == 0 {
		return
	}
	loc := prog.uniformLocs[idx]
	if !loc.Truthy() {
		return
	}
	if prog == r.spriteShader {
		key := (prog.id << 16) | uint32(idx)
		switch len(values) {
		case 1:
			if old, exists := r.uniformF1Cache[key]; exists && old == values[0] {
				return
			}
			r.uniformF1Cache[key] = values[0]
		case 2:
			v2 := [2]float32{values[0], values[1]}
			if old, exists := r.uniformF2Cache[key]; exists && old == v2 {
				return
			}
			r.uniformF2Cache[key] = v2
		case 3:
			v3 := [3]float32{values[0], values[1], values[2]}
			if old, exists := r.uniformF3Cache[key]; exists && old == v3 {
				return
			}
			r.uniformF3Cache[key] = v3
		case 4:
			v4 := [4]float32{values[0], values[1], values[2], values[3]}
			if old, exists := r.uniformF4Cache[key]; exists && old == v4 {
				return
			}
			r.uniformF4Cache[key] = v4
		}
	}
	switch len(values) {
	case 1:
		r.gl.Call("uniform1f", loc, values[0])
	case 2:
		r.gl.Call("uniform2f", loc, values[0], values[1])
	case 3:
		r.gl.Call("uniform3f", loc, values[0], values[1], values[2])
	case 4:
		r.gl.Call("uniform4f", loc, values[0], values[1], values[2], values[3])
	}
}

func (r *Renderer_WebGL) SetUniformFvSub(prog *ShaderProgram_WebGL, idx int, values []float32) {
	if idx < 0 || idx >= len(prog.uniformLocs) || len(values) == 0 {
		return
	}
	switch len(values) {
	case 1, 2, 3, 4:
		r.SetUniformFSub(prog, idx, values...)
	default:
		loc := prog.uniformLocs[idx]
		if !loc.Truthy() {
			return
		}
		if len(values) == 8 {
			r.gl.Call("uniform4fv", loc, r.scratch.floats(values))
		} else {
			r.gl.Call("uniform1fv", loc, r.scratch.floats(values))
		}
	}
}

func (r *Renderer_WebGL) SetUniformI(name string, val int) {
	if r.currentProgram == nil {
		return
	}
	r.SetUniformISub(r.currentProgram, r.currentProgram.uniformIndex(name), int32(val))
}

func (r *Renderer_WebGL) SetUniformF(name string, values ...float32) {
	if r.currentProgram == nil {
		return
	}
	r.SetUniformFSub(r.currentProgram, r.currentProgram.uniformIndex(name), values...)
}

func (r *Renderer_WebGL) SetUniformFv(name string, values []float32) {
	if r.currentProgram == nil {
		return
	}
	r.SetUniformFvSub(r.currentProgram, r.currentProgram.uniformIndex(name), values)
}

func (r *Renderer_WebGL) SetUniformMatrix(name string, value []float32) {
	if r.currentProgram == nil {
		return
	}
	idx := r.currentProgram.uniformIndex(name)
	if idx < 0 || idx >= len(r.currentProgram.uniformLocs) {
		return
	}
	loc := r.currentProgram.uniformLocs[idx]
	if !loc.Truthy() {
		return
	}
	r.gl.Call("uniformMatrix4fv", loc, false, r.scratch.floats(value))
}

func (r *Renderer_WebGL) SetModelUniformI(name string, val int)                   {}
func (r *Renderer_WebGL) SetModelUniformF(name string, values ...float32)         {}
func (r *Renderer_WebGL) SetModelUniformFv(name string, values []float32)         {}
func (r *Renderer_WebGL) SetModelUniformMatrix(name string, value []float32)      {}
func (r *Renderer_WebGL) SetModelUniformMatrix3(name string, value []float32)     {}
func (r *Renderer_WebGL) SetModelTexture(name string, t Texture)                  {}
func (r *Renderer_WebGL) SetShadowMapUniformI(name string, val int)               {}
func (r *Renderer_WebGL) SetShadowMapUniformF(name string, values ...float32)     {}
func (r *Renderer_WebGL) SetShadowMapUniformFv(name string, values []float32)     {}
func (r *Renderer_WebGL) SetShadowMapUniformMatrix(name string, value []float32)  {}
func (r *Renderer_WebGL) SetShadowMapUniformMatrix3(name string, value []float32) {}
func (r *Renderer_WebGL) SetShadowMapTexture(name string, t Texture)              {}
func (r *Renderer_WebGL) SetShadowFrameTexture(i uint32)                          {}
func (r *Renderer_WebGL) SetShadowFrameCubeTexture(i uint32)                      {}

func (r *Renderer_WebGL) SetActiveTexture0() {
	r.gl.Call("activeTexture", r.c("TEXTURE0"))
	if len(r.texCacheTexSerial) > 0 {
		r.texCacheTexSerial[0] = 0
		r.texCacheLastUsed[0] = 0
	}
}

func (r *Renderer_WebGL) SetTexture(name string, tex Texture) {
	if r.currentProgram == nil {
		return
	}
	prog := r.currentProgram
	t := tex.(*Texture_WebGL)
	idx := prog.uniformIndex(name)

	// LRU texture-unit cache for the sprite shader
	if prog == r.spriteShader {
		r.texCacheTimer++
		var oldestUnit int32 = 0
		var minTime uint64 = ^uint64(0)
		for i := range r.texCacheTexSerial {
			if r.texCacheTexSerial[i] == t.serial {
				r.texCacheLastUsed[i] = r.texCacheTimer
				r.SetUniformISub(prog, idx, int32(i))
				return
			}
			if r.texCacheLastUsed[i] < minTime {
				minTime = r.texCacheLastUsed[i]
				oldestUnit = int32(i)
			}
		}
		r.gl.Call("activeTexture", r.ci("TEXTURE0")+int(oldestUnit))
		r.gl.Call("bindTexture", r.c("TEXTURE_2D"), t.handle)
		r.texCacheTexSerial[oldestUnit] = t.serial
		r.texCacheLastUsed[oldestUnit] = r.texCacheTimer
		r.SetUniformISub(prog, idx, oldestUnit)
		return
	}

	fixedUnit := prog.textures[name]
	r.gl.Call("activeTexture", r.ci("TEXTURE0")+fixedUnit)
	r.gl.Call("bindTexture", r.c("TEXTURE_2D"), t.handle)
	r.SetUniformISub(prog, idx, int32(fixedUnit))
}

func (r *Renderer_WebGL) SetVertexData(values ...float32) {
	r.gl.Call("bindBuffer", r.c("ARRAY_BUFFER"), r.vertexBuffer)
	r.gl.Call("bufferData", r.c("ARRAY_BUFFER"), r.scratch.floats(values), r.c("STATIC_DRAW"))
}

func (r *Renderer_WebGL) SetModelVertexData(bufferIndex uint32, values []byte)   {}
func (r *Renderer_WebGL) SetModelIndexData(bufferIndex uint32, values ...uint32) {}

func (r *Renderer_WebGL) RenderQuad() {
	r.gl.Call("drawArrays", r.c("TRIANGLE_STRIP"), 0, 4)
}

func (r *Renderer_WebGL) RenderElements(mode PrimitiveMode, count, offset int)          {}
func (r *Renderer_WebGL) RenderShadowMapElements(mode PrimitiveMode, count, offset int) {}
func (r *Renderer_WebGL) RenderCubeMap(envTexture Texture, cubeTexture Texture)         {}
func (r *Renderer_WebGL) RenderFilteredCubeMap(distribution int32, cubeTexture Texture, filteredTexture Texture, mipmapLevel, sampleCount int32, roughness float32) {
}
func (r *Renderer_WebGL) RenderLUT(distribution int32, cubeTexture Texture, lutTexture Texture, sampleCount int32) {
}

func (r *Renderer_WebGL) LoadCustomSpriteShader(shaderName string, shaderData []byte) uint32 {
	fragSource := string(shaderData)
	shader, err := r.newShaderProgram(vertShader, fragSource, "", "Custom: "+shaderName, false)
	if err != nil {
		return 0
	}
	shader.RegisterAttributes("position", "uv")
	shader.RegisterUniforms("modelview", "projection", "x1x2x4x3",
		"alpha", "tint", "mask", "neg", "gray", "add", "mult", "isFlat", "isRgba", "isTrapez", "hue",
		"iTime", "iResolution", "aspectRatio", "sTime")
	shader.RegisterTextures("pal", "tex", "tex1", "tex2", "bgl_RenderedTexture")
	shader.needsGrabPass = strings.Contains(fragSource, "bgl_RenderedTexture")

	id := r.nextShaderID
	r.nextShaderID++
	r.customShaders[id] = shader
	r.customShaderMap[shaderName] = id
	sys.appendToConsole(fmt.Sprintf("Loaded Custom Shader: %s (ID: %d)", shaderName, id))
	return id
}

func (r *Renderer_WebGL) UnloadCustomSpriteShader(shaderName string) {
	if id, exists := r.customShaderMap[shaderName]; exists {
		if shader, hasProg := r.customShaders[id]; hasProg {
			r.gl.Call("deleteProgram", shader.program)
			delete(r.customShaders, id)
			if r.currentProgram == shader {
				r.currentProgram = nil
			}
		}
		delete(r.customShaderMap, shaderName)
	}
}

func (r *Renderer_WebGL) SetSpritePipeline(shaderName string) {
	targetShader := r.spriteShader
	if shaderName != "" {
		if id, ok := r.customShaderMap[shaderName]; ok {
			if shader, ok := r.customShaders[id]; ok {
				targetShader = shader
			}
		}
	}
	if r.programID != targetShader.id {
		r.currentProgram = targetShader
		r.ChangeProgram(targetShader)
		r.gl.Call("bindVertexArray", r.spriteVAO)
	}
}

func (r *Renderer_WebGL) SetCustomUniforms(params [16]float32) {
	if r.currentProgram == nil {
		return
	}
	for i := 0; i < 16; i++ {
		loc := r.gl.Call("getUniformLocation", r.currentProgram.program, fmt.Sprintf("p%d", i))
		if loc.Truthy() {
			r.gl.Call("uniform1f", loc, params[i])
		}
	}
}

func (r *Renderer_WebGL) NeedsGrabPass() bool {
	if r.currentProgram != nil {
		return r.currentProgram.needsGrabPass
	}
	return false
}

func (r *Renderer_WebGL) ResolveBackBuffer() Texture {
	r.SetActiveTexture0()
	r.gl.Call("bindTexture", r.c("TEXTURE_2D"), r.grabTexture.handle)
	r.gl.Call("bindFramebuffer", r.c("READ_FRAMEBUFFER"), r.fbo)
	r.gl.Call("copyTexSubImage2D", r.c("TEXTURE_2D"), 0, 0, 0, 0, 0, r.grabTexture.width, r.grabTexture.height)
	r.gl.Call("bindFramebuffer", r.c("FRAMEBUFFER"), r.fbo)
	return r.grabTexture
}

func (r *Renderer_WebGL) PerspectiveProjectionMatrix(angle, aspect, near, far float32) mgl.Mat4 {
	return mgl.Perspective(angle, aspect, near, far)
}

func (r *Renderer_WebGL) OrthographicProjectionMatrix(left, right, bottom, top, near, far float32) mgl.Mat4 {
	return mgl.Ortho(left, right, bottom, top, near, far)
}

func (r *Renderer_WebGL) SetVSync(interval int) {
	// Frame pacing is requestAnimationFrame; nothing to configure.
}

func (r *Renderer_WebGL) NewWorkerThread() bool { return false }
