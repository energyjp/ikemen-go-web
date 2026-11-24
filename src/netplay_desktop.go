//go:build !js

package main

import (
	"fmt"
	"io"
	"net"
	"time"

	"github.com/ikemen-engine/ggpo/transport"
)

// Desktop netplay transport: plain TCP with the IKEMENGO handshake,
// exactly as upstream. The js/wasm build replaces these in netplay_js.go.

// Desktop keeps upstream's netplay deadlines (Netplay.SyncTimeout and
// Netplay.LoadingTimeout, see netDeadline in netplay.go).
const platformNetDeadlines = true

// platformGGPOTransport returns extra transports for
// ggpo.Peer.InitializeConnection; none on desktop (default UDP).
func platformGGPOTransport() []transport.Connection {
	return nil
}

// platformRollbackPeer: desktop reads the rollback peer from the TCP stream
// and the negotiated ports (rollback.go preMatchSetup), so it has no override.
func platformRollbackPeer(host bool) (remoteIp string, localPort, remotePort int, ok bool) {
	return "", 0, 0, false
}

func (nc *NetConnection) Accept(port int) error {
	ln, err := net.Listen("tcp", net.JoinHostPort("", Itoa(port)))
	if err != nil {
		return err
	}

	tcpLn, ok := ln.(*net.TCPListener)
	if !ok {
		ln.Close()
		return fmt.Errorf("failed to cast net.Listener to *net.TCPListener")
	}

	nc.ln = tcpLn
	nc.host = true
	nc.conn = nil // Make sure this is a new connection
	// Channel 0 is the host and channel 1 the guest, as in rollback/replays.
	// Character ownership is selected separately through inputRemap.
	nc.locIn, nc.remIn = 0, 1

	lnLocal := tcpLn
	SafeGo(func() {
		defer lnLocal.Close()

		tempConn, err := lnLocal.AcceptTCP()
		if err != nil {
			return
		}

		if nc.isClosing() {
			tempConn.Close()
			return
		}

		// Don't allow the handshake to block forever (important when shutting down).
		_ = tempConn.SetDeadline(time.Now().Add(2 * time.Second))

		//Send handshake
		if _, err := tempConn.Write([]byte("IKEMENGO")); err != nil {
			tempConn.Close()
			return
		}

		// Wait for client acknowledgment
		ack := make([]byte, 8) // Length of our "password"
		_, err = io.ReadFull(tempConn, ack)
		if err != nil || string(ack) != "IKEMENGO" {
			tempConn.Close()
			return
		}

		// Handshake complete; clear deadlines for normal play.
		_ = tempConn.SetDeadline(time.Time{})

		// Rollback reads the peer address from the adopted socket at match startup.
		nc.publishConnection(tempConn)
	})

	return nil
}

func (nc *NetConnection) Connect(server string, port int) {
	nc.host = false
	nc.conn = nil // Make sure this is a new connection
	nc.remIn, nc.locIn = 0, 1

	SafeGo(func() {
		d := net.Dialer{Timeout: 1 * time.Second}
		for {
			if nc.isClosing() {
				return
			}
			tempConn, err := d.Dial("tcp", net.JoinHostPort(server, Itoa(port)))
			if err != nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}

			tcpConn := tempConn.(*net.TCPConn)
			if nc.isClosing() {
				tcpConn.Close()
				return
			}

			// Don't allow the handshake to block forever (important when shutting down).
			_ = tcpConn.SetDeadline(time.Now().Add(2 * time.Second))

			// Wait for host handshake
			buf := make([]byte, 8)
			_, err = io.ReadFull(tcpConn, buf)
			if err != nil || string(buf) != "IKEMENGO" {
				tcpConn.Close()
				time.Sleep(100 * time.Millisecond)
				continue
			}

			// Send acknowledgment
			if _, err := tcpConn.Write([]byte("IKEMENGO")); err != nil {
				tcpConn.Close()
				time.Sleep(100 * time.Millisecond)
				continue
			}

			// Handshake complete; clear deadlines for normal play.
			_ = tcpConn.SetDeadline(time.Time{})

			nc.publishConnection(tcpConn)
			return
		}
	})
}

// platformNetStats is a browser-overlay affordance (see netplay_js.go).
func platformNetStats(pingMs int) {}

// platformOnlineNames is a browser matchmaking affordance (see
// netplay_js.go); desktop netplay has no verified-name source.
// No web bridge on desktop, so there is no cached standing to report.
func platformRankStatus() string { return "" }

func platformOnlineNames() string { return "" }

// platformLobby: winner-stays lobbies are the browser page's (netplay_js.go).
func platformLobby(cmd, arg string) string { return "" }

// platformReportResult is likewise browser-only: there is no leaderboard
// endpoint to report to from a desktop build.
func platformReportResult(winSide int, p1Char, p2Char string) {}

// platformMatchGone: only the browser page can know that the other player
// left a lobby (netplay_js.go); desktop waits for GGPO's disconnect.
func platformMatchGone() bool { return false }

// platformNetFail: desktop shows a failed connection in the session warning
// only (netplay_js.go also hands it to the page).
func platformNetFail(msg string) {}
