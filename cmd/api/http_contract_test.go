package main

import (
	"encoding/json"
	"github.com/gofiber/fiber/v3"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"panda-backend/internal/handler"
)

// Exercise the production route table, not an AST approximation of middleware wiring.
func TestHTTPRouteAuthorizationContract(t *testing.T) {
	h := &handler.Handler{JWTSecret: []byte("test-secret-that-is-at-least-32-bytes")}
	r := newRouter(h)
	for _, route := range r.GetRoutes(true) {
		if route.Path == "/api/v1/public/stats" || route.Path == "/api/v1/auth/login" {
			continue
		}
		t.Run(route.Method+" "+route.Path, func(t *testing.T) {
			for _, credentials := range []string{"", "Bearer invalid", "expired", "query"} {
				path := strings.ReplaceAll(route.Path, ":id", "contract-id")
				req := httptest.NewRequest(route.Method, path, nil)
				switch credentials {
				case "expired":
					token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"exp": time.Now().Add(-time.Hour).Unix()}).SignedString(h.JWTSecret)
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Authorization", "Bearer "+token)
				case "query":
					req.URL.RawQuery = "token=untrusted"
				default:
					req.Header.Set("Authorization", credentials)
				}
				w := testHTTP(t, r, req)
				var body map[string]string
				if w.Code != http.StatusUnauthorized || json.Unmarshal(w.Body.Bytes(), &body) != nil || body["error"] == "" {
					t.Fatalf("credentials=%q: %d %s", credentials, w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestHTTPPublicAndRoutingContract(t *testing.T) {
	r := newRouter(&handler.Handler{})
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/api/v1/auth/login", `{}`, 400},
		{"POST", "/api/v1/auth/login", `{"badge_number":"badge"}`, 400},
		{"POST", "/api/v1/auth/login", `{"password":"Panda"}`, 400},
		{"POST", "/api/v1/auth/login", `{`, 400},
		{"OPTIONS", "/api/v1/documents/upload", "", 204},
		{"GET", "/api/v1/unknown", "", 404},
		{"GET", "/API/v1/auth/me", "", 404},
		{"PUT", "/api/v1/auth/me", "", 404},
		{"GET", "/api/v1/auth/me/", "", 301},
	} {
		t.Run(tc.method+tc.path+tc.body, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := testHTTP(t, r, req)
			if w.Code != tc.status {
				t.Fatalf("got %d want %d: %s", w.Code, tc.status, w.Body.String())
			}
		})
	}
}

func testHTTP(t *testing.T, app *fiber.App, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	response, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	w := httptest.NewRecorder()
	for key, values := range response.Header {
		w.Header()[key] = values
	}
	w.WriteHeader(response.StatusCode)
	if _, err := io.Copy(w, response.Body); err != nil {
		t.Fatal(err)
	}
	return w
}
