package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

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
