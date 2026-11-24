package transport

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/ikemen-engine/ggpo/internal/messages"
)

func TestUdpBindFailure(t *testing.T) {
	occupied, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	u, err := NewUdp(nil, occupied.LocalAddr().(*net.UDPAddr).Port)
	if err == nil {
		u.Close()
		t.Fatal("binding an occupied port succeeded")
	}
	if u.IsInitialized() {
		t.Fatal("failed bind left an initialized transport")
	}
	u.Close()
}

func TestUdpCloseStopsSenderAndAllowsRebind(t *testing.T) {
	port := 0
	for i := 0; i < 20; i++ {
		u, err := NewUdp(nil, port)
		if err != nil {
			t.Fatal(err)
		}
		port = u.listener.LocalAddr().(*net.UDPAddr).Port
		copyOfTransport := u
		var wg sync.WaitGroup
		for j := 0; j < 4; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				copyOfTransport.Close()
			}()
		}
		wg.Wait()
		select {
		case <-u.sendDone:
		default:
			t.Fatal("Close returned before sender exited")
		}
		// Sending more than the queue capacity after close must not block.
		sent := make(chan struct{})
		go func() {
			defer close(sent)
			for j := 0; j < 512; j++ {
				u.SendTo(messages.NewUDPMessage(messages.KeepAliveMsg), "127.0.0.1", port)
			}
		}()
		select {
		case <-sent:
		case <-time.After(time.Second):
			t.Fatal("SendTo blocked after Close")
		}
	}
}

// An observed socket read makes this deterministic: Close happens only after a
// packet arrives, while no consumer is available on the delivery channel.
type observedPacketConn struct {
	net.PacketConn
	received chan struct{}
	once     sync.Once
}

func (c *observedPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(p)
	if n > 0 {
		c.once.Do(func() { close(c.received) })
	}
	return n, addr, err
}

func TestUdpCloseUnblocksUndeliveredPacket(t *testing.T) {
	u, err := NewUdp(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	reader := u
	observed := &observedPacketConn{PacketConn: u.listener, received: make(chan struct{})}
	reader.listener = observed
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		reader.Read(make(chan MessageChannelItem))
	}()
	sender, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: u.listener.LocalAddr().(*net.UDPAddr).Port})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	if _, err := sender.Write(messages.NewUDPMessage(messages.KeepAliveMsg).ToBytes()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-observed.received:
	case <-time.After(time.Second):
		t.Fatal("packet did not reach reader")
	}
	u.Close()
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("reader stayed blocked delivering a packet after Close")
	}
}
