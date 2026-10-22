package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"panda-backend/internal/service"
)

func bindChatRequest(c *gin.Context) (service.AIChatRequest, bool) {
	var req service.AIChatRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Prompt) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Pertanyaan / Prompt tidak boleh kosong"})
		return req, false
	}
	return req, true
}

func chatError(err error) (int, string) {
	var agentErr *service.AgentError
	if errors.As(err, &agentErr) {
		return http.StatusBadGateway, agentErr.Message
	}
	return http.StatusInternalServerError, "Gagal memproses pencarian Agentic AI"
}

func (h *Handler) AgenticAIChat(c *gin.Context) {
	req, ok := bindChatRequest(c)
	if !ok {
		return
	}
	actor, impersonating := getRealUser(c)
	ctx := c.Request.Context()
	resp, err := service.RunChat(ctx, h.DB, getCurrentUser(c), actor, impersonating, req, nil)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		log.Printf("[Agentic AI] gagal: %v", err)
		status, message := chatError(err)
		c.JSON(status, gin.H{"error": message})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) AgenticAIChatStream(c *gin.Context) {
	req, ok := bindChatRequest(c)
	if !ok {
		return
	}
	actor, impersonating := getRealUser(c)
	user := getCurrentUser(c)
	streamChat(c.Request.Context(), c.Writer, func(ctx context.Context, emit service.AgentEmitter) (*service.AIChatResponse, error) {
		return service.RunChat(ctx, h.DB, user, actor, impersonating, req, emit)
	})
}

// streamChat is the SSE transport adapter. The use case owns generation and persistence;
// only this adapter emits one terminal done/error event and owns the heartbeat lifetime.
func streamChat(parent context.Context, w http.ResponseWriter, run func(context.Context, service.AgentEmitter) (*service.AIChatResponse, error)) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	var mu sync.Mutex
	terminal := false
	flushWriter := w
	if wrapped, ok := w.(interface{ Unwrap() http.ResponseWriter }); ok {
		flushWriter = wrapped.Unwrap() // Gin's Flush hides the underlying flush error.
	}
	write := func(frame string) {
		if _, err := fmt.Fprint(w, frame); err != nil {
			cancel()
			return
		}
		if err := http.NewResponseController(flushWriter).Flush(); err != nil {
			cancel()
		}
	}
	emit := func(event string, data any) {
		payload, err := json.Marshal(data)
		if err != nil {
			log.Printf("[Agentic AI] invalid SSE payload: %v", err)
			cancel()
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if terminal || ctx.Err() != nil {
			return
		}
		terminal = event == "done" || event == "error"
		write(fmt.Sprintf("event: %s\ndata: %s\n\n", event, payload))
	}
	var heartbeat sync.WaitGroup
	heartbeat.Add(1)
	go func() {
		defer heartbeat.Done()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				mu.Lock()
				if !terminal && ctx.Err() == nil {
					write(": ping\n\n")
				}
				mu.Unlock()
			}
		}
	}()
	defer func() { cancel(); heartbeat.Wait() }()
	resp, err := run(ctx, emit)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		log.Printf("[Agentic AI] stream gagal: %v", err)
		_, message := chatError(err)
		emit("error", gin.H{"error": message})
		return
	}
	emit("done", resp)
}
