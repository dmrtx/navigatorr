package smbprobe

import (
	"context"
	"net"
)

// netDialer is kept behind a tiny seam so all network setup remains in Run and
// the semantic probe can be exercised without a network in unit tests.
type netDialer struct{}

func (*netDialer) dial(ctx context.Context, address string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", address)
}
