package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/sse"
	"gorm.io/gorm"
	"panda-backend/internal/models"
	"panda-backend/internal/service"
)

func TestChatEndpointsValidateBeforeDatabaseAccess(t *testing.T) {
	for _, stream := range []bool{false, true} {
		h := &Handler{}
		r := fiber.New(HTTPConfig())
		endpoint := h.AgenticAIChat
		if stream {
			endpoint = h.AgenticAIChatStream
		}
		r.Post("/chat", endpoint)
		for _, body := range []string{`{`, `{}`, `{"prompt":"  "}`} {
			req := httptest.NewRequest("POST", "/chat", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := testHTTP(t, r, req)
			if w.Code != 400 || strings.Contains(w.Header().Get("Content-Type"), "event-stream") {
				t.Fatalf("stream=%v: %d %s", stream, w.Code, w.Body.String())
			}
		}
	}
}

func TestStreamChatLifecycle(t *testing.T) {
	for _, mode := range []string{"done", "error", "cancel", "concurrent"} {
		t.Run(mode, func(t *testing.T) {
			r := fiber.New(HTTPConfig())
			r.Get("/stream", sse.New(sse.Config{DisableHeartbeat: true, Handler: func(_ fiber.Ctx, s *sse.Stream) error {
				ctx, cancel := context.WithCancel(s.Context())
				defer cancel()
				return streamChat(ctx, s, func(ctx context.Context, emit service.AgentEmitter) (*service.AIChatResponse, error) {
					if mode == "cancel" {
						cancel()
						emit("answer_delta", "late")
						return &service.AIChatResponse{}, nil
					}
					var workers sync.WaitGroup
					for i := 0; i < 8; i++ {
						workers.Add(1)
						go func() { defer workers.Done(); emit("answer_delta", map[string]string{"text": "partial"}) }()
					}
					workers.Wait()
					if mode == "error" {
						return nil, errors.New("private database error")
					}
					return &service.AIChatResponse{ReplyMarkdown: "complete", SessionID: "persisted-session", MessageID: 42}, nil
				})
			}}))
			w := testHTTP(t, r, httptest.NewRequest("GET", "/stream", nil))
			body := w.Body.String()
			expected := 1
			if mode == "cancel" {
				expected = 0
			}
			if strings.Count(body, "event: done")+strings.Count(body, "event: error") != expected || strings.Contains(body, "private database error") {
				t.Fatalf("unsafe terminal events: %s", body)
			}
			if mode == "cancel" && body != "" {
				t.Fatalf("canceled stream emitted: %s", body)
			}
			if mode == "done" && (!strings.Contains(body, `"message_id":42`) || !strings.Contains(body, `"session_id":"persisted-session"`)) {
				t.Fatalf("persisted IDs lost: %s", body)
			}
			if mode == "concurrent" && strings.Count(body, "event: answer_delta") != 8 {
				t.Fatalf("concurrent events lost: %s", body)
			}
		})
	}
}

func TestChatHistoryReadFailureIsNotEmptySuccess(t *testing.T) {
	db := authorizationDB(t)
	if err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) {
		switch dest := tx.Statement.Dest.(type) {
		case *models.AIChatSession:
			*dest = models.AIChatSession{ID: "owned", UserID: "owner"}
		case *[]models.AIChatMessage:
			tx.AddError(errors.New("private read failure"))
		default:
			t.Errorf("unexpected query %T", dest)
		}
	}); err != nil {
		t.Fatal(err)
	}
	r := fiber.New(HTTPConfig())
	r.Use(func(c fiber.Ctx) error { c.Locals("currentUser", models.User{ID: "owner"}); return c.Next() })
	h := &Handler{DB: db}
	r.Get("/sessions/:id", h.GetChatSession)
	w := testHTTP(t, r, httptest.NewRequest(http.MethodGet, "/sessions/owned", nil))
	if w.Code != 500 || strings.Contains(w.Body.String(), "private read failure") {
		t.Fatalf("history failure masked or leaked: %d %s", w.Code, w.Body.String())
	}
}
