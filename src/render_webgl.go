//go:build js

package main

import (
	"fmt"
	"math"
	"runtime"
	"strings"
	"syscall/js"

	mgl "github.com/go-gl/mathgl/mgl32"
)

// WebGL2 renderer for the browser build: a port of render_gl33.go's 2D
// sprite/postprocess pipeline onto WebGL2 via syscall/js.
//
// Deliberate differences from the GL33 renderer:
//   - The 3D model pipeline lives in render_webgl_model.go: glTF stages draw
//     their models, with environment lighting where the browser can render
//     into float targets. Model SHADOWS are not available (IsShadowEnabled is
//     false): they need geometry shaders and cube map arrays, which WebGL2
//     lacks. If the model pipeline cannot be set up, IsModelEnabled is false
//     and the engine skips the models, keeping the stages' 2D layers.
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
	// Command-buffer handles (see render_webgl_cmdbuf.go): the program object
	// and each uniform location registered in the JS-side table. -1 = location
	// not valid (mirrors !loc.Truthy()).
	cmdID         int32
	uniformCmdIDs []int32
	// Uniform indices of spriteUV, palUV, tex1UV, tex2UV (the atlas rects
	// SetTexture sends with "tex", "pal", "tex1", "tex2"), and of "param".
	// Looked up once, on the first SetTexture after registration.
	uvIdx    [4]int
	paramIdx int
	uvIdxSet bool
}

func (s *ShaderProgram_WebGL) uniformIndex(name string) int {
	if idx, ok := s.uniformIdx[name]; ok {
		return idx
	}
	return -1
}

// ---------------------------------------------------------------------------
// textures

// Texture_WebGL is a GL texture, or a rectangle of one: sprites, glyphs and
// palettes live in atlas pages (render.go TextureAtlas, PalTexture), and each
// one is a sub-texture that shares its page's handle, serial and table slot.
type Texture_WebGL struct {
	width  int32
	height int32
	depth  int32
	offset [2]int32   // texel position inside the root (palette slots)
	uvst   [4]float32 // the rectangle inside the root, in UV
	filter bool
	handle js.Value
	serial uint64 // the root's: the texture-unit cache keys on it
	cmdID  int32  // handle in the command-buffer object table
	// The texture that owns the handle (itself for a page or a plain texture).
	// A sub-texture keeps its page alive, so the page's finalizer runs only
	// after every sprite in it is gone.
	root *Texture_WebGL
	// Model pipeline textures (render_webgl_model.go): a cube map, an RGBA16F
	// image uploaded from float data (the environment panorama), and an
	// environment map whose render pass has run.
	cube        bool
	halfFloat   bool
	envRendered bool
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
	r.glc("bindTexture", r.c("TEXTURE_2D"), t.handle)
	r.glc("pixelStorei", r.c("UNPACK_ALIGNMENT"), 1)
	r.glc("pixelStorei", r.c("UNPACK_ROW_LENGTH"), 0)
	return r.gl
}

func (t *Texture_WebGL) SetData(data []byte) {
	if t.root != nil && t.root != t {
		// A sub-texture must not reallocate its page (texImage2D on the shared
		// handle would drop every other sprite or palette in it): fill its own
		// rectangle instead, with zeros for nil.
		if data == nil {
			data = make([]byte, int(t.width)*int(t.height)*int(t.bytesPerPixel()))
		}
		t.SetSubData(data)
		return
	}
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

func (t *Texture_WebGL) bytesPerPixel() int32 {
	if bpp := t.depth / 8; bpp >= 1 {
		return bpp
	}
	return 1
}

// SetSubData fills the texture's own rectangle (a palette's 256x1 slot).
func (t *Texture_WebGL) SetSubData(data []byte) {
	t.SetSubData2(data, t.offset[0], t.offset[1], t.width, t.height, t.width*t.bytesPerPixel())
}

// SetSubData2 fills a rectangle of the texture's storage, in root texels.
func (t *Texture_WebGL) SetSubData2(data []byte, x, y, width, height, stride int32) {
	r := gfx.(*Renderer_WebGL)
	gl := t.bindAndPrepare()
	_, format, _ := t.mapFormat(Max(t.depth, 8))
	bytesPerPixel := t.bytesPerPixel()
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

// SetDataG uploads a model texture (RGBA8) and builds its mipmaps.
//   - An image larger than this GPU's MAX_TEXTURE_SIZE is halved (2x2 box)
//     until it fits. At its full size texImage2D fails with INVALID_VALUE and
//     the material samples black. Desktop GPUs allow 16384; phones and older
//     integrated GPUs often 4096 or 8192. One console warning per size.
//   - Level 0 is allocated empty and filled in row bands through the
//     long-lived scratch buffer, never from one Uint8Array the size of the
//     image: a js.Value lives until the next Go GC runs its finalizer, which
//     after a stage reload can be a long time, and CVS-P's 8000x4934 texture
//     kept ~150 MB of ArrayBuffer alive that way.
func (t *Texture_WebGL) SetDataG(data []byte, mag, min, ws, wt TextureSamplingParam) {
	r := gfx.(*Renderer_WebGL)
	bpp := int(Max(t.depth, 8) / 8)
	if limit := r.maxTextureSize(); bpp == 4 && limit > 0 && (t.width > limit || t.height > limit) &&
		len(data) >= int(t.width)*int(t.height)*4 {
		w0, h0 := t.width, t.height
		for t.width > limit || t.height > limit {
			data, t.width, t.height = halveRGBA8(data, t.width, t.height)
		}
		r.wglWarn(fmt.Sprintf("WebGL: a %dx%d model texture is over this GPU's limit (%d); drawn at %dx%d",
			w0, h0, limit, t.width, t.height))
	}
	gl := t.bindAndPrepare()
	internal, format, _ := t.mapFormat(Max(t.depth, 8))
	gl.Call("texImage2D", r.c("TEXTURE_2D"), 0, internal, t.width, t.height, 0,
		format, r.c("UNSIGNED_BYTE"), js.Null())
	if row := int(t.width) * bpp; row > 0 {
		band := (1 << 20) / row
		if band < 1 {
			band = 1
		}
		for y := 0; y < int(t.height); y += band {
			n := int(t.height) - y
			if n > band {
				n = band
			}
			if (y+n)*row > len(data) {
				break
			}
			gl.Call("texSubImage2D", r.c("TEXTURE_2D"), 0, 0, y, t.width, n,
				format, r.c("UNSIGNED_BYTE"), r.scratch.bytes(data[y*row:(y+n)*row]))
		}
	}
	gl.Call("generateMipmap", r.c("TEXTURE_2D"))
	gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MAG_FILTER"), t.samplingParam(mag))
	gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MIN_FILTER"), t.samplingParam(min))
	gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_S"), t.samplingParam(ws))
	gl.Call("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_T"), t.samplingParam(wt))
}

// SetPixelData uploads float texels: the model pipeline's joint matrices and
// morph targets (RGBA32F, sampled NEAREST) and the environment panorama
// (RGBA16F, see newHDRTexture).
func (t *Texture_WebGL) SetPixelData(data []float32) {
	r := gfx.(*Renderer_WebGL)
	if t.halfFloat && !r.model.envOK {
		return // the environment is never rendered; skip the upload
	}
	internal, _, _ := t.mapFormat(Max(t.depth, 8))
	_, format, _ := t.mapFormat(Max(t.depth/4, 8))
	if t.halfFloat {
		internal = r.c("RGBA16F")
	}
	channels := int(Max(t.depth, 8) / 32)
	if channels < 1 {
		channels = 1
	}
	src := r.uploadFloats(data, int(t.width)*int(t.height)*channels)
	gl := t.bindAndPrepare()
	gl.Call("texImage2D", r.c("TEXTURE_2D"), 0, internal, t.width, t.height, 0,
		format, r.c("FLOAT"), src)
}

// CopyData only serves TextureAtlas.Resize, which no atlas reaches (every
// CreateTextureAtlas leaves resize off).
func (t *Texture_WebGL) CopyData(src *Texture) {}

// MarkNonSwappable: WebGL has no VRAM swap-out.
func (t *Texture_WebGL) MarkNonSwappable() {}

func (t *Texture_WebGL) IsValid() bool {
	return t.width != 0 && t.height != 0 && t.handle.Truthy()
}

func (t *Texture_WebGL) GetWidth() int32   { return t.width }
func (t *Texture_WebGL) GetHeight() int32  { return t.height }
func (t *Texture_WebGL) GetUV() [4]float32 { return t.uvst }

// GetFilter reports whether the texture samples with linear filtering (image.go
// keeps the index buffer of unfiltered 8-bit sprites for mouse hit tests).
func (t *Texture_WebGL) GetFilter() bool { return t.filter }

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
	programID     uint32
	blendEnabled  bool
	blendEquation BlendEquation
	blendSrc      BlendFunc
	blendDst      BlendFunc
	depthTest     bool
	// MUST start true: this mirrors GL state, and WebGL's own default for
	// DEPTH_WRITEMASK is true. Left at Go's zero value it claimed the mask was
	// already false, so the single SetDepthMask(false) call took the "no
	// change" early-out and never emitted anything - the real depth write mask
	// stayed enabled for the whole session.
	//
	// That is not merely a wasted call. ANGLE enables the
	// forceDepthAttachmentInitOnClear workaround on AMD ONLY, and it triggers
	// for any clear including DEPTH_BUFFER_BIT unless depth is masked out -
	// so an AMD machine paid an extra depth-attachment init on every clear
	// while NVIDIA paid nothing. See newRenderer_WebGL for the init that keeps
	// this honest.
	depthMask      bool
	scissorEnabled bool
	// A disable that has been REQUESTED but not emitted. RenderSprite calls
	// DisableScissor after every single sprite, which cost a rasterizer-state
	// change per sprite AND made EnableScissor's rect dedup unreachable - that
	// early-out requires scissorEnabled to still be true, and the disable had
	// just cleared it. Deferring means consecutive sprites sharing a window
	// emit nothing at all. Anything that must actually see scissor off calls
	// flushScissorDisable.
	scissorPending bool
	scissorRect    [4]int32
	// The GL viewport as last emitted (GL's bottom-left origin), so a repeated
	// SetViewport costs nothing. viewportSet is false until the first emit.
	viewportRect [4]int32
	viewportSet  bool

	// uniform dedup caches (keyed by programID<<16 | uniformIdx)
	uniformICache  map[uint32]int32
	uniformF1Cache map[uint32]float32
	uniformF2Cache map[uint32][2]float32
	uniformF3Cache map[uint32][3]float32
	uniformF4Cache map[uint32][4]float32

	// sprite texture unit cache, keyed on the root texture's serial (an atlas
	// page bound once serves every sprite in it)
	texCacheTexSerial []uint64
	texCacheLastUsed  []uint64
	texCacheTimer     uint64
	texSerial         uint64 // last serial given out (0 = empty cache slot)

	// custom-shader param[16] dedup, keyed like the uniform caches
	uniformF16Cache map[uint32][16]float32

	// Screenshots (see BeginScreenshot): the size asked for, whether the next
	// EndFrame reads it, and the pixels FinishScreenshot hands over.
	shotW, shotH int
	shotWant     bool
	shotData     []byte
	shotPending  bool

	// GL command buffer (render_webgl_cmdbuf.go) + table handles for the
	// hot-path buffer objects
	cmd         wglCmdBuf
	vertexBufID int32
	spriteVAOID int32

	// 3D model pipeline (render_webgl_model.go)
	model wglModel
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
		r.glc("getParameter", r.c("VERSION")).String(),
		r.glc("getParameter", r.c("RENDERER")).String())
}

const webglESHeader = "#version 300 es\nprecision highp float;\nprecision highp int;\n"

func (r *Renderer_WebGL) compileShader(shaderType js.Value, src string) (js.Value, error) {
	// The sprite shader and custom shaders #include the sprite library; GLSL ES
	// has no #include, so expand it first, as the GL backends do.
	src = expandShaderIncludes(strings.TrimRight(src, "\x00"))
	if !strings.HasPrefix(strings.TrimSpace(src), "#version") {
		src = webglESHeader + src
	}
	shader := r.glc("createShader", shaderType)
	r.glc("shaderSource", shader, src)
	r.glc("compileShader", shader)
	if !r.glc("getShaderParameter", shader, r.c("COMPILE_STATUS")).Bool() {
		log := r.glc("getShaderInfoLog", shader).String()
		r.glc("deleteShader", shader)
		return js.Null(), Error("shader compile error: " + log)
	}
	return shader, nil
}

// Attribute locations fixed before linking. The font VAO (vert, vertTexCoord)
// also draws TrueType text through custom sprite shaders (position, uv), and
// the sprite VAO serves every custom shader, so the pairs must share
// locations in every program instead of trusting the driver's assignment.
var (
	wglSpriteAttribs = []string{"position", "uv"}
	wglFontAttribs   = []string{"vert", "vertTexCoord"}
)

func (r *Renderer_WebGL) newShaderProgram(vert, frag, geo, name string, crashWhenFail bool, attribs ...string) (*ShaderProgram_WebGL, error) {
	if len(geo) > 0 {
		err := Error("geometry shaders are not supported on WebGL")
		chkEX(err, "Shader compilation error on "+name+"\n", crashWhenFail)
		return nil, err
	}
	s, what, err := r.buildProgram(vert, frag, name, attribs...)
	if err != nil {
		chkEX(err, what+name+"\n", crashWhenFail)
		return nil, err
	}
	return s, nil
}

// buildProgram compiles and links without reporting the failure: the caller
// decides between the error dialog (newShaderProgram, the 2D shaders) and a
// feature that turns itself off (the 3D model pipeline). `what` names the
// failed step in the dialog's wording.
// attribs, when given, are bound to locations 0, 1, ... before linking.
func (r *Renderer_WebGL) buildProgram(vert, frag, name string, attribs ...string) (s *ShaderProgram_WebGL, what string, err error) {
	vertObj, err := r.compileShader(r.c("VERTEX_SHADER"), vert)
	if err != nil {
		return nil, "Shader compilation error on ", err
	}
	fragObj, err := r.compileShader(r.c("FRAGMENT_SHADER"), frag)
	if err != nil {
		r.glc("deleteShader", vertObj)
		return nil, "Shader compilation error on ", err
	}
	prog := r.glc("createProgram")
	r.glc("attachShader", prog, vertObj)
	r.glc("attachShader", prog, fragObj)
	for i, a := range attribs {
		r.glc("bindAttribLocation", prog, i, a)
	}
	r.glc("linkProgram", prog)
	r.glc("deleteShader", vertObj)
	r.glc("deleteShader", fragObj)
	if !r.glc("getProgramParameter", prog, r.c("LINK_STATUS")).Bool() {
		log := r.glc("getProgramInfoLog", prog).String()
		r.glc("deleteProgram", prog)
		return nil, "Link program error on ", Error("program link error: " + log)
	}
	r.nextShaderID++
	return &ShaderProgram_WebGL{
		program:    prog,
		name:       name,
		attributes: make(map[string]int),
		uniformIdx: make(map[string]int),
		textures:   make(map[string]int),
		id:         r.nextShaderID,
		cmdID:      r.regJS(prog),
		paramIdx:   -1,
	}, "", nil
}

func (s *ShaderProgram_WebGL) RegisterAttributes(names ...string) {
	r := gfx.(*Renderer_WebGL)
	for _, name := range names {
		s.attributes[name] = r.glc("getAttribLocation", s.program, name).Int()
	}
}

func (s *ShaderProgram_WebGL) RegisterUniforms(names ...string) {
	r := gfx.(*Renderer_WebGL)
	for _, name := range names {
		loc := r.glc("getUniformLocation", s.program, name)
		s.uniformIdx[name] = len(s.uniformLocs)
		s.uniformLocs = append(s.uniformLocs, loc)
		s.uniformCmdIDs = append(s.uniformCmdIDs, r.regLoc(loc))
	}
}

func (s *ShaderProgram_WebGL) RegisterTextures(names ...string) {
	r := gfx.(*Renderer_WebGL)
	for _, name := range names {
		loc := r.glc("getUniformLocation", s.program, name)
		s.uniformIdx[name] = len(s.uniformLocs)
		s.uniformLocs = append(s.uniformLocs, loc)
		s.uniformCmdIDs = append(s.uniformCmdIDs, r.regLoc(loc))
		s.textures[name] = len(s.textures)
	}
}

func (r *Renderer_WebGL) generateTexture(width, height, depth int32, filter bool) *Texture_WebGL {
	h := r.glc("createTexture")
	r.texSerial++
	t := &Texture_WebGL{
		width: width, height: height, depth: depth, filter: filter,
		uvst:   [4]float32{0, 0, 1, 1},
		handle: h, serial: r.texSerial,
		cmdID: r.regJS(h),
	}
	t.root = t
	return t
}

// newTexture makes a plain texture or an atlas page. It is deleted once the
// Go object is collected (gcRelease), as render_gl33.go's generateTexture
// does; sub-textures hold their page through root, so a page outlives them.
func (r *Renderer_WebGL) newTexture(width, height, depth int32, filter bool) (Texture, error) {
	r.SetActiveTexture0()
	t := r.generateTexture(width, height, depth, filter)
	r.gcRelease(t)
	internal, format, _ := t.mapFormat(Max(depth, 8))
	r.glc("bindTexture", r.c("TEXTURE_2D"), t.handle)
	r.glc("texImage2D", r.c("TEXTURE_2D"), 0, internal, width, height, 0,
		format, r.c("UNSIGNED_BYTE"), js.Null())
	// WebGL2 defaults MIN_FILTER to mipmapping; set safe params up front.
	r.glc("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MIN_FILTER"), r.c("NEAREST"))
	r.glc("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MAG_FILTER"), r.c("NEAREST"))
	r.glc("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_S"), r.c("CLAMP_TO_EDGE"))
	r.glc("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_T"), r.c("CLAMP_TO_EDGE"))
	r.glc("bindTexture", r.c("TEXTURE_2D"), js.Null())
	return t, nil
}

// newPaletteTexture is a 256x1 slot of a palette atlas page
// (sys.palTexture, render.go NewPaletteTexture). The slot goes back to the
// free list once the palette is collected.
func (r *Renderer_WebGL) newPaletteTexture(slot [2]uint32, offset [2]int32, uvst [4]float32) Texture {
	parent := (*(sys.palTexture.textures[slot[0]])).(*Texture_WebGL)
	t := &Texture_WebGL{width: 256, height: 1, depth: 32, offset: offset, uvst: uvst,
		handle: parent.handle, serial: parent.serial, cmdID: parent.cmdID, root: parent.root}
	runtime.SetFinalizer(t, func(t *Texture_WebGL) {
		sys.mainThreadTask <- func() {
			sys.palTexture.emptySlot.PushFront(slot)
		}
	})
	return t
}

// newSubTexture is a rectangle (uvst) of tex's page: a sprite or a glyph.
func (r *Renderer_WebGL) newSubTexture(tex Texture, width, height int32, uvst [4]float32) Texture {
	p := tex.(*Texture_WebGL)
	return &Texture_WebGL{width: width, height: height, depth: p.depth, uvst: uvst, filter: p.filter,
		handle: p.handle, serial: p.serial, cmdID: p.cmdID, root: p.root}
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
	r.initCmdBuf()
	LogMessage("Using %v", r.DebugInfo())

	// No MSAA on the WebGL path.
	if sys.msaa > 0 {
		sys.msaa = 0
	}

	r.customShaders = make(map[uint32]*ShaderProgram_WebGL)
	r.customShaderMap = make(map[string]uint32)
	r.nextShaderID = 1
	r.currentProgram = nil
	// Seed the state-mirror caches to the values GL actually starts with, not
	// to Go zero values. DEPTH_WRITEMASK defaults to true in WebGL; claiming
	// false here makes the first SetDepthMask(false) a no-op and leaves depth
	// writes on for the entire session.
	r.depthMask = true

	r.uniformICache = make(map[uint32]int32)
	r.uniformF1Cache = make(map[uint32]float32)
	r.uniformF2Cache = make(map[uint32][2]float32)
	r.uniformF3Cache = make(map[uint32][3]float32)
	r.uniformF4Cache = make(map[uint32][4]float32)
	r.uniformF16Cache = make(map[uint32][16]float32)

	maxUnits := r.glc("getParameter", r.c("MAX_TEXTURE_IMAGE_UNITS")).Int()
	if maxUnits > 16 {
		maxUnits = 16
	}
	r.texCacheTexSerial = make([]uint64, maxUnits)
	r.texCacheLastUsed = make([]uint64, maxUnits)
	r.texCacheTimer = 1

	// VAOs and buffers
	r.spriteVAO = r.glc("createVertexArray")
	r.postVAO = r.glc("createVertexArray")
	r.vertexBuffer = r.glc("createBuffer")
	r.postVertBuffer = r.glc("createBuffer")
	r.spriteVAOID = r.regJS(r.spriteVAO)
	r.vertexBufID = r.regJS(r.vertexBuffer)

	// Post-processing quad
	postVerts := []float32{-1, -1, 1, -1, -1, 1, 1, 1}
	r.glc("bindBuffer", r.c("ARRAY_BUFFER"), r.postVertBuffer)
	r.glc("bufferData", r.c("ARRAY_BUFFER"), r.scratch.floats(postVerts), r.c("STATIC_DRAW"))
	r.glc("bindBuffer", r.c("ARRAY_BUFFER"), js.Null())

	// Sprite shader (the uniform list of render_gl33.go)
	r.spriteShader, _ = r.newShaderProgram(vertShader, fragShader, "", "Main Shader", true, wglSpriteAttribs...)
	r.spriteShader.RegisterAttributes("position", "uv")
	r.spriteShader.RegisterUniforms("modelview", "projection", "x1x2x4x3",
		"alpha", "tint", "mask", "neg", "gray", "add", "mult", "isFlat", "isRgba", "isTrapez", "hue",
		"spriteUV", "palUV", "quad", "useQuad")
	r.spriteShader.RegisterTextures("pal", "tex")

	// Sprite VAO layout
	r.glc("bindVertexArray", r.spriteVAO)
	r.glc("bindBuffer", r.c("ARRAY_BUFFER"), r.vertexBuffer)
	locPos := r.spriteShader.attributes["position"]
	r.glc("enableVertexAttribArray", locPos)
	r.glc("vertexAttribPointer", locPos, 2, r.c("FLOAT"), false, 16, 0)
	locUV := r.spriteShader.attributes["uv"]
	r.glc("enableVertexAttribArray", locUV)
	r.glc("vertexAttribPointer", locUV, 2, r.c("FLOAT"), false, 16, 8)
	r.glc("bindVertexArray", js.Null())

	// Post-processing shaders (external ones + trailing identity shader)
	r.postShaderSelect = make([]*ShaderProgram_WebGL, len(sys.cfg.Video.ExternalShaders)+1)
	r.glc("bindVertexArray", r.postVAO)
	r.glc("bindBuffer", r.c("ARRAY_BUFFER"), r.postVertBuffer)
	for i := 0; i < len(sys.cfg.Video.ExternalShaders); i++ {
		r.postShaderSelect[i], _ = r.newShaderProgram(string(sys.externalShaders[0][i]),
			string(sys.externalShaders[1][i]), "", fmt.Sprintf("Postprocess Shader #%v", i), true)
		r.postShaderSelect[i].RegisterAttributes("VertCoord")
		r.postShaderSelect[i].RegisterUniforms("Texture_GL33", "TextureSize", "CurrentTime")
		if loc, ok := r.postShaderSelect[i].attributes["VertCoord"]; ok && loc >= 0 {
			r.glc("enableVertexAttribArray", loc)
			r.glc("vertexAttribPointer", loc, 2, r.c("FLOAT"), false, 0, 0)
		}
	}
	identShader, _ := r.newShaderProgram(identVertShader, identFragShader, "", "Identity Postprocess", true)
	identShader.RegisterAttributes("VertCoord")
	// ident.vert places the final image with this (EndFrame); unset it is
	// vec4(0) and every vertex collapses to the center: a black screen.
	identShader.RegisterUniforms("OutputTransform")
	if loc, ok := identShader.attributes["VertCoord"]; ok && loc >= 0 {
		r.glc("enableVertexAttribArray", loc)
		r.glc("vertexAttribPointer", loc, 2, r.c("FLOAT"), false, 0, 0)
	}
	r.postShaderSelect[len(r.postShaderSelect)-1] = identShader
	r.glc("bindVertexArray", js.Null())
	r.glc("bindBuffer", r.c("ARRAY_BUFFER"), js.Null())

	// Screen-sized targets (scene framebuffer, ping-pong post-processing
	// framebuffers, grab-pass texture) at the render target size.
	if err := r.sizeRenderTargets(sys.renderTargetRect[2], sys.renderTargetRect[3]); err != nil {
		LogMessage("WebGL: %v", err)
	}

	// 3D model pipeline (render_webgl_model.go); turns itself off on failure.
	r.initModel()
}

func (r *Renderer_WebGL) Close() {}

// sizeRenderTargets gives every screen-sized target its storage: the scene
// framebuffer's color texture and depth buffer, both post-processing
// ping-pong textures and the grab-pass texture. The GL objects are made on
// the first call; later calls re-specify their storage in place (the grab
// texture keeps its Go object, which other code holds).
func (r *Renderer_WebGL) sizeRenderTargets(width, height int32) error {
	tex2D := r.c("TEXTURE_2D")
	first := r.fbo.IsUndefined()
	r.SetActiveTexture0()
	screenTexture := func(t js.Value) {
		r.glc("bindTexture", tex2D, t)
		r.glc("texParameteri", tex2D, r.c("TEXTURE_MAG_FILTER"), r.c("NEAREST"))
		r.glc("texParameteri", tex2D, r.c("TEXTURE_MIN_FILTER"), r.c("NEAREST"))
		r.glc("texParameteri", tex2D, r.c("TEXTURE_WRAP_S"), r.c("CLAMP_TO_EDGE"))
		r.glc("texParameteri", tex2D, r.c("TEXTURE_WRAP_T"), r.c("CLAMP_TO_EDGE"))
		r.glc("texImage2D", tex2D, 0, r.c("RGBA"), width, height, 0,
			r.c("RGBA"), r.c("UNSIGNED_BYTE"), js.Null())
	}

	// Grab-pass texture
	if r.grabTexture == nil {
		grab, _ := r.newTexture(width, height, 32, true)
		r.grabTexture = grab.(*Texture_WebGL)
	}
	r.grabTexture.width, r.grabTexture.height = width, height
	r.grabTexture.SetData(nil)

	// Main offscreen framebuffer: RGBA color texture + depth renderbuffer
	if first {
		r.fboTexture = r.glc("createTexture")
		r.rboDepth = r.glc("createRenderbuffer")
	}
	screenTexture(r.fboTexture)
	r.glc("bindRenderbuffer", r.c("RENDERBUFFER"), r.rboDepth)
	r.glc("renderbufferStorage", r.c("RENDERBUFFER"), r.c("DEPTH_COMPONENT24"), width, height)
	r.glc("bindRenderbuffer", r.c("RENDERBUFFER"), js.Null())
	if first {
		r.fbo = r.glc("createFramebuffer")
		r.glc("bindFramebuffer", r.c("FRAMEBUFFER"), r.fbo)
		r.glc("framebufferTexture2D", r.c("FRAMEBUFFER"), r.c("COLOR_ATTACHMENT0"), tex2D, r.fboTexture, 0)
		r.glc("framebufferRenderbuffer", r.c("FRAMEBUFFER"), r.c("DEPTH_ATTACHMENT"), r.c("RENDERBUFFER"), r.rboDepth)
	}

	// Ping-pong post-processing framebuffers
	if first {
		r.fboPP = make([]js.Value, 2)
		r.fboPPTexture = make([]js.Value, 2)
	}
	for i := 0; i < 2; i++ {
		if first {
			r.fboPPTexture[i] = r.glc("createTexture")
		}
		screenTexture(r.fboPPTexture[i])
		if first {
			r.fboPP[i] = r.glc("createFramebuffer")
			r.glc("bindFramebuffer", r.c("FRAMEBUFFER"), r.fboPP[i])
			r.glc("framebufferTexture2D", r.c("FRAMEBUFFER"), r.c("COLOR_ATTACHMENT0"), tex2D, r.fboPPTexture[i], 0)
		}
	}
	r.glc("bindTexture", tex2D, js.Null())

	var err error
	r.glc("bindFramebuffer", r.c("FRAMEBUFFER"), r.fbo)
	if st := r.glc("checkFramebufferStatus", r.c("FRAMEBUFFER")).Int(); st != r.ci("FRAMEBUFFER_COMPLETE") {
		err = fmt.Errorf("framebuffer incomplete at %dx%d: 0x%x", width, height, st)
	}
	r.glc("bindFramebuffer", r.c("FRAMEBUFFER"), js.Null())
	// The scene framebuffer's size changed: the next SetViewport must emit.
	r.viewportSet = false
	return err
}

// ResizeRenderTargets is modern-input's instant resolution change: every
// screen-sized target is reallocated at the new size (render_gl33.go's
// contract). The browser has no MSAA path, so any sample count is refused.
func (r *Renderer_WebGL) ResizeRenderTargets(width, height, samples int32) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("invalid rendering resolution %dx%d", width, height)
	}
	if samples > 0 {
		return fmt.Errorf("MSAA is not available in the browser")
	}
	return r.sizeRenderTargets(width, height)
}

// ReloadPostShaders swaps the external post-processing chain at run time
// (render_gl33.go's version on WebGL). The identity shader stays last; a
// failed compile keeps the running chain.
func (r *Renderer_WebGL) ReloadPostShaders(src [][][]byte) error {
	if len(src) != 2 || len(src[0]) != len(src[1]) {
		return fmt.Errorf("invalid post shader chain")
	}
	fresh := make([]*ShaderProgram_WebGL, 0, len(src[0])+1)
	for i := range src[0] {
		p, err := r.newShaderProgram(string(src[0][i]), string(src[1][i]), "", fmt.Sprintf("Postprocess Shader #%d", i), false)
		if err != nil {
			for _, f := range fresh {
				r.glc("deleteProgram", f.program)
			}
			return fmt.Errorf("post shader %d: %w", i+1, err)
		}
		p.RegisterAttributes("VertCoord")
		p.RegisterUniforms("Texture_GL33", "TextureSize", "CurrentTime")
		fresh = append(fresh, p)
	}
	identity := r.postShaderSelect[len(r.postShaderSelect)-1]
	r.glc("bindVertexArray", r.postVAO)
	r.glc("bindBuffer", r.c("ARRAY_BUFFER"), r.postVertBuffer)
	for _, p := range fresh {
		if loc, ok := p.attributes["VertCoord"]; ok && loc >= 0 {
			r.glc("enableVertexAttribArray", loc)
			r.glc("vertexAttribPointer", loc, 2, r.c("FLOAT"), false, 0, 0)
		}
	}
	r.glc("bindVertexArray", js.Null())
	r.glc("bindBuffer", r.c("ARRAY_BUFFER"), js.Null())
	for _, p := range r.postShaderSelect[:len(r.postShaderSelect)-1] {
		if p != nil {
			r.glc("deleteProgram", p.program)
		}
	}
	r.postShaderSelect = append(fresh, identity)
	// The program in use may have been deleted.
	r.programID = 0
	return nil
}

// SetModelOptions is modern-input's runtime 3D model toggle. WebGL has no
// shadow pass, so shadow stays off; the model pipeline is built on the first
// enable and kept for later toggles.
func (r *Renderer_WebGL) SetModelOptions(model, shadow bool) error {
	m := &r.model
	if !model {
		m.enabled = false
		return nil
	}
	if m.enabled {
		return nil
	}
	if !m.built {
		if err := r.initModelPipeline(); err != nil {
			r.wglWarn("WebGL: 3D models disabled: " + err.Error())
			return err
		}
		m.built = true
	}
	m.enabled = true
	return nil
}

// SetViewport is the area of the render target a pass draws to (the fight's
// or the menus' aspect inside it, modern-input's drawViewport), given from
// the top-left like render_gl33.go and flipped to GL's bottom-left origin.
func (r *Renderer_WebGL) SetViewport(x, y, width, height int32) {
	r.glViewport(x, sys.renderTargetRect[3]-(y+height), width, height)
}

// glViewport emits gl.viewport through the command buffer unless it is the
// viewport already set. Every viewport change goes through here, so the
// cache always matches GL.
func (r *Renderer_WebGL) glViewport(x, y, width, height int32) {
	rect := [4]int32{x, y, width, height}
	if r.viewportSet && rect == r.viewportRect {
		return
	}
	r.need(5)
	r.emitI(wcViewport)
	r.emitI(x)
	r.emitI(y)
	r.emitI(width)
	r.emitI(height)
	r.viewportRect, r.viewportSet = rect, true
}

func (r *Renderer_WebGL) BeginFrame(clearColor bool) {
	// A clear obeys the scissor rect, so a deferred disable must land first or
	// only the last sprite's window gets cleared.
	r.DisableScissor()
	r.flushScissorDisable()
	r.glc("bindFramebuffer", r.c("FRAMEBUFFER"), r.fbo)
	viewport := sys.currentDrawViewport()
	r.SetViewport(viewport[0], viewport[1], viewport[2], viewport[3])
	if clearColor {
		r.glc("clear", r.ci("COLOR_BUFFER_BIT")|r.ci("DEPTH_BUFFER_BIT"))
	} else {
		r.glc("clear", r.ci("DEPTH_BUFFER_BIT"))
	}
}

func (r *Renderer_WebGL) EndFrame() {
	if len(r.fboPP) == 0 {
		return
	}
	width, height := sys.renderTargetRect[2], sys.renderTargetRect[3]
	now := jsPerformance.Call("now").Float()

	scaleMode := r.c("NEAREST")
	if sys.cfg.Video.WindowScaleMode {
		scaleMode = r.c("LINEAR")
	}

	// The post-processing passes cover the whole render target, unclipped.
	r.DisableScissor()
	r.flushScissorDisable()
	r.glViewport(0, 0, width, height)
	for i := 0; i < 2; i++ {
		r.glc("bindFramebuffer", r.c("FRAMEBUFFER"), r.fboPP[i])
		r.glc("clear", r.ci("COLOR_BUFFER_BIT"))
	}
	r.SetActiveTexture0()

	r.DisableBlending()
	r.SetDepthTest(false)
	r.SetDepthMask(false)

	for i := 0; i < len(r.postShaderSelect); i++ {
		postShader := r.postShaderSelect[i]
		r.ChangeProgram(postShader)
		r.glc("bindVertexArray", r.postVAO)

		if i%2 == 0 {
			r.glc("bindFramebuffer", r.c("FRAMEBUFFER"), r.fboPP[0])
			if i == 0 {
				r.glc("bindTexture", r.c("TEXTURE_2D"), r.fboTexture)
			} else {
				r.glc("bindTexture", r.c("TEXTURE_2D"), r.fboPPTexture[1])
			}
		} else {
			r.glc("bindFramebuffer", r.c("FRAMEBUFFER"), r.fboPP[1])
			r.glc("bindTexture", r.c("TEXTURE_2D"), r.fboPPTexture[0])
		}

		if i >= len(r.postShaderSelect)-1 {
			// Last pass: the identity shader draws over the whole canvas and
			// places the image with OutputTransform (scale xy, clip-space offset
			// zw), as render_gl33.go does; the old viewport-only placement leaves
			// ident.vert's transform at zero, which draws nothing.
			x, y, w, h := sys.window.GetScaledViewportSize()
			dw, dh := sys.window.GetDrawablePixelSize()
			r.glViewport(0, 0, dw, dh)
			if idx := postShader.uniformIndex("OutputTransform"); idx >= 0 && dw > 0 && dh > 0 {
				fw, fh := float32(dw), float32(dh)
				r.SetUniformFSub(postShader, idx, w/fw, h/fh, (2*x+w)/fw-1, 1-(2*y+h)/fh)
			}
			r.glc("bindFramebuffer", r.c("FRAMEBUFFER"), js.Null())
			r.glc("clear", r.ci("COLOR_BUFFER_BIT")|r.ci("DEPTH_BUFFER_BIT"))
			// Clip the picture to the fight, menus and bezel art, so a post
			// shader leaves the black bars alone (render_gl33.go). Set through
			// the scissor mirror, so the deferred-disable state stays true.
			clip := sys.window.GetPresentationScissor()
			r.setScissor(clip[0], dh-clip[1]-clip[3], clip[2], clip[3])
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

		r.glc("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MAG_FILTER"), scaleMode)
		r.glc("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MIN_FILTER"), scaleMode)

		r.glc("drawArrays", r.c("TRIANGLE_STRIP"), 0, 4)
	}
	// Turn off the clipping used for the final on-screen pass.
	r.DisableScissor()
	r.flushScissorDisable()
	// Unbind the post-process texture so it is not still the active sampler
	// when the NEXT frame renders into the scene FBO - that pairing is what
	// makes some drivers spam "Feedback loop formed between Framebuffer and
	// active Texture" every frame (harmless per the spec, but the driver-side
	// error handling is a real per-frame cost on the machines that report it).
	r.glc("bindTexture", r.c("TEXTURE_2D"), js.Null())

	// A screenshot asked for by BeginScreenshot is read here, in the same
	// task that drew the frame: see BeginScreenshot.
	if r.shotWant {
		r.shotWant = false
		r.shotData = r.shotData[:4*r.shotW*r.shotH]
		r.ReadPixels(r.shotData, r.shotW, r.shotH)
		r.shotPending = true
	}
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
	case POINTS:
		return r.c("POINTS")
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
		r.need(4)
		r.emitI(wcEnable)
		r.emitI(int32(r.ci("DEPTH_TEST")))
		r.emitI(wcDepthFunc)
		r.emitI(int32(r.ci("LESS")))
	} else {
		r.need(2)
		r.emitI(wcDisable)
		r.emitI(int32(r.ci("DEPTH_TEST")))
	}
}

func (r *Renderer_WebGL) SetDepthMask(depthMask bool) {
	if depthMask == r.depthMask {
		return
	}
	r.depthMask = depthMask
	r.need(2)
	r.emitI(wcDepthMask)
	r.emitI(int32(Btoi(depthMask)))
}

func (r *Renderer_WebGL) ChangeProgram(prog *ShaderProgram_WebGL) {
	if prog == nil || r.programID == prog.id {
		return
	}
	r.need(2)
	r.emitI(wcUseProgram)
	r.emitI(prog.cmdID)
	r.programID = prog.id
	for i := range r.texCacheTexSerial {
		r.texCacheTexSerial[i] = 0
		r.texCacheLastUsed[i] = 0
	}
	r.texCacheTimer = 1
}

func (r *Renderer_WebGL) EnableBlending(eq BlendEquation, src, dst BlendFunc) {
	if !r.blendEnabled {
		r.need(2)
		r.emitI(wcEnable)
		r.emitI(int32(r.ci("BLEND")))
		r.blendEnabled = true
	}
	if eq != r.blendEquation {
		r.blendEquation = eq
		r.need(2)
		r.emitI(wcBlendEq)
		r.emitI(int32(r.mapBlendEquation(eq).Int()))
	}
	if src != r.blendSrc || dst != r.blendDst {
		r.blendSrc = src
		r.blendDst = dst
		r.need(3)
		r.emitI(wcBlendFunc)
		r.emitI(int32(r.mapBlendFunction(src).Int()))
		r.emitI(int32(r.mapBlendFunction(dst).Int()))
	}
}

func (r *Renderer_WebGL) DisableBlending() {
	if r.blendEnabled {
		r.need(2)
		r.emitI(wcDisable)
		r.emitI(int32(r.ci("BLEND")))
		r.blendEnabled = false
	}
}

func (r *Renderer_WebGL) ReadPixels(data []uint8, width, height int) {
	r.glc("bindFramebuffer", r.c("READ_FRAMEBUFFER"), js.Null())
	r.scratch.ensure(len(data))
	dst := r.scratch.u8.Call("subarray", 0, len(data))
	r.glc("readPixels", 0, 0, width, height, r.c("RGBA"), r.c("UNSIGNED_BYTE"), dst)
	js.CopyBytesToGo(data, dst)
}

// BeginScreenshot only books the capture: the NEXT EndFrame reads the canvas
// right after drawing it, and FinishScreenshot (called by the following
// System.await) hands the pixels over, bottom-up as GL's. The canvas cannot be
// read here: System.await calls this from a deferred function, after its frame
// wait has yielded to the browser, and with preserveDrawingBuffer off the
// presented frame is gone by then (the read came back all black). WebGL2 has
// no pixel-pack buffer mapping to wait on instead.
func (r *Renderer_WebGL) BeginScreenshot(width, height int) {
	r.shotPending = false
	n := 4 * width * height
	if n <= 0 {
		r.shotWant = false
		return
	}
	if cap(r.shotData) < n {
		r.shotData = make([]byte, n)
	}
	r.shotW, r.shotH, r.shotWant = width, height, true
}

func (r *Renderer_WebGL) FinishScreenshot(data []uint8, width, height int) bool {
	n := 4 * width * height
	if !r.shotPending || width != r.shotW || height != r.shotH || len(data) < n || len(r.shotData) < n {
		return false
	}
	copy(data, r.shotData[:n])
	r.shotPending = false
	return true
}

func (r *Renderer_WebGL) EnableScissor(x, y, width, height int32) {
	// Game pixels to render target pixels (the draw viewport), then GL's
	// bottom-left origin.
	rect := sys.viewportScissor([4]int32{x, y, width, height})
	r.setScissor(rect[0], sys.renderTargetRect[3]-(rect[1]+rect[3]), rect[2], rect[3])
}

// setScissor turns the scissor test on with a rectangle in GL pixels of the
// bound framebuffer, through the mirrors (scissorEnabled, scissorRect,
// scissorPending).
func (r *Renderer_WebGL) setScissor(x, realY, width, height int32) {
	// We want scissor ON, so cancel any deferred disable outright rather than
	// emitting a disable/enable pair.
	r.scissorPending = false
	if r.scissorEnabled &&
		r.scissorRect[0] == x && r.scissorRect[1] == realY &&
		r.scissorRect[2] == width && r.scissorRect[3] == height {
		return
	}
	if !r.scissorEnabled {
		r.need(2)
		r.emitI(wcEnable)
		r.emitI(int32(r.ci("SCISSOR_TEST")))
		r.scissorEnabled = true
	}
	r.need(5)
	r.emitI(wcScissor)
	r.emitI(x)
	r.emitI(realY)
	r.emitI(width)
	r.emitI(height)
	r.scissorRect = [4]int32{x, realY, width, height}
}

// DEFERRED. Records the intent without emitting; see scissorPending. The GL
// state only changes when flushScissorDisable runs, which every operation that
// genuinely needs an unclipped target does.
func (r *Renderer_WebGL) DisableScissor() {
	r.scissorPending = true
}

// Emit the deferred disable, if one is outstanding. Called before anything that
// must not be clipped by a leftover sprite window: frame clears, the post-
// processing passes, and text.
func (r *Renderer_WebGL) flushScissorDisable() {
	if !r.scissorPending {
		return
	}
	r.scissorPending = false
	if r.scissorEnabled {
		r.need(2)
		r.emitI(wcDisable)
		r.emitI(int32(r.ci("SCISSOR_TEST")))
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
	// Dedup for EVERY program, not just the sprite shader: GL uniform state is
	// per-program and persists across program switches, and the cache key is
	// namespaced by program id. Text rendering repeats identical uniform sets
	// per line per frame (a screen of TrueType text = hundreds of redundant
	// uniform calls a second, each an allocating js bridge crossing).
	{
		key := (prog.id << 16) | uint32(idx)
		if old, exists := r.uniformICache[key]; exists && old == val {
			return
		}
		r.uniformICache[key] = val
	}
	if cid := prog.uniformCmdIDs[idx]; cid >= 0 {
		r.need(3)
		r.emitI(wcUniform1i)
		r.emitI(cid)
		r.emitI(val)
		return
	}
	r.glc("uniform1i", loc, val)
}

func (r *Renderer_WebGL) SetUniformFSub(prog *ShaderProgram_WebGL, idx int, values ...float32) {
	if idx < 0 || idx >= len(prog.uniformLocs) || len(values) == 0 {
		return
	}
	loc := prog.uniformLocs[idx]
	if !loc.Truthy() {
		return
	}
	// Dedup for every program - see SetUniformISub.
	{
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
	if cid := prog.uniformCmdIDs[idx]; cid >= 0 && len(values) <= 4 {
		r.need(2 + len(values))
		r.emitI(wcUniform1f + int32(len(values)) - 1) // ops 3..6 are 1f..4f
		r.emitI(cid)
		for _, v := range values {
			r.emitF(v)
		}
		return
	}
	switch len(values) {
	case 1:
		r.glc("uniform1f", loc, values[0])
	case 2:
		r.glc("uniform2f", loc, values[0], values[1])
	case 3:
		r.glc("uniform3f", loc, values[0], values[1], values[2])
	case 4:
		r.glc("uniform4f", loc, values[0], values[1], values[2], values[3])
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
			if cid := prog.uniformCmdIDs[idx]; cid >= 0 {
				r.need(10)
				r.emitI(wcUniform4fv8)
				r.emitI(cid)
				for _, v := range values {
					r.emitF(v)
				}
				return
			}
			r.glc("uniform4fv", loc, r.scratch.floats(values))
		} else {
			r.glc("uniform1fv", loc, r.scratch.floats(values))
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
	if cid := r.currentProgram.uniformCmdIDs[idx]; cid >= 0 && len(value) == 16 {
		r.need(18)
		r.emitI(wcUniformMat4)
		r.emitI(cid)
		for _, v := range value {
			r.emitF(v)
		}
		return
	}
	r.glc("uniformMatrix4fv", loc, false, r.scratch.floats(value))
}

func (r *Renderer_WebGL) SetActiveTexture0() {
	r.need(2)
	r.emitI(wcActiveTex)
	r.emitI(0)
	if len(r.texCacheTexSerial) > 0 {
		r.texCacheTexSerial[0] = 0
		r.texCacheLastUsed[0] = 0
	}
}

// uvUniform sends the atlas rectangle that goes with sampler name.
func (r *Renderer_WebGL) uvUniform(prog *ShaderProgram_WebGL, name string, t *Texture_WebGL) {
	if !prog.uvIdxSet {
		prog.uvIdx = [4]int{prog.uniformIndex("spriteUV"), prog.uniformIndex("palUV"),
			prog.uniformIndex("tex1UV"), prog.uniformIndex("tex2UV")}
		prog.paramIdx = prog.uniformIndex("param")
		prog.uvIdxSet = true
	}
	i := -1
	switch name {
	case "tex":
		i = 0
	case "pal":
		i = 1
	case "tex1":
		i = 2
	case "tex2":
		i = 3
	}
	if i < 0 || prog.uvIdx[i] < 0 {
		return
	}
	r.SetUniformFSub(prog, prog.uvIdx[i], t.uvst[0], t.uvst[1], t.uvst[2], t.uvst[3])
}

func (r *Renderer_WebGL) SetTexture(name string, tex Texture) {
	if r.currentProgram == nil {
		return
	}
	prog := r.currentProgram
	t := tex.(*Texture_WebGL)
	idx := prog.uniformIndex(name)
	// An atlas sprite samples its own rectangle: without spriteUV the shader
	// maps every sprite over the whole page (0,0,0,0 divides by zero).
	r.uvUniform(prog, name, t)

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
		r.need(3)
		r.emitI(wcActiveBind)
		r.emitI(oldestUnit)
		r.emitI(t.cmdID)
		r.texCacheTexSerial[oldestUnit] = t.serial
		r.texCacheLastUsed[oldestUnit] = r.texCacheTimer
		r.SetUniformISub(prog, idx, oldestUnit)
		return
	}

	fixedUnit := prog.textures[name]
	r.need(3)
	r.emitI(wcActiveBind)
	r.emitI(int32(fixedUnit))
	r.emitI(t.cmdID)
	r.SetUniformISub(prog, idx, int32(fixedUnit))
}

func (r *Renderer_WebGL) SetVertexData(values ...float32) {
	r.need(3 + len(values))
	r.emitI(wcVertexData)
	r.emitI(r.vertexBufID)
	r.emitI(int32(len(values)))
	for _, v := range values {
		r.emitF(v)
	}
}

// SetQuadVertices: one quad as 4 vertices of x, y, u, v. Option (a) of the
// port plan: it goes through the vertex buffer like any other draw, and
// useQuad stays false (the uniform-quad path is a later, measured change).
func (r *Renderer_WebGL) SetQuadVertices(v *[16]float32) {
	r.SetVertexData(v[:]...)
}

func (r *Renderer_WebGL) RenderQuad() {
	r.RenderQuadBatch(4)
}

func (r *Renderer_WebGL) RenderQuadBatch(vertexCount int32) {
	r.need(4)
	r.emitI(wcDrawArrays)
	r.emitI(int32(r.ci("TRIANGLE_STRIP")))
	r.emitI(0)
	r.emitI(vertexCount)
}

func (r *Renderer_WebGL) LoadCustomSpriteShader(shaderName string, shaderData []byte) uint32 {
	if id, ok := r.customShaderMap[shaderName]; ok {
		return id
	}
	fragSource := string(shaderData)
	shader, err := r.newShaderProgram(vertShader, fragSource, "", "Custom: "+shaderName, false, wglSpriteAttribs...)
	if err != nil {
		LogMessage("[WebGL] Failed to compile custom shader %s: %v", shaderName, err)
		return 0
	}
	shader.RegisterAttributes("position", "uv")
	shader.RegisterUniforms("modelview", "projection", "x1x2x4x3",
		"alpha", "tint", "mask", "neg", "gray", "add", "mult", "isFlat", "isRgba", "isTrapez", "hue",
		"spriteUV", "palUV", "tex1UV", "tex2UV", "quad", "useQuad",
		"iTime", "iResolution", "aspectRatio", "sTime", "param")
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
			r.glc("deleteProgram", shader.program)
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
		r.need(2)
		r.emitI(wcBindVAO)
		r.emitI(r.spriteVAOID)
	}
}

// SetCustomUniforms sends a custom shader's "uniform float param[16]" as one
// uniform1fv through the command buffer (it replaced p0..p15 upstream).
func (r *Renderer_WebGL) SetCustomUniforms(params [16]float32) {
	prog := r.currentProgram
	if prog == nil {
		return
	}
	idx := prog.paramIdx
	if !prog.uvIdxSet {
		idx = prog.uniformIndex("param")
	}
	if idx < 0 || idx >= len(prog.uniformCmdIDs) {
		return
	}
	cid := prog.uniformCmdIDs[idx]
	if cid < 0 {
		return
	}
	key := (prog.id << 16) | uint32(idx)
	if old, ok := r.uniformF16Cache[key]; ok && old == params {
		return
	}
	r.uniformF16Cache[key] = params
	r.need(18)
	r.emitI(wcUniform1fv16)
	r.emitI(cid)
	for _, v := range params {
		r.emitF(v)
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
	r.glc("bindTexture", r.c("TEXTURE_2D"), r.grabTexture.handle)
	r.glc("bindFramebuffer", r.c("READ_FRAMEBUFFER"), r.fbo)
	r.glc("copyTexSubImage2D", r.c("TEXTURE_2D"), 0, 0, 0, 0, 0, r.grabTexture.width, r.grabTexture.height)
	r.glc("bindFramebuffer", r.c("FRAMEBUFFER"), r.fbo)
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
