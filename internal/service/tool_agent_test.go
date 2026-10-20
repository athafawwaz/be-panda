package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"panda-backend/internal/models"
)

func sseContent(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	b, _ := json.Marshal(text)
	fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s},\"finish_reason\":\"stop\"}]}\n\n", b)
	fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20}}\n\ndata: [DONE]\n\n")
}

func sseToolCall(w http.ResponseWriter, thought, id, name, args string) {
	w.Header().Set("Content-Type", "text/event-stream")
	if thought != "" {
		b, _ := json.Marshal(thought)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", b)
	}
	a, _ := json.Marshal(args)
	fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":%q,\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%s}}]},\"finish_reason\":\"tool_calls\"}]}\n\n", id, name, a)
	fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":200,\"completion_tokens\":15}}\n\ndata: [DONE]\n\n")
}

func agentTestDocs() []models.Document {
	app := &models.Application{ID: "app-vista", AppCode: "VISTA", AppName: "Visitor is The Best", Status: "PRODUCTION"}
	secID, appID := "sec-11", "app-vista"
	return []models.Document{
		{ID: "doc-uat", ModuleSource: "APPLICATION", DocCategory: "UAT", Title: "Laporan UAT VISTA", VersionLabel: "v1.1",
			ApplicationID: &appID, Application: app, SectionID: &secID,
			Section:   &models.AppChangeSection{ID: secID, ApplicationID: appID, VersionLabel: "v1.1", SectionTitle: "Change Request", SortOrder: 2},
			AISummary: "Seluruh skenario UAT lulus.", UploadedByName: "Budi", UploadedByBadge: "2419901",
			UploadedAt: time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)},
		{ID: "doc-sop", ModuleSource: "EVIDENCE", DocCategory: "SOP", Title: "SOP Backup Harian", VersionLabel: "v1.0",
			VisibilityScope: "ALL_TIM", AISummary: "Prosedur pencadangan harian.", UploadedByName: "Rina", UploadedByBadge: "2419902",
			UploadedAt: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)},
	}
}

type recordedEvents struct {
	mu     sync.Mutex
	events []string
	deltas strings.Builder
}

func (r *recordedEvents) emit(event string, data any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	if event == "answer_delta" {
		r.deltas.WriteString(data.(map[string]string)["text"])
	}
	if event == "answer_reset" {
		r.deltas.Reset()
	}
}

func newTestToolAgent(cfg LLMConfig, prompt string, thinking bool, rec *recordedEvents) *agentRun {
	var emit AgentEmitter
	if rec != nil {
		emit = rec.emit
	}
	req := AIChatRequest{Prompt: prompt, Thinking: thinking}
	return newAgentRunWithDocs(nil, models.User{Role: models.RoleSuperAdmin}, req, cfg, newTracer(thinking, emit), agentTestDocs())
}

func stepKinds(resp *AIChatResponse) []string {
	var kinds []string
	for _, s := range resp.Trace.Steps {
		kinds = append(kinds, s.Kind+":"+s.Status)
	}
	return kinds
}

func TestToolAgentAnswersCapabilityWithoutTools(t *testing.T) {
	var sent map[string]any
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		sent = body
		sseContent(w, "Secara umum, saya PANDA AI. Saya punya tool search_documents, read_document, dan lainnya.")
	})
	rec := &recordedEvents{}
	resp, err := runToolAgent(context.Background(), newTestToolAgent(cfg, "apa kamu ada tools?", false, rec), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if resp.AnswerBasis != AnswerBasisGeneral || len(resp.MatchedDocuments) != 0 || !strings.Contains(resp.EngineMode, generalEngineMarker) {
		t.Fatalf("jawaban kemampuan harus berlabel pengetahuan umum tanpa kartu dokumen: %+v", resp)
	}
	if sent["tool_choice"] != "auto" || len(sent["tools"].([]any)) != len(agentToolset) {
		t.Fatalf("semua tool harus ditawarkan dengan tool_choice auto: %v", sent["tool_choice"])
	}
	system := sent["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(system, "WAJIB memakai tool") || !strings.Contains(system, "Secara umum") || !strings.Contains(system, "Visitor is The Best (VISTA)") {
		t.Fatalf("system prompt harus memuat kebijakan bebas-dengan-label & daftar aplikasi: %s", system)
	}
	if got := strings.Join(stepKinds(resp), ","); got != "answer:done" {
		t.Fatalf("langkah salah: %s", got)
	}
	if rec.deltas.String() != resp.ReplyMarkdown {
		t.Fatalf("jawaban harus dialirkan: %q", rec.deltas.String())
	}
}

func TestToolAgentCallsToolAndCitesOnlyKnownRefs(t *testing.T) {
	var bodies []map[string]any
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		bodies = append(bodies, body)
		if len(bodies) == 1 {
			sseToolCall(w, "", "call_1", "list_documents", `{"category":"UAT"}`)
			return
		}
		sseContent(w, "Ada satu dokumen UAT, yaitu Laporan UAT VISTA [D1]. Lihat juga [D9].")
	})
	rec := &recordedEvents{}
	resp, err := runToolAgent(context.Background(), newTestToolAgent(cfg, "dokumen UAT apa saja?", false, rec), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.MatchedDocuments) != 1 || resp.MatchedDocuments[0].ID != "doc-uat" || resp.AnswerBasis != AnswerBasisDocuments {
		t.Fatalf("kartu dokumen harus hanya dokumen yang dikutip: %+v basis=%s", resp.MatchedDocuments, resp.AnswerBasis)
	}
	if strings.Contains(resp.ReplyMarkdown, "D9") || !strings.Contains(resp.ReplyMarkdown, "[D1]") {
		t.Fatalf("ref tak dikenal harus dibuang: %q", resp.ReplyMarkdown)
	}
	if !strings.Contains(resp.EngineMode, "PANDA Agent · test-model · 1 tool call") {
		t.Fatalf("engine mode salah: %q", resp.EngineMode)
	}

	var tool *AgentStep
	notes := 0
	for i, s := range resp.Trace.Steps {
		if s.Kind == "tool" {
			tool = &resp.Trace.Steps[i]
		}
		if s.Title == "Sitasi tidak dikenal dibuang" {
			notes++
		}
	}
	if tool == nil || tool.Status != "done" || tool.Tool.Name != "list_documents" || tool.Tool.Arguments["category"] != "UAT" ||
		tool.Tool.ResultCount != 1 || !strings.Contains(tool.Tool.Output, "Laporan UAT VISTA") || strings.Contains(tool.Tool.Output, "SOP Backup") {
		t.Fatalf("detail tool di log salah: %+v", tool)
	}
	if tool.Round != 1 || tool.Tool.ResultChars == 0 || tool.Tool.Phase != "" || len(tool.Results) != 1 || notes != 1 {
		t.Fatalf("log tool tidak lengkap: %+v notes=%d", tool, notes)
	}

	// Putaran kedua mengirim hasil tool dengan tool_call_id yang cocok
	msgs := bodies[1]["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	assistant := msgs[len(msgs)-2].(map[string]any)
	if last["role"] != "tool" || last["tool_call_id"] != "call_1" || assistant["tool_calls"] == nil {
		t.Fatalf("pesan tool salah: %v / %v", assistant, last)
	}
	if u := resp.Trace.Usage; u.Calls != 2 || u.TotalTokens != 215+120 {
		t.Fatalf("token harus dijumlahkan per putaran: %+v", u)
	}
}

func TestToolAgentForcesAnswerWhenBudgetExhausted(t *testing.T) {
	var choices []string
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		choices = append(choices, fmt.Sprint(body["tool_choice"]))
		if body["tool_choice"] == "none" {
			sseContent(w, "Jawaban dari bukti yang ada.")
			return
		}
		sseToolCall(w, "", fmt.Sprintf("call_%d", len(choices)), "get_repository_stats", `{"group_by":"category"}`)
	})
	resp, err := runToolAgent(context.Background(), newTestToolAgent(cfg, "statistik", false, nil), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(choices) != toolAgentStandardRounds || choices[len(choices)-1] != "none" {
		t.Fatalf("putaran terakhir harus memaksa jawaban: %v", choices)
	}
	stopped := false
	for _, s := range resp.Trace.Steps {
		stopped = stopped || (s.Title == "Penelusuran dihentikan" && strings.Contains(s.Detail, "putaran"))
	}
	if !stopped || resp.ReplyMarkdown != "Jawaban dari bukti yang ada." {
		t.Fatalf("alasan berhenti harus tercatat: %v", stepKinds(resp))
	}
}

func TestToolAgentReportsToolsUnsupported(t *testing.T) {
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"This model does not support tools"}}`)
	})
	a := newTestToolAgent(cfg, "cari UAT", false, nil)
	_, err := runToolAgent(context.Background(), a, nil, "")
	var unsupported *ErrToolsUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("harus mengembalikan ErrToolsUnsupported agar pemanggil beralih ke agen JSON: %v", err)
	}
	if a.tr.steps[0].Status != "error" || !strings.Contains(a.tr.steps[0].Detail, "agen penelusuran JSON") {
		t.Fatalf("alasan fallback harus tampil di log: %+v", a.tr.steps[0])
	}
	cfgCopy := cfg
	markToolsUnsupported(cfgCopy)
	if providerSupportsTools(cfgCopy) {
		t.Fatalf("provider yang menolak tools harus diingat")
	}
}

func TestToolAgentMovesThoughtOutOfAnswer(t *testing.T) {
	round := 0
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		round++
		if round == 1 {
			sseToolCall(w, "Saya cek daftar SOP dulu.", "c1", "list_documents", `{"category":"SOP"}`)
			return
		}
		sseContent(w, "Ada SOP Backup Harian [D1].")
	})
	rec := &recordedEvents{}
	resp, err := runToolAgent(context.Background(), newTestToolAgent(cfg, "SOP apa saja?", false, rec), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	events := strings.Join(rec.events, ",")
	if !strings.Contains(events, "answer_reset") || rec.deltas.String() != "Ada SOP Backup Harian [D1]." {
		t.Fatalf("teks sebelum tool call harus ditarik dari jawaban: events=%s answer=%q", events, rec.deltas.String())
	}
	think := resp.Trace.Steps[0]
	if think.Kind != "think" || think.Detail != "Saya cek daftar SOP dulu." || think.Finish != "tool_calls" || think.Model != "test-model" {
		t.Fatalf("pemikiran model harus jadi detail langkah: %+v", think)
	}
}

func TestToolAgentInvalidArgumentsBecomeToolError(t *testing.T) {
	var bodies []map[string]any
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		bodies = append(bodies, body)
		if len(bodies) == 1 {
			sseToolCall(w, "", "c1", "list_documents", `{"category":`)
			return
		}
		sseContent(w, "Maaf, saya ulangi.")
	})
	resp, err := runToolAgent(context.Background(), newTestToolAgent(cfg, "x", false, nil), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	tool := resp.Trace.Steps[1]
	if tool.Kind != "tool" || tool.Status != "error" || tool.Tool.RawArgs == "" || tool.Tool.Error == "" {
		t.Fatalf("argumen rusak harus jadi langkah error: %+v", tool)
	}
	msgs := bodies[1]["messages"].([]any)
	if c := msgs[len(msgs)-1].(map[string]any)["content"].(string); !strings.Contains(c, "bukan JSON valid") {
		t.Fatalf("model harus diberi tahu argumennya rusak: %s", c)
	}
}

func TestToolAgentUnknownToolAndValidation(t *testing.T) {
	a := newTestToolAgent(LLMConfig{Model: "m"}, "x", false, nil)
	out, _, _ := a.runToolCall(context.Background(), toolCall{ID: "c1", Function: toolCallFunction{Name: "delete_everything", Arguments: "{}"}}, 1)
	if !strings.Contains(out, "tidak ada") {
		t.Fatalf("tool tak dikenal harus ditolak: %s", out)
	}
	out, _, _ = a.runToolCall(context.Background(), toolCall{ID: "c2", Function: toolCallFunction{Name: "list_documents", Arguments: `{"category":"RAHASIA"}`}}, 1)
	if !strings.Contains(out, "tidak dikenal") {
		t.Fatalf("kategori di luar daftar harus ditolak: %s", out)
	}
	out, _, _ = a.runToolCall(context.Background(), toolCall{ID: "c3", Function: toolCallFunction{Name: "read_document", Arguments: `{"ref":"E77"}`}}, 1)
	if !strings.Contains(out, "ref tidak dikenal") {
		t.Fatalf("ref tebakan harus ditolak: %s", out)
	}
}

func TestToolAgentFailureFallsBackToBuiltin(t *testing.T) {
	round := 0
	cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
		round++
		if round == 1 {
			sseToolCall(w, "", "c1", "get_repository_stats", `{}`)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	resp, err := runToolAgent(context.Background(), newTestToolAgent(cfg, "x", false, nil), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.ReplyMarkdown, "Penjelasan AI sedang tidak tersedia") || resp.AnswerBasis != AnswerBasisGeneral ||
		!strings.Contains(resp.EngineMode, "Fallback") {
		t.Fatalf("kegagalan model harus jatuh ke jawaban otomatis: %+v", resp)
	}
}

func TestToolAgentApplicationAndStatsTools(t *testing.T) {
	a := newTestToolAgent(LLMConfig{Model: "m"}, "x", false, nil)
	out, _, _ := a.runToolCall(context.Background(), toolCall{ID: "c1", Function: toolCallFunction{Name: "get_application", Arguments: `{"app":"vista"}`}}, 1)
	var app struct {
		Found    bool `json:"found"`
		Versions []struct {
			Version     string   `json:"version"`
			MissingCore []string `json:"missing_core_categories"`
		} `json:"versions"`
	}
	if json.Unmarshal([]byte(out), &app) != nil || !app.Found || len(app.Versions) != 1 || strings.Join(app.Versions[0].MissingCore, ",") != "PRD,BAST" {
		t.Fatalf("profil aplikasi salah: %s", out)
	}
	out, _, _ = a.runToolCall(context.Background(), toolCall{ID: "c2", Function: toolCallFunction{Name: "get_repository_stats", Arguments: `{"group_by":"module"}`}}, 1)
	if !strings.Contains(out, `"total_documents":2`) || !strings.Contains(out, `"Evidence"`) {
		t.Fatalf("statistik salah: %s", out)
	}
}

func TestResolveCitations(t *testing.T) {
	known := func(ref string) bool { return ref == "E1" || ref == "D2" }
	cleaned, refs, dropped := resolveCitations("A [E1]. B [e1, E9]. C [D2][D5]. D [E7].", known)
	if cleaned != "A [E1]. B [E1]. C [D2]. D." || strings.Join(refs, ",") != "E1,D2" || strings.Join(dropped, ",") != "E9,D5,E7" {
		t.Fatalf("hasil salah: %q refs=%v dropped=%v", cleaned, refs, dropped)
	}
	if answerBasisFor("Secara umum, UAT adalah ... [E1]", true) != AnswerBasisMixed || answerBasisFor("x", false) != AnswerBasisGeneral ||
		answerBasisFor("Hasil UAT lulus [E1]", true) != AnswerBasisDocuments {
		t.Fatalf("label basis jawaban salah")
	}
}

func TestIsSmallTalkOrCapability(t *testing.T) {
	yes := []string{"halo", "Halo!", "apa kamu ada tools?", "bukan apa kamu punya tool ?", "kamu bisa apa", "terima kasih banyak", "siapa kamu?"}
	no := []string{"cari dokumen UAT VISTA", "halo, cari BAST CorePay versi 1.1 yang terbaru dong", "apa itu UAT?", "SOP backup harian"}
	for _, p := range yes {
		if !isSmallTalkOrCapability(p) {
			t.Errorf("%q harus dianggap sapaan/pertanyaan kemampuan", p)
		}
	}
	for _, p := range no {
		if isSmallTalkOrCapability(p) {
			t.Errorf("%q bukan sapaan/pertanyaan kemampuan", p)
		}
	}
	if reply := builtinCapabilityReply("Semua Tim", 12); !strings.Contains(reply, "search_documents") || !strings.Contains(reply, "12 dokumen") {
		t.Fatalf("balasan kemampuan harus menyebut tool & jumlah dokumen: %s", reply)
	}
}

func TestRenderAgentMessagesCompactsOldToolResults(t *testing.T) {
	full, compact := "FULL", "COMPACT"
	var msgs []agentChatMsg
	for i := 0; i < 6; i++ {
		msgs = append(msgs, agentChatMsg{msg: chatMessage{Role: "tool", ToolCallID: fmt.Sprint(i), Content: &full}, compact: compact})
	}
	out := renderAgentMessages(msgs, 4)
	got := ""
	for _, m := range out {
		got += m.text()[:1]
	}
	if got != "CCFFFF" {
		t.Fatalf("hanya 4 hasil tool terbaru yang dikirim utuh: %s", got)
	}
}

func TestClosestDocumentsSuggestions(t *testing.T) {
	docs := agentTestDocs()
	got := closestDocuments(docs, "laporan vista bulan ini", 3)
	if len(got) == 0 || got[0].ID != "doc-uat" {
		t.Fatalf("saran dokumen terdekat salah: %v", got)
	}
	if s := notFoundSuggestions(got); !strings.Contains(s, "Laporan UAT VISTA") {
		t.Fatalf("saran harus memuat judul: %s", s)
	}
	if closestDocuments(docs, "klaim asuransi kendaraan", 3) != nil && len(closestDocuments(docs, "klaim asuransi kendaraan", 3)) != 0 {
		t.Fatalf("tanpa kecocokan tidak boleh ada saran")
	}
}

func TestDocFilterMatching(t *testing.T) {
	a := newTestToolAgent(LLMConfig{Model: "m"}, "x", false, nil)
	if !a.anyDocMatches(docFilter{AppCode: "vista"}) || !a.anyDocMatches(docFilter{AppCode: "Visitor"}) {
		t.Fatalf("kode atau potongan nama aplikasi harus cocok")
	}
	if a.anyDocMatches(docFilter{AppCode: "Digimon"}) {
		t.Fatalf("nama yang bukan aplikasi tidak boleh dianggap cocok (dipakai sebagai kata kunci)")
	}
	uat := a.docByID["doc-uat"]
	for _, f := range []docFilter{{Version: "1.1"}, {Version: "V1.1"}, {Category: "uat"}, {Module: "application"}} {
		if !f.matches(uat) {
			t.Errorf("filter %+v harus cocok dengan UAT VISTA v1.1", f)
		}
	}
	if (docFilter{Version: "v1.0"}).matches(uat) || (docFilter{Module: "EVIDENCE"}).matches(uat) {
		t.Fatalf("versi/modul berbeda tidak boleh cocok")
	}
	if got := versionVariants(" v2.0 "); strings.Join(got, ",") != "v2.0,V2.0,2.0" {
		t.Fatalf("varian versi salah: %v", got)
	}
}
