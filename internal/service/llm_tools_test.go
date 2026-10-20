package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func toolReq(msgs ...chatMessage) toolChatRequest {
	return toolChatRequest{Messages: msgs, Tools: []toolDef{{Type: "function", Function: toolFunctionDef{Name: "search_documents",
		Parameters: map[string]any{"type": "object"}}}}, Timeout: 5 * time.Second}
}

func TestChatCompletionToolsParsesToolCalls(t *testing.T) {
	var sent map[string]any
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		sent = body
		fmt.Fprint(w, `{"choices":[{"message":{"content":null,"tool_calls":[
			{"id":"call_a","type":"function","function":{"name":"search_documents","arguments":"{\"query\":\"UAT VISTA\"}"}},
			{"type":"function","function":{"name":"list_documents","arguments":{"category":"UAT"}}}]},
			"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":300,"completion_tokens":20}}`)
	})
	res, err := chatCompletionTools(context.Background(), cfg, toolReq(textMessage("user", "cari UAT")))
	if err != nil {
		t.Fatal(err)
	}
	if sent["tool_choice"] != "auto" || sent["tools"] == nil {
		t.Fatalf("tools & tool_choice harus dikirim: %v", sent)
	}
	if len(res.ToolCalls) != 2 || res.FinishReason != "tool_calls" || res.Usage.TotalTokens != 320 {
		t.Fatalf("hasil salah: %+v", res)
	}
	if res.ToolCalls[0].ID != "call_a" || res.ToolCalls[0].Function.Arguments != `{"query":"UAT VISTA"}` {
		t.Fatalf("tool call pertama salah: %+v", res.ToolCalls[0])
	}
	// id kosong dibuatkan; arguments berbentuk objek diterima
	if res.ToolCalls[1].ID == "" || res.ToolCalls[1].Function.Name != "list_documents" || !strings.Contains(res.ToolCalls[1].Function.Arguments, `"category":"UAT"`) {
		t.Fatalf("tool call kedua salah: %+v", res.ToolCalls[1])
	}
}

func TestChatCompletionStreamToolsAccumulatesFragments(t *testing.T) {
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`{"choices":[{"delta":{"content":"Saya akan mencari dulu."}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search_documents","arguments":""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"read_document","arguments":"{\"ref\":\"E1\"}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"BAST CorePay\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":500,"completion_tokens":40}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	var deltas strings.Builder
	var started []string
	res, err := chatCompletionStreamTools(context.Background(), cfg, toolReq(textMessage("user", "x")),
		func(s string) { deltas.WriteString(s) }, func(name string) { started = append(started, name) })
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolCalls) != 2 || res.ToolCalls[0].Function.Arguments != `{"query":"BAST CorePay"}` || res.ToolCalls[1].ID != "call_2" {
		t.Fatalf("potongan argumen harus disambung per index: %+v", res.ToolCalls)
	}
	var args map[string]string
	if json.Unmarshal([]byte(res.ToolCalls[0].Function.Arguments), &args) != nil || args["query"] != "BAST CorePay" {
		t.Fatalf("argumen hasil sambungan bukan JSON valid: %q", res.ToolCalls[0].Function.Arguments)
	}
	if deltas.String() != "Saya akan mencari dulu." || res.Content != deltas.String() || res.FinishReason != "tool_calls" {
		t.Fatalf("konten stream salah: %q / %+v", deltas.String(), res)
	}
	if strings.Join(started, ",") != "search_documents,read_document" || res.Usage.TotalTokens != 540 {
		t.Fatalf("onToolStart / usage salah: %v %+v", started, res.Usage)
	}
}

func TestChatCompletionStreamToolsWithoutIndex(t *testing.T) {
	// Sebagian endpoint (mis. Gemini OpenAI-compatible) mengirim tool call utuh tanpa index & tanpa id
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"function":{"name":"list_documents","arguments":"{}"}},{"function":{"name":"get_repository_stats","arguments":"{\"group_by\":\"category\"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	res, err := chatCompletionStreamTools(context.Background(), cfg, toolReq(textMessage("user", "x")), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolCalls) != 2 || res.ToolCalls[0].ID != "call_1" || res.ToolCalls[1].ID != "call_2" || res.ToolCalls[1].Function.Name != "get_repository_stats" {
		t.Fatalf("tool call tanpa index harus dipisah & dibuatkan id: %+v", res.ToolCalls)
	}
	if !res.Usage.Estimated || res.Usage.CompletionTokens == 0 {
		t.Fatalf("usage harus diperkirakan termasuk argumen tool: %+v", res.Usage)
	}
}

func TestChatCompletionStreamToolsContentOnlyAndPlainJSON(t *testing.T) {
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		// Provider mengabaikan stream dan membalas JSON biasa
		fmt.Fprint(w, `{"choices":[{"message":{"content":"Halo! Saya PANDA AI."},"finish_reason":"stop"}]}`)
	})
	var deltas strings.Builder
	res, err := chatCompletionStreamTools(context.Background(), cfg, toolReq(textMessage("user", "halo")), func(s string) { deltas.WriteString(s) }, nil)
	if err != nil || res.Content != "Halo! Saya PANDA AI." || len(res.ToolCalls) != 0 || deltas.String() != res.Content {
		t.Fatalf("jawaban tanpa tool salah: %+v err=%v", res, err)
	}
}

func TestToolsUnsupportedClassification(t *testing.T) {
	calls := 0
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"tools is not supported for this model"}}`)
	})
	_, err := chatCompletionStreamTools(context.Background(), cfg, toolReq(textMessage("user", "x")), nil, nil)
	var unsupported *ErrToolsUnsupported
	if !errors.As(err, &unsupported) || calls != 1 {
		t.Fatalf("400 yang menyebut tools harus ditandai ErrToolsUnsupported tanpa diulang: calls=%d err=%v", calls, err)
	}

	cfg = testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"invalid api key for tools"}`)
	})
	if _, err := chatCompletionTools(context.Background(), cfg, toolReq(textMessage("user", "x"))); errors.As(err, &unsupported) {
		t.Fatalf("401 bukan tanda provider tanpa tools: %v", err)
	}
}

func TestChatCompletionStreamToolsRetriesWithoutStreamOptions(t *testing.T) {
	calls := 0
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		calls++
		if _, ok := body["stream_options"]; ok {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"unknown field stream_options"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	})
	res, err := chatCompletionStreamTools(context.Background(), cfg, toolReq(textMessage("user", "x")), nil, nil)
	if err != nil || calls != 2 || res.Content != "ok" {
		t.Fatalf("harus diulang tanpa stream_options: calls=%d res=%+v err=%v", calls, res, err)
	}
}

func TestToolChatPayloadMessagesSerialization(t *testing.T) {
	msgs := []chatMessage{
		textMessage("user", "x"),
		{Role: "assistant", ToolCalls: []toolCall{{ID: "c1", Type: "function", Function: toolCallFunction{Name: "list_documents", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "c1", Content: strPtr(`{"items":[]}`)},
	}
	b, _ := json.Marshal(toolChatPayload(LLMConfig{Model: "m"}, toolChatRequest{Messages: msgs, ToolChoice: "none",
		Tools: []toolDef{{Type: "function"}}}, false))
	s := string(b)
	if !strings.Contains(s, `"content":null,"tool_calls"`) || !strings.Contains(s, `"tool_call_id":"c1"`) || !strings.Contains(s, `"tool_choice":"none"`) {
		t.Fatalf("serialisasi pesan tool salah: %s", s)
	}
	if strings.Contains(s, `"stream"`) {
		t.Fatalf("payload non-stream tidak boleh memuat stream: %s", s)
	}
}

func strPtr(s string) *string { return &s }

func TestToolCallKeepsGeminiThoughtSignature(t *testing.T) {
	// Gemini 3 (OpenAI-compatible) mewajibkan extra_content.google.thought_signature dikirim balik pada putaran berikutnya
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"g1","type":"function","function":{"name":"list_documents","arguments":"{}"},"extra_content":{"google":{"thought_signature":"SIG123"}}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	res, err := chatCompletionStreamTools(context.Background(), cfg, toolReq(textMessage("user", "x")), nil, nil)
	if err != nil || len(res.ToolCalls) != 1 || !strings.Contains(string(res.ToolCalls[0].ExtraContent), "SIG123") {
		t.Fatalf("thought_signature harus disimpan: %+v %v", res.ToolCalls, err)
	}
	b, _ := json.Marshal(chatMessage{Role: "assistant", ToolCalls: res.ToolCalls})
	if !strings.Contains(string(b), `"extra_content":{"google":{"thought_signature":"SIG123"}}`) {
		t.Fatalf("thought_signature harus dikirim balik persis: %s", b)
	}

	cfg = testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		fmt.Fprint(w, `{"choices":[{"message":{"tool_calls":[{"id":"g2","type":"function","function":{"name":"list_documents","arguments":"{}"},"extra_content":{"google":{"thought_signature":"SIG456"}}}]},"finish_reason":"tool_calls"}]}`)
	})
	res, err = chatCompletionTools(context.Background(), cfg, toolReq(textMessage("user", "x")))
	if err != nil || !strings.Contains(string(res.ToolCalls[0].ExtraContent), "SIG456") {
		t.Fatalf("thought_signature non-stream harus disimpan: %+v %v", res.ToolCalls, err)
	}
	if b, _ := json.Marshal(toolCall{ID: "x", Type: "function"}); strings.Contains(string(b), "extra_content") {
		t.Fatalf("extra_content kosong tidak boleh dikirim: %s", b)
	}
}

func TestThoughtSignatureErrorIsNotToolsUnsupported(t *testing.T) {
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":400,"message":"Function call is missing a thought_signature in functionCall parts."}}`)
	})
	_, err := chatCompletionStreamTools(context.Background(), cfg, toolReq(textMessage("user", "x")), nil, nil)
	var unsupported *ErrToolsUnsupported
	if err == nil || errors.As(err, &unsupported) {
		t.Fatalf("error validasi tool call bukan tanda provider tanpa tools: %v", err)
	}
}
