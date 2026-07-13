package transport

import "github.com/ikemen-engine/ggpo/internal/messages"

type MessageHandler interface {
	HandleMessage(ipAddress string, port int, msg messages.UDPMessage, len int)
}
