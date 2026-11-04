package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestToolAgentCanceledPartialAnswerIsNotSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		cancel()
	})
	rec := &recordedEvents{}
	resp, err := runToolAgent(ctx, newTestToolAgent(cfg, "x", false, rec), nil, "")
	if resp != nil || !errors.Is(err, context.Canceled) || rec.deltas.Len() != 0 {
		t.Fatalf("cancellation must not return a completed response: resp=%v err=%v deltas=%q", resp, err, rec.deltas.String())
	}
}

func TestToolAgentRejectsIncompleteCallsBeforeExecution(t *testing.T) {
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"list_documents","arguments":"{}"}}]}}]}`+"\n\n")
	})
	a := newTestToolAgent(cfg, "x", false, nil)
	resp, err := runToolAgent(context.Background(), a, nil, "")
	if err == nil || resp != nil {
		t.Fatalf("incomplete stream accepted: %v %v", resp, err)
	}
	for _, step := range a.tr.steps {
		if step.Kind == "tool" {
			t.Fatal("incomplete tool call was executed")
		}
	}
}

func TestToolAgentResearchDeadlineAllowsOneBoundedFinalRequest(t *testing.T) {
	var rounds atomic.Int32
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		rounds.Add(1)
		if body["tool_choice"] == "none" {
			sseContent(w, "Bukti belum cukup.")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(80 * time.Millisecond)
	})
	rec := &recordedEvents{}
	budget := toolAgentBudget{rounds: 5, calls: 8, deadline: time.Now().Add(30 * time.Millisecond)}
	resp, err := runToolAgentWithBudget(context.Background(), newTestToolAgent(cfg, "x", false, rec), nil, "", budget)
	if err != nil || resp == nil || resp.ReplyMarkdown != "Bukti belum cukup." || rounds.Load() != 2 || rec.deltas.String() != resp.ReplyMarkdown {
		t.Fatalf("deadline must discard partial research and make one final request: resp=%v rounds=%d err=%v", resp, rounds.Load(), err)
	}
}

func TestToolAgentThinkingDoesNotGuessSemanticStall(t *testing.T) {
	var rounds atomic.Int32
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		n := rounds.Add(1)
		if n == 5 {
			if body["tool_choice"] != "auto" {
				t.Error("stats calls must not force semantic stall")
			}
			sseContent(w, "Statistik telah diperiksa.")
			return
		}
		sseToolCall(w, "", fmt.Sprint(n), "get_repository_stats", `{}`)
	})
	cfg.ThinkingMaxSteps = 6
	resp, err := runToolAgent(context.Background(), newTestToolAgent(cfg, "statistik", true, nil), nil, "")
	if err != nil || resp == nil || rounds.Load() != 5 {
		t.Fatalf("unexpected stall: %v %v", resp, err)
	}
}

func TestToolAgentOnlyCitesRefsDeliveredToModel(t *testing.T) {
	a := newTestToolAgent(LLMConfig{Model: "m"}, "x", false, nil)
	hidden := a.docRef(a.docByID["doc-sop"])
	out, _, _ := a.runToolCall(context.Background(), toolCall{ID: "c1", Function: toolCallFunction{Name: "list_documents", Arguments: `{"category":"UAT"}`}}, 1)
	var payload struct {
		Items []struct {
			Ref string `json:"ref"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil || len(payload.Items) != 1 {
		t.Fatalf("bad result %s %v", out, err)
	}
	resp := a.finishToolAnswer("Visible ["+payload.Items[0].Ref+"]. Hidden ["+hidden+"].", 1)
	if len(resp.MatchedDocuments) != 1 || resp.MatchedDocuments[0].ID != "doc-uat" || strings.Contains(resp.ReplyMarkdown, "["+hidden+"]") || !strings.Contains(resp.ReplyMarkdown, "belum terverifikasi") {
		t.Fatalf("hidden ref accepted: %+v", resp)
	}
	out, _, _ = a.runToolCall(context.Background(), toolCall{ID: "c2", Function: toolCallFunction{Name: "read_document", Arguments: `{"ref":"` + hidden + `"}`}}, 2)
	if !strings.Contains(out, "ref tidak dikenal") {
		t.Fatalf("guessed read accepted: %s", out)
	}
}

func TestToolAgentStatsAndContentProvenance(t *testing.T) {
	a := newTestToolAgent(LLMConfig{Model: "m", ShareDocContent: true}, "x", false, nil)
	a.runToolCall(context.Background(), toolCall{ID: "s", Function: toolCallFunction{Name: "get_repository_stats", Arguments: `{}`}}, 1)
	resp := a.finishToolAnswer("Ada dua dokumen.", 1)
	if resp.AnswerBasis == AnswerBasisGeneral || strings.Contains(resp.EngineMode, generalEngineMarker) || resp.Trace.DocContentUsed == nil || *resp.Trace.DocContentUsed {
		t.Fatalf("stats mislabeled: %+v", resp)
	}
	a.docContentUsed = true
	resp = a.finishToolAnswer("Isi tanpa sitasi.", 1)
	if !*resp.Trace.DocContentUsed || replyFreeOfDocContent(resp.Trace.DocContentUsed) {
		t.Fatal("document content must not be marked safe by answer wording")
	}
	unknown := historyMessages([]ChatTurn{{Prompt: "x", Reply: "sensitive", EngineMode: "pengetahuan umum"}}, false)
	if strings.Contains(unknown[1]["content"], "sensitive") {
		t.Fatal("legacy display label must not bypass history policy")
	}
}

func TestToolAgentRefusesToolsAfterForcedFinal(t *testing.T) {
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		sseToolCall(w, "", "c1", "get_repository_stats", `{}`)
	})
	a := newTestToolAgent(cfg, "x", false, nil)
	budget := toolAgentBudget{rounds: 1, calls: 8, deadline: time.Now().Add(time.Second)}
	resp, err := runToolAgentWithBudget(context.Background(), a, nil, "", budget)
	if resp != nil || err == nil || a.successfulTools != 0 {
		t.Fatalf("tool_choice none must not be bypassed: resp=%v err=%v", resp, err)
	}
}

func TestToolAgentCancellationStopsRemainingToolBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		fmt.Fprint(w, `{"choices":[{"message":{"tool_calls":[{"id":"c1","type":"function","function":{"name":"get_repository_stats","arguments":"{}"}},{"id":"c2","type":"function","function":{"name":"get_repository_stats","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	})
	a := newTestToolAgent(cfg, "x", false, nil)
	a.tr.emit = func(event string, data any) {
		if step, ok := data.(AgentStep); ok && event == "step" && step.Kind == "tool" && step.Status == "done" {
			cancel()
		}
	}
	resp, err := runToolAgent(ctx, a, nil, "")
	if resp != nil || !errors.Is(err, context.Canceled) || a.successfulTools != 1 {
		t.Fatalf("remaining tool batch ran after cancellation: count=%d resp=%v err=%v", a.successfulTools, resp, err)
	}
}

func TestCompactToolResultPreservesUnreferencedStructuredData(t *testing.T) {
	res := toolResult{Output: `{"total_documents":2,"groups":[{"key":"SOP","count":1}]}`, Summary: "2 dokumen"}
	if got := compactToolResult("get_repository_stats", res); got != res.Output {
		t.Fatalf("stats without document refs must retain their structured evidence: %s", got)
	}
}
