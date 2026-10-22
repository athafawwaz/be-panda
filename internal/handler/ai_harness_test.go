package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"panda-backend/internal/models"
	"panda-backend/internal/service"
)

func TestChatEndpointsValidateBeforeDatabaseAccess(t *testing.T) {
	for _, stream := range []bool{false, true} {
		h := &Handler{}
		r := gin.New()
		endpoint := h.AgenticAIChat
		if stream {
			endpoint = h.AgenticAIChatStream
		}
		r.POST("/chat", endpoint)
		for _, body := range []string{`{`, `{}`, `{"prompt":"  "}`} {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest || strings.Contains(w.Header().Get("Content-Type"), "event-stream") {
				t.Fatalf("stream=%v body=%q: status=%d response=%s", stream, body, w.Code, w.Body.String())
			}
		}
	}
}

func TestStreamChatLifecycle(t *testing.T) {
	for _, fail := range []bool{false, true} {
		w := httptest.NewRecorder()
		var runCtx context.Context
		streamChat(context.Background(), w, func(ctx context.Context, emit service.AgentEmitter) (*service.AIChatResponse, error) {
			runCtx = ctx
			emit("answer_delta", map[string]string{"text": "partial"})
			if fail {
				return nil, errors.New("private database error")
			}
			return &service.AIChatResponse{ReplyMarkdown: "complete", SessionID: "persisted-session", MessageID: 42}, nil
		})
		body := w.Body.String()
		if runCtx.Err() == nil {
			t.Fatal("stream context must close with handler")
		}
		if strings.Count(body, "event: done")+strings.Count(body, "event: error") != 1 {
			t.Fatalf("expected one terminal event: %s", body)
		}
		if fail {
			if !strings.Contains(body, "event: error") || strings.Contains(body, "private database error") {
				t.Fatalf("unsafe stream error: %s", body)
			}
		} else if !strings.Contains(body, `"message_id":42`) || !strings.Contains(body, `"session_id":"persisted-session"`) {
			t.Fatalf("done must carry persisted IDs: %s", body)
		}
		if w.Header().Get("Cache-Control") != "no-cache, no-transform" {
			t.Fatal("proxy buffering guard missing")
		}
	}
}

func TestStreamChatCancellationHasNoTerminalEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w := httptest.NewRecorder()
	streamChat(ctx, w, func(ctx context.Context, emit service.AgentEmitter) (*service.AIChatResponse, error) {
		cancel()
		emit("answer_delta", map[string]string{"text": "late"})
		return &service.AIChatResponse{}, nil
	})
	if w.Body.Len() != 0 {
		t.Fatalf("canceled stream emitted data: %s", w.Body.String())
	}
}

type brokenChatWriter struct{ header http.Header }

func (w *brokenChatWriter) Header() http.Header       { return w.header }
func (w *brokenChatWriter) WriteHeader(int)           {}
func (w *brokenChatWriter) Write([]byte) (int, error) { return 0, errors.New("connection closed") }
func (w *brokenChatWriter) Flush()                    {}

func TestStreamChatDisconnectCancelsRun(t *testing.T) {
	w := &brokenChatWriter{header: make(http.Header)}
	streamChat(context.Background(), w, func(ctx context.Context, emit service.AgentEmitter) (*service.AIChatResponse, error) {
		emit("answer_delta", map[string]string{"text": "hello"})
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatal("write failure must cancel agent execution")
		}
		return nil, ctx.Err()
	})
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
			t.Fatalf("unexpected query %T", dest)
		}
	}); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("currentUser", models.User{ID: "owner"}) })
	h := &Handler{DB: db}
	r.GET("/sessions/:id", h.GetChatSession)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sessions/owned", nil))
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "private read failure") {
		t.Fatalf("history failure was masked or leaked: %d %s", w.Code, w.Body.String())
	}
}

func TestStreamChatFlushFailureCancelsRun(t *testing.T) {
	w := &flushErrorChatWriter{ResponseRecorder: httptest.NewRecorder()}
	streamChat(context.Background(), w, func(ctx context.Context, emit service.AgentEmitter) (*service.AIChatResponse, error) {
		emit("answer_delta", map[string]string{"text": "hello"})
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatal("flush failure must cancel agent execution")
		}
		return nil, ctx.Err()
	})
}

type flushErrorChatWriter struct{ *httptest.ResponseRecorder }

func (w *flushErrorChatWriter) FlushError() error { return errors.New("flush failed") }

func TestStreamChatSerializesConcurrentEvents(t *testing.T) {
	w := httptest.NewRecorder()
	streamChat(context.Background(), w, func(ctx context.Context, emit service.AgentEmitter) (*service.AIChatResponse, error) {
		var workers sync.WaitGroup
		for i := 0; i < 8; i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				emit("answer_delta", map[string]string{"text": "chunk"})
			}()
		}
		workers.Wait()
		return &service.AIChatResponse{ReplyMarkdown: "complete"}, nil
	})
	body := w.Body.String()
	if strings.Count(body, "event: answer_delta") != 8 || strings.Count(body, "event: done") != 1 {
		t.Fatalf("concurrent SSE frames were lost or corrupted: %s", body)
	}
}

func TestStreamChatThroughGinWriter(t *testing.T) {
	r := gin.New()
	r.GET("/stream", func(c *gin.Context) {
		streamChat(c.Request.Context(), c.Writer, func(ctx context.Context, emit service.AgentEmitter) (*service.AIChatResponse, error) {
			emit("answer_delta", map[string]string{"text": "hello"})
			if ctx.Err() != nil {
				t.Fatalf("Gin writer flush canceled stream: %v", ctx.Err())
			}
			return &service.AIChatResponse{ReplyMarkdown: "hello"}, nil
		})
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/stream", nil))
	if !w.Flushed || w.Code != http.StatusOK || !strings.HasSuffix(w.Body.String(), "\n\n") || strings.Count(w.Body.String(), "event: done") != 1 {
		t.Fatalf("Gin SSE response failed: %d %s", w.Code, w.Body.String())
	}
}

func TestGinFlushFailureCancelsChat(t *testing.T) {
	r := gin.New()
	r.GET("/stream", func(c *gin.Context) {
		streamChat(c.Request.Context(), c.Writer, func(ctx context.Context, emit service.AgentEmitter) (*service.AIChatResponse, error) {
			emit("answer_delta", map[string]string{"text": "hello"})
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatal("Gin hid underlying flush failure")
			}
			return nil, ctx.Err()
		})
	})
	w := &flushErrorChatWriter{ResponseRecorder: httptest.NewRecorder()}
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/stream", nil))
}
