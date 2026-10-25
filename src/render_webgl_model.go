//go:build js

package main

// 3D model (glTF) pipeline for the WebGL2 renderer: a port of the model and
// image-based-lighting paths of render_gles32.go (OpenGL ES 3.2, the closest
// API to WebGL2's ES 3.0) and render_gl33.go.
//
// What is not ported, on purpose:
//   - Shadows. The shadow pass needs a geometry shader and a cube-map-array
//     depth texture; WebGL2 has neither. IsShadowEnabled is false, the shadow
//     methods are no-ops and the model shader is compiled WITHOUT
//     ENABLE_SHADOW, so it never declares samplerCubeArray.
//
// Differences from the GL backends:
//   - Environment lighting (panorama -> cube map -> filtered cube maps + GGX
//     LUT) renders into RGBA16F targets. A float color attachment needs
//     EXT_color_buffer_float (or EXT_color_buffer_half_float) in WebGL2. Without
//     either, models still draw, without environment lighting, after one
//     console warning. The HDR panorama is stored as RGBA16F rather than
//     RGBA32F: half float is LINEAR-filterable in core WebGL2, 32-bit float
//     needs OES_texture_float_linear. Data textures (joint matrices, morph
//     targets, the LUT) are sampled NEAREST, as upstream.
//   - The model draws in its own VAO, and every sampler of the model shader
//     has a texture of the right target bound at prepare time (a 1x1 dummy
//     until a material binds its own): WebGL warns and samples black for an
//     empty unit, and raises INVALID_OPERATION when a 2D and a cube sampler
//     share a unit.
//   - Any setup failure (shader compile/link, too few texture units) turns the
//     pipeline off: IsModelEnabled returns false and stage.go keeps the stage's
//     2D layers without the model, as before this port.
//   - The textures this pipeline creates are deleted when the Go object is
//     collected (the GL backends' finalizer contract, render_gl33.go). The
//     WebGL renderer's 2D textures still are not; that is unchanged.
//
// Every hot-path call goes through the command buffer (render_webgl_cmdbuf.go).
// The 2D sprite/post pipeline relies on the state mirrors in Renderer_WebGL.
// The GL state the model pipeline changes, and why 2D drawing after it is safe:
//   - Blend, depth test/mask, scissor and program: through those mirrors.
//     Blend and depth are saved at the first prepare and put back by
//     ReleaseModelPipeline; a pending scissor disable is flushed first.
//   - Cull face, front face, VAO and the active texture unit: put back by
//     ReleaseModelPipeline (off, CCW, none, unit 0).
//   - Framebuffer and viewport: the model draw binds the scene framebuffer at
//     full size, which is what 2D draws into between BeginFrame and EndFrame;
//     the environment passes put both back when they finish.
//   - ARRAY_BUFFER and ELEMENT_ARRAY_BUFFER: the element binding lives in the
//     model VAO; the 2D paths bind their own vertex buffer before every upload
//     (wcVertexData, wcSubData).
//   - Texture bindings on units 0-9 (2D and cube): the program is left on the
//     model shader, and every program change resets the sprite pipeline's
//     texture-unit cache, so the sprite, font and post paths bind their own.
//   - Generic vertex attribute values (vertexAttrib4f on disabled model
//     attributes): the 2D VAOs enable every attribute their shaders read, so
//     a generic value is never read there.
// A 2D change that relies on any of this state carrying over must revisit it.

import (
	"encoding/binary"
	"math"
	"runtime"
	"strings"
	"syscall/js"
	"unsafe"

	mgl "github.com/go-gl/mathgl/mgl32"
)

// Model vertex attributes, in the order SetModelPipeline lays them out in the
// vertex buffer.
const (
	wmaVertexId = iota
	wmaPosition
	wmaUV
	wmaNormal
	wmaTangent
	wmaVertColor
	wmaJoints0
	wmaWeights0
	wmaJoints1
	wmaWeights1
	wmaOutline
	wmaCount
)

var wglModelAttrNames = [wmaCount]string{"inVertexId", "position", "uv", "normalIn", "tangentIn",
	"vertColor", "joints_0", "weights_0", "joints_1", "weights_1", "outlineAttributeIn"}

// The value a disabled attribute reads, as render_gles32.go's
// ReleaseModelPipeline leaves it (VertexAttrib2f/3f fill w with 1).
var wglModelAttrDefault = [wmaCount][4]float32{
	{0, 0, 0, 1}, {0, 0, 0, 1}, {0, 0, 0, 1}, {0, 0, 0, 1}, {0, 0, 0, 0},
	{1, 1, 1, 1}, {0, 0, 0, 0}, {0, 0, 0, 0}, {0, 0, 0, 0}, {0, 0, 0, 0}, {0, 0, 0, 0}}

// The model shader's samplers by target. RegisterTextures gives each its unit
// in registration order (tex 0 ... GGXLUT 9), every one distinct.
var wglModel2DSamplers = []string{"tex", "morphTargetValues", "jointMatrices", "normalMap",
	"metallicRoughnessMap", "ambientOcclusionMap", "emissionMap", "GGXLUT"}
var wglModelCubeSamplers = []string{"lambertianEnvSampler", "GGXEnvSampler"}

type wglModel struct {
	enabled bool
	// Float color attachments are available, so environment lighting renders.
	envOK bool

	shader   *ShaderProgram_WebGL
	panorama *ShaderProgram_WebGL
	filter   *ShaderProgram_WebGL

	vao, panoramaVAO, filterVAO js.Value
	vaoID, nullID               int32
	vbuf, ibuf                  [2]js.Value
	vbufID, ibufID              [2]int32
	fboEnv                      js.Value
	dummy2D, dummyCube          *Texture_WebGL

	attrLoc [wmaCount]int32
	attrOn  [wmaCount]bool

	// GL state the 2D pipeline does not mirror: CULL_FACE and frontFace.
	cullOn  bool
	frontCW bool

	// State saved by prepareModelPipeline and put back by ReleaseModelPipeline.
	active       bool
	savedBlend   bool
	savedEq      BlendEquation
	savedSrc     BlendFunc
	savedDst     BlendFunc
	savedDepth   bool
	savedDepthWr bool

	// This draw's model has an environment the renderer could not build, and
	// whether the draw set any light (see SetModelUniformI).
	envMissing bool
	lightsSet  bool

	// MAX_TEXTURE_SIZE, read on the first model texture (see SetDataG); -1 if
	// the GPU did not say.
	maxTex int32

	// Per-primitive lookups (see modelSlot and modelMatrix): uniform names by
	// their string's address, and the value each matrix uniform holds. Both
	// belong to slotsFor, the model shader they were read from.
	slotsFor *ShaderProgram_WebGL
	slots    [256]wglUniformSlot
	mats     [][16]float32
	matSet   []bool

	warned map[string]bool
}

// wglUniformSlot is one model-shader uniform name, looked up once.
type wglUniformSlot struct {
	name string
	set  bool
	idx  int // uniform index, -1 if the shader has no such uniform
	unit int // texture unit if it is a sampler, else -1
}

// WebGL enums the per-primitive paths emit. r.ci resolves a name through a
// string map, and SetModelPipeline and SetModelTexture ran it about a dozen
// times per primitive. Values from the WebGL 2 spec; the command interpreter
// hardcodes some the same way.
const (
	wglINT          = 0x1404
	wglFLOAT        = 0x1406
	wglTEXTURE_2D   = 0x0DE1
	wglTEXTURE_CUBE = 0x8513
)

// wglWarn prints a warning to the browser console once per message.
func (r *Renderer_WebGL) wglWarn(msg string) {
	if r.model.warned == nil {
		r.model.warned = make(map[string]bool)
	}
	if r.model.warned[msg] {
		return
	}
	r.model.warned[msg] = true
	js.Global().Get("console").Call("warn", msg)
}

// The model shaders get their own header: the fragment shader's active
// #extension line must precede every non-preprocessor token in ESSL 3.00, and
// the line itself (cube map arrays, shadow-only) does not exist in WebGL.
// Samplers default to lowp in ES; the HDR environment and the float data
// textures need highp.
const wglModelHeader = "#version 300 es\nprecision highp float;\nprecision highp int;\n" +
	"precision highp sampler2D;\nprecision highp samplerCube;\n"

func wglModelSource(src string) string {
	src = strings.TrimRight(src, "\x00")
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "#extension") {
			lines[i] = ""
		}
	}
	return wglModelHeader + strings.Join(lines, "\n")
}

// initModel sets up the model pipeline, or leaves it off with one console
// warning. Called at the end of Init.
func (r *Renderer_WebGL) initModel() {
	m := &r.model
	m.enabled = false
	if !sys.cfg.Video.EnableModel {
		return
	}
	if err := r.initModelPipeline(); err != nil {
		r.wglWarn("WebGL: 3D models disabled: " + err.Error())
		m.enabled = false
		return
	}
	m.enabled = true
}

func (r *Renderer_WebGL) initModelPipeline() error {
	m := &r.model
	r.glc("getError") // an error from the 2D setup is not this pipeline's
	units := r.glc("getParameter", r.c("MAX_COMBINED_TEXTURE_IMAGE_UNITS")).Int()
	fragUnits := r.glc("getParameter", r.c("MAX_TEXTURE_IMAGE_UNITS")).Int()
	vertUnits := r.glc("getParameter", r.c("MAX_VERTEX_TEXTURE_IMAGE_UNITS")).Int()
	if units < 11 || fragUnits < 8 || vertUnits < 2 {
		return Error("too few texture units")
	}

	var err error
	if m.shader, err = r.buildModelProgram(modelVertShader, modelFragShader, "Model Shader"); err != nil {
		return err
	}
	m.shader.RegisterAttributes(wglModelAttrNames[:]...)
	m.shader.RegisterUniforms("model", "view", "projection", "normalMatrix", "unlit", "baseColorFactor", "add", "mult", "useTexture", "useNormalMap", "useMetallicRoughnessMap", "useEmissionMap", "neg", "gray", "hue",
		"enableAlpha", "alphaThreshold", "numJoints", "morphTargetWeight", "morphTargetOffset", "morphTargetTextureDimension", "numTargets", "numVertices",
		"metallicRoughness", "ambientOcclusionStrength", "emission", "environmentIntensity", "mipCount", "meshOutline",
		"cameraPosition", "environmentRotation", "texTransform", "normalMapTransform", "metallicRoughnessMapTransform", "ambientOcclusionMapTransform", "emissionMapTransform",
		"lightMatrices[0]", "lightMatrices[1]", "lightMatrices[2]", "lightMatrices[3]",
		"lights[0].direction", "lights[0].range", "lights[0].color", "lights[0].intensity", "lights[0].position", "lights[0].innerConeCos", "lights[0].outerConeCos", "lights[0].type", "lights[0].shadowBias", "lights[0].shadowMapFar",
		"lights[1].direction", "lights[1].range", "lights[1].color", "lights[1].intensity", "lights[1].position", "lights[1].innerConeCos", "lights[1].outerConeCos", "lights[1].type", "lights[1].shadowBias", "lights[1].shadowMapFar",
		"lights[2].direction", "lights[2].range", "lights[2].color", "lights[2].intensity", "lights[2].position", "lights[2].innerConeCos", "lights[2].outerConeCos", "lights[2].type", "lights[2].shadowBias", "lights[2].shadowMapFar",
		"lights[3].direction", "lights[3].range", "lights[3].color", "lights[3].intensity", "lights[3].position", "lights[3].innerConeCos", "lights[3].outerConeCos", "lights[3].type", "lights[3].shadowBias", "lights[3].shadowMapFar",
	)
	// Same order as the GL backends: the units are the registration index.
	m.shader.RegisterTextures("tex", "morphTargetValues", "jointMatrices",
		"normalMap", "metallicRoughnessMap", "ambientOcclusionMap", "emissionMap",
		"lambertianEnvSampler", "GGXEnvSampler", "GGXLUT")
	for i, name := range wglModelAttrNames {
		m.attrLoc[i] = int32(m.shader.attributes[name])
	}
	if m.attrLoc[wmaPosition] < 0 {
		return Error("model shader has no position attribute")
	}

	if m.panorama, err = r.buildModelProgram(identVertShader, panoramaToCubeMapFragShader, "Panorama To Cubemap Shader"); err != nil {
		return err
	}
	m.panorama.RegisterAttributes("VertCoord")
	m.panorama.RegisterUniforms("currentFace")
	m.panorama.RegisterTextures("panorama")

	if m.filter, err = r.buildModelProgram(identVertShader, cubemapFilteringFragShader, "Cubemap Filtering Shader"); err != nil {
		return err
	}
	m.filter.RegisterAttributes("VertCoord")
	m.filter.RegisterUniforms("sampleCount", "distribution", "width", "currentFace", "roughness", "intensityScale", "isLUT")
	m.filter.RegisterTextures("cubeMap")

	// Buffers and VAOs. The index buffers get their WebGL buffer type
	// (element array) by a first bind inside the model VAO.
	m.nullID = r.regJS(js.Null())
	m.vao = r.glc("createVertexArray")
	m.vaoID = r.regJS(m.vao)
	r.glc("bindVertexArray", m.vao)
	for i := 0; i < 2; i++ {
		m.vbuf[i] = r.glc("createBuffer")
		m.vbufID[i] = r.regJS(m.vbuf[i])
		r.glc("bindBuffer", r.c("ARRAY_BUFFER"), m.vbuf[i])
		m.ibuf[i] = r.glc("createBuffer")
		m.ibufID[i] = r.regJS(m.ibuf[i])
		r.glc("bindBuffer", r.c("ELEMENT_ARRAY_BUFFER"), m.ibuf[i])
	}
	r.glc("bindVertexArray", js.Null())
	r.glc("bindBuffer", r.c("ARRAY_BUFFER"), js.Null())
	// A disabled attribute reads its generic value. Nothing else in this
	// renderer sets generic values, so these hold until a transition changes
	// them.
	for i := 0; i < wmaCount; i++ {
		if loc := m.attrLoc[i]; loc >= 0 {
			d := wglModelAttrDefault[i]
			r.glc("vertexAttrib4f", loc, d[0], d[1], d[2], d[3])
		}
	}
	m.attrOn = [wmaCount]bool{}

	// The environment passes draw the post-process quad; each program gets a
	// VAO so its VertCoord location need not match the other's.
	mkQuadVAO := func(s *ShaderProgram_WebGL) js.Value {
		vao := r.glc("createVertexArray")
		r.glc("bindVertexArray", vao)
		r.glc("bindBuffer", r.c("ARRAY_BUFFER"), r.postVertBuffer)
		if loc, ok := s.attributes["VertCoord"]; ok && loc >= 0 {
			r.glc("enableVertexAttribArray", loc)
			r.glc("vertexAttribPointer", loc, 2, r.c("FLOAT"), false, 0, 0)
		}
		r.glc("bindVertexArray", js.Null())
		r.glc("bindBuffer", r.c("ARRAY_BUFFER"), js.Null())
		return vao
	}
	m.panoramaVAO = mkQuadVAO(m.panorama)
	m.filterVAO = mkQuadVAO(m.filter)
	m.fboEnv = r.glc("createFramebuffer")

	// Rendering into the RGBA16F environment maps. getExtension enables it.
	m.envOK = r.glc("getExtension", "EXT_color_buffer_float").Truthy() ||
		r.glc("getExtension", "EXT_color_buffer_half_float").Truthy()

	// 1x1 placeholders every sampler points at until a material binds its own.
	r.SetActiveTexture0()
	m.dummy2D = r.generateTexture(1, 1, 32, false)
	r.glc("bindTexture", r.c("TEXTURE_2D"), m.dummy2D.handle)
	r.glc("texImage2D", r.c("TEXTURE_2D"), 0, r.c("RGBA"), 1, 1, 0, r.c("RGBA"), r.c("UNSIGNED_BYTE"),
		r.scratch.bytes([]byte{255, 255, 255, 255}))
	r.glc("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MIN_FILTER"), r.c("NEAREST"))
	r.glc("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MAG_FILTER"), r.c("NEAREST"))
	r.glc("bindTexture", r.c("TEXTURE_2D"), js.Null())
	m.dummyCube = r.generateTexture(1, 1, 32, false)
	m.dummyCube.cube = true
	r.glc("bindTexture", r.c("TEXTURE_CUBE_MAP"), m.dummyCube.handle)
	for f := 0; f < 6; f++ {
		r.glc("texImage2D", r.ci("TEXTURE_CUBE_MAP_POSITIVE_X")+f, 0, r.c("RGBA"), 1, 1, 0, r.c("RGBA"), r.c("UNSIGNED_BYTE"),
			r.scratch.bytes([]byte{0, 0, 0, 255}))
	}
	r.glc("texParameteri", r.c("TEXTURE_CUBE_MAP"), r.c("TEXTURE_MIN_FILTER"), r.c("NEAREST"))
	r.glc("texParameteri", r.c("TEXTURE_CUBE_MAP"), r.c("TEXTURE_MAG_FILTER"), r.c("NEAREST"))
	r.glc("bindTexture", r.c("TEXTURE_CUBE_MAP"), js.Null())

	// Sampler uniforms point at their fixed units for the program's lifetime.
	for _, s := range []*ShaderProgram_WebGL{m.shader, m.panorama, m.filter} {
		r.ChangeProgram(s)
		for name, unit := range s.textures {
			r.SetUniformISub(s, s.uniformIndex(name), int32(unit))
		}
	}
	r.flushCmds()
	r.SetActiveTexture0()
	r.flushCmds()

	if err := r.glc("getError").Int(); err != 0 {
		return Error("GL error during setup: " + js.ValueOf(err).Call("toString", 16).String())
	}
	if !m.envOK {
		r.wglWarn("WebGL: no float color buffers (EXT_color_buffer_float); 3D models draw without environment lighting")
	}
	return nil
}

// buildModelProgram compiles and links one of the model pipeline's programs.
// A failure is returned (and the pipeline turns off) instead of raising the
// error dialog the 2D shaders use.
func (r *Renderer_WebGL) buildModelProgram(vert, frag, name string) (*ShaderProgram_WebGL, error) {
	s, _, err := r.buildProgram(wglModelSource(vert), wglModelSource(frag), name)
	if err != nil {
		return nil, Error(name + ": " + err.Error())
	}
	return s, nil
}

func (r *Renderer_WebGL) IsModelEnabled() bool  { return r.model.enabled }
func (r *Renderer_WebGL) IsShadowEnabled() bool { return false }

// ---------------------------------------------------------------------------
// GL state the 2D pipeline does not use

func (r *Renderer_WebGL) setCullFace(doubleSided bool) {
	on := !doubleSided
	if on == r.model.cullOn {
		return
	}
	r.model.cullOn = on
	r.need(2)
	if on {
		r.emitI(wcEnable) // cullFace stays at its default, BACK
	} else {
		r.emitI(wcDisable)
	}
	r.emitI(int32(r.ci("CULL_FACE")))
}

func (r *Renderer_WebGL) setFrontFace(invert bool) {
	if invert == r.model.frontCW {
		return
	}
	r.model.frontCW = invert
	r.need(2)
	r.emitI(wcFrontFace)
	if invert {
		r.emitI(int32(r.ci("CW")))
	} else {
		r.emitI(int32(r.ci("CCW")))
	}
}

func (r *Renderer_WebGL) bindTexUnit(unit int, target int32, t *Texture_WebGL) {
	r.need(4)
	r.emitI(wcActiveBindT)
	r.emitI(int32(unit))
	r.emitI(target)
	r.emitI(t.cmdID)
}

func (r *Renderer_WebGL) modelAttr(slot int, size int32, typ int32, offset uint32) {
	m := &r.model
	loc := m.attrLoc[slot]
	if loc < 0 {
		return
	}
	if !m.attrOn[slot] {
		m.attrOn[slot] = true
		r.need(2)
		r.emitI(wcAttribOn)
		r.emitI(loc)
	}
	r.need(7)
	r.emitI(wcAttribPtr)
	r.emitI(loc)
	r.emitI(size)
	r.emitI(typ)
	r.emitI(0)
	r.emitI(0)
	r.emitI(int32(offset))
}

func (r *Renderer_WebGL) modelAttrOff(slot int) {
	m := &r.model
	loc := m.attrLoc[slot]
	if loc < 0 || !m.attrOn[slot] {
		return
	}
	m.attrOn[slot] = false
	d := wglModelAttrDefault[slot]
	r.need(6)
	r.emitI(wcAttribOff)
	r.emitI(loc)
	r.emitF(d[0])
	r.emitF(d[1])
	r.emitF(d[2])
	r.emitF(d[3])
}

// ---------------------------------------------------------------------------
// model drawing

// envUsable reports whether env's maps were rendered and can be sampled.
func (r *Renderer_WebGL) envUsable(env *Environment) bool {
	if env == nil || !r.model.envOK {
		return false
	}
	for _, g := range []*GLTFTexture{env.lambertianTexture, env.GGXTexture, env.GGXLUT} {
		if g == nil || g.tex == nil {
			return false
		}
		t, ok := g.tex.(*Texture_WebGL)
		if !ok || t == nil || !t.envRendered {
			return false
		}
	}
	return true
}

func (r *Renderer_WebGL) prepareModelPipeline(bufferIndex uint32, env *Environment) {
	m := &r.model
	if !m.enabled || bufferIndex > 1 {
		return
	}
	// The deferred scissor disable must land first or the model (and the depth
	// clear) would be clipped to the last sprite's window.
	r.flushScissorDisable()
	if !m.active {
		m.active = true
		m.savedBlend, m.savedEq, m.savedSrc, m.savedDst = r.blendEnabled, r.blendEquation, r.blendSrc, r.blendDst
		m.savedDepth, m.savedDepthWr = r.depthTest, r.depthMask
	}

	r.ChangeProgram(m.shader)
	r.need(2)
	r.emitI(wcBindVAO)
	r.emitI(m.vaoID)
	r.glc("bindFramebuffer", r.c("FRAMEBUFFER"), r.fbo)
	r.glc("viewport", 0, 0, sys.scrrect[2], sys.scrrect[3])
	r.SetDepthMask(true)
	r.glc("clear", r.ci("DEPTH_BUFFER_BIT"))
	// As upstream: blending on with the current function; each primitive sets
	// its own in SetModelPipeline.
	r.EnableBlending(r.blendEquation, r.blendSrc, r.blendDst)
	r.SetDepthTest(true)

	r.need(6)
	r.emitI(wcBindBuffer)
	r.emitI(int32(r.ci("ARRAY_BUFFER")))
	r.emitI(m.vbufID[bufferIndex])
	r.emitI(wcBindBuffer)
	r.emitI(int32(r.ci("ELEMENT_ARRAY_BUFFER")))
	r.emitI(m.ibufID[bufferIndex])

	// Every sampler on a complete texture of its own target.
	s := m.shader
	for _, name := range wglModel2DSamplers {
		r.bindTexUnit(s.textures[name], wglTEXTURE_2D, m.dummy2D)
	}
	usable := r.envUsable(env)
	if usable {
		r.bindTexUnit(s.textures["lambertianEnvSampler"], wglTEXTURE_CUBE, env.lambertianTexture.tex.(*Texture_WebGL))
		r.bindTexUnit(s.textures["GGXEnvSampler"], wglTEXTURE_CUBE, env.GGXTexture.tex.(*Texture_WebGL))
		r.bindTexUnit(s.textures["GGXLUT"], wglTEXTURE_2D, env.GGXLUT.tex.(*Texture_WebGL))
		r.SetUniformFSub(s, s.uniformIndex("environmentIntensity"), env.environmentIntensity)
		r.SetUniformISub(s, s.uniformIndex("mipCount"), env.mipmapLevels)
		rot := mgl.Rotate3DX(math.Pi).Mul3(mgl.Rotate3DY(0.5 * math.Pi))
		r.SetModelUniformMatrix3("environmentRotation", rot[:])
	} else {
		for _, name := range wglModelCubeSamplers {
			r.bindTexUnit(s.textures[name], wglTEXTURE_CUBE, m.dummyCube)
		}
		r.SetUniformFSub(s, s.uniformIndex("environmentIntensity"), 0)
	}
	m.envMissing = env != nil && !usable
	m.lightsSet = false
	r.SetActiveTexture0()
}

func (r *Renderer_WebGL) SetModelPipeline(eq BlendEquation, src, dst BlendFunc, depthTest, depthMask, doubleSided, invertFrontFace,
	useUV, useNormal, useTangent, useVertColor, useJoint0, useJoint1, useOutlineAttribute bool, numVertices, vertAttrOffset uint32) {
	if !r.model.enabled || !r.model.active {
		return
	}
	r.SetDepthTest(depthTest)
	r.SetDepthMask(depthMask)
	r.setFrontFace(invertFrontFace)
	r.setCullFace(doubleSided)
	r.EnableBlending(eq, src, dst)

	// Attributes are tightly packed, one after another (model.go).
	n := numVertices
	offset := vertAttrOffset
	r.modelAttr(wmaVertexId, 1, wglINT, offset)
	offset += 4 * n
	r.modelAttr(wmaPosition, 3, wglFLOAT, offset)
	offset += 12 * n
	if useUV {
		r.modelAttr(wmaUV, 2, wglFLOAT, offset)
		offset += 8 * n
	} else {
		r.modelAttrOff(wmaUV)
	}
	if useNormal {
		r.modelAttr(wmaNormal, 3, wglFLOAT, offset)
		offset += 12 * n
	} else {
		r.modelAttrOff(wmaNormal)
	}
	if useTangent {
		r.modelAttr(wmaTangent, 4, wglFLOAT, offset)
		offset += 16 * n
	} else {
		r.modelAttrOff(wmaTangent)
	}
	if useVertColor {
		r.modelAttr(wmaVertColor, 4, wglFLOAT, offset)
		offset += 16 * n
	} else {
		r.modelAttrOff(wmaVertColor)
	}
	if useJoint0 {
		r.modelAttr(wmaJoints0, 4, wglFLOAT, offset)
		offset += 16 * n
		r.modelAttr(wmaWeights0, 4, wglFLOAT, offset)
		offset += 16 * n
		if useJoint1 {
			r.modelAttr(wmaJoints1, 4, wglFLOAT, offset)
			offset += 16 * n
			r.modelAttr(wmaWeights1, 4, wglFLOAT, offset)
			offset += 16 * n
		} else {
			r.modelAttrOff(wmaJoints1)
			r.modelAttrOff(wmaWeights1)
		}
	} else {
		r.modelAttrOff(wmaJoints0)
		r.modelAttrOff(wmaWeights0)
		r.modelAttrOff(wmaJoints1)
		r.modelAttrOff(wmaWeights1)
	}
	if useOutlineAttribute {
		r.modelAttr(wmaOutline, 4, wglFLOAT, offset)
	} else {
		r.modelAttrOff(wmaOutline)
	}
}

func (r *Renderer_WebGL) SetMeshOutlinePipeline(invertFrontFace bool, meshOutline float32) {
	if !r.model.enabled || !r.model.active {
		return
	}
	r.setFrontFace(invertFrontFace)
	r.SetDepthTest(true)
	r.SetDepthMask(true)
	r.SetModelUniformF("meshOutline", meshOutline)
}

// ReleaseModelPipeline puts back what the 2D pipeline assumes. The program is
// left on the model shader: a program change is what makes the sprite, font
// and post-process paths rebind their own program, VAO and texture-unit cache
// (ChangeProgram resets the cache).
func (r *Renderer_WebGL) ReleaseModelPipeline() {
	m := &r.model
	if !m.enabled || !m.active {
		return
	}
	m.active = false
	r.need(2)
	r.emitI(wcBindVAO)
	r.emitI(m.nullID)
	r.setCullFace(true)
	r.setFrontFace(false)
	r.SetDepthTest(m.savedDepth)
	r.SetDepthMask(m.savedDepthWr)
	if m.savedBlend {
		r.EnableBlending(m.savedEq, m.savedSrc, m.savedDst)
	} else {
		r.DisableBlending()
	}
	r.SetActiveTexture0()
	for i := range r.texCacheTexSerial {
		r.texCacheTexSerial[i] = 0
		r.texCacheLastUsed[i] = 0
	}
}

func (r *Renderer_WebGL) RenderElements(mode PrimitiveMode, count, offset int) {
	if !r.model.enabled || !r.model.active || count <= 0 {
		return
	}
	r.need(4)
	r.emitI(wcDrawElements)
	r.emitI(int32(r.mapPrimitiveMode(mode).Int()))
	r.emitI(int32(count))
	r.emitI(int32(offset))
}

// ---------------------------------------------------------------------------
// model uniforms and textures

func (r *Renderer_WebGL) modelUniform(name string) (*ShaderProgram_WebGL, int) {
	m := &r.model
	if !m.enabled || r.programID != m.shader.id {
		return nil, -1
	}
	return m.shader, r.modelSlot(name).idx
}

// modelSlot looks a model-shader uniform up by name: its index and, for a
// sampler, its texture unit. model.go names uniforms with string constants
// (and a table of light names), so a name's address picks its slot and the
// content check turns any collision into a miss; a miss reads the shader's
// maps once. The maps hash the whole name on every lookup, about 40 times a
// primitive (on a stage of 1,000 primitives, string-map lookups were 14% of
// the main thread's busy time).
func (r *Renderer_WebGL) modelSlot(name string) *wglUniformSlot {
	m := &r.model
	if m.slotsFor != m.shader {
		m.slotsFor, m.slots = m.shader, [256]wglUniformSlot{}
		m.mats, m.matSet = make([][16]float32, len(m.shader.uniformLocs)), make([]bool, len(m.shader.uniformLocs))
	}
	p := uintptr(unsafe.Pointer(unsafe.StringData(name)))
	e := &m.slots[(p^p>>8^uintptr(len(name)))&255]
	if !e.set || e.name != name {
		unit, ok := m.shader.textures[name]
		if !ok {
			unit = -1
		}
		*e = wglUniformSlot{name: name, set: true, idx: m.shader.uniformIndex(name), unit: unit}
	}
	return e
}

func (r *Renderer_WebGL) SetModelUniformI(name string, val int) {
	s, idx := r.modelUniform(name)
	if s == nil {
		return
	}
	// model.draw lights a model that has neither lights nor an environment
	// unlit. A model whose environment this renderer could not build gets the
	// same rule, or it would draw black: no light reached it and its
	// environment lighting is off. model.draw sets every light's type before
	// it draws, so a draw that set none has no lights.
	if strings.HasPrefix(name, "lights[") && strings.HasSuffix(name, "].type") {
		r.model.lightsSet = true
	} else if name == "unlit" && val == 0 && r.model.envMissing && !r.model.lightsSet {
		val = 1
	}
	r.SetUniformISub(s, idx, int32(val))
}

func (r *Renderer_WebGL) SetModelUniformF(name string, values ...float32) {
	if s, idx := r.modelUniform(name); s != nil {
		r.SetUniformFSub(s, idx, values...)
	}
}

func (r *Renderer_WebGL) SetModelUniformFv(name string, values []float32) {
	if s, idx := r.modelUniform(name); s != nil {
		r.SetUniformFvSub(s, idx, values)
	}
}

func (r *Renderer_WebGL) modelMatrix(name string, value []float32, n int, op int32) {
	s, idx := r.modelUniform(name)
	if s == nil || idx < 0 || len(value) < n {
		return
	}
	cid := s.uniformCmdIDs[idx]
	if cid < 0 {
		return
	}
	// Skip a matrix the uniform already holds, bit for bit, as SetUniformFSub
	// does for vectors: the five texture transforms are the same for most
	// primitives (identity without KHR_texture_transform), and each layer's
	// draw sets the same projection and view. modelSlot sized the cache.
	m := &r.model
	if idx < len(m.mats) {
		c := &m.mats[idx]
		if m.matSet[idx] {
			same := true
			for i, v := range value[:n] {
				if math.Float32bits(c[i]) != math.Float32bits(v) {
					same = false
					break
				}
			}
			if same {
				return
			}
		}
		copy(c[:n], value[:n])
		m.matSet[idx] = true
	}
	r.need(2 + n)
	r.emitI(op)
	r.emitI(cid)
	for _, v := range value[:n] {
		r.emitF(v)
	}
}

func (r *Renderer_WebGL) SetModelUniformMatrix(name string, value []float32) {
	if len(value) == 9 {
		r.modelMatrix(name, value, 9, wcUniformMat3)
		return
	}
	r.modelMatrix(name, value, 16, wcUniformMat4)
}

func (r *Renderer_WebGL) SetModelUniformMatrix3(name string, value []float32) {
	r.modelMatrix(name, value, 9, wcUniformMat3)
}

func (r *Renderer_WebGL) SetModelTexture(name string, tex Texture) {
	s, idx := r.modelUniform(name)
	if s == nil || tex == nil {
		return
	}
	t, ok := tex.(*Texture_WebGL)
	if !ok || t == nil || !t.handle.Truthy() {
		return
	}
	unit := r.modelSlot(name).unit
	if unit < 0 {
		return
	}
	target := int32(wglTEXTURE_2D)
	if t.cube {
		target = wglTEXTURE_CUBE
	}
	r.bindTexUnit(unit, target, t)
	r.SetUniformISub(s, idx, int32(unit))
}

// ---------------------------------------------------------------------------
// shadows: not available in WebGL2 (no geometry shaders, no cube map arrays)

func (r *Renderer_WebGL) prepareShadowMapPipeline(bufferIndex uint32) {}
func (r *Renderer_WebGL) setShadowMapPipeline(doubleSided, invertFrontFace, useUV, useNormal, useTangent, useVertColor, useJoint0, useJoint1 bool, numVertices, vertAttrOffset uint32) {
}
func (r *Renderer_WebGL) ReleaseShadowPipeline()                                        {}
func (r *Renderer_WebGL) SetShadowMapUniformI(name string, val int)                     {}
func (r *Renderer_WebGL) SetShadowMapUniformF(name string, values ...float32)           {}
func (r *Renderer_WebGL) SetShadowMapUniformFv(name string, values []float32)           {}
func (r *Renderer_WebGL) SetShadowMapUniformMatrix(name string, value []float32)        {}
func (r *Renderer_WebGL) SetShadowMapUniformMatrix3(name string, value []float32)       {}
func (r *Renderer_WebGL) SetShadowMapTexture(name string, t Texture)                    {}
func (r *Renderer_WebGL) SetShadowFrameTexture(i uint32)                                {}
func (r *Renderer_WebGL) SetShadowFrameCubeTexture(i uint32)                            {}
func (r *Renderer_WebGL) RenderShadowMapElements(mode PrimitiveMode, count, offset int) {}

// ---------------------------------------------------------------------------
// model buffers

// uploadBytes returns a Uint8Array holding data. Large uploads (a whole
// model's vertex or index buffer) get a temporary array instead of growing the
// long-lived scratch buffer to their size. Model textures do not come here:
// SetDataG uploads them in bands.
func (r *Renderer_WebGL) uploadBytes(data []byte) js.Value {
	if len(data) <= 1<<20 {
		return r.scratch.bytes(data)
	}
	u8 := js.Global().Get("Uint8Array").New(len(data))
	js.CopyBytesToJS(u8, data)
	return u8
}

// uploadFloats returns a Float32Array of n floats holding values, zero-padded
// when values is shorter (a morph-target buffer can be: WebGL rejects a
// texImage2D source smaller than the image, where desktop GL reads past it).
func (r *Renderer_WebGL) uploadFloats(values []float32, n int) js.Value {
	if n < len(values) {
		n = len(values)
	}
	if n*4 <= 1<<20 {
		r.scratch.ensure(n * 4)
		b := f32SliceAsByteSlice(values)
		js.CopyBytesToJS(r.scratch.u8.Call("subarray", 0, len(b)), b)
		if n > len(values) {
			r.scratch.u8.Call("fill", 0, len(b), n*4)
		}
		return r.scratch.f32.Call("subarray", 0, n)
	}
	b := make([]byte, n*4)
	for i, v := range values {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(v))
	}
	buf := js.Global().Get("ArrayBuffer").New(n * 4)
	js.CopyBytesToJS(js.Global().Get("Uint8Array").New(buf), b)
	return js.Global().Get("Float32Array").New(buf)
}

func (r *Renderer_WebGL) SetModelVertexData(bufferIndex uint32, values []byte) {
	m := &r.model
	if !m.enabled || bufferIndex > 1 || len(values) == 0 {
		return
	}
	data := r.uploadBytes(values)
	prev := r.glc("getParameter", r.c("ARRAY_BUFFER_BINDING"))
	r.glc("bindBuffer", r.c("ARRAY_BUFFER"), m.vbuf[bufferIndex])
	r.glc("bufferData", r.c("ARRAY_BUFFER"), data, r.c("STATIC_DRAW"))
	r.glc("bindBuffer", r.c("ARRAY_BUFFER"), prev)
}

func (r *Renderer_WebGL) SetModelIndexData(bufferIndex uint32, values ...uint32) {
	m := &r.model
	if !m.enabled || bufferIndex > 1 || len(values) == 0 {
		return
	}
	b := make([]byte, len(values)*4)
	for i, v := range values {
		binary.LittleEndian.PutUint32(b[i*4:], v)
	}
	data := r.uploadBytes(b)
	// The element array binding is VAO state: bind it inside the model's VAO
	// and put back whichever VAO was bound.
	prev := r.glc("getParameter", r.c("VERTEX_ARRAY_BINDING"))
	r.glc("bindVertexArray", m.vao)
	r.glc("bindBuffer", r.c("ELEMENT_ARRAY_BUFFER"), m.ibuf[bufferIndex])
	r.glc("bufferData", r.c("ELEMENT_ARRAY_BUFFER"), data, r.c("STATIC_DRAW"))
	r.glc("bindVertexArray", prev)
}

// ---------------------------------------------------------------------------
// model textures

// gcRelease deletes t's GL texture once the Go object is collected, on the
// main thread, as the GL backends do (render_gl33.go generateTexture).
func (r *Renderer_WebGL) gcRelease(t *Texture_WebGL) {
	runtime.SetFinalizer(t, func(t *Texture_WebGL) {
		sys.mainThreadTask <- func() {
			if !t.handle.Truthy() {
				return
			}
			r.glc("deleteTexture", t.handle) // glc flushes: no queued command names it after this
			r.unregJS(t.cmdID)
			t.handle = js.Null()
		}
	})
}

// maxTextureSize is the largest texture side this GPU takes.
func (r *Renderer_WebGL) maxTextureSize() int32 {
	if r.model.maxTex == 0 {
		r.model.maxTex = -1
		if v := r.glc("getParameter", r.c("MAX_TEXTURE_SIZE")); v.Type() == js.TypeNumber && v.Int() > 0 {
			r.model.maxTex = int32(v.Int())
		}
	}
	return r.model.maxTex
}

// halveRGBA8 box-filters a tightly packed RGBA8 image to half its size
// (rounded up): each texel is the rounded mean of a 2x2 block; an odd last
// column or row averages the texels it has. The data is premultiplied (see
// model.go), so a plain mean is the right filter.
func halveRGBA8(src []byte, w, h int32) ([]byte, int32, int32) {
	nw, nh := (w+1)/2, (h+1)/2
	W, H, NW := int(w), int(h), int(nw)
	dst := make([]byte, NW*int(nh)*4)
	for y := 0; y < int(nh); y++ {
		y0 := 2 * y
		y1 := y0 + 1
		if y1 >= H {
			y1 = y0
		}
		for x := 0; x < NW; x++ {
			x0 := 2 * x
			x1 := x0 + 1
			if x1 >= W {
				x1 = x0
			}
			a, b := (y0*W+x0)*4, (y0*W+x1)*4
			c, d := (y1*W+x0)*4, (y1*W+x1)*4
			o := (y*NW + x) * 4
			for k := 0; k < 4; k++ {
				dst[o+k] = byte((uint32(src[a+k]) + uint32(src[b+k]) + uint32(src[c+k]) + uint32(src[d+k]) + 2) >> 2)
			}
		}
	}
	return dst, nw, nh
}

func (r *Renderer_WebGL) newModelTexture(width, height, depth int32, filter bool) Texture {
	// SetDataG gives it its storage, mipmaps and sampling.
	r.SetActiveTexture0()
	t := r.generateTexture(width, height, depth, filter)
	r.gcRelease(t)
	return t
}

func (r *Renderer_WebGL) newDataTexture(width, height int32) Texture {
	r.SetActiveTexture0()
	t := r.generateTexture(width, height, 128, false)
	r.glc("bindTexture", r.c("TEXTURE_2D"), t.handle)
	r.glc("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MAG_FILTER"), r.c("NEAREST"))
	r.glc("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MIN_FILTER"), r.c("NEAREST"))
	r.glc("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_S"), r.c("CLAMP_TO_EDGE"))
	r.glc("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_T"), r.c("CLAMP_TO_EDGE"))
	r.gcRelease(t)
	return t
}

// newHDRTexture holds the environment panorama. RGBA16F, not RGBA32F: it is
// sampled LINEAR, which 32-bit float needs OES_texture_float_linear for.
func (r *Renderer_WebGL) newHDRTexture(width, height int32) Texture {
	r.SetActiveTexture0()
	t := r.generateTexture(width, height, 128, false)
	t.halfFloat = true
	r.glc("bindTexture", r.c("TEXTURE_2D"), t.handle)
	r.glc("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MIN_FILTER"), r.c("LINEAR"))
	r.glc("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_MAG_FILTER"), r.c("LINEAR"))
	r.glc("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_S"), r.c("MIRRORED_REPEAT"))
	r.glc("texParameteri", r.c("TEXTURE_2D"), r.c("TEXTURE_WRAP_T"), r.c("MIRRORED_REPEAT"))
	r.gcRelease(t)
	return t
}

// newCubeMapTexture: RGBA16F faces (HALF_FLOAT, as render_gles32.go). Without
// float color buffers the environment is never rendered, so no storage.
func (r *Renderer_WebGL) newCubeMapTexture(widthHeight int32, mipmap bool, lowestMipLevel int32) Texture {
	r.SetActiveTexture0()
	t := r.generateTexture(widthHeight, widthHeight, 24, false)
	t.cube = true
	r.gcRelease(t)
	if !r.model.envOK {
		return t
	}
	cube := r.c("TEXTURE_CUBE_MAP")
	r.glc("bindTexture", cube, t.handle)
	for i := 0; i < 6; i++ {
		r.glc("texImage2D", r.ci("TEXTURE_CUBE_MAP_POSITIVE_X")+i, 0, r.c("RGBA16F"), widthHeight, widthHeight, 0,
			r.c("RGBA"), r.c("HALF_FLOAT"), js.Null())
	}
	if mipmap {
		r.glc("texParameteri", cube, r.c("TEXTURE_MIN_FILTER"), r.c("LINEAR_MIPMAP_LINEAR"))
		r.glc("generateMipmap", cube)
	} else {
		r.glc("texParameteri", cube, r.c("TEXTURE_MIN_FILTER"), r.c("LINEAR"))
	}
	r.glc("texParameteri", cube, r.c("TEXTURE_MAG_FILTER"), r.c("LINEAR"))
	r.glc("texParameteri", cube, r.c("TEXTURE_WRAP_S"), r.c("CLAMP_TO_EDGE"))
	r.glc("texParameteri", cube, r.c("TEXTURE_WRAP_T"), r.c("CLAMP_TO_EDGE"))
	r.glc("bindTexture", cube, js.Null())
	return t
}

// ---------------------------------------------------------------------------
// environment (image-based lighting) passes
//
// These run once per environment load, from a main-thread task: between
// frames, or mid-frame when a loader drains the task queue. They leave the
// scene framebuffer bound with the full viewport, and blend/depth/scissor/cull
// as they found them.

type wglEnvPass struct {
	blend    bool
	eq       BlendEquation
	src, dst BlendFunc
	depth    bool
}

func (r *Renderer_WebGL) beginEnvPass(prog *ShaderProgram_WebGL, vao js.Value) wglEnvPass {
	r.flushScissorDisable()
	p := wglEnvPass{r.blendEnabled, r.blendEquation, r.blendSrc, r.blendDst, r.depthTest}
	r.DisableBlending()
	r.SetDepthTest(false)
	r.setCullFace(true)
	r.ChangeProgram(prog)
	r.glc("bindVertexArray", vao)
	r.glc("bindFramebuffer", r.c("FRAMEBUFFER"), r.model.fboEnv)
	return p
}

func (r *Renderer_WebGL) endEnvPass(p wglEnvPass) {
	r.glc("framebufferTexture2D", r.c("FRAMEBUFFER"), r.c("COLOR_ATTACHMENT0"), r.c("TEXTURE_2D"), js.Null(), 0)
	r.glc("bindVertexArray", js.Null())
	r.glc("bindFramebuffer", r.c("FRAMEBUFFER"), r.fbo)
	r.glc("viewport", 0, 0, sys.scrrect[2], sys.scrrect[3])
	r.glc("activeTexture", r.ci("TEXTURE0"))
	r.glc("bindTexture", r.c("TEXTURE_2D"), js.Null())
	r.glc("bindTexture", r.c("TEXTURE_CUBE_MAP"), js.Null())
	r.SetActiveTexture0()
	r.SetDepthTest(p.depth)
	if p.blend {
		r.EnableBlending(p.eq, p.src, p.dst)
	} else {
		r.DisableBlending()
	}
}

// envTarget attaches one face (or the 2D LUT) and checks the framebuffer once;
// an incomplete one turns environment lighting off.
func (r *Renderer_WebGL) envTarget(target js.Value, t *Texture_WebGL, level int32) bool {
	r.glc("framebufferTexture2D", r.c("FRAMEBUFFER"), r.c("COLOR_ATTACHMENT0"), target, t.handle, level)
	if r.glc("checkFramebufferStatus", r.c("FRAMEBUFFER")).Int() != r.ci("FRAMEBUFFER_COMPLETE") {
		r.model.envOK = false
		r.wglWarn("WebGL: float render target incomplete; 3D models draw without environment lighting")
		return false
	}
	return true
}

func (r *Renderer_WebGL) envTextures(a, b Texture) (*Texture_WebGL, *Texture_WebGL, bool) {
	if !r.model.enabled || !r.model.envOK || a == nil || b == nil {
		return nil, nil, false
	}
	ta, ok1 := a.(*Texture_WebGL)
	tb, ok2 := b.(*Texture_WebGL)
	return ta, tb, ok1 && ok2 && ta != nil && tb != nil && ta.handle.Truthy() && tb.handle.Truthy()
}

func (r *Renderer_WebGL) RenderCubeMap(envTex Texture, cubeTex Texture) {
	env, cube, ok := r.envTextures(envTex, cubeTex)
	if !ok {
		return
	}
	s := r.model.panorama
	p := r.beginEnvPass(s, r.model.panoramaVAO)
	defer r.endEnvPass(p)
	size := cube.width
	r.glc("viewport", 0, 0, size, size)
	unit := s.textures["panorama"]
	r.glc("activeTexture", r.ci("TEXTURE0")+unit)
	r.glc("bindTexture", r.c("TEXTURE_2D"), env.handle)
	for i := 0; i < 6; i++ {
		if !r.envTarget(js.ValueOf(r.ci("TEXTURE_CUBE_MAP_POSITIVE_X")+i), cube, 0) {
			return
		}
		r.glc("clear", r.ci("COLOR_BUFFER_BIT"))
		r.SetUniformISub(s, s.uniformIndex("currentFace"), int32(i))
		r.glc("drawArrays", r.c("TRIANGLE_STRIP"), 0, 4)
	}
	r.glc("framebufferTexture2D", r.c("FRAMEBUFFER"), r.c("COLOR_ATTACHMENT0"), r.c("TEXTURE_2D"), js.Null(), 0)
	r.glc("bindTexture", r.c("TEXTURE_2D"), js.Null())
	r.glc("bindTexture", r.c("TEXTURE_CUBE_MAP"), cube.handle)
	r.glc("generateMipmap", r.c("TEXTURE_CUBE_MAP"))
	r.glc("bindTexture", r.c("TEXTURE_CUBE_MAP"), js.Null())
	cube.envRendered = true
}

func (r *Renderer_WebGL) envFilterUniforms(s *ShaderProgram_WebGL, distribution, sampleCount, width int32, roughness float32, isLUT bool) {
	r.SetUniformISub(s, s.uniformIndex("sampleCount"), sampleCount)
	r.SetUniformISub(s, s.uniformIndex("distribution"), distribution)
	r.SetUniformISub(s, s.uniformIndex("width"), width)
	r.SetUniformFSub(s, s.uniformIndex("roughness"), roughness)
	r.SetUniformFSub(s, s.uniformIndex("intensityScale"), 1)
	r.SetUniformISub(s, s.uniformIndex("isLUT"), int32(Btoi(isLUT)))
}

func (r *Renderer_WebGL) RenderFilteredCubeMap(distribution int32, cubeTex Texture, filteredTex Texture, mipmapLevel, sampleCount int32, roughness float32) {
	cube, filtered, ok := r.envTextures(cubeTex, filteredTex)
	if !ok || !cube.envRendered {
		return
	}
	s := r.model.filter
	p := r.beginEnvPass(s, r.model.filterVAO)
	defer r.endEnvPass(p)
	size := filtered.width >> mipmapLevel
	r.glc("viewport", 0, 0, size, size)
	unit := s.textures["cubeMap"]
	r.glc("activeTexture", r.ci("TEXTURE0")+unit)
	r.glc("bindTexture", r.c("TEXTURE_CUBE_MAP"), cube.handle)
	r.envFilterUniforms(s, distribution, sampleCount, filtered.width, roughness, false)
	for i := 0; i < 6; i++ {
		if !r.envTarget(js.ValueOf(r.ci("TEXTURE_CUBE_MAP_POSITIVE_X")+i), filtered, mipmapLevel) {
			return
		}
		r.glc("clear", r.ci("COLOR_BUFFER_BIT"))
		r.SetUniformISub(s, s.uniformIndex("currentFace"), int32(i))
		r.glc("drawArrays", r.c("TRIANGLE_STRIP"), 0, 4)
	}
	filtered.envRendered = true
}

func (r *Renderer_WebGL) RenderLUT(distribution int32, cubeTex Texture, lutTex Texture, sampleCount int32) {
	cube, lut, ok := r.envTextures(cubeTex, lutTex)
	if !ok || !cube.envRendered {
		return
	}
	s := r.model.filter
	p := r.beginEnvPass(s, r.model.filterVAO)
	defer r.endEnvPass(p)
	r.glc("viewport", 0, 0, lut.width, lut.height)
	unit := s.textures["cubeMap"]
	r.glc("activeTexture", r.ci("TEXTURE0")+unit)
	// The LUT becomes a half-float render target. Re-specified on a unit the
	// filter shader's samplerCube does not read through its 2D target, then
	// unbound before it is drawn into.
	r.glc("bindTexture", r.c("TEXTURE_2D"), lut.handle)
	r.glc("texImage2D", r.c("TEXTURE_2D"), 0, r.c("RGBA16F"), lut.width, lut.height, 0, r.c("RGBA"), r.c("HALF_FLOAT"), js.Null())
	r.glc("bindTexture", r.c("TEXTURE_2D"), js.Null())
	r.glc("bindTexture", r.c("TEXTURE_CUBE_MAP"), cube.handle)
	r.envFilterUniforms(s, distribution, sampleCount, lut.width, 0, true)
	r.SetUniformISub(s, s.uniformIndex("currentFace"), 0)
	if !r.envTarget(r.c("TEXTURE_2D"), lut, 0) {
		return
	}
	r.glc("clear", r.ci("COLOR_BUFFER_BIT"))
	r.glc("drawArrays", r.c("TRIANGLE_STRIP"), 0, 4)
	lut.envRendered = true
}
