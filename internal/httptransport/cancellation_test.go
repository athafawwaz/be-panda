package httptransport

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/sse"
)

// This deliberately passes when the gate is NO-GO: it records the unsupported
// JSON disconnect contract without leaving a failing production test suite.
func TestTCPDisconnectGate(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			started, exited := make(chan struct{}), make(chan struct{})
			observed := make(chan time.Duration, 1)
			release := make(chan struct{})
			defer close(release)
			app := fiber.New(fiber.Config{WriteTimeout: time.Second})
			run := func(ctx context.Context) {
				defer close(exited)
				close(started)
				begin := time.Now()
				select {
				case <-ctx.Done():
					observed <- time.Since(begin)
				case <-release:
				}
			}
			if stream {
				app.Post("/chat", sse.New(sse.Config{HeartbeatInterval: 20 * time.Millisecond, Handler: func(_ fiber.Ctx, s *sse.Stream) error { run(s.Context()); return nil }}))
			} else {
				app.Post("/chat", func(c fiber.Ctx) error { run(c.Context()); return c.JSON(fiber.Map{"done": true}) })
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			serverDone := make(chan error, 1)
			go func() { serverDone <- app.Listener(ln) }()
			defer func() {
				if err := app.ShutdownWithTimeout(2 * time.Second); err != nil {
					t.Error(err)
				}
				select {
				case err := <-serverDone:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(3 * time.Second):
					t.Error("server goroutine did not exit")
				}
			}()
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := fmt.Fprint(conn, "POST /chat HTTP/1.1\r\nHost: localhost\r\nContent-Length: 2\r\n\r\n{}"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("handler did not start")
			}
			if stream {
				if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
					t.Fatal(err)
				}
			}
			// RST makes the disconnect unambiguous; no provider deltas are emitted.
			if err := conn.(*net.TCPConn).SetLinger(0); err != nil {
				t.Fatal(err)
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case elapsed := <-observed:
				if !stream {
					t.Fatal("JSON unexpectedly cancels: reassess NO-GO finding")
				}
				t.Logf("SSE silent provider canceled after %s (test heartbeat 20ms)", elapsed)
			case <-time.After(300 * time.Millisecond):
				if stream {
					t.Fatal("SSE did not detect disconnect within 300ms")
				}
				t.Log("NO-GO: JSON provider still running 300ms after TCP reset; context has no disconnect signal")
				release <- struct{}{}
			}
			select {
			case <-exited:
			case <-time.After(time.Second):
				t.Fatal("provider goroutine leaked")
			}
		})
	}
}
