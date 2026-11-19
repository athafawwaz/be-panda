package handler

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"gorm.io/gorm"
	"panda-backend/internal/models"
)

func TestDocumentHTTPFileContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.pdf")
	content := "%PDF-1.4\ncontract document\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	modified := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	db := authorizationDB(t)
	if err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		doc, ok := tx.Statement.Dest.(*models.Document)
		if !ok {
			t.Errorf("unexpected query %T", tx.Statement.Dest)
			return
		}
		*doc = models.Document{ID: "doc", FilePath: path, StandardizedName: "laporan-é.pdf"}
	}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{DB: db}
	r := fiber.New(HTTPConfig())
	r.Use(func(c fiber.Ctx) error {
		c.Locals("currentUser", models.User{Role: models.RoleSuperAdmin})
		return c.Next()
	})
	r.Get("/preview/:id", h.PreviewDocument)
	r.Head("/preview/:id", h.PreviewDocument)
	r.Get("/download/:id", h.DownloadDocument)
	for _, tc := range []struct {
		name, method, path, rangeHeader, since string
		status                                 int
		body                                   string
	}{
		{"preview", "GET", "/preview/doc", "", "", 200, content},
		{"download", "GET", "/download/doc", "", "", 200, content},
		{"range", "GET", "/preview/doc", "bytes=0-3", "", 206, "%PDF"},
		{"invalid range", "GET", "/preview/doc", "bytes=9999-", "", 416, ""},
		{"head", "HEAD", "/preview/doc", "", "", 200, ""},
		{"not modified", "GET", "/preview/doc", "", modified.Format(http.TimeFormat), 304, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Range", tc.rangeHeader)
			req.Header.Set("If-Modified-Since", tc.since)
			w := testHTTP(t, r, req)
			if w.Code != tc.status {
				t.Fatalf("got %d want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if tc.status != 416 && w.Body.String() != tc.body {
				t.Fatalf("unexpected body %q", w.Body.String())
			}
			if tc.status == 200 || tc.status == 206 {
				if w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
					t.Fatal("file privacy headers lost")
				}
				if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/pdf") {
					t.Fatal("PDF content type lost")
				}
				disposition := "inline"
				if tc.name == "download" {
					disposition = "attachment"
				}
				if !strings.HasPrefix(w.Header().Get("Content-Disposition"), disposition+";") {
					t.Fatal("file disposition lost")
				}
			}
			if tc.status == 206 && w.Header().Get("Content-Range") == "" {
				t.Fatal("range metadata missing")
			}
		})
	}
}
