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

// platformGGPOTransport returns extra transports for
// ggpo.Peer.InitializeConnection; none on desktop (default UDP).
func platformGGPOTransport() []transport.Connection {
	return nil
}

func (nc *NetConnection) Accept(port string) error {
	ln, err := net.Listen("tcp", ":"+port)
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
	nc.locIn, nc.remIn = nc.GetHostGuestRemap()

	lnLocal := nc.ln
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

		if sys.cfg.Netplay.RollbackNetcode {
			sys.rollback.session.remoteIp = tempConn.RemoteAddr().(*net.TCPAddr).IP.String()
		}

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

		// Handshake complete. Make temp connection permanent
		if nc.isClosing() {
			tempConn.Close()
			return
		}
		nc.conn = tempConn
	})

	return nil
}

func (nc *NetConnection) Connect(server, port string) {
	nc.host = false
	nc.conn = nil // Make sure this is a new connection
	nc.remIn, nc.locIn = nc.GetHostGuestRemap()

	SafeGo(func() {
		d := net.Dialer{Timeout: 1 * time.Second}
		for {
			if nc.isClosing() {
				return
			}
			tempConn, err := d.Dial("tcp", server+":"+port)
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

			// Handshake complete. Make temp connection permanent
			if nc.isClosing() {
				tcpConn.Close()
				return
			}
			nc.conn = tcpConn
			return
		}
	})
}


// platformNetStats is a browser-overlay affordance (see netplay_js.go).
func platformNetStats(pingMs int) {}
