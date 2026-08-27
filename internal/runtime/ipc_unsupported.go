//go:build !darwin && !linux

package runtime

import (
	"context"
	"net"
)

type unsupportedLocalEndpoint struct{}

func listenLocalEndpoint(string) (localEndpoint, error) { return nil, ErrUnsupportedPlatform }

func (*unsupportedLocalEndpoint) Accept() (net.Conn, error) { return nil, ErrUnsupportedPlatform }
func (*unsupportedLocalEndpoint) Interrupt() error          { return nil }
func (*unsupportedLocalEndpoint) Close() error              { return nil }

func dialLocalEndpoint(context.Context, string) (net.Conn, error) {
	return nil, ErrUnsupportedPlatform
}

func verifyLocalPeer(net.Conn) error { return ErrUnsupportedPlatform }
