//go:build linux

package httptransport

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

func TestSocketJSONCancellation(t *testing.T) {
	for _, mode := range []string{"reset", "close", "half-close", "pipeline", "keepalive", "tls/reset", "tls/close", "tls/half-close", "tls/pipeline", "tls/keepalive"} {
		t.Run(mode, func(t *testing.T) {
			secure := strings.HasPrefix(mode, "tls/")
			mode = strings.TrimPrefix(mode, "tls/")
			started, exited := make(chan struct{}), make(chan struct{})
			canceled := make(chan struct{})
			release := make(chan struct{})
			// Release first on failures, then shut down: never strand the provider.
			app := fiber.New()
			app.Post("/chat", func(c fiber.Ctx) error {
				ctx, cleanup, err := RequestContext(c.Context(), c.RequestCtx().Conn())
				if err != nil {
					return err
				}
				defer cleanup()
				defer close(exited)
				close(started)
				select {
				case <-ctx.Done():
					close(canceled)
					return nil
				case <-release:
					return c.JSON(fiber.Map{"done": true})
				}
			})
			app.Get("/next", func(c fiber.Ctx) error { return c.SendString("next") })
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			if secure {
				certServer := httptest.NewTLSServer(http.NotFoundHandler())
				certificates := certServer.TLS.Certificates
				certServer.Close()
				ln = tls.NewListener(ln, &tls.Config{Certificates: certificates, MinVersion: tls.VersionTLS12})
			}
			serverDone := make(chan error, 1)
			go func() { serverDone <- app.Listener(ln) }()
			defer func() {
				close(release)
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
			var conn net.Conn
			if secure {
				// Self-signed, ephemeral loopback server; trust bypass is test-only.
				conn, err = tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
			} else {
				conn, err = net.Dial("tcp", ln.Addr().String())
			}
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			request := "POST /chat HTTP/1.1\r\nHost: localhost\r\nContent-Length: 2\r\n\r\n{}"
			if mode == "pipeline" {
				request += "GET /next HTTP/1.1\r\nHost: localhost\r\n\r\n"
			}
			if _, err := fmt.Fprint(conn, request); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("provider did not start")
			}
			start := time.Now()
			rawConn := conn
			if secure {
				rawConn = conn.(*tls.Conn).NetConn()
			}
			switch mode {
			case "reset":
				if err := rawConn.(*net.TCPConn).SetLinger(0); err != nil {
					t.Fatal(err)
				}
				if err := conn.Close(); err != nil {
					t.Fatal(err)
				}
			case "close":
				if err := conn.Close(); err != nil {
					t.Fatal(err)
				}
			case "half-close":
				if err := rawConn.(*net.TCPConn).CloseWrite(); err != nil {
					t.Fatal(err)
				}
			default:
				select {
				case <-canceled:
					t.Fatal("live connection canceled")
				case <-time.After(50 * time.Millisecond):
				}
				release <- struct{}{}
				reader := bufio.NewReader(conn)
				response, err := http.ReadResponse(reader, nil)
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != 200 {
					t.Fatalf("JSON status: %d", response.StatusCode)
				}
				var payload struct {
					Done bool `json:"done"`
				}
				if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if !payload.Done {
					t.Fatal("JSON contract changed")
				}
				if mode == "keepalive" {
					if _, err := fmt.Fprint(conn, "GET /next HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
						t.Fatal(err)
					}
				}
				response, err = http.ReadResponse(reader, nil)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil || string(body) != "next" {
					t.Fatalf("next request corrupted: %q %v", body, err)
				}
			}
			if mode != "pipeline" && mode != "keepalive" {
				select {
				case <-canceled:
					t.Logf("provider canceled %s after %s", mode, time.Since(start))
				case <-time.After(250 * time.Millisecond):
					t.Fatal("disconnect not detected within 250ms")
				}
			}
			select {
			case <-exited:
			case <-time.After(time.Second):
				t.Fatal("provider leaked")
			}
		})
	}
}
