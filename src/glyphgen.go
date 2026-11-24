package main

import (
	"reflect"

	"gopkg.in/ini.v1"
)

// Glyph generation: a counter that changes whenever anything glyph resolution
// reads may have changed, so text can keep its resolved glyph runs (font.go
// TextSprite.cachedRuns) and hint texts (motif.go HintProperties.text) across
// frames instead of resolving every tag again 60 times a second.
//
// Resolving a tag reads the motif (glyphs, controller packs, actions), the
// glyph pack chosen for each device, the Controller Mappings and Button
// Setups, the devices and the players they belong to, the device whose
// buttons hints show (the last one used, a focused one, the open text
// field's, the pause menu player's), the mouse,
// netplay and replay state, the Command List options and the facing of the
// player who opened the pause menu. glyphState copies all of that in a form
// that compares cheaply; glyphGeneration compares it with the last copy and
// moves the counter on a difference. It is recomputed on every call, so a
// change takes effect on the very next text drawn, even within a frame.
//
// Maps that are only ever replaced, never edited in place (the Controller
// Mappings and Button Setups of config.ini, rebuilt by loadInputConfig,
// and the netplay session setups, cloned on every write) count by identity.
type glyphState struct {
	motif                    *ini.File
	mappings, setups         uintptr
	sessionSetups            uintptr
	uiGlyphDevice, menuFocus int
	menuPlayer               int
	textDevice               int
	textActive, textKeyboard bool
	mouseActive, mouseRemote bool
	mouseMotif               bool
	commandMirror            bool
	commandMapping           bool
	mirrored                 bool
	replay, connection       bool
	synchronized             bool
	locIn                    int
	inputs                   uint64
}

// Identity of a map value (its runtime header), 0 for nil. Maps are
// pointer-shaped, so this does not allocate.
func mapIdentity(m interface{}) uintptr {
	v := reflect.ValueOf(m)
	if v.IsNil() {
		return 0
	}
	return v.Pointer()
}

const (
	glyphHashOffset uint64 = 14695981039346656037
	glyphHashPrime  uint64 = 1099511628211
)

func glyphHashInt(h uint64, v int) uint64 {
	x := uint64(v)
	for i := 0; i < 8; i++ {
		h ^= x & 0xff
		h *= glyphHashPrime
		x >>= 8
	}
	return h
}

func glyphHashString(h uint64, s string) uint64 {
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= glyphHashPrime
	}
	return glyphHashInt(h, len(s))
}

func (s *System) captureGlyphState() glyphState {
	st := glyphState{
		motif:          s.motif.IniFile,
		mappings:       mapIdentity(s.inputCfg.mappings),
		setups:         mapIdentity(s.inputCfg.setups),
		sessionSetups:  mapIdentity(s.sessionSetups),
		uiGlyphDevice:  s.uiGlyphDevice,
		menuFocus:      s.menuFocus,
		menuPlayer:     s.menuPlayer,
		textDevice:     s.textInput.device,
		textActive:     s.textInput.active,
		textKeyboard:   s.textInput.keyboard,
		mouseActive:    s.mouse.active,
		mouseRemote:    s.mouse.remote,
		mouseMotif:     s.motif.Mouse.Enabled,
		commandMirror:  s.cfg.Input.CommandMirror,
		commandMapping: s.cfg.Input.CommandMapping,
		replay:         s.replayFile != nil,
		locIn:          -1,
	}
	if nc := s.inputConnection(); nc != nil {
		st.connection = true
		st.locIn = nc.locIn
	}
	st.synchronized = s.synchronizedInput()
	// The facing test of resolveGlyph's Mirror Commands.
	if st.commandMirror {
		p := s.menuPlayer - 1
		if p >= 0 && p < MaxSimul*2 && s.tmode[p&1] == TM_Turns {
			p &= 1
		}
		st.mirrored = p >= 0 && p < len(s.chars) && len(s.chars[p]) > 0 &&
			s.chars[p][0] != nil && s.chars[p][0].facing < 0
	}
	h := glyphHashOffset
	for _, d := range s.inputs.Devices {
		if d == nil {
			continue
		}
		h = glyphHashInt(h, d.ID)
		h = glyphHashInt(h, d.Joy)
		h = glyphHashInt(h, int(Btoi(d.Connected)))
		// virtualGlyph draws a raw joystick's controls with the standard art.
		h = glyphHashInt(h, int(Btoi(d.Raw)))
		h = glyphHashString(h, d.Setup)
	}
	h = glyphHashInt(h, len(s.inputs.Devices))
	for _, p := range s.inputs.Participants {
		h = glyphHashInt(h, p.Device)
		h = glyphHashInt(h, int(p.Side))
	}
	h = glyphHashInt(h, len(s.inputs.Participants))
	// Shared devices are edited in place: an order-free sum of the entries.
	var shared uint64
	for k, ids := range s.inputs.sharedDevices {
		e := glyphHashInt(glyphHashOffset, k)
		for _, id := range ids {
			e = glyphHashInt(e, id)
		}
		shared += e
	}
	h = glyphHashInt(h, int(shared))
	// The glyph pack chosen per device (Controller Mapping > Glyphs) is read first
	// by GlyphPack. Choosing one edits this config map in place
	// (rememberControllerGlyphs -> SetValueUpdate), so it counts by content:
	// an order-free sum of its keys and values.
	var packs uint64
	for k, v := range s.cfg.Input.ControllerGlyphs {
		packs += glyphHashString(glyphHashString(glyphHashOffset, k), v)
	}
	h = glyphHashInt(h, int(packs))
	for _, r := range s.inputRemap {
		h = glyphHashInt(h, r)
	}
	if st.synchronized {
		for i := range s.menuChannels {
			h = glyphHashInt(h, int(Btoi(s.menuChannels[i].mouse.available())))
		}
	}
	st.inputs = h
	return st
}

// The counter and the state it was taken from. Package variables rather than
// System fields: a render-side cache, never part of the simulation or of
// rollback state.
var (
	glyphGen  uint64
	glyphLast glyphState
)

// The current glyph generation (never 0, which marks text without tags).
func (s *System) glyphGeneration() uint64 {
	st := s.captureGlyphState()
	if glyphGen == 0 || st != glyphLast {
		glyphLast = st
		glyphGen++
	}
	return glyphGen
}
