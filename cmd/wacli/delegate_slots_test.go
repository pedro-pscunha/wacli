package main

import (
	"context"
	"net"
)

// handleSendDelegateConn serves one connection with a send slot only. Bulk and
// media kinds are refused on it. Production uses serveDelegateConn with the
// full delegateSlots.
func handleSendDelegateConn(ctx context.Context, conn net.Conn, execute sendDelegateExecutor, sendSlot chan struct{}, pacer *sendPacer) {
	serveDelegateConn(ctx, conn, execute, &delegateSlots{send: sendSlot, bulk: map[string]chan struct{}{}}, pacer)
}
