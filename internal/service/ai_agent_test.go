package service

import (
	"strings"
	"testing"

	"panda-backend/internal/models"
)

// ─── Satu saklar isi dokumen (R1) ────────────────────────────────────────────

func TestChatContextExcludesChunksWhenSharingDisabled(t *testing.T) {
	docs := []AIDocumentCitation{{ID: "d1", Title: "UAT CorePay", AISummary: "Ringkasan UAT CorePay v1.1"}}
	chunks := map[string][]chunkHit{"d1": {{ChunkIndex: 0, Text: "RAHASIA-ISI-CHUNK-777"}}}

	ctx := buildChatContext(docs, chunks, false)
	if strings.Contains(ctx, "RAHASIA-ISI-CHUNK-777") || strings.Contains(ctx, "Kutipan isi dokumen") {
		t.Fatalf("isi dokumen ikut dikirim ke LLM padahal saklar mati:\n%s", ctx)
	}
	if !strings.Contains(ctx, "Ringkasan UAT CorePay v1.1") || !strings.Contains(ctx, "UAT CorePay") {
		t.Fatalf("metadata & ringkasan tetap harus dikirim:\n%s", ctx)
	}
	if !strings.Contains(chatSystemPrompt("Semua Tim", false, false, false), "isi file tidak disertakan") {
		t.Fatalf("system prompt harus menjelaskan bahwa isi file tidak disertakan")
	}
}

func TestChatContextIncludesChunksWhenSharingEnabled(t *testing.T) {
	docs := []AIDocumentCitation{{ID: "d1", Title: "UAT CorePay"}}
	chunks := map[string][]chunkHit{"d1": {{ChunkIndex: 2, Text: "RAHASIA-ISI-CHUNK-777"}}}
	ctx := buildChatContext(docs, chunks, true)
	if !strings.Contains(ctx, "RAHASIA-ISI-CHUNK-777") || !strings.Contains(ctx, "bagian ke-3") {
		t.Fatalf("kutipan isi harus disertakan saat saklar nyala:\n%s", ctx)
	}
	if strings.Contains(chatSystemPrompt("Semua Tim", true, false, false), "isi file tidak disertakan") {
		t.Fatalf("system prompt saat saklar nyala tidak boleh menyebut isi file disembunyikan")
	}
}

// ─── Pertanyaan lanjutan (R4) ────────────────────────────────────────────────

func TestIsFollowUpQuestion(t *testing.T) {
	history := []ChatTurn{{Prompt: "Apa hasil UAT CorePay versi 1.1?", DocumentIDs: []string{"d1"}}}
	cases := []struct {
		prompt  string
		primary int
		history []ChatTurn
		want    bool
	}{
		{"bagaimana dengan versi sebelumnya?", 1, history, true},
		{"apa isinya?", 0, history, true},
		{"siapa yang menandatangani dokumen tersebut?", 1, history, true},
		{"Apa itu UAT?", 1, history, false},
		{"Cari BAST SIMSKBDN", 2, history, false},
		{"bagaimana dengan versi sebelumnya?", 1, nil, false},
	}
	for _, c := range cases {
		if got := isFollowUpQuestion(c.prompt, c.primary, c.history); got != c.want {
			t.Errorf("%q (riwayat %d): dapat %v, seharusnya %v", c.prompt, len(c.history), got, c.want)
		}
	}
}

func TestHistoryTermsUseLatestQuestionWithKeywords(t *testing.T) {
	history := []ChatTurn{
		{Prompt: "Apa hasil UAT CorePay?"},
		{Prompt: "apa isinya?"}, // pertanyaan lanjutan tanpa kata kunci
	}
	existing := []searchTerm{{Term: "uat", Display: "uat", Weight: 1}}
	terms := historyTerms(history, map[string]bool{"uat": true, "corepay": true}, existing)
	got, ok := findTerm(terms, "corepay")
	if !ok || got.Weight != historyTermWeight {
		t.Fatalf("kata dari pertanyaan sebelumnya harus dipakai dengan bobot %.1f, dapat %+v", historyTermWeight, terms)
	}
	if _, dup := findTerm(terms, "uat"); dup {
		t.Fatalf("kata yang sudah ada tidak boleh diduplikasi: %+v", terms)
	}
}

func TestLastCitedDocumentsSkipsGuardedTurns(t *testing.T) {
	history := []ChatTurn{{DocumentIDs: []string{"d1", "d2"}}, {Prompt: "topik lain"}}
	docs := lastCitedDocuments(history)
	if !docs["d1"] || !docs["d2"] || len(docs) != 2 {
		t.Fatalf("harus memakai jawaban terakhir yang mengutip dokumen, dapat %v", docs)
	}
}

func TestCleanRewrittenQuestion(t *testing.T) {
	cases := map[string]string{
		"Pertanyaan: \"Apa hasil UAT CorePay versi 1.0?\"\nCatatan: x": "Apa hasil UAT CorePay versi 1.0?",
		"\n  “Siapa penandatangan BAST SIMSKBDN v1.1?”  ":              "Siapa penandatangan BAST SIMSKBDN v1.1?",
		"   ": "",
	}
	for in, want := range cases {
		if got := cleanRewrittenQuestion(in); got != want {
			t.Errorf("%q: dapat %q, seharusnya %q", in, got, want)
		}
	}
}

func TestFollowUpContextNote(t *testing.T) {
	if note := followUpContextNote(&RetrievalResult{}); note != "" {
		t.Fatalf("pertanyaan baru tidak butuh catatan, dapat %q", note)
	}
	// Riwayat sudah dikirim sebagai pesan; catatan hanya untuk pertanyaan yang ditulis ulang
	if note := followUpContextNote(&RetrievalResult{FollowUp: true}); note != "" {
		t.Fatalf("tanpa tulis ulang tidak perlu catatan, dapat %q", note)
	}
	r := &RetrievalResult{FollowUp: true, Rewritten: true, SearchQuery: "Apa hasil UAT CorePay versi 1.0?"}
	if note := followUpContextNote(r); !strings.Contains(note, "versi 1.0") {
		t.Fatalf("catatan harus memuat pertanyaan hasil tulis ulang, dapat %q", note)
	}
}

// ─── Memori percakapan ───────────────────────────────────────────────────────

func TestIsConversationQuestion(t *testing.T) {
	cases := map[string]bool{
		"hai panda sebelumnya saya nanya apa ya":    true,
		"tadi saya tanya apa?":                      true,
		"tolong ringkas percakapan kita":            true,
		"jawabanmu yang tadi maksudnya apa?":        true,
		"apa pertanyaan terakhir saya?":             true,
		"saya tanya soal SOP backup harian":         false, // pertanyaan biasa tentang dokumen
		"apa isi percakapan rapat koordinasi SKBDN": false,
		"siapa yang menandatangani BAST CorePay?":   false,
	}
	for prompt, want := range cases {
		if got := isConversationQuestion(prompt); got != want {
			t.Errorf("%q: dapat %v, seharusnya %v", prompt, got, want)
		}
	}
}

func TestHistoryMessagesRespectDocContentSwitch(t *testing.T) {
	history := []ChatTurn{
		{Prompt: "Apa hasil UAT CorePay?", Reply: "> RAHASIA-KUTIPAN-ISI\nHasil UAT lulus.", EngineMode: "Built-in Go Smart Document Agent (Zero API Key)",
			DocTitles: []string{"UAT CorePay (v1.1)"}},
		{Prompt: "apa nama dokumennya", Reply: "Nama dokumennya UAT CorePay [1].", EngineMode: "External LLM RAG (gemini) · isi dokumen tidak dikirim"},
	}

	off := historyMessages(history, false)
	if len(off) != 4 || off[0]["role"] != "user" || off[1]["role"] != "assistant" || off[0]["content"] != "Apa hasil UAT CorePay?" {
		t.Fatalf("riwayat harus berupa pasangan user/assistant urut lama → baru: %+v", off)
	}
	if strings.Contains(off[1]["content"], "RAHASIA-KUTIPAN-ISI") || !strings.Contains(off[1]["content"], "UAT CorePay (v1.1)") {
		t.Fatalf("saat saklar mati, jawaban berisi kutipan isi diganti keterangan judul dokumen: %q", off[1]["content"])
	}
	if off[3]["content"] != "Nama dokumennya UAT CorePay [1]." {
		t.Fatalf("jawaban yang disusun tanpa isi file tetap dikirim utuh: %q", off[3]["content"])
	}

	on := historyMessages(history, true)
	if !strings.Contains(on[1]["content"], "RAHASIA-KUTIPAN-ISI") {
		t.Fatalf("saat saklar nyala, jawaban lama dikirim apa adanya: %q", on[1]["content"])
	}
	if !strings.Contains(chatSystemPrompt("Semua Tim", true, true, false), "tadi saya tanya apa?") {
		t.Fatalf("system prompt harus mengizinkan AI menjawab pertanyaan tentang percakapan")
	}
}

func TestBuiltinConversationReplies(t *testing.T) {
	history := []ChatTurn{
		{Prompt: "Cari dokumen Digimon", DocTitles: []string{"Dokument Digital Monitoring (Digimon) (v1.1)"}},
		{Prompt: "apa nama dokumennya"},
	}
	reply := builtinConversationReply(history)
	for _, want := range []string{"1. \"Cari dokumen Digimon\"", "Digital Monitoring", "**\"apa nama dokumennya\"**"} {
		if !strings.Contains(reply, want) {
			t.Errorf("jawaban riwayat harus memuat %q:\n%s", want, reply)
		}
	}
	if !strings.Contains(noHistoryConversationReply(0), "tidak membaca percakapan sebelumnya") {
		t.Errorf("setting 0 harus dijelaskan ke user")
	}
	if !strings.Contains(noHistoryConversationReply(10), "Belum ada pertanyaan sebelumnya") {
		t.Errorf("awal percakapan harus dijelaskan ke user")
	}
}

func TestClampThinkingMaxSteps(t *testing.T) {
	for in, want := range map[int]int{-1: ThinkingStepsAuto, 0: ThinkingStepsAuto, 1: MinThinkingMaxSteps, 8: 8, 99: MaxThinkingMaxSteps} {
		if got := clampThinkingMaxSteps(in); got != want {
			t.Errorf("clamp(%d) = %d, seharusnya %d", in, got, want)
		}
	}
	if DefaultThinkingMaxSteps != ThinkingStepsAuto {
		t.Errorf("default Thinking harus otomatis (tanpa batas langkah)")
	}
}

func TestClampChatHistoryTurns(t *testing.T) {
	for in, want := range map[int]int{-3: 0, 0: 0, 7: 7, MaxChatHistoryTurns + 5: MaxChatHistoryTurns} {
		if got := clampChatHistoryTurns(in); got != want {
			t.Errorf("clamp(%d) = %d, seharusnya %d", in, got, want)
		}
	}
}

// ─── Kecocokan metadata ──────────────────────────────────────────────────────

func TestMetadataTermSetMatchesStems(t *testing.T) {
	set := metadataTermSet(models.Document{Title: "Laporan Pengujian Aplikasinya", DocCategory: "UAT"})
	for _, term := range []string{"uji", "pengujian", "aplikasi", "uat", "lapor"} {
		if !setHasTerm(set, term) {
			t.Errorf("kata %q harus cocok dengan metadata", term)
		}
	}
	if setHasTerm(set, "ua") || setHasTerm(set, "bast") {
		t.Errorf("kata pendek harus cocok persis & kata lain tidak boleh cocok")
	}
}
