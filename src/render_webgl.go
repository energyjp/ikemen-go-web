//go:build js

package main

import (
	mgl "github.com/go-gl/mathgl/mgl32"
)

// WebGL2 renderer for the browser build.
//
// STATUS: interface-complete skeleton. The real implementation adapts
// render_gl33.go onto WebGL2 via syscall/js (GL3.3 core and GLES3.0 are
// near-identical for everything the 2D pipeline uses). Methods panic
// loudly until implemented so partial ports fail fast instead of
// rendering garbage. 3D model/shadow pipelines (glTF stages) are stubbed
// as disabled - IsModelEnabled/IsShadowEnabled return false, which makes
// the engine skip those passes entirely.

const webglTODO = "webgl renderer: not implemented yet"

type Texture_WebGL struct {
	width  int32
	height int32
	depth  int32
	filter bool
}

func (t *Texture_WebGL) SetData(data []byte)  { panic(webglTODO) }
func (t *Texture_WebGL) SetSubData(data []byte, x, y, width, height, stride int32) {
	panic(webglTODO)
}
func (t *Texture_WebGL) SetDataG(data []byte, mag, min, ws, wt TextureSamplingParam) {
	panic(webglTODO)
}
func (t *Texture_WebGL) SetPixelData(data []float32) { panic(webglTODO) }
func (t *Texture_WebGL) IsValid() bool               { return false }
func (t *Texture_WebGL) GetWidth() int32             { return t.width }
func (t *Texture_WebGL) GetHeight() int32            { return t.height }
func (t *Texture_WebGL) CopyData(src *Texture)       { panic(webglTODO) }

type Renderer_WebGL struct {
}

func (r *Renderer_WebGL) GetName() string   { return "WebGL 2" }
func (r *Renderer_WebGL) DebugInfo() string { return "WebGL 2 (browser)" }
func (r *Renderer_WebGL) Init()             { panic(webglTODO) }
func (r *Renderer_WebGL) Close()            {}
func (r *Renderer_WebGL) BeginFrame(clearColor bool) { panic(webglTODO) }
func (r *Renderer_WebGL) EndFrame()          { panic(webglTODO) }
func (r *Renderer_WebGL) Await()             {}
func (r *Renderer_WebGL) IsModelEnabled() bool  { return false }
func (r *Renderer_WebGL) IsShadowEnabled() bool { return false }

func (r *Renderer_WebGL) LoadCustomSpriteShader(shaderName string, shaderData []byte) uint32 {
	return 0
}
func (r *Renderer_WebGL) UnloadCustomSpriteShader(shaderName string) {}
func (r *Renderer_WebGL) SetSpritePipeline(shaderName string)        { panic(webglTODO) }
func (r *Renderer_WebGL) SetCustomUniforms(params [16]float32)       {}
func (r *Renderer_WebGL) NeedsGrabPass() bool                        { return false }
func (r *Renderer_WebGL) ResolveBackBuffer() Texture                 { panic(webglTODO) }

func (r *Renderer_WebGL) EnableBlending(eq BlendEquation, src, dst BlendFunc) { panic(webglTODO) }
func (r *Renderer_WebGL) DisableBlending()                                    { panic(webglTODO) }

func (r *Renderer_WebGL) prepareShadowMapPipeline(bufferIndex uint32) {}
func (r *Renderer_WebGL) setShadowMapPipeline(doubleSided, invertFrontFace, useUV, useNormal, useTangent, useVertColor, useJoint0, useJoint1 bool, numVertices, vertAttrOffset uint32) {
}
func (r *Renderer_WebGL) ReleaseShadowPipeline()                                {}
func (r *Renderer_WebGL) prepareModelPipeline(bufferIndex uint32, env *Environment) {}
func (r *Renderer_WebGL) SetModelPipeline(eq BlendEquation, src, dst BlendFunc, depthTest, depthMask, doubleSided, invertFrontFace, useUV, useNormal, useTangent, useVertColor, useJoint0, useJoint1, useOutlineAttribute bool, numVertices, vertAttrOffset uint32) {
}
func (r *Renderer_WebGL) SetMeshOutlinePipeline(invertFrontFace bool, meshOutline float32) {}
func (r *Renderer_WebGL) ReleaseModelPipeline()                                            {}

func (r *Renderer_WebGL) newTexture(width, height, depth int32, filter bool) (t Texture) {
	panic(webglTODO)
}
func (r *Renderer_WebGL) newPaletteTexture() (t Texture) { panic(webglTODO) }
func (r *Renderer_WebGL) newModelTexture(width, height, depth int32, filter bool) (t Texture) {
	panic(webglTODO)
}
func (r *Renderer_WebGL) newDataTexture(width, height int32) (t Texture) { panic(webglTODO) }
func (r *Renderer_WebGL) newHDRTexture(width, height int32) (t Texture)  { panic(webglTODO) }
func (r *Renderer_WebGL) newCubeMapTexture(widthHeight int32, mipmap bool, lowestMipLevel int32) (t Texture) {
	panic(webglTODO)
}

func (r *Renderer_WebGL) ReadPixels(data []uint8, width, height int)  { panic(webglTODO) }
func (r *Renderer_WebGL) EnableScissor(x, y, width, height int32)     { panic(webglTODO) }
func (r *Renderer_WebGL) DisableScissor()                             { panic(webglTODO) }

func (r *Renderer_WebGL) SetUniformI(name string, val int)                  { panic(webglTODO) }
func (r *Renderer_WebGL) SetUniformF(name string, values ...float32)        { panic(webglTODO) }
func (r *Renderer_WebGL) SetUniformFv(name string, values []float32)        { panic(webglTODO) }
func (r *Renderer_WebGL) SetUniformMatrix(name string, value []float32)     { panic(webglTODO) }
func (r *Renderer_WebGL) SetTexture(name string, tex Texture)               { panic(webglTODO) }
func (r *Renderer_WebGL) SetModelUniformI(name string, val int)             {}
func (r *Renderer_WebGL) SetModelUniformF(name string, values ...float32)   {}
func (r *Renderer_WebGL) SetModelUniformFv(name string, values []float32)   {}
func (r *Renderer_WebGL) SetModelUniformMatrix(name string, value []float32) {}
func (r *Renderer_WebGL) SetModelUniformMatrix3(name string, value []float32) {}
func (r *Renderer_WebGL) SetModelTexture(name string, t Texture)              {}
func (r *Renderer_WebGL) SetShadowMapUniformI(name string, val int)           {}
func (r *Renderer_WebGL) SetShadowMapUniformF(name string, values ...float32) {}
func (r *Renderer_WebGL) SetShadowMapUniformFv(name string, values []float32) {}
func (r *Renderer_WebGL) SetShadowMapUniformMatrix(name string, value []float32)  {}
func (r *Renderer_WebGL) SetShadowMapUniformMatrix3(name string, value []float32) {}
func (r *Renderer_WebGL) SetShadowMapTexture(name string, t Texture)              {}
func (r *Renderer_WebGL) SetShadowFrameTexture(i uint32)                          {}
func (r *Renderer_WebGL) SetShadowFrameCubeTexture(i uint32)                      {}
func (r *Renderer_WebGL) SetVertexData(values ...float32)                { panic(webglTODO) }
func (r *Renderer_WebGL) SetModelVertexData(bufferIndex uint32, values []byte) {}
func (r *Renderer_WebGL) SetModelIndexData(bufferIndex uint32, values ...uint32) {}

func (r *Renderer_WebGL) RenderQuad()                                          { panic(webglTODO) }
func (r *Renderer_WebGL) RenderElements(mode PrimitiveMode, count, offset int) {}
func (r *Renderer_WebGL) RenderShadowMapElements(mode PrimitiveMode, count, offset int) {}
func (r *Renderer_WebGL) RenderCubeMap(envTexture Texture, cubeTexture Texture)         {}
func (r *Renderer_WebGL) RenderFilteredCubeMap(distribution int32, cubeTexture Texture, filteredTexture Texture, mipmapLevel, sampleCount int32, roughness float32) {
}
func (r *Renderer_WebGL) RenderLUT(distribution int32, cubeTexture Texture, lutTexture Texture, sampleCount int32) {
}

func (r *Renderer_WebGL) PerspectiveProjectionMatrix(angle, aspect, near, far float32) mgl.Mat4 {
	return mgl.Perspective(angle, aspect, near, far)
}
func (r *Renderer_WebGL) OrthographicProjectionMatrix(left, right, bottom, top, near, far float32) mgl.Mat4 {
	return mgl.Ortho(left, right, bottom, top, near, far)
}

func (r *Renderer_WebGL) SetVSync(interval int) {}
func (r *Renderer_WebGL) NewWorkerThread() bool { return false }

// FontRenderer_WebGL: bitmap fonts render through the sprite pipeline;
// this interface only matters for TTF fonts, which the browser build
// does not support (LoadFntTtf panics in util_js.go).
type FontRenderer_WebGL struct{}

func (fr *FontRenderer_WebGL) Init(renderer interface{}) {}

func (fr *FontRenderer_WebGL) LoadFont(file string, scale int32, windowWidth int, windowHeight int) (interface{}, error) {
	return nil, Error("TrueType fonts are not supported in the browser build")
}
