//go:build !js

package main

import "github.com/ikemen-engine/ggpo/transport"

// Watching runs in the browser build only for now: the desktop build has no
// page to carry the watchers' connections, so a desktop host never offers a
// match and a desktop engine is never a watcher.

func platformWatchHosting() bool                                           { return false }
func platformWatchOffer(match int, raw string)                             {}
func platformWatchStart(match int) []int                                   { return nil }
func platformWatchSend(slot int, raw string)                               {}
func platformWatchHub(conns []transport.Connection) []transport.Connection { return conns }
func platformWatchSlotGone(slot int) bool                                  { return false }
func platformWatchLog(msg string)                                          {}
func platformWatchNext() (string, bool)                                    { return "", false }
func platformWatchTransport() []transport.Connection                       { return nil }
func platformWatchTrail(frame int, crc uint32)                             {}
func platformWatchHubClose()                                               {}
func platformWatchAvailable() bool                                         { return false }
func platformWatchBegin() bool                                             { return false }
func platformWatchLeave()                                                  {}
