//go:build !linux

package httptransport

import (
	"context"
	"errors"
	"net"
)

func RequestContext(context.Context, net.Conn) (context.Context, func(), error) {
	return nil, nil, errors.New("request disconnect cancellation requires Linux TCP/TLS")
}
