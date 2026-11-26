package protocol_test

import (
	"testing"

	"github.com/ikemen-engine/ggpo/internal/messages"
	"github.com/ikemen-engine/ggpo/internal/mocks"
	"github.com/ikemen-engine/ggpo/internal/protocol"
)

// syncWith runs an endpoint's own sync to the end, every reply carrying the
// remote's magic.
func syncWith(t *testing.T, endpoint *protocol.UdpProtocol, connection *mocks.FakeConnection, magic uint16) {
	t.Helper()
	endpoint.Synchronize()
	for i := 0; i < protocol.NumSyncPackets; i++ {
		request := connection.LastSentMessage.(*messages.SyncRequestPacket)
		reply := messages.NewUDPMessage(messages.SyncReplyMsg).(*messages.SyncReplyPacket)
		reply.RandomReply = request.RandomRequest
		reply.SetHeader(magic, uint16(i))
		endpoint.OnSyncReply(reply, reply.PacketSize())
	}
	if !endpoint.IsSynchronized() {
		t.Fatalf("the endpoint did not sync")
	}
}

func syncRequestWith(magic uint16, random uint32) *messages.SyncRequestPacket {
	request := messages.NewUDPMessage(messages.SyncRequestMsg).(*messages.SyncRequestPacket)
	request.RandomRequest = random
	request.SetHeader(magic, 99)
	return request
}

// A watcher's line answers sync requests only from the host session it synced
// with. A newer session of the same host (the next Turns turn, a rematch)
// reaching it while it finishes its last frames gets nothing back: an answer
// would let that session's endpoint for the watcher sync with this line and
// pin this line's magic, and then reject the watcher's real next session.
// A player's endpoint keeps answering as before.
func TestWatcherLineAnswersOnlyItsOwnHostSession(t *testing.T) {
	const hostMagic, nextSessionMagic = 0x1234, 0x4321
	for _, c := range []struct {
		name         string
		watcherLine  bool
		answersOther bool
	}{{"watcher line", true, false}, {"player endpoint", false, true}} {
		t.Run(c.name, func(t *testing.T) {
			connectStatus := []messages.UdpConnectStatus{{LastFrame: -1}, {LastFrame: -1}}
			connection := mocks.NewFakeConnection()
			const addr, port = "127.2.1.1", 7600
			endpoint := protocol.NewUdpProtocol(&connection, 0, addr, port, &connectStatus)
			if c.watcherLine {
				endpoint.SetWatcherLine()
			}
			// Before it has synced, every request is answered: the line does
			// not know its host yet.
			if handled, err := endpoint.OnSyncRequest(syncRequestWith(nextSessionMagic, 7), 0); !handled || err != nil {
				t.Fatalf("an unsynced endpoint did not answer a sync request: %v %v", handled, err)
			}
			syncWith(t, &endpoint, &connection, hostMagic)
			sent := len(connection.SendMap[addr+":7600"])

			handled, err := endpoint.OnSyncRequest(syncRequestWith(nextSessionMagic, 11), 0)
			if err != nil {
				t.Fatal(err)
			}
			after := len(connection.SendMap[addr+":7600"])
			if c.answersOther {
				if !handled || after != sent+1 {
					t.Fatalf("a player's endpoint no longer answers another magic (handled %v, sent %d)", handled, after-sent)
				}
			} else if handled || after != sent {
				t.Fatalf("the watcher's line answered another host session's sync request (handled %v, sent %d)", handled, after-sent)
			}

			// Its own host's request is answered, with the nonce echoed.
			handled, err = endpoint.OnSyncRequest(syncRequestWith(hostMagic, 13), 0)
			if err != nil || !handled {
				t.Fatalf("the endpoint did not answer its own host (handled %v, err %v)", handled, err)
			}
			reply, ok := connection.LastSentMessage.(*messages.SyncReplyPacket)
			if !ok || reply.RandomReply != 13 {
				t.Fatalf("its own host got %v, want a sync reply echoing 13", connection.LastSentMessage)
			}
		})
	}
}
