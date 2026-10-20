package service

import (
	"strings"
	"testing"

	"panda-backend/internal/models"
)

// ─── Rerank ──────────────────────────────────────────────────────────────────

func testEvidence(ref, docID, text string, rrf, bm25 float64) *evidence {
	return &evidence{Ref: ref, Doc: &models.Document{ID: docID, Title: "Dok " + docID}, ChunkID: ref, Text: text, RRF: rrf, BestBM25: bm25}
}

func TestProximityScore(t *testing.T) {
	if got := proximityScore([][]int{{3}, nil}); got != 0 {
		t.Fatalf("satu kata kunci saja tidak punya skor kedekatan, dapat %v", got)
	}
	near := proximityScore([][]int{{3}, {4}})
	far := proximityScore([][]int{{3}, {40}})
	if near != 1 || far >= near || far <= 0 {
		t.Fatalf("kata berdekatan harus lebih tinggi: near=%v far=%v", near, far)
	}
}

func TestRerankEvidencePrefersCoverageAndAgentChoice(t *testing.T) {
	focus := []searchTerm{{Term: "corepay", Display: "corepay", Weight: 1}, {Term: "uji", Display: "pengujian", Weight: 1}}
	a := testEvidence("E1", "A", "Pengujian aplikasi CorePay modul pembayaran dinyatakan lulus.", 0.03, 5)
	b := testEvidence("E2", "B", "Dokumen CorePay berisi daftar modul.", 0.033, 6)
	pool := []*evidence{b, a}
	rerankEvidence(pool, focus, nil, nil)
	if pool[0].Ref != "E1" || a.Features.Coverage != 1 || a.Features.Proximity == 0 {
		t.Fatalf("temuan yang memuat semua kata kunci berdekatan harus teratas: %+v / %+v", a.Features, b.Features)
	}

	// Pilihan agen LLM ikut menentukan urutan akhir
	b.AgentRank = 1
	rerankEvidence(pool, focus, nil, nil)
	if b.Features.Agent != 1 || a.Features.Agent != 0 || b.Score <= 0 {
		t.Fatalf("fitur pilihan agen salah: %+v", b.Features)
	}
	for _, ev := range pool {
		if ev.Score < 0 || ev.Score > 1 {
			t.Fatalf("skor harus 0–1, dapat %v", ev.Score)
		}
	}
}

func TestSelectEvidenceKeepsDiversity(t *testing.T) {
	var pool []*evidence
	for i := 0; i < 5; i++ { // 5 potongan dari dokumen yang sama
		ev := testEvidence("A"+string(rune('0'+i)), "A", "x", 0, 0)
		ev.Score = 1 - float64(i)*0.01
		pool = append(pool, ev)
	}
	for i := 0; i < 8; i++ { // 8 dokumen lain
		ev := testEvidence("D"+string(rune('0'+i)), "doc"+string(rune('0'+i)), "x", 0, 0)
		ev.Score = 0.5
		pool = append(pool, ev)
	}
	selected := selectEvidence(pool)
	perDoc := map[string]int{}
	for _, ev := range selected {
		perDoc[ev.Doc.ID]++
	}
	if perDoc["A"] != maxChunksPerDoc || len(perDoc) != maxSelectedDocs {
		t.Fatalf("maksimal %d potongan per dokumen & %d dokumen, dapat %v", maxChunksPerDoc, maxSelectedDocs, perDoc)
	}
}

func TestLatestSectionDocs(t *testing.T) {
	app := "app1"
	docs := []models.Document{
		{ID: "old", ApplicationID: &app, DocCategory: "UAT", Section: &models.AppChangeSection{SortOrder: 1}},
		{ID: "new", ApplicationID: &app, DocCategory: "UAT", Section: &models.AppChangeSection{SortOrder: 2}},
		{ID: "prd", ApplicationID: &app, DocCategory: "PRD", Section: &models.AppChangeSection{SortOrder: 1}},
		{ID: "evidence"},
	}
	latest := latestSectionDocs(docs)
	if latest["old"] || !latest["new"] || !latest["prd"] || !latest["evidence"] {
		t.Fatalf("penanda versi terbaru salah: %v", latest)
	}
}

// ─── Agen ────────────────────────────────────────────────────────────────────

func TestParseAgentAction(t *testing.T) {
	act, ok := parseAgentAction("Berikut keputusan:\n```json\n{\"thought\":\"Perlu cek BAST\",\"action\":\"Search\",\"queries\":[\"BAST CorePay\",\"serah terima CorePay\"]}\n```")
	if !ok || act.Action != "search" || len(act.Queries) != 2 || act.Thought != "Perlu cek BAST" {
		t.Fatalf("aksi search gagal dibaca: %+v %v", act, ok)
	}
	if act, ok := parseAgentAction(`{"action":"finish","refs":[]}`); !ok || act.Action != "finish" {
		t.Fatalf("finish tanpa bukti tetap sah: %+v", act)
	}
	for _, bad := range []string{"tidak ada json", `{"action":"search","queries":[]}`, `{"action":"hapus"}`, `{"action":"read"}`} {
		if _, ok := parseAgentAction(bad); ok {
			t.Errorf("keputusan tidak sah harus ditolak: %s", bad)
		}
	}
}

func TestBuiltinQueryPlan(t *testing.T) {
	known := map[string]bool{"corepay": true, "uat": true, "bast": true}
	queries := builtinQueryPlan("Apa hasil UAT CorePay dan kapan serah terima CorePay?", known, nil, 6)
	if queries[0] != "Apa hasil UAT CorePay dan kapan serah terima CorePay?" {
		t.Fatalf("kueri pertama harus pertanyaan asli: %v", queries)
	}
	joined := strings.ToLower(strings.Join(queries, " | "))
	if !strings.Contains(joined, "kapan serah terima corepay") || !strings.Contains(joined, "bast") {
		t.Fatalf("pertanyaan majemuk harus dipecah & diberi sinonim: %v", queries)
	}
	seen := map[string]bool{}
	for _, q := range queries {
		if seen[normalizeQuery(q)] {
			t.Fatalf("kueri duplikat: %v", queries)
		}
		seen[normalizeQuery(q)] = true
	}
	if len(builtinQueryPlan("Apa hasil UAT CorePay dan kapan serah terima CorePay?", known, nil, 2)) != 2 {
		t.Fatalf("jumlah kueri harus mengikuti batas langkah")
	}
}

func TestCompactAgentMessagesKeepsRecentObservationsFull(t *testing.T) {
	msgs := []agentMsg{{Role: "system", Content: "sistem"}, {Role: "user", Content: "pertanyaan"}}
	for i := 1; i <= 6; i++ {
		msgs = append(msgs,
			agentMsg{Role: "assistant", Content: "keputusan"},
			agentMsg{Role: "user", Content: "observasi lengkap " + string(rune('0'+i)), Compact: "ringkas " + string(rune('0'+i))})
	}
	out := compactAgentMessages(msgs, 4)
	if len(out) != len(msgs) || out[0]["content"] != "sistem" || out[1]["content"] != "pertanyaan" {
		t.Fatalf("pesan non-observasi tidak boleh berubah: %v", out[:2])
	}
	var got []string
	for _, m := range out[2:] {
		if m["role"] == "user" {
			got = append(got, m["content"])
		}
	}
	want := "ringkas 1|ringkas 2|observasi lengkap 3|observasi lengkap 4|observasi lengkap 5|observasi lengkap 6"
	if strings.Join(got, "|") != want {
		t.Fatalf("hanya 4 observasi terbaru yang dikirim utuh:\n%v", got)
	}
}

func TestObserveSearchCompact(t *testing.T) {
	found := []*evidence{{Ref: "E3", Doc: &models.Document{Title: "BAST CorePay", VersionLabel: "v1.1"}}}
	if got := observeSearchCompact("serah terima", found); !strings.Contains(got, "E3 BAST CorePay (v1.1)") {
		t.Fatalf("ringkasan observasi harus memuat ref & judul: %q", got)
	}
}

func TestTracerEmitsStepsUsageAndAnswer(t *testing.T) {
	var events []string
	tr := newTracer(true, func(event string, data any) { events = append(events, event) })
	st := tr.begin("search", "Mencari dokumen")
	tr.addUsage(st, LLMUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, Calls: 1})
	tr.end(st)
	tr.delta("Halo")
	resp := tr.finish(&AIChatResponse{ReplyMarkdown: "Halo"})

	if strings.Join(events, ",") != "step,usage,step,answer_delta" {
		t.Fatalf("urutan event salah: %v", events)
	}
	trace := resp.Trace
	if trace == nil || !trace.Thinking || len(trace.Steps) != 1 || trace.Steps[0].Status != "done" || trace.Steps[0].Tokens != 120 || trace.Usage.TotalTokens != 120 {
		t.Fatalf("jejak salah: %+v", trace)
	}
	if resp.MatchedDocuments == nil || resp.QueriedAt.IsZero() {
		t.Fatalf("respons harus lengkap untuk disimpan & ditampilkan")
	}
}

// ─── Balas & teruskan ────────────────────────────────────────────────────────

func TestResolveReplyRefFromClientText(t *testing.T) {
	turn, note := resolveReplyRef(nil, "", &ChatReplyRef{Role: "question", Text: "Apa hasil UAT CorePay?", Mode: "reply"}, false)
	if turn == nil || !turn.Focus || turn.Prompt != "Apa hasil UAT CorePay?" || !strings.Contains(note, "membalas pertanyaannya sendiri") {
		t.Fatalf("balasan pertanyaan salah: %+v %q", turn, note)
	}

	turn, note = resolveReplyRef(nil, "", &ChatReplyRef{Role: "answer", Text: "> RAHASIA-ISI\nHasil lulus.", Mode: "forward"}, false)
	if strings.Contains(note, "RAHASIA-ISI") || !strings.Contains(note, "meneruskan") {
		t.Fatalf("saat saklar mati, isi jawaban yang tidak dikenal tidak boleh dikirim: %q", note)
	}
	if _, note = resolveReplyRef(nil, "", &ChatReplyRef{Role: "answer", Text: "Hasil lulus."}, true); !strings.Contains(note, "Hasil lulus.") {
		t.Fatalf("saat saklar nyala, isi jawaban yang dibalas dikirim: %q", note)
	}
	if turn, _ := resolveReplyRef(nil, "", nil, true); turn != nil {
		t.Fatalf("tanpa balasan tidak ada giliran fokus")
	}
}

func TestFocusTurnAlwaysFollowUp(t *testing.T) {
	history := []ChatTurn{{Prompt: "Apa hasil UAT CorePay?", Focus: true}}
	if !isFollowUpQuestion("jelaskan lebih detail modul pembayaran", 3, history) {
		t.Fatalf("pertanyaan yang membalas sebuah pesan harus memakai pesan itu sebagai konteks")
	}
}
