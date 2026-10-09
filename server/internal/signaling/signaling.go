// Package signaling relays WebRTC offer/answer/ICE between peers. The server
// never terminates media (D2: data plane is P2P); it stamps the sender and
// forwards bytes. Authority never rides this path (D3).
package signaling

import (
	"errors"

	"fleetplatform/sdk/go/protocol"
)

var ErrBadSignal = errors.New("signaling: missing target or unknown kind")

// Route validates a client-sent signal and returns the delivery copy with
// `from` stamped server-side (the sender's claim is discarded).
func Route(sig protocol.Signal, fromID string) (string, protocol.Signal, error) {
	switch sig.Kind {
	case "offer", "answer", "ice":
	default:
		return "", protocol.Signal{}, ErrBadSignal
	}
	if sig.To == "" {
		return "", protocol.Signal{}, ErrBadSignal
	}
	target := sig.To
	out := protocol.Signal{From: fromID, Kind: sig.Kind, Data: sig.Data}
	return target, out, nil
}
