package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
)

// Dukungan tool calling OpenAI-compatible (/chat/completions dengan "tools"). Fungsi teks biasa di llm_client.go
// tetap dipakai fitur lain (analisis upload dan tes koneksi).

// toolDef adalah satu tool yang ditawarkan ke model (format OpenAI "tools").
type toolDef struct {
	Type     string          `json:"type"` // selalu "function"
	Function toolFunctionDef `json:"function"`
}

type toolFunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// toolCall adalah satu panggilan tool dari model. ExtraContent dikirim balik persis seperti diterima: model Gemini 3
// (endpoint OpenAI-compatible) menaruh thought_signature di sana dan menolak putaran berikutnya bila hilang.
type toolCall struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Function     toolCallFunction `json:"function"`
	ExtraContent json.RawMessage  `json:"extra_content,omitempty"`
}

type toolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON mentah dari model
}

// chatMessage adalah pesan percakapan yang mendukung tool calling. Content nil hanya untuk assistant yang
// semata-mata memanggil tool (dikirim sebagai null).
type chatMessage struct {
	Role       string     `json:"role"` // system | user | assistant | tool
	Content    *string    `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

func textMessage(role, content string) chatMessage {
	return chatMessage{Role: role, Content: &content}
}

func (m chatMessage) text() string {
	if m.Content == nil {
		return ""
	}
	return *m.Content
}

// chatMessagesFromMaps mengubah pesan teks lama (mis. historyMessages) menjadi chatMessage.
func chatMessagesFromMaps(msgs []map[string]string) []chatMessage {
	out := make([]chatMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, textMessage(m["role"], m["content"]))
	}
	return out
}

// toolChatResult adalah hasil satu panggilan model dengan tools.
type toolChatResult struct {
	Content      string
	ToolCalls    []toolCall
	FinishReason string // stop | tool_calls | length | …
	Usage        LLMUsage
}

// toolChatRequest mengatur satu panggilan model dengan tools.
type toolChatRequest struct {
	Messages    []chatMessage
	Tools       []toolDef
	ToolChoice  string // auto | none | required ("" = auto bila ada tools)
	Temperature *float64
	MaxTokens   int
	Timeout     time.Duration
}

// ErrToolsUnsupported: provider menolak tools/tool_choice; agent melaporkan error tanpa protokol pengganti.
type ErrToolsUnsupported struct{ Cause error }

func (e *ErrToolsUnsupported) Error() string {
	return "provider tidak mendukung tool calling: " + e.Cause.Error()
}
func (e *ErrToolsUnsupported) Unwrap() error { return e.Cause }

// classifyToolsError menandai error yang berarti provider tidak mendukung tool calling: status 400/404/422 yang
// menyebut tools/function sekaligus menyatakan tidak didukung/tidak dikenal. Error validasi lain (mis. thought_signature
// hilang) tetap dilaporkan apa adanya agar tidak salah dianggap provider tanpa tools.
func classifyToolsError(err error) error {
	var llmErr *LLMError
	if !errors.As(err, &llmErr) {
		return err
	}
	if llmErr.Status != http.StatusBadRequest && llmErr.Status != http.StatusNotFound && llmErr.Status != http.StatusUnprocessableEntity {
		return err
	}
	body := strings.ToLower(llmErr.Body)
	mentionsTools := strings.Contains(body, "tool") || strings.Contains(body, "function")
	for _, marker := range []string{"support", "unknown", "unrecognized", "not allowed", "not permitted", "extra inputs"} {
		if mentionsTools && strings.Contains(body, marker) {
			return &ErrToolsUnsupported{Cause: err}
		}
	}
	return err
}

func toolChatPayload(cfg LLMConfig, req toolChatRequest, stream bool) openai.ChatCompletionNewParams {
	temperature := 0.2
	if req.Temperature != nil {
		temperature = *req.Temperature
	}
	payload := openai.ChatCompletionNewParams{Model: cfg.Model, Temperature: openai.Float(temperature)}
	for _, msg := range req.Messages {
		// Preserve provider extensions (Gemini thought signatures) and null assistant content.
		payload.Messages = append(payload.Messages, param.Override[openai.ChatCompletionMessageParamUnion](msg))
	}
	for _, tool := range req.Tools {
		payload.Tools = append(payload.Tools, openai.ChatCompletionFunctionTool(openai.FunctionDefinitionParam{
			Name: tool.Function.Name, Description: openai.String(tool.Function.Description),
			Parameters: openai.FunctionParameters(tool.Function.Parameters),
		}))
	}
	if req.MaxTokens > 0 {
		payload.MaxTokens = openai.Int(int64(req.MaxTokens))
	}
	if len(req.Tools) > 0 {
		choice := req.ToolChoice
		if choice == "" {
			choice = "auto"
		}
		payload.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: openai.String(choice)}
	}
	if stream {
		payload.SetExtraFields(map[string]any{"stream": true, "stream_options": map[string]bool{"include_usage": true}})
	}
	return payload
}

func postToolChat(ctx context.Context, cfg LLMConfig, payload openai.ChatCompletionNewParams, timeout time.Duration) (*http.Response, error) {
	client := openai.NewChatCompletionService(
		option.WithBaseURL(cfg.BaseURL), option.WithAPIKey(cfg.APIKey),
		option.WithMaxRetries(0), option.WithHTTPClient(&http.Client{Timeout: timeout}),
	)
	var raw *http.Response
	// ponytail: retain strict raw decoding for compatible gateways; use SDK streaming when it guarantees terminal completion and extension round-trips.
	_, err := client.New(ctx, payload, option.WithResponseBodyInto(&raw))
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || ctx.Err() != nil {
			return nil, err
		}
		if raw != nil && raw.StatusCode >= 300 && raw.Body != nil {
			body, readErr := io.ReadAll(raw.Body)
			raw.Body.Close()
			if readErr != nil {
				return nil, fmt.Errorf("read LLM response: %w", readErr)
			}
			return nil, &LLMError{Status: raw.StatusCode, Body: string(body)}
		}
		var apiErr *openai.Error
		if errors.As(err, &apiErr) {
			if raw != nil && raw.Body != nil {
				raw.Body.Close()
			}
			return nil, &LLMError{Status: apiErr.StatusCode, Body: apiErr.RawJSON()}
		}
		return nil, err
	}
	return raw, nil
}

// rawToolCall menampung tool call dari respons (lengkap) atau potongan stream (delta).
type rawToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
	ExtraContent json.RawMessage `json:"extra_content"`
}

// argumentsText: sebagian provider mengirim arguments sebagai objek JSON, bukan string berisi JSON.
func (r rawToolCall) argumentsText() string {
	raw := strings.TrimSpace(string(r.Function.Arguments))
	if raw == "" || raw == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(r.Function.Arguments, &s) == nil {
		return s
	}
	return raw
}

type toolResponseBody struct {
	Error   json.RawMessage `json:"error"`
	Choices []struct {
		Message struct {
			Content   *string       `json:"content"`
			ToolCalls []rawToolCall `json:"tool_calls"`
		} `json:"message"`
		Delta struct {
			Content   *string       `json:"content"`
			ToolCalls []rawToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// toolCallAccumulator menyusun tool call dari potongan stream: id & nama di potongan pertama, arguments disambung.
// Potongan tanpa index (sebagian endpoint Gemini) dianggap panggilan baru bila id-nya berbeda.
type toolCallAccumulator struct {
	calls   []toolCall
	byIndex map[int]int
}

func (a *toolCallAccumulator) add(d rawToolCall) {
	if a.byIndex == nil {
		a.byIndex = map[int]int{}
	}
	pos := -1
	switch {
	case d.Index != nil:
		if p, ok := a.byIndex[*d.Index]; ok {
			pos = p
		} else {
			a.calls = append(a.calls, toolCall{Type: "function"})
			pos = len(a.calls) - 1
			a.byIndex[*d.Index] = pos
		}
	case len(a.calls) > 0 && (d.ID == "" || d.ID == a.calls[len(a.calls)-1].ID) && d.Function.Name == "":
		pos = len(a.calls) - 1
	default:
		a.calls = append(a.calls, toolCall{Type: "function"})
		pos = len(a.calls) - 1
	}
	c := &a.calls[pos]
	if d.ID != "" {
		c.ID = d.ID
	}
	if d.Function.Name != "" {
		c.Function.Name = d.Function.Name
	}
	c.Function.Arguments += d.argumentsText()
	if extra := strings.TrimSpace(string(d.ExtraContent)); extra != "" && extra != "null" {
		c.ExtraContent = append(json.RawMessage(nil), d.ExtraContent...)
	}
}

// finish melengkapi id yang kosong agar setiap pesan tool punya tool_call_id yang cocok.
func (a *toolCallAccumulator) finish() []toolCall {
	for i := range a.calls {
		if a.calls[i].ID == "" {
			a.calls[i].ID = fmt.Sprintf("call_%d", i+1)
		}
		if a.calls[i].Type == "" {
			a.calls[i].Type = "function"
		}
	}
	return a.calls
}

func toolUsage(body *toolResponseBody, msgs []chatMessage, content string, calls []toolCall) LLMUsage {
	if body != nil && body.Usage != nil && body.Usage.PromptTokens+body.Usage.CompletionTokens > 0 {
		return LLMUsage{PromptTokens: body.Usage.PromptTokens, CompletionTokens: body.Usage.CompletionTokens,
			TotalTokens: body.Usage.PromptTokens + body.Usage.CompletionTokens, Calls: 1}
	}
	promptChars := 0
	for _, m := range msgs {
		promptChars += len([]rune(m.text()))
		for _, c := range m.ToolCalls {
			promptChars += len([]rune(c.Function.Arguments)) + len(c.Function.Name)
		}
	}
	completionChars := len([]rune(content))
	for _, c := range calls {
		completionChars += len([]rune(c.Function.Arguments)) + len(c.Function.Name)
	}
	u := LLMUsage{PromptTokens: estimateTokens(promptChars), CompletionTokens: estimateTokens(completionChars), Calls: 1, Estimated: true}
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
	return u
}

// chatCompletionTools memanggil model dengan tools tanpa streaming.
func chatCompletionTools(ctx context.Context, cfg LLMConfig, req toolChatRequest) (toolChatResult, error) {
	resp, err := postToolChat(ctx, cfg, toolChatPayload(cfg, req, false), req.Timeout)
	if err != nil {
		status := 0
		var llmErr *LLMError
		if errors.As(err, &llmErr) {
			status = llmErr.Status
		}
		recordLLMUsage(cfg, status, 0, 0, err)
		return toolChatResult{Usage: LLMUsage{Calls: 1}}, classifyToolsError(err)
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		recordLLMUsage(cfg, resp.StatusCode, 0, 0, err)
		return toolChatResult{Usage: LLMUsage{Calls: 1}}, fmt.Errorf("read LLM response: %w", err)
	}
	return parseToolResponse(cfg, resp.StatusCode, respBytes, req.Messages, nil)
}

func parseToolResponse(cfg LLMConfig, status int, respBytes []byte, msgs []chatMessage, onDelta func(string)) (toolChatResult, error) {
	var parsed toolResponseBody
	decodeErr := json.Unmarshal(respBytes, &parsed)
	if decodeErr != nil || (len(parsed.Error) > 0 && string(parsed.Error) != "null") || len(parsed.Choices) == 0 {
		err := errors.New("invalid LLM response")
		if decodeErr == nil && len(parsed.Error) > 0 && string(parsed.Error) != "null" {
			err = errors.New("LLM provider returned an error")
		}
		usage := toolUsage(&parsed, msgs, "", nil)
		recordLLMUsage(cfg, status, usage.PromptTokens, usage.CompletionTokens, err)
		return toolChatResult{Usage: usage}, err
	}
	choice := parsed.Choices[0]
	var acc toolCallAccumulator
	for _, c := range choice.Message.ToolCalls {
		acc.add(c)
	}
	res := toolChatResult{ToolCalls: acc.finish()}
	if choice.Message.Content != nil {
		res.Content = *choice.Message.Content
	}
	if choice.FinishReason != nil {
		res.FinishReason = *choice.FinishReason
	}
	res.Usage = toolUsage(&parsed, msgs, res.Content, res.ToolCalls)
	if res.FinishReason == "length" && len(res.ToolCalls) > 0 {
		err := errors.New("LLM tool calls truncated by token limit")
		recordLLMUsage(cfg, status, res.Usage.PromptTokens, res.Usage.CompletionTokens, err)
		return toolChatResult{Usage: res.Usage}, err
	}
	if onDelta != nil && res.Content != "" {
		onDelta(res.Content)
	}
	recordLLMUsage(cfg, status, res.Usage.PromptTokens, res.Usage.CompletionTokens, nil)
	return res, nil
}

// chatCompletionStreamTools memanggil model dengan tools secara streaming. onDelta (boleh nil) menerima setiap
// potongan konten; onToolStart (boleh nil) dipanggil saat nama tool pertama kali diketahui, agar langkahnya bisa
// langsung tampil. Provider yang menolak stream_options diulang tanpa opsi itu; provider yang mengabaikan stream
// (membalas JSON biasa) tetap dilayani.
func chatCompletionStreamTools(ctx context.Context, cfg LLMConfig, req toolChatRequest, onDelta func(string), onToolStart func(name string)) (toolChatResult, error) {
	payload := toolChatPayload(cfg, req, true)
	resp, err := postToolChat(ctx, cfg, payload, req.Timeout)
	var rejected *LLMError
	if errors.As(err, &rejected) && rejected.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(rejected.Body), "stream_options") {
		payload.SetExtraFields(map[string]any{"stream": true})
		resp, err = postToolChat(ctx, cfg, payload, req.Timeout)
	}
	if err != nil {
		status := 0
		var llmErr *LLMError
		if errors.As(err, &llmErr) {
			status = llmErr.Status
		}
		recordLLMUsage(cfg, status, 0, 0, err)
		return toolChatResult{Usage: LLMUsage{Calls: 1}}, classifyToolsError(err)
	}
	defer resp.Body.Close()

	if !strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		respBytes, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			recordLLMUsage(cfg, resp.StatusCode, 0, 0, readErr)
			return toolChatResult{Usage: LLMUsage{Calls: 1}}, fmt.Errorf("read LLM response: %w", readErr)
		}
		res, err := parseToolResponse(cfg, resp.StatusCode, respBytes, req.Messages, onDelta)
		if err == nil && onToolStart != nil {
			for _, c := range res.ToolCalls {
				onToolStart(c.Function.Name)
			}
		}
		return res, err
	}

	var sb strings.Builder
	var acc toolCallAccumulator
	var last *toolResponseBody
	finish := ""
	done := false
	var streamErr error
	announced := map[int]bool{}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			done = true
			break
		}
		var chunk toolResponseBody
		decodeErr := json.Unmarshal([]byte(data), &chunk)
		if chunk.Usage != nil {
			c := chunk
			last = &c
		}
		if decodeErr != nil {
			streamErr = errors.New("invalid LLM stream response")
			break
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			streamErr = errors.New("LLM provider returned an error")
			break
		}
		if len(chunk.Choices) == 0 && chunk.Usage == nil {
			var metadata map[string]json.RawMessage
			if json.Unmarshal([]byte(data), &metadata) == nil && metadata["prompt_filter_results"] != nil {
				continue // Azure mengirim metadata filter sebelum delta pilihan.
			}
			streamErr = errors.New("invalid LLM stream response")
			break
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != nil && *c.Delta.Content != "" {
				sb.WriteString(*c.Delta.Content)
				if onDelta != nil {
					onDelta(*c.Delta.Content)
				}
			}
			for _, d := range c.Delta.ToolCalls {
				acc.add(d)
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				finish = *c.FinishReason
			}
		}
		if onToolStart != nil {
			for i, c := range acc.calls {
				if !announced[i] && c.Function.Name != "" {
					announced[i] = true
					onToolStart(c.Function.Name)
				}
			}
		}
	}
	res := toolChatResult{Content: sb.String(), ToolCalls: acc.finish(), FinishReason: finish}
	res.Usage = toolUsage(last, req.Messages, res.Content, res.ToolCalls)
	switch {
	case ctx.Err() != nil:
		streamErr = ctx.Err()
	case scanner.Err() != nil:
		streamErr = fmt.Errorf("read LLM stream: %w", scanner.Err())
	case streamErr != nil:
	case !done && finish == "":
		streamErr = fmt.Errorf("LLM stream ended without completion: %w", io.ErrUnexpectedEOF)
	case finish == "length" && len(res.ToolCalls) > 0:
		streamErr = errors.New("LLM tool calls truncated by token limit")
	}
	recordLLMUsage(cfg, resp.StatusCode, res.Usage.PromptTokens, res.Usage.CompletionTokens, streamErr)
	if streamErr != nil {
		return toolChatResult{Usage: res.Usage}, streamErr
	}
	return res, nil
}
