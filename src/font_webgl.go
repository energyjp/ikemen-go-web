//go:build js

package main

import (
	"fmt"
	"image"
	"io"
	"os"
	"syscall/js"

	"github.com/golang/freetype"
	"github.com/golang/freetype/truetype"
	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

type Font_WebGL struct {
	fontChar     map[rune]*character
	ttf          *truetype.Font
	metrics      font.Metrics // at the DEF size, set by GenerateGlyphs (as font_gl33.go)
	scale        int32
	windowWidth  int
	windowHeight int
	textures     []*TextureAtlas
	color        color
	blendEq      BlendEquation
	blendSrc     BlendFunc
	blendDst     BlendFunc
	shaderPalFX  ShaderPalFX
	customShader CustomShaderRenderData

	// Glyphs for other on-screen sizes, by pixel size (font.go ttfRasterSize).
	// They hold glyphs only; the drawing settings stay on the DEF-size font.
	rasterFonts map[int32]*Font_WebGL

	// Reused across Printf and Width calls: text draws run per line, per
	// frame, so fresh allocations here become megabytes per second of garbage
	// on the single wasm thread (a screen-full of TrueType text measurably
	// shortened the GC cycle and lengthened marks). Grown once, reused forever.
	runeBuf []rune
	vertBuf []float32
}

// The engine reaches these through interfaces only (util_js.go selectRenderer,
// and LoadFntTtf's `ttf.(Font)` assertion, which would otherwise fail at run
// time on the first TrueType font), so the compiler checks them here.
var (
	_ Renderer      = (*Renderer_WebGL)(nil)
	_ Texture       = (*Texture_WebGL)(nil)
	_ FontRenderer  = (*FontRenderer_WebGL)(nil)
	_ Font          = (*Font_WebGL)(nil)
	_ TtfFont       = (*Font_WebGL)(nil)
	_ ttfStringFont = (*Font_WebGL)(nil)
)

type FontRenderer_WebGL struct {
	shaderProgram *ShaderProgram_WebGL
	vao           js.Value
	vbo           js.Value
	vaoID         int32 // command-buffer table handles
	vboID         int32
}

// Reused uniform data for text drawn through a custom shader (see Printf):
// a slice of a local array passed to the gfx methods would escape and
// allocate per call.
var (
	fontCustomProj  [16]float32
	fontCustomIdent = [16]float32{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
	fontCustomMult  [3]float32
	fontCustomTint  [4]float32
)

func (fr *FontRenderer_WebGL) Init(renderer interface{}) {
	r := gfx.(*Renderer_WebGL)

	fr.shaderProgram, _ = r.newShaderProgram(vertexFontShader, fragmentFontShader, "", "font shader", true, wglFontAttribs...)
	fr.shaderProgram.RegisterAttributes("vert", "vertTexCoord")
	fr.shaderProgram.RegisterUniforms("textColor", "resolution", "tex", "palAdd", "palMul", "palGray", "palHue", "palNeg")

	fr.vao = r.glc("createVertexArray")
	fr.vbo = r.glc("createBuffer")
	fr.vaoID = r.regJS(fr.vao)
	fr.vboID = r.regJS(fr.vbo)
	r.glc("bindVertexArray", fr.vao)
	r.glc("bindBuffer", r.c("ARRAY_BUFFER"), fr.vbo)

	// Pre-allocate for maximum batch size (6 vertices * 4 floats * 4 bytes per glyph)
	r.glc("bufferData", r.c("ARRAY_BUFFER"), MaxFontBatchSize*6*4*4, r.c("DYNAMIC_DRAW"))

	// Interleaved layout: vec2 vert, vec2 vertTexCoord (16-byte stride).
	// Locations 0 and 1 (wglFontAttribs), the same as position and uv in the
	// sprite shaders, so a custom shader can draw from this VAO too.
	vLoc := fr.shaderProgram.attributes["vert"]
	r.glc("enableVertexAttribArray", vLoc)
	r.glc("vertexAttribPointer", vLoc, 2, r.c("FLOAT"), false, 16, 0)
	tLoc := fr.shaderProgram.attributes["vertTexCoord"]
	r.glc("enableVertexAttribArray", tLoc)
	r.glc("vertexAttribPointer", tLoc, 2, r.c("FLOAT"), false, 16, 8)

	r.glc("bindVertexArray", js.Null())
	r.glc("bindBuffer", r.c("ARRAY_BUFFER"), js.Null())
}

// LoadFont loads the specified font at the given scale.
func (r *FontRenderer_WebGL) LoadFont(file string, scale int32, windowWidth int, windowHeight int) (interface{}, error) {
	fd, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer fd.Close()

	f, err := r.LoadTrueTypeFont(fd, scale, 32, 127, LeftToRight)
	if err != nil {
		return nil, err
	}
	f.windowWidth = windowWidth
	f.windowHeight = windowHeight
	return f, nil
}

// Metrics is the font's ascent, descent and height at its DEF size (font.go
// uses it to place controller glyphs next to TrueType text).
// HasRune reports whether the font has a glyph for r (font.go ttfRuneFont).
func (f *Font_WebGL) HasRune(r rune) bool {
	return f.ttf != nil && f.ttf.Index(r) != 0
}

func (f *Font_WebGL) Metrics() font.Metrics {
	return f.metrics
}

// SetColor allows you to set the text color to be used when you draw the text
func (f *Font_WebGL) SetColor(red float32, green float32, blue float32, alpha float32) {
	f.color.r = red
	f.color.g = green
	f.color.b = blue
	f.color.a = alpha
}

// SetBlending sets how Printf blends the text when blending is on.
func (f *Font_WebGL) SetBlending(eq BlendEquation, src, dst BlendFunc) {
	f.blendEq, f.blendSrc, f.blendDst = eq, src, dst
}

func (f *Font_WebGL) SetPalFX(state ShaderPalFX) {
	f.shaderPalFX = state
}

func (f *Font_WebGL) SetCustomShader(shader CustomShaderRenderData) {
	f.customShader = shader
}

func (f *Font_WebGL) UpdateResolution(windowWidth int, windowHeight int) {
	f.windowWidth = windowWidth
	f.windowHeight = windowHeight
}

func (fr *FontRenderer_WebGL) SetFontPipeline() {
	r := gfx.(*Renderer_WebGL)

	// Do nothing if we were already using the font shader
	if r.programID == fr.shaderProgram.id {
		return
	}

	r.currentProgram = fr.shaderProgram
	r.ChangeProgram(fr.shaderProgram)
	r.need(2)
	r.emitI(wcBindVAO)
	r.emitI(fr.vaoID)
	// The old explicit ARRAY_BUFFER bind is intentionally dropped: buffer
	// binding is only observed by bufferData/bufferSubData, and every such op
	// in the command stream (wcVertexData/wcSubData) rebinds its own buffer
	// first - so GL state is identical at every point where it is read.
}

// customProgram returns the custom sprite shader set by SetCustomShader, or
// nil when there is none or it is not loaded.
func (f *Font_WebGL) customProgram(r *Renderer_WebGL) *ShaderProgram_WebGL {
	if f.customShader.name == "" {
		return nil
	}
	if id, ok := r.customShaderMap[f.customShader.name]; ok {
		if shader, ok := r.customShaders[id]; ok {
			return shader
		}
	}
	return nil
}

// decode fills f.runeBuf with the formatted text. The universal "%s"
// single-string call skips fmt.Sprintf and its allocation.
func (f *Font_WebGL) decode(fs string, argv []interface{}) []rune {
	var text string
	if fs == "%s" && len(argv) == 1 {
		if s, ok := argv[0].(string); ok {
			text = s
		} else {
			text = fmt.Sprintf(fs, argv...)
		}
	} else {
		text = fmt.Sprintf(fs, argv...)
	}
	return f.decodeString(text)
}

// decodeString fills f.runeBuf with text.
func (f *Font_WebGL) decodeString(text string) []rune {
	f.runeBuf = f.runeBuf[:0]
	for _, rn := range text {
		f.runeBuf = append(f.runeBuf, rn)
	}
	return f.runeBuf
}

// Printf draws a string to the screen, takes a list of arguments like printf
func (f *Font_WebGL) Printf(x, y float32, xscl, yscl float32, spacingXAdd float32,
	align int32, blend bool, window [4]int32,
	rxadd float32, rot Rotation, projectionMode int32, fLength float32, rcx, rcy float32,
	fs string, argv ...interface{}) error {
	return f.printRunes(x, y, xscl, yscl, spacingXAdd, align, blend, window,
		rxadd, rot, projectionMode, fLength, rcx, rcy, f.decode(fs, argv))
}

// PrintString is Printf("%s", txt) without boxing txt into an interface
// (font.go ttfStringFont): the engine's text goes through here every frame.
func (f *Font_WebGL) PrintString(x, y float32, xscl, yscl float32, spacingXAdd float32,
	align int32, blend bool, window [4]int32,
	rxadd float32, rot Rotation, projectionMode int32, fLength float32, rcx, rcy float32,
	txt string) error {
	return f.printRunes(x, y, xscl, yscl, spacingXAdd, align, blend, window,
		rxadd, rot, projectionMode, fLength, rcx, rcy, f.decodeString(txt))
}

func (f *Font_WebGL) printRunes(x, y float32, xscl, yscl float32, spacingXAdd float32,
	align int32, blend bool, window [4]int32,
	rxadd float32, rot Rotation, projectionMode int32, fLength float32, rcx, rcy float32,
	indices []rune) error {
	r := gfx.(*Renderer_WebGL)
	fr := gfxFont.(*FontRenderer_WebGL)

	if len(indices) == 0 {
		return nil
	}

	// Activate corresponding render state
	program := fr.shaderProgram
	custom := false
	if cp := f.customProgram(r); cp != nil {
		program = cp
		custom = true
	}
	if custom {
		// Like SetFontPipeline, but with the custom program. The glyph data
		// lives in the font's VAO and VBO.
		r.currentProgram = program
		r.ChangeProgram(program)
		r.need(2)
		r.emitI(wcBindVAO)
		r.emitI(fr.vaoID)
	} else {
		fr.SetFontPipeline()
	}

	// Reusable vertex batch buffer (see the struct comment). A custom shader
	// draws glyph by glyph, each with its own atlas rectangle (spriteUV).
	batchSize := Min(MaxFontBatchSize, int32(len(indices)))
	if custom {
		batchSize = 1
	}
	if cap(f.vertBuf) < int(batchSize)*6*4 {
		f.vertBuf = make([]float32, 0, int(batchSize)*6*4)
	}
	batchVertices := f.vertBuf[:0]

	//setup blending mode
	if blend {
		r.EnableBlending(f.blendEq, f.blendSrc, f.blendDst)
	} else {
		r.DisableBlending()
	}

	//restrict drawing to a certain part of the window
	r.EnableScissor(window[0], window[1], window[2], window[3])

	if custom {
		// The sprite vertex shader draws glyph quads given in window pixels
		w, h := float32(f.windowWidth), float32(f.windowHeight)
		proj := gfx.OrthographicProjectionMatrix(0, w, h, 0, -1, 1)
		fontCustomProj = proj
		r.SetUniformMatrix("projection", fontCustomProj[:])
		r.SetUniformMatrix("modelview", fontCustomIdent[:])

		r.SetUniformI("isFlat", 0)
		r.SetUniformI("isRgba", 1)
		r.SetUniformI("mask", 0)
		r.SetUniformI("isTrapez", 0)
		r.SetUniformI("useQuad", 0)
		r.SetUniformI("neg", int(Btoi(f.shaderPalFX.neg)))
		r.SetUniformF("alpha", f.color.a)
		r.SetUniformF("add", f.shaderPalFX.add[0], f.shaderPalFX.add[1], f.shaderPalFX.add[2])
		fontCustomMult = [3]float32{f.shaderPalFX.mult[0] * f.color.r, f.shaderPalFX.mult[1] * f.color.g, f.shaderPalFX.mult[2] * f.color.b}
		r.SetUniformFv("mult", fontCustomMult[:])
		r.SetUniformF("gray", f.shaderPalFX.gray)
		r.SetUniformF("hue", f.shaderPalFX.hue)
		r.SetUniformFv("tint", fontCustomTint[:])

		// Same engine-provided values RenderSprite gives custom shaders
		if f.customShader.tex1 != nil {
			r.SetTexture("tex1", f.customShader.tex1)
		}
		if f.customShader.tex2 != nil {
			r.SetTexture("tex2", f.customShader.tex2)
		}
		f.customShader.setUniforms()
	} else {
		// Set texture location
		r.SetUniformISub(program, program.uniformIndex("tex"), 0)

		//set text color
		r.SetUniformFSub(program, program.uniformIndex("textColor"), f.color.r, f.color.g, f.color.b, f.color.a)

		// Set PalFX uniforms
		r.SetUniformFSub(program, program.uniformIndex("palAdd"), f.shaderPalFX.add[0], f.shaderPalFX.add[1], f.shaderPalFX.add[2])
		r.SetUniformFSub(program, program.uniformIndex("palMul"), f.shaderPalFX.mult[0], f.shaderPalFX.mult[1], f.shaderPalFX.mult[2])
		r.SetUniformFSub(program, program.uniformIndex("palGray"), f.shaderPalFX.gray)
		r.SetUniformFSub(program, program.uniformIndex("palHue"), f.shaderPalFX.hue)
		r.SetUniformISub(program, program.uniformIndex("palNeg"), int32(Btoi(f.shaderPalFX.neg)))

		//set screen resolution
		r.SetUniformFSub(program, program.uniformIndex("resolution"), float32(f.windowWidth), float32(f.windowHeight))

		r.SetActiveTexture0()
	}

	// Use glyphs made at the on-screen size; k scales them back to font units
	rscl := yscl
	if rscl == 0 {
		rscl = xscl
	}
	gs := f.rasterFont(rscl)
	k := float32(f.scale) / float32(gs.scale)
	gxscl, gyscl := xscl*k, yscl*k

	//calculate alignment position
	alignScale := xscl
	if alignScale == 0 {
		alignScale = yscl
	}
	if align == 0 {
		x -= gs.widthRunes(indices, alignScale*k, spacingXAdd*alignScale) * 0.5
	} else if align < 0 {
		x -= gs.widthRunes(indices, alignScale*k, spacingXAdd*alignScale)
	}
	needsTransform := rxadd != 0 || !rot.IsZero()
	// textureID tracks the atlas index of the glyphs currently in the batch, so
	// a run of glyphs sharing an atlas draws in one call and a different atlas
	// forces a flush (see the atlas-index note on GenerateGlyphs).
	textureID := int32(-1)
	spacing := spacingXAdd * xscl
	renderedAny := false
	// Iterate through all characters in string
	for i := range indices {
		//get rune
		runeIndex := indices[i]

		//find rune in fontChar list
		ch, ok := gs.fontChar[runeIndex]

		//load missing runes in batches of 32
		if !ok {
			low := runeIndex - (runeIndex % 32)
			gs.GenerateGlyphs(low, low+31)
			ch, ok = gs.fontChar[runeIndex]
		}

		//skip runes that are not in font character range
		if !ok {
			continue
		}

		if !custom && (int32(len(batchVertices)/24) >= batchSize || (textureID != -1 && textureID != int32(ch.textureID))) {
			// Render the current batch
			gs.renderGlyphBatch(batchVertices, textureID)
			// Clear the batch buffers
			batchVertices = batchVertices[:0]
		}
		textureID = int32(ch.textureID)

		if renderedAny {
			x += spacing
		}

		//calculate position and size for current rune
		xpos := x + float32(ch.bearingH)*gxscl
		ypos := y - float32(ch.height-ch.bearingV)*gyscl
		w := float32(ch.width) * gxscl
		h := float32(ch.height) * gyscl

		x1, y1 := xpos+w, ypos
		x2, y2 := xpos, ypos
		x3, y3 := xpos, ypos+h
		x4, y4 := xpos+w, ypos+h
		if needsTransform {
			x1, y1, x2, y2, x3, y3, x4, y4 = transformTextQuad(
				x1, y1, x2, y2, x3, y3, x4, y4,
				rxadd, rot, projectionMode, fLength, rcx, rcy,
			)
		}

		batchVertices = append(batchVertices,
			x1, y1, ch.uv[2], ch.uv[1],
			x2, y2, ch.uv[0], ch.uv[1],
			x3, y3, ch.uv[0], ch.uv[3],

			x3, y3, ch.uv[0], ch.uv[3],
			x4, y4, ch.uv[2], ch.uv[3],
			x1, y1, ch.uv[2], ch.uv[1],
		)
		// Now advance cursors for next glyph (note that advance is number of 1/64 pixels)
		x += float32((ch.advance >> 6)) * gxscl // Bitshift by 6 to get value in pixels (2^6 = 64 (divide amount of 1/64th pixels by 64 to get amount of pixels))
		renderedAny = true
		if custom {
			f.renderGlyph(batchVertices, ch.texture)
			batchVertices = batchVertices[:0]
		}
	}

	// Render any remaining glyphs in the batch
	if len(batchVertices) > 0 {
		gs.renderGlyphBatch(batchVertices, textureID)
	}
	f.vertBuf = batchVertices[:0] // keep any growth

	if custom {
		// The sprite pipeline binds its VAO only on a program change, and the
		// font VAO is bound under this custom program now: force the next
		// SetSpritePipeline/SetFontPipeline to switch.
		r.programID = 0
	}

	// Disable scissor just in case
	r.flushScissorDisable()

	return nil
}

// Text width, with spacing added between glyphs (spacing in pixels)
func (f *Font_WebGL) widthRunes(indices []rune, scale float32, spacing float32) float32 {
	if len(indices) == 0 {
		return 0
	}

	var width float32
	renderedAny := false

	// Iterate through all characters in string
	for i := range indices {

		//get rune
		runeIndex := indices[i]

		//find rune in fontChar list
		ch, ok := f.fontChar[runeIndex]

		//load missing runes in batches of 32
		if !ok {
			low := runeIndex - (runeIndex % 32)
			f.GenerateGlyphs(low, low+31)
			ch, ok = f.fontChar[runeIndex]
		}

		//skip runes that are not in font character range
		if !ok {
			continue
		}

		if renderedAny {
			width += spacing
		}

		// Now advance cursors for next glyph (note that advance is number of 1/64 pixels)
		width += float32(ch.advance>>6) * scale // Bitshift by 6 to get value in pixels (2^6 = 64 (divide amount of 1/64th pixels by 64 to get amount of pixels))
		renderedAny = true
	}

	return width
}

// renderGlyphBatch uploads the batched quads and draws them with the atlas
// texture identified by atlasIndex (stored in character.textureID). f is the
// font that made the glyphs (a raster size), whose atlases they live in.
func (f *Font_WebGL) renderGlyphBatch(vertices []float32, atlasIndex int32) {
	r := gfx.(*Renderer_WebGL)
	fr := gfxFont.(*FontRenderer_WebGL)

	atlas := f.textures[atlasIndex].texture.(*Texture_WebGL)

	// bind + upload + bind atlas + draw, all through the command buffer
	r.need(3 + len(vertices))
	r.emitI(wcSubData)
	r.emitI(fr.vboID)
	r.emitI(int32(len(vertices)))
	for _, v := range vertices {
		r.emitF(v)
	}
	r.need(7)
	r.emitI(wcActiveBind) // = activeTexture(TEXTURE0) + bindTexture, as before
	r.emitI(0)
	r.emitI(atlas.cmdID)
	if len(r.texCacheTexSerial) > 0 { // keep the unit-0 cache reset SetActiveTexture0 did
		r.texCacheTexSerial[0] = 0
		r.texCacheLastUsed[0] = 0
	}
	r.emitI(wcDrawArrays)
	r.emitI(int32(r.ci("TRIANGLES")))
	r.emitI(0)
	r.emitI(int32(len(vertices) / 4))
}

// renderGlyph draws one glyph through a custom shader: its sub-texture gives
// the shader the glyph's atlas rectangle (spriteUV).
func (f *Font_WebGL) renderGlyph(vertices []float32, texture Texture) {
	r := gfx.(*Renderer_WebGL)
	fr := gfxFont.(*FontRenderer_WebGL)

	r.need(3 + len(vertices))
	r.emitI(wcSubData)
	r.emitI(fr.vboID)
	r.emitI(int32(len(vertices)))
	for _, v := range vertices {
		r.emitF(v)
	}
	r.SetTexture("tex", texture)
	r.need(4)
	r.emitI(wcDrawArrays)
	r.emitI(int32(r.ci("TRIANGLES")))
	r.emitI(0)
	r.emitI(int32(len(vertices) / 4))
}

// Width returns the width of a piece of text in pixels. rscl is the y scale
// the text is drawn at (its glyphs come from that raster size); 0 measures at
// the DEF size.
func (f *Font_WebGL) Width(rscl, scale float32, spacingXAdd float32, fs string, argv ...interface{}) float32 {
	gs := f
	if rscl != 0 {
		gs = f.rasterFont(rscl)
	}
	k := float32(f.scale) / float32(gs.scale)
	return gs.widthRunes(f.decode(fs, argv), scale*k, spacingXAdd*scale)
}

// WidthString is Width with "%s", txt, without boxing txt (see PrintString).
func (f *Font_WebGL) WidthString(rscl, scale float32, spacingXAdd float32, txt string) float32 {
	gs := f
	if rscl != 0 {
		gs = f.rasterFont(rscl)
	}
	k := float32(f.scale) / float32(gs.scale)
	return gs.widthRunes(f.decodeString(txt), scale*k, spacingXAdd*scale)
}

// rasterFont returns the glyphs for text drawn at yscl. The DEF size uses f
// itself; at most ttfMaxRasterSizes other sizes are made, then the closest one
// is reused.
func (f *Font_WebGL) rasterFont(yscl float32) *Font_WebGL {
	size := ttfRasterSize(f.scale, yscl)
	if size == f.scale {
		return f
	}
	size = ttfPickRasterSize(f.rasterFonts, size)
	if rf, ok := f.rasterFonts[size]; ok {
		return rf
	}
	if f.rasterFonts == nil {
		f.rasterFonts = make(map[int32]*Font_WebGL)
	}
	// Holds glyphs only, drawing settings stay on f
	rf := &Font_WebGL{
		fontChar: make(map[rune]*character),
		ttf:      f.ttf,
		scale:    size,
		textures: []*TextureAtlas{CreateTextureAtlas(sys.cfg.Video.FontTextureAtlasSize, sys.cfg.Video.FontTextureAtlasSize, 32, true)},
	}
	f.rasterFonts[size] = rf
	return rf
}

// GenerateGlyphs builds a set of textures based on a ttf files glyphs
func (f *Font_WebGL) GenerateGlyphs(low, high rune) error {
	//create a freetype context for drawing
	c := freetype.NewContext()
	c.SetDPI(72)
	c.SetFont(f.ttf)
	c.SetFontSize(float64(f.scale))
	c.SetHinting(font.HintingFull)

	//create new face to measure glyph dimensions
	ttfFace := truetype.NewFace(f.ttf, &truetype.Options{
		Size:    float64(f.scale),
		DPI:     72,
		Hinting: font.HintingFull,
	})
	f.metrics = ttfFace.Metrics()

	// Add padding to prevent cropping
	// https://github.com/ikemen-engine/Ikemen-GO/issues/3085
	padding := 2

	//make each glyph
	for ch := low; ch <= high; ch++ {
		char := new(character)

		drawGlyph := true
		gBnd, gAdv, ok := ttfFace.GlyphBounds(ch)
		if !ok {
			// Some fonts do not provide bounds for every requested rune. This is common for control chars.
			// Keep loading the font and create an invisible placeholder so future lookups do not retry this glyph forever.
			drawGlyph = false
			if adv, advOK := ttfFace.GlyphAdvance(ch); advOK {
				gAdv = adv
			} else {
				gAdv = 0
			}
			gBnd = f.ttf.Bounds(fixed.Int26_6(f.scale))
		}

		gh := int32((gBnd.Max.Y - gBnd.Min.Y) >> 6)
		gw := int32((gBnd.Max.X - gBnd.Min.X) >> 6)

		//if glyph has no dimensions set to a max value
		if gw == 0 || gh == 0 {
			gBnd = f.ttf.Bounds(fixed.Int26_6(f.scale))
			gw = int32((gBnd.Max.X - gBnd.Min.X) >> 6)
			gh = int32((gBnd.Max.Y - gBnd.Min.Y) >> 6)

			//above can sometimes yield 0 for font smaller than 48pt, 1 is minimum
			if gw == 0 || gh == 0 {
				gw = 1
				gh = 1
			}
		}

		// Distance from the glyph's top to its baseline.
		gAscent := int(-gBnd.Min.Y) >> 6

		//set w,h and adv, bearing V and bearing H in char
		char.width = int(gw) + (padding * 2)
		char.height = int(gh) + (padding * 2)
		char.advance = int(gAdv)
		// The raster's baseline is padding + gAscent below its top (pt below),
		// so the glyph sits on the text's baseline (upstream's font_gl33.go
		// fix; bearingV = descent drew every line about padding px high).
		char.bearingV = char.height - (padding + gAscent)
		char.bearingH = (int(gBnd.Min.X) >> 6) - padding

		//create image to draw glyph
		fg := image.White
		rect := image.Rect(0, 0, char.width, char.height)
		rgba := image.NewRGBA(rect)

		//set the glyph dot
		px := padding - (int(gBnd.Min.X) >> 6)
		py := padding + gAscent
		pt := freetype.Pt(px, py)

		// Draw the text from mask to image
		c.SetClip(rgba.Bounds())
		c.SetDst(rgba)
		c.SetSrc(fg)
		if drawGlyph {
			if _, err := c.DrawString(string(ch), pt); err != nil {
				// Some fonts have broken hinting bytecode. Keep full hinting for normal fonts.
				// Retry glyph without hinting instead of failing the whole font load.
				c2 := freetype.NewContext()
				c2.SetDPI(72)
				c2.SetFont(f.ttf)
				c2.SetFontSize(float64(f.scale))
				c2.SetHinting(font.HintingNone)
				c2.SetClip(rgba.Bounds())
				c2.SetDst(rgba)
				c2.SetSrc(fg)
				if _, err := c2.DrawString(string(ch), pt); err != nil {
					return err
				}
			}
		}

		var uv [4]float32
		textureIndex := 0
		w, h := int32(rgba.Rect.Dx()), int32(rgba.Rect.Dy())
		pix := rgba.Pix
		stride := int32(rgba.Stride)

		for {
			if textureIndex >= len(f.textures) {
				f.textures = append(f.textures, CreateTextureAtlas(sys.cfg.Video.FontTextureAtlasSize, sys.cfg.Video.FontTextureAtlasSize, 32, true))
			}

			var inserted bool
			uv, inserted = f.textures[textureIndex].AddImage(w, h, stride, pix)

			if inserted {
				break
			}

			textureIndex++
		}

		char.uv = uv
		// WebGL texture handles are js.Value, which does not fit character.textureID
		// (a uint32). Store the atlas index instead and resolve the actual texture
		// in renderGlyphBatch; the Printf batch loop only needs this to be a stable
		// per-atlas identifier.
		char.textureID = uint32(textureIndex)
		// The glyph's own rectangle, for text drawn through a custom shader
		char.texture = gfx.newSubTexture(f.textures[textureIndex].texture, int32(char.width), int32(char.height), char.uv)

		//add char to fontChar list
		f.fontChar[ch] = char
	}

	r := gfx.(*Renderer_WebGL)
	r.glc("bindTexture", r.c("TEXTURE_2D"), js.Null())
	return nil
}

// LoadTrueTypeFont builds glyph textures based on a ttf file
func (r *FontRenderer_WebGL) LoadTrueTypeFont(reader io.Reader, scale int32, low, high rune, dir Direction) (*Font_WebGL, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	// Read the truetype font.
	ttf, err := truetype.Parse(data)
	if err != nil && err.Error() == "freetype: invalid TrueType format: bad kern table length" {
		ttf, err = truetype.Parse(stripKernTable(data))
	}
	if err != nil {
		return nil, err
	}

	//make Font stuct type
	f := new(Font_WebGL)
	f.fontChar = make(map[rune]*character)
	f.ttf = ttf
	f.scale = scale
	f.SetColor(1.0, 1.0, 1.0, 1.0) //set default white
	f.SetBlending(BlendAdd, BlendSrcAlpha, BlendOneMinusSrcAlpha)
	f.SetPalFX(NewShaderPalFX())
	f.textures = append(f.textures, CreateTextureAtlas(sys.cfg.Video.FontTextureAtlasSize, sys.cfg.Video.FontTextureAtlasSize, 32, true))

	err = f.GenerateGlyphs(low, high)
	if err != nil {
		return nil, err
	}

	return f, nil
}
