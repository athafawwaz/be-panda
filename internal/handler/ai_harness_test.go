package handler

import (
	"context"
	"testing"

	"panda-backend/internal/models"
	"panda-backend/internal/service"
)

func TestCanceledAgentResponseDoesNotPersist(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A canceled response must return before even accessing the database.
	h := &Handler{}
	h.recordAgenticChat(ctx, models.User{}, models.User{}, false, service.AIChatRequest{}, &service.AIChatResponse{})
}
