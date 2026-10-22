//go:build linux

package httptransport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// RequestContext cancels on a peer disconnect without reading or writing socket
// bytes. Call cleanup before returning the handler or releasing its connection.
// ponytail: Linux TCP/TLS only; add a verified OS implementation before deploying elsewhere.
func RequestContext(parent context.Context, conn net.Conn) (context.Context, func(), error) {
	if secure, ok := conn.(*tls.Conn); ok {
		conn = secure.NetConn()
	}
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return nil, nil, fmt.Errorf("disconnect detection requires syscall.Conn, got %T", conn)
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return nil, nil, fmt.Errorf("socket access: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var pollErr error
				disconnected := false
				controlErr := raw.Control(func(fd uintptr) {
					fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLRDHUP}}
					_, pollErr = unix.Poll(fds, 0)
					disconnected = fds[0].Revents&(unix.POLLRDHUP|unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0
				})
				if controlErr == nil && errors.Is(pollErr, syscall.EINTR) {
					continue
				}
				if controlErr != nil || pollErr != nil || disconnected {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() { cancel(); <-done }, nil
}
