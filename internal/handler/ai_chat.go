package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/sse"
	"panda-backend/internal/httptransport"
	"panda-backend/internal/service"
)

func bindChatRequest(c fiber.Ctx) (service.AIChatRequest, error) {
	var req service.AIChatRequest
	if err := c.Bind().JSON(&req); err != nil || strings.TrimSpace(req.Prompt) == "" {
		return req, fiber.NewError(http.StatusBadRequest, "Pertanyaan / Prompt tidak boleh kosong")
	}
	return req, nil
}

func chatError(err error) (int, string) {
	var agentErr *service.AgentError
	if errors.As(err, &agentErr) {
		return http.StatusBadGateway, agentErr.Message
	}
	return http.StatusInternalServerError, "Gagal memproses pencarian Agentic AI"
}

func (h *Handler) AgenticAIChat(c fiber.Ctx) error {
	req, err := bindChatRequest(c)
	if err != nil {
		return err
	}
	ctx, cleanup, err := httptransport.RequestContext(c.Context(), c.RequestCtx().Conn())
	if err != nil {
		return err
	}
	defer cleanup()
	actor, impersonating := getRealUser(c)
	response, err := service.RunChat(ctx, h.DB, getCurrentUser(c), actor, impersonating, req, nil)
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		log.Printf("[Agentic AI] gagal: %v", err)
		status, message := chatError(err)
		return fiber.NewError(status, message)
	}
	return c.JSON(response)
}

func (h *Handler) AgenticAIChatStream(c fiber.Ctx) error {
	req, err := bindChatRequest(c)
	if err != nil {
		return err
	}
	// Decode and capture owned values before the streaming callback outlives this handler.
	actor, impersonating := getRealUser(c)
	user := getCurrentUser(c)
	c.Set("Cache-Control", "no-cache, no-transform")
	return sse.New(sse.Config{Handler: func(streamCtx fiber.Ctx, stream *sse.Stream) error {
		ctx, cleanup, err := httptransport.RequestContext(stream.Context(), streamCtx.RequestCtx().Conn())
		if err != nil {
			return err
		}
		defer cleanup()
		return streamChat(ctx, stream, func(ctx context.Context, emit service.AgentEmitter) (*service.AIChatResponse, error) {
			return service.RunChat(ctx, h.DB, user, actor, impersonating, req, emit)
		})
	}})(c)
}

func streamChat(ctx context.Context, stream *sse.Stream, run func(context.Context, service.AgentEmitter) (*service.AIChatResponse, error)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	terminal := false
	emit := func(event string, data any) {
		mu.Lock()
		defer mu.Unlock()
		if terminal || ctx.Err() != nil {
			return
		}
		terminal = event == "done" || event == "error"
		if err := stream.Event(sse.Event{Name: event, Data: data}); err != nil {
			log.Printf("[Agentic AI] SSE write: %v", err)
			cancel()
		}
	}
	response, err := run(ctx, emit)
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		log.Printf("[Agentic AI] stream gagal: %v", err)
		_, message := chatError(err)
		emit("error", fiber.Map{"error": message})
	} else {
		emit("done", response)
	}
	return stream.Err()
}
