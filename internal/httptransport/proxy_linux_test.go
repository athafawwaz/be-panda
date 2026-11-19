//go:build linux

package httptransport

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

func TestProxyDisconnectCancelsJSON(t *testing.T) {
	started, exited := make(chan struct{}), make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
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
	target, err := url.Parse("http://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	reverseProxy := httputil.NewSingleHostReverseProxy(target)
	// Cancellation at the proxy is expected here, not a provider error.
	reverseProxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) { w.WriteHeader(http.StatusBadGateway) }
	proxy := httptest.NewServer(reverseProxy)
	defer proxy.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+"/chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	clientDone := make(chan error, 1)
	go func() {
		resp, err := proxy.Client().Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		clientDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("proxy disconnect did not reach provider")
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("provider leaked")
	}
	select {
	case err := <-clientDone:
		if err == nil {
			t.Fatal("canceled request succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("client leaked")
	}
}
