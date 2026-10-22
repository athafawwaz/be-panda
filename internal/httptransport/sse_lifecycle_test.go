package httptransport

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/sse"
)

func TestSSELifecycleTCP(t *testing.T) {
	for _, mode := range []string{"done", "error", "cancel", "slow reader"} {
		t.Run(mode, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			exited := make(chan struct{})
			app := fiber.New(fiber.Config{WriteTimeout: 100 * time.Millisecond})
			app.Use(func(c fiber.Ctx) error { c.SetContext(parent); return c.Next() })
			app.Post("/chat", sse.New(sse.Config{HeartbeatInterval: 10 * time.Millisecond, Handler: func(_ fiber.Ctx, stream *sse.Stream) error {
				defer close(exited)
				switch mode {
				case "cancel":
					cancel()
					<-stream.Context().Done()
					return nil
				case "slow reader":
					payload := strings.Repeat("x", 1<<20)
					for {
						if err := stream.Event(sse.Event{Name: "answer_delta", Data: payload}); err != nil {
							return err
						}
					}
				default:
					if err := stream.Event(sse.Event{Name: "answer_delta", Data: map[string]string{"text": "partial"}}); err != nil {
						return err
					}
					return stream.Event(sse.Event{Name: mode, Data: map[string]string{"status": "safe"}})
				}
			}}))
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			serverDone := make(chan error, 1)
			go func() { serverDone <- app.Listener(ln) }()
			defer func() {
				cancel()
				if err := app.ShutdownWithTimeout(time.Second); err != nil {
					t.Error(err)
				}
				select {
				case err := <-serverDone:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(2 * time.Second):
					t.Error("server leaked")
				}
			}()
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Fprint(conn, "POST /chat HTTP/1.1\r\nHost: localhost\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			if mode != "slow reader" {
				wire, err := io.ReadAll(bufio.NewReader(conn))
				if err != nil {
					t.Fatal(err)
				}
				terminals := strings.Count(string(wire), "event: done") + strings.Count(string(wire), "event: error")
				expected := 1
				if mode == "cancel" {
					expected = 0
				}
				if terminals != expected {
					t.Fatalf("terminal count %d want %d: %s", terminals, expected, wire)
				}
			}
			select {
			case <-exited:
			case <-time.After(time.Second):
				t.Fatal("stream write/provider goroutine did not stop")
			}
		})
	}
}
