package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testLLMServer(t *testing.T, handler func(w http.ResponseWriter, body map[string]any)) LLMConfig {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		handler(w, body)
	}))
	t.Cleanup(srv.Close)
	return LLMConfig{Provider: "custom", BaseURL: srv.URL, Model: "test-model", APIKey: "k"}
}

func TestChatCompletionCtxReportsUsage(t *testing.T) {
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"halo"}}],"usage":{"prompt_tokens":120,"completion_tokens":8}}`)
	})
	content, usage, err := chatCompletionCtx(context.Background(), cfg, []map[string]string{{"role": "user", "content": "x"}}, 5*time.Second, chatOptions{})
	if err != nil || content != "halo" {
		t.Fatalf("jawaban salah: %q %v", content, err)
	}
	if usage.PromptTokens != 120 || usage.CompletionTokens != 8 || usage.TotalTokens != 128 || usage.Calls != 1 || usage.Estimated {
		t.Fatalf("usage salah: %+v", usage)
	}
}

func TestChatCompletionCtxRetriesWithoutJSONMode(t *testing.T) {
	calls := 0
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		calls++
		if _, ok := body["response_format"]; ok {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"response_format tidak didukung"}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"action\":\"finish\"}"}}]}`)
	})
	content, usage, err := chatCompletionCtx(context.Background(), cfg, []map[string]string{{"role": "user", "content": "abcdefgh"}}, 5*time.Second, chatOptions{JSON: true})
	if err != nil || calls != 2 || !strings.Contains(content, "finish") {
		t.Fatalf("provider tanpa JSON mode harus diulang tanpa response_format: calls=%d err=%v", calls, err)
	}
	if !usage.Estimated || usage.TotalTokens == 0 {
		t.Fatalf("usage harus diperkirakan bila provider tidak melaporkannya: %+v", usage)
	}
}

func TestChatCompletionStreamParsesDeltasAndUsage(t *testing.T) {
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, part := range []string{"Hasil ", "UAT ", "lulus."} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", part)
		}
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":300,\"completion_tokens\":12}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	var deltas []string
	content, usage, err := chatCompletionStream(context.Background(), cfg, []map[string]string{{"role": "user", "content": "x"}}, 5*time.Second,
		func(s string) { deltas = append(deltas, s) })
	if err != nil || content != "Hasil UAT lulus." || len(deltas) != 3 {
		t.Fatalf("stream salah: %q %v %v", content, deltas, err)
	}
	if usage.TotalTokens != 312 || usage.Estimated {
		t.Fatalf("usage dari chunk terakhir harus dipakai: %+v", usage)
	}
}

func TestChatCompletionStreamFallbacks(t *testing.T) {
	// Provider menolak stream_options → diulang tanpa opsi itu; provider mengabaikan stream → JSON biasa
	calls := 0
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		calls++
		if _, ok := body["stream_options"]; ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"jawaban utuh"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	})
	var got strings.Builder
	content, usage, err := chatCompletionStream(context.Background(), cfg, nil, 5*time.Second, func(s string) { got.WriteString(s) })
	if err != nil || calls != 2 || content != "jawaban utuh" || got.String() != "jawaban utuh" || usage.TotalTokens != 12 {
		t.Fatalf("fallback stream salah: calls=%d content=%q delta=%q usage=%+v err=%v", calls, content, got.String(), usage, err)
	}
}

func TestChatCompletionCtxCancelled(t *testing.T) {
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		time.Sleep(2 * time.Second)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := chatCompletionCtx(ctx, cfg, nil, 5*time.Second, chatOptions{}); err == nil {
		t.Fatalf("permintaan yang dibatalkan harus gagal")
	}
}
