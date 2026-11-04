package handler

import (
	"bufio"
	"bytes"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

func TestUploadHTTPBoundary(t *testing.T) {
	t.Setenv("UPLOAD_MAX_MB", "1")
	app := fiber.New(HTTPConfig())
	app.Post("/upload", func(c fiber.Ctx) error {
		if status, message := parseUploadForm(c); status != 0 {
			return fiber.NewError(status, message)
		}
		file, status, message := readUploadedFile(c)
		if status != 0 {
			return fiber.NewError(status, message)
		}
		return c.JSON(fiber.Map{"name": file.OriginalName})
	})
	for _, tc := range []struct {
		name   string
		data   []byte
		status int
	}{
		{"valid.pdf", []byte("%PDF-1.4\n"), 200},
		{"empty.pdf", nil, 400},
		{"wrong.pdf", []byte("not a pdf"), 415},
		{"blocked.exe", []byte("executable"), 415},
		{"large.pdf", append([]byte("%PDF-1.4\n"), make([]byte, 1<<20)...), 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			part, err := writer.CreateFormFile("file", tc.name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := part.Write(tc.data); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("POST", "/upload", &body)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			w := testHTTP(t, app, req)
			if w.Code != tc.status {
				t.Fatalf("got %d want %d: %s", w.Code, tc.status, w.Body.String())
			}
		})
	}
	t.Run("body rejected before handler", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- app.Listener(ln) }()
		defer func() {
			if err := app.ShutdownWithTimeout(time.Second); err != nil {
				t.Error(err)
			}
			if err := <-done; err != nil {
				t.Error(err)
			}
		}()
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprint(conn, "POST /upload HTTP/1.1\r\nHost: localhost\r\nContent-Length: 3145728\r\nContent-Type: multipart/form-data; boundary=test\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 413 {
			t.Fatalf("oversized body status=%d", response.StatusCode)
		}
	})
}
