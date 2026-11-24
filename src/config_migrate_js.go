//go:build js

package main

import "strings"

// The device key of the pad a 1.0-based page configured a [Joystick_Pn] slot
// with. That page wrote the browser's gamepad id as the slot's GUID, and this
// engine derives a pad's key from the same id (attachPad: padGUID of
// parsePadID), so the setup follows the same pad.
func legacyPadKey(guid string) string {
	if strings.TrimSpace(guid) == "" {
		return ""
	}
	return padGUID(parsePadID(guid))
}
