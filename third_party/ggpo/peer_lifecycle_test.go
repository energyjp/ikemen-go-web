package ggpo_test

import (
	"net"
	"strconv"
	"testing"

	"github.com/ikemen-engine/ggpo"
	"github.com/ikemen-engine/ggpo/internal/mocks"
)

func TestBackendCloseBeforeAddPlayerReleasesPort(t *testing.T) {
	for _, spectator := range []bool{false, true} {
		t.Run(strconv.FormatBool(spectator), func(t *testing.T) {
			probe, err := net.ListenPacket("udp4", "0.0.0.0:0")
			if err != nil {
				t.Fatal(err)
			}
			port := probe.LocalAddr().(*net.UDPAddr).Port
			probe.Close()
			session := mocks.NewFakeSession()
			p := ggpo.NewPeer(&session, port, 2, 8)
			var peer ggpo.Backend = &p
			if spectator {
				s := ggpo.NewSpectator(&session, port, 2, 8, "127.0.0.1", 1)
				peer = &s
			}
			if err := peer.InitializeConnection(); err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			if err := peer.Close(); err != nil {
				t.Fatal(err)
			}
			rebound, err := net.ListenPacket("udp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
			if err != nil {
				t.Fatal("Peer.Close left its UDP port bound:", err)
			}
			rebound.Close()
		})
	}
}
