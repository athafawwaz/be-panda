package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"panda-backend/internal/models"
)

func TestChatCanceledBeforePersistence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := recordChat(ctx, nil, models.User{}, models.User{}, false, AIChatRequest{}, &AIChatResponse{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled persistence accessed DB: %v", err)
	}
	if resp, err := RunChat(ctx, nil, models.User{}, models.User{}, false, AIChatRequest{}, nil); resp != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled run accessed DB: %v %v", resp, err)
	}
}

func TestChatTitle(t *testing.T) {
	if got := chatTitle("  hello\n world  "); got != "hello world" {
		t.Fatal(got)
	}
	if got := chatTitle(strings.Repeat("界", 81)); got != strings.Repeat("界", 80)+"…" {
		t.Fatal(got)
	}
}
