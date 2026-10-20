package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LLMUsage adalah pemakaian token LLM eksternal, dijumlahkan per jawaban (bisa beberapa panggilan saat Thinking).
type LLMUsage struct {
	PromptTokens     int  `json:"prompt_tokens"`
	CompletionTokens int  `json:"completion_tokens"`
	TotalTokens      int  `json:"total_tokens"`
	Calls            int  `json:"calls"`
	Estimated        bool `json:"estimated,omitempty"` // sebagian angka diperkirakan karena provider tidak melaporkan usage
}

func (u *LLMUsage) Add(o LLMUsage) {
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
	u.Calls += o.Calls
	u.Estimated = u.Estimated || o.Estimated
}

// chatOptions mengatur satu panggilan /chat/completions.
type chatOptions struct {
	JSON        bool     // minta keluaran objek JSON (response_format); diulang tanpa bila provider menolak
	Temperature *float64 // nil = 0,2
	MaxTokens   int      // 0 = default provider
}

func floatPtr(v float64) *float64 { return &v }

// chatCompletion memanggil endpoint OpenAI-compatible /chat/completions (tanpa pembatalan & tanpa usage).
func chatCompletion(cfg LLMConfig, messages []map[string]string, timeout time.Duration) (string, error) {
	content, _, err := chatCompletionCtx(context.Background(), cfg, messages, timeout, chatOptions{})
	return content, err
}

func chatPayload(cfg LLMConfig, messages []map[string]string, opts chatOptions) map[string]interface{} {
	temperature := 0.2
	if opts.Temperature != nil {
		temperature = *opts.Temperature
	}
	payload := map[string]interface{}{
		"model":       cfg.Model,
		"messages":    messages,
		"temperature": temperature,
	}
	if opts.MaxTokens > 0 {
		payload["max_tokens"] = opts.MaxTokens
	}
	if opts.JSON {
		payload["response_format"] = map[string]string{"type": "json_object"}
	}
	return payload
}

func postChat(ctx context.Context, cfg LLMConfig, payload map[string]interface{}, timeout time.Duration) (*http.Response, error) {
	bodyBytes, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(cfg.BaseURL, "/")+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	return (&http.Client{Timeout: timeout}).Do(req)
}

type chatResponseBody struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// usageFrom memakai angka usage dari provider, atau memperkirakan (±4 karakter per token) bila tidak dilaporkan.
func usageFrom(body *chatResponseBody, messages []map[string]string, content string) LLMUsage {
	if body != nil && body.Usage != nil && body.Usage.PromptTokens+body.Usage.CompletionTokens > 0 {
		return LLMUsage{PromptTokens: body.Usage.PromptTokens, CompletionTokens: body.Usage.CompletionTokens,
			TotalTokens: body.Usage.PromptTokens + body.Usage.CompletionTokens, Calls: 1}
	}
	promptChars := 0
	for _, m := range messages {
		promptChars += len([]rune(m["content"]))
	}
	u := LLMUsage{PromptTokens: estimateTokens(promptChars), CompletionTokens: estimateTokens(len([]rune(content))), Calls: 1, Estimated: true}
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
	return u
}

func estimateTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	return chars/4 + 1
}

func isBadRequest(err error) bool {
	var llmErr *LLMError
	return errors.As(err, &llmErr) && llmErr.Status == http.StatusBadRequest
}

// chatCompletionCtx memanggil /chat/completions dengan pembatalan (ctx) dan mengembalikan pemakaian token.
func chatCompletionCtx(ctx context.Context, cfg LLMConfig, messages []map[string]string, timeout time.Duration, opts chatOptions) (string, LLMUsage, error) {
	content, usage, err := doChatCompletion(ctx, cfg, messages, timeout, opts)
	if err != nil && opts.JSON && isBadRequest(err) {
		// Sebagian provider OpenAI-compatible belum mendukung response_format
		opts.JSON = false
		return doChatCompletion(ctx, cfg, messages, timeout, opts)
	}
	return content, usage, err
}

func doChatCompletion(ctx context.Context, cfg LLMConfig, messages []map[string]string, timeout time.Duration, opts chatOptions) (string, LLMUsage, error) {
	resp, err := postChat(ctx, cfg, chatPayload(cfg, messages, opts), timeout)
	if err != nil {
		recordLLMUsage(cfg, 0, 0, 0, err)
		return "", LLMUsage{}, err
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		llmErr := &LLMError{Status: resp.StatusCode, Body: string(respBytes)}
		recordLLMUsage(cfg, resp.StatusCode, 0, 0, llmErr)
		return "", LLMUsage{}, llmErr
	}
	var parsed chatResponseBody
	if err := json.Unmarshal(respBytes, &parsed); err != nil || len(parsed.Choices) == 0 {
		// Permintaan sudah diterima provider (memakai kuota) walau isinya tidak bisa dibaca
		recordLLMUsage(cfg, resp.StatusCode, 0, 0, nil)
		return "", LLMUsage{Calls: 1}, fmt.Errorf("invalid LLM response")
	}
	content := parsed.Choices[0].Message.Content
	usage := usageFrom(&parsed, messages, content)
	recordLLMUsage(cfg, resp.StatusCode, usage.PromptTokens, usage.CompletionTokens, nil)
	return content, usage, nil
}

// chatCompletionStream memanggil /chat/completions dengan stream=true dan meneruskan setiap potongan jawaban ke
// onDelta. Provider yang tidak mendukung stream_options diulang tanpa opsi itu; provider yang mengabaikan stream
// (membalas JSON biasa) tetap dilayani dengan satu potongan utuh.
func chatCompletionStream(ctx context.Context, cfg LLMConfig, messages []map[string]string, timeout time.Duration, onDelta func(string)) (string, LLMUsage, error) {
	payload := chatPayload(cfg, messages, chatOptions{})
	payload["stream"] = true
	payload["stream_options"] = map[string]bool{"include_usage": true}

	resp, err := postChat(ctx, cfg, payload, timeout)
	if err == nil && resp.StatusCode == http.StatusBadRequest {
		resp.Body.Close()
		delete(payload, "stream_options")
		resp, err = postChat(ctx, cfg, payload, timeout)
	}
	if err != nil {
		recordLLMUsage(cfg, 0, 0, 0, err)
		return "", LLMUsage{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBytes, _ := io.ReadAll(resp.Body)
		llmErr := &LLMError{Status: resp.StatusCode, Body: string(respBytes)}
		recordLLMUsage(cfg, resp.StatusCode, 0, 0, llmErr)
		return "", LLMUsage{}, llmErr
	}

	if !strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		respBytes, _ := io.ReadAll(resp.Body)
		var parsed chatResponseBody
		if err := json.Unmarshal(respBytes, &parsed); err != nil || len(parsed.Choices) == 0 {
			recordLLMUsage(cfg, resp.StatusCode, 0, 0, nil)
			return "", LLMUsage{Calls: 1}, fmt.Errorf("invalid LLM response")
		}
		content := parsed.Choices[0].Message.Content
		onDelta(content)
		usage := usageFrom(&parsed, messages, content)
		recordLLMUsage(cfg, resp.StatusCode, usage.PromptTokens, usage.CompletionTokens, nil)
		return content, usage, nil
	}

	var sb strings.Builder
	var last *chatResponseBody
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk chatResponseBody
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil {
			last = &chunk
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				sb.WriteString(c.Delta.Content)
				onDelta(c.Delta.Content)
			}
		}
	}
	if err := scanner.Err(); err != nil && sb.Len() == 0 {
		recordLLMUsage(cfg, resp.StatusCode, 0, 0, err)
		return "", LLMUsage{Calls: 1}, err
	}
	content := sb.String()
	usage := usageFrom(last, messages, content)
	recordLLMUsage(cfg, resp.StatusCode, usage.PromptTokens, usage.CompletionTokens, nil)
	return content, usage, nil
}
