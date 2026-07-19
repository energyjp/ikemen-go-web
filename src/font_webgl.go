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
	scale        int32
	windowWidth  int
	windowHeight int
	textures     []*TextureAtlas
	color        color
	shaderPalFX  ShaderPalFX
	// Reused across Printf calls: text draws run per line, per frame, so
	// fresh allocations here become megabytes per second of garbage on the
	// single wasm thread (a screen-full of TrueType text measurably shortened
	// the GC cycle and lengthened marks). Grown once, reused forever.
	runeBuf []rune
	vertBuf []float32
}

type FontRenderer_WebGL struct {
	shaderProgram *ShaderProgram_WebGL
	vao           js.Value
	vbo           js.Value
	vaoID         int32 // command-buffer table handles
	vboID         int32
}

func (fr *FontRenderer_WebGL) Init(renderer interface{}) {
	r := gfx.(*Renderer_WebGL)

	fr.shaderProgram, _ = r.newShaderProgram(vertexFontShader, fragmentFontShader, "", "font shader", true)
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

	// Interleaved layout: vec2 vert, vec2 vertTexCoord (16-byte stride)
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

// SetColor allows you to set the text color to be used when you draw the text
func (f *Font_WebGL) SetColor(red float32, green float32, blue float32, alpha float32) {
	f.color.r = red
	f.color.g = green
	f.color.b = blue
	f.color.a = alpha
}

func (f *Font_WebGL) SetPalFX(state ShaderPalFX) {
	f.shaderPalFX = state
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

// Printf draws a string to the screen, takes a list of arguments like printf
func (f *Font_WebGL) Printf(x, y float32, xscl, yscl float32, spacingXAdd float32,
	align int32, blend bool, window [4]int32,
	rxadd float32, rot Rotation, projectionMode int32, fLength float32, rcx, rcy float32,
	fs string, argv ...interface{}) error {

	// Fast path for the universal "%s" single-string call: skip fmt.Sprintf
	// and its allocation. Anything fancier still formats normally.
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
	// Decode into the reusable rune buffer instead of allocating []rune(text).
	f.runeBuf = f.runeBuf[:0]
	for _, rn := range text {
		f.runeBuf = append(f.runeBuf, rn)
	}
	indices := f.runeBuf
	r := gfx.(*Renderer_WebGL)
	fr := gfxFont.(*FontRenderer_WebGL)

	if len(indices) == 0 {
		return nil
	}

	// Activate corresponding render state
	fr.SetFontPipeline()
	program := fr.shaderProgram

	// Reusable vertex batch buffer (see the struct comment).
	batchSize := Min(MaxFontBatchSize, int32(len(indices)))
	if cap(f.vertBuf) < int(batchSize)*6*4 {
		f.vertBuf = make([]float32, 0, int(batchSize)*6*4)
	}
	batchVertices := f.vertBuf[:0]

	//setup blending mode
	if blend {
		r.EnableBlending(BlendAdd, BlendSrcAlpha, BlendOneMinusSrcAlpha)
	} else {
		r.DisableBlending()
	}

	//restrict drawing to a certain part of the window
	r.EnableScissor(window[0], window[1], window[2], window[3])

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

	//calculate alignment position
	alignScale := xscl
	if alignScale == 0 {
		alignScale = yscl
	}
	if align == 0 {
		x -= f.widthRunes(indices, alignScale, spacingXAdd) * 0.5
	} else if align < 0 {
		x -= f.widthRunes(indices, alignScale, spacingXAdd)
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

		if int32(len(batchVertices)/24) >= batchSize || (textureID != -1 && textureID != int32(ch.textureID)) {
			// Render the current batch
			f.renderGlyphBatch(batchVertices, textureID)
			// Clear the batch buffers
			batchVertices = batchVertices[:0]
		}
		textureID = int32(ch.textureID)

		if renderedAny {
			x += spacing
		}

		//calculate position and size for current rune
		xpos := x + float32(ch.bearingH)*xscl
		ypos := y - float32(ch.height-ch.bearingV)*yscl
		w := float32(ch.width) * xscl
		h := float32(ch.height) * yscl

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
		x += float32((ch.advance >> 6)) * xscl // Bitshift by 6 to get value in pixels (2^6 = 64 (divide amount of 1/64th pixels by 64 to get amount of pixels))
		renderedAny = true
	}

	// Render any remaining glyphs in the batch
	if len(batchVertices) > 0 {
		f.renderGlyphBatch(batchVertices, textureID)
	}

	// Disable scissor just in case
	r.DisableScissor()

	return nil
}

func (f *Font_WebGL) widthRunes(indices []rune, scale float32, spacingXAdd float32) float32 {
	if len(indices) == 0 {
		return 0
	}

	spacing := spacingXAdd * scale
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
// texture identified by atlasIndex (stored in character.textureID).
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

// Width returns the width of a piece of text in pixels
func (f *Font_WebGL) Width(scale float32, spacingXAdd float32, fs string, argv ...interface{}) float32 {
	return f.widthRunes([]rune(fmt.Sprintf(fs, argv...)), scale, spacingXAdd)
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

		//The glyph's ascent and descent equal -bounds.Min.Y and +bounds.Max.Y.
		gAscent := int(-gBnd.Min.Y) >> 6
		gdescent := int(gBnd.Max.Y) >> 6

		//set w,h and adv, bearing V and bearing H in char
		char.width = int(gw) + (padding * 2)
		char.height = int(gh) + (padding * 2)
		char.advance = int(gAdv)
		char.bearingV = gdescent
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
				f.textures = append(f.textures, CreateTextureAtlas(256, 256, 32, true))
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

		//add char to fontChar list
		f.fontChar[ch] = char
	}

	gfx.(*Renderer_WebGL).gl.Call("bindTexture", gfx.(*Renderer_WebGL).c("TEXTURE_2D"), js.Null())
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
	f.SetPalFX(NewShaderPalFX())
	f.textures = append(f.textures, CreateTextureAtlas(256, 256, 32, true))

	err = f.GenerateGlyphs(low, high)
	if err != nil {
		return nil, err
	}

	return f, nil
}
