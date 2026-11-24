//go:build !js

package main

import (
	"encoding/hex"
	"strings"
)

// The device key of the pad a 1.0 config configured a [Joystick_Pn] slot
// with: 1.0 wrote SDL's GUID, which is the first part of this engine's key
// (controllerKeyMatch finds the pad by it).
func legacyPadKey(guid string) string {
	guid = strings.ToLower(strings.TrimSpace(guid))
	if _, err := hex.DecodeString(guid); err != nil || len(guid) != 32 {
		return ""
	}
	return guid
}
