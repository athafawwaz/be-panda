package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"

	"panda-backend/internal/models"
)

const (
	rewriteHistoryTurns    = 3                // tanya-jawab terakhir yang dipakai LLM untuk menulis ulang pertanyaan lanjutan
	followUpRewriteTimeout = 10 * time.Second // batas waktu LLM menulis ulang pertanyaan lanjutan
	historyTermWeight      = 0.5              // bobot kata kunci dari pertanyaan sebelumnya
	maxRewrittenRunes      = 400
	maxHistoryPromptRunes  = 1000 // panjang maksimal satu pertanyaan lama yang dikirim ke LLM
	maxHistoryReplyRunes   = 2000 // panjang maksimal satu jawaban lama yang dikirim ke LLM

	// conversationEngineMode: jawaban builtin tentang percakapan itu sendiri (hanya pertanyaan & judul dokumen)
	conversationEngineMode = "PANDA Riwayat Percakapan (Built-in)"
)

// ChatTurn adalah satu tanya-jawab sebelumnya dalam percakapan yang sama.
type ChatTurn struct {
	Prompt      string
	Reply       string   // jawaban AI (markdown) sebagaimana ditampilkan ke user
	EngineMode  string   // engine yang menyusun jawaban, menentukan apakah jawaban memuat kutipan isi dokumen
	DocumentIDs []string // dokumen yang dikutip pada jawaban
	DocTitles   []string // "Judul (versi)", hanya metadata — tanpa isi dokumen
	Focus       bool     // pesan yang sedang dibalas / diteruskan user: selalu menjadi konteks pertanyaan
}

// LoadRecentTurns membaca n tanya-jawab terakhir (urut lama → baru) dari sesi chat milik userID.
// Sesi milik user lain atau sesi kosong menghasilkan nil, sehingga riwayat tidak bisa disuntikkan dari request.
func LoadRecentTurns(db *gorm.DB, sessionID, userID string, n int) []ChatTurn {
	if sessionID == "" || userID == "" || n <= 0 {
		return nil
	}
	var owned int64
	db.Model(&models.AIChatSession{}).Where("id = ? AND user_id = ?", sessionID, userID).Count(&owned)
	if owned == 0 {
		return nil
	}
	var msgs []models.AIChatMessage
	db.Select("id", "prompt", "reply_markdown", "engine_mode", "matched_documents_json").
		Where("session_id = ?", sessionID).
		Order("id DESC").Limit(n).
		Find(&msgs)

	turns := make([]ChatTurn, 0, len(msgs))
	for i := len(msgs) - 1; i >= 0; i-- {
		var docs []AIDocumentCitation
		_ = json.Unmarshal([]byte(msgs[i].MatchedDocumentsJSON), &docs)
		t := ChatTurn{Prompt: msgs[i].Prompt, Reply: msgs[i].ReplyMarkdown, EngineMode: msgs[i].EngineMode}
		for _, d := range docs {
			t.DocumentIDs = append(t.DocumentIDs, d.ID)
			t.DocTitles = append(t.DocTitles, fmt.Sprintf("%s (%s)", d.Title, d.VersionLabel))
		}
		turns = append(turns, t)
	}
	return turns
}

// followUpMarkers: kata rujukan yang menandakan pertanyaan bergantung pada jawaban sebelumnya.
// Sengaja tidak memuat "itu" saja karena "Apa itu UAT?" adalah pertanyaan baru.
var followUpMarkers = []string{
	"tersebut", "tadi", "sebelumnya", "barusan", "di atas",
	"yang pertama", "yang kedua", "yang ketiga", "yang terakhir", "yang lain", "lainnya",
	"dokumen ini", "dokumen itu", "file ini", "file itu", "versi lain", "versi lainnya",
}

// isFollowUpQuestion: ada riwayat percakapan dan pertanyaan memakai kata rujukan, atau tidak punya kata kunci
// sendiri (mis. "apa isinya?").
func isFollowUpQuestion(prompt string, primaryTerms int, history []ChatTurn) bool {
	if len(history) == 0 {
		return false
	}
	if primaryTerms == 0 || history[len(history)-1].Focus {
		return true
	}
	padded := " " + strings.Join(splitWords(strings.ToLower(prompt)), " ") + " "
	for _, m := range followUpMarkers {
		if strings.Contains(padded, " "+m+" ") {
			return true
		}
	}
	return false
}

// lastCitedDocuments: dokumen pada jawaban terakhir yang mengutip dokumen.
func lastCitedDocuments(history []ChatTurn) map[string]bool {
	for i := len(history) - 1; i >= 0; i-- {
		if len(history[i].DocumentIDs) > 0 {
			docs := make(map[string]bool, len(history[i].DocumentIDs))
			for _, id := range history[i].DocumentIDs {
				docs[id] = true
			}
			return docs
		}
	}
	return nil
}

// historyTerms: kata kunci dari pertanyaan terakhir yang punya kata kunci, berbobot lebih rendah, sebagai konteks
// pertanyaan lanjutan bila LLM tidak dipakai untuk menulis ulang pertanyaan.
func historyTerms(history []ChatTurn, known map[string]bool, existing []searchTerm) []searchTerm {
	have := map[string]bool{}
	for _, t := range existing {
		have[t.Term] = true
	}
	for i := len(history) - 1; i >= 0; i-- {
		prev := contentSearchTerms(history[i].Prompt, known)
		if len(prev) == 0 {
			continue
		}
		var out []searchTerm
		for _, t := range prev {
			if !have[t.Term] {
				t.Weight = historyTermWeight
				out = append(out, t)
			}
		}
		return out
	}
	return nil
}

// rewriteFollowUpQuestion meminta LLM eksternal menulis ulang pertanyaan lanjutan menjadi pertanyaan mandiri.
// Hanya pertanyaan & judul dokumen sebelumnya yang dikirim (tanpa isi dokumen). Gagal/timeout/kuota habis → false,
// pemanggil memakai heuristik kata kunci riwayat.
func rewriteFollowUpQuestion(db *gorm.DB, cfg LLMConfig, history []ChatTurn, prompt string) (string, LLMUsage, bool) {
	if !cfg.UsesExternalLLM() {
		return "", LLMUsage{}, false
	}
	if quota := GetLLMQuotaStatus(db, cfg, false); quota.ExhaustedUntil != nil {
		return "", LLMUsage{}, false
	}

	if len(history) > rewriteHistoryTurns {
		history = history[len(history)-rewriteHistoryTurns:]
	}
	var sb strings.Builder
	for i, t := range history {
		label := "Pertanyaan"
		if t.Focus {
			label = "Pesan yang sedang dibalas user (pertanyaan saat itu)"
		}
		sb.WriteString(fmt.Sprintf("%d. %s: %s\n", i+1, label, clip(t.Prompt, 300)))
		if len(t.DocTitles) > 0 {
			sb.WriteString("   Dokumen yang ditemukan: " + strings.Join(t.DocTitles, "; ") + "\n")
		}
	}
	reply, usage, err := chatCompletionCtx(context.Background(), cfg, []map[string]string{
		{"role": "system", "content": "Tugas Anda menulis ulang pertanyaan lanjutan menjadi satu pertanyaan mandiri dalam Bahasa Indonesia. " +
			"Pakai riwayat percakapan untuk melengkapi rujukan seperti 'tersebut', 'tadi', 'yang kedua', atau 'versi sebelumnya' " +
			"dengan nama aplikasi, jenis dokumen, atau versi yang dimaksud. Jangan menjawab pertanyaan dan jangan menambah topik baru. " +
			"Balas HANYA dengan pertanyaan hasil tulis ulang dalam satu baris."},
		{"role": "user", "content": "Riwayat percakapan:\n" + sb.String() + "\nPertanyaan lanjutan: " + prompt},
	}, followUpRewriteTimeout, chatOptions{Temperature: floatPtr(0), MaxTokens: 200})
	if err != nil {
		log.Printf("[Agentic AI] tulis ulang pertanyaan lanjutan gagal, memakai kata kunci riwayat: %v", err)
		return "", usage, false
	}
	q := cleanRewrittenQuestion(reply)
	return q, usage, q != ""
}

// resolveReplyRef membaca pesan yang dibalas/diteruskan user. Bila message_id ada, isi pesan dibaca ulang dari
// riwayat chat milik ownerID (bukan dipercaya dari klien). Hasilnya: giliran "fokus" untuk pencarian (dokumen yang
// dikutip pesan itu diutamakan) dan catatan untuk LLM. Jawaban lama yang memuat kutipan isi dokumen tidak dikirim ke
// LLM eksternal bila saklar isi dokumen mati — hanya judul dokumennya.
func resolveReplyRef(db *gorm.DB, ownerID string, ref *ChatReplyRef, shareContent bool) (*ChatTurn, string) {
	if ref == nil || (ref.MessageID == 0 && strings.TrimSpace(ref.Text) == "") {
		return nil, ""
	}
	answer := ref.Role == "answer"
	turn := ChatTurn{Focus: true}
	text, engine, known := "", "", false

	if ref.MessageID > 0 && ownerID != "" {
		var m models.AIChatMessage
		err := db.Table("ai_chat_messages AS m").Select("m.*").
			Joins("JOIN ai_chat_sessions s ON s.id = m.session_id").
			Where("m.id = ? AND s.user_id = ?", ref.MessageID, ownerID).
			Take(&m).Error
		if err == nil {
			known = true
			turn.Prompt = m.Prompt
			if answer {
				text, engine = m.ReplyMarkdown, m.EngineMode
				var docs []AIDocumentCitation
				_ = json.Unmarshal([]byte(m.MatchedDocumentsJSON), &docs)
				for _, d := range docs {
					turn.DocumentIDs = append(turn.DocumentIDs, d.ID)
					turn.DocTitles = append(turn.DocTitles, fmt.Sprintf("%s (%s)", d.Title, d.VersionLabel))
				}
			} else {
				text = m.Prompt
			}
		}
	}
	if !known {
		text = clip(strings.TrimSpace(ref.Text), maxHistoryReplyRunes)
		if !answer {
			turn.Prompt = text
		}
	}
	turn.Reply, turn.EngineMode = text, engine

	shown := clip(text, maxHistoryReplyRunes)
	if answer && !shareContent && !replyFreeOfDocContent(engine) {
		shown = "(isi jawaban tidak disertakan karena memuat kutipan isi dokumen, sesuai kebijakan data perusahaan"
		if len(turn.DocTitles) > 0 {
			shown += "; dokumen yang dibahas: " + strings.Join(turn.DocTitles, "; ")
		}
		shown += ")"
	}
	what := "pertanyaannya sendiri sebelumnya"
	if answer {
		what = "jawaban PANDA AI sebelumnya"
		if turn.Prompt != "" {
			what += fmt.Sprintf(" (untuk pertanyaan \"%s\")", clip(turn.Prompt, 200))
		}
	}
	verb := "membalas"
	if ref.Mode == "forward" {
		verb = "meneruskan dari percakapan lain"
	}
	note := fmt.Sprintf("Catatan sistem: user %s %s berikut, dan pertanyaan terbarunya merujuk pesan ini:\n«%s»\n\n", verb, what, shown)
	return &turn, note
}

// cleanRewrittenQuestion mengambil baris pertama jawaban LLM tanpa label & tanda kutip.
func cleanRewrittenQuestion(reply string) string {
	for _, line := range strings.Split(reply, "\n") {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"Pertanyaan mandiri:", "Pertanyaan:", "Hasil:"} {
			if strings.HasPrefix(strings.ToLower(line), strings.ToLower(prefix)) {
				line = strings.TrimSpace(line[len(prefix):])
			}
		}
		line = strings.Trim(line, "\"'“”‘’` ")
		if line != "" {
			return clip(line, maxRewrittenRunes)
		}
	}
	return ""
}

// followUpContextNote memberi tahu LLM penyusun jawaban bagaimana pertanyaan lanjutan dipahami saat mencari dokumen.
// Riwayat percakapannya sendiri dikirim sebagai pesan terpisah (historyMessages).
func followUpContextNote(r *RetrievalResult) string {
	if !r.FollowUp || !r.Rewritten {
		return ""
	}
	return fmt.Sprintf("Catatan sistem: ini pertanyaan lanjutan; dokumen dicari untuk maksud: \"%s\".\n\n", r.SearchQuery)
}

// ─── Pertanyaan tentang percakapan itu sendiri ───────────────────────────────

// conversationMarkers: frasa yang jelas merujuk percakapan ("percakapan kita", "jawabanmu tadi").
var conversationMarkers = []string{
	"percakapan kita", "percakapan ini", "obrolan kita", "obrolan ini", "chat kita", "chat ini",
	"kita bahas", "kita bicarakan", "kita obrolkan", "jawabanmu", "jawaban kamu", "jawaban sebelumnya",
	"jawaban tadi", "pertanyaan sebelumnya", "pertanyaan tadi", "pertanyaan terakhir",
}

// selfAskMarkers hanya dihitung bersama penanda waktu ("tadi saya tanya apa"), karena "saya tanya soal SOP"
// adalah pertanyaan biasa tentang dokumen.
var (
	selfAskMarkers = []string{"saya tanya", "saya nanya", "saya tanyakan", "saya bertanya", "aku tanya", "aku nanya", "aku tanyakan", "aku bertanya"}
	timeMarkers    = []string{"tadi", "sebelumnya", "terakhir", "barusan", "pertama", "awal"}
)

// isConversationQuestion: pertanyaan tentang percakapan itu sendiri ("sebelumnya saya nanya apa?",
// "ringkas percakapan kita"), yang jawabannya ada di riwayat, bukan di dokumen.
func isConversationQuestion(prompt string) bool {
	padded := " " + strings.Join(splitWords(strings.ToLower(prompt)), " ") + " "
	has := func(phrases []string) bool {
		for _, p := range phrases {
			if strings.Contains(padded, " "+p+" ") {
				return true
			}
		}
		return false
	}
	return has(conversationMarkers) || (has(selfAskMarkers) && has(timeMarkers))
}

// replyFreeOfDocContent: jawaban lama yang aman dikirim ke LLM eksternal walau isi dokumen tidak boleh dikirim,
// yaitu jawaban yang memang disusun tanpa isi file.
func replyFreeOfDocContent(engineMode string) bool {
	return strings.Contains(engineMode, "isi dokumen tidak dikirim") ||
		strings.Contains(engineMode, generalEngineMarker) ||
		strings.HasPrefix(engineMode, "PANDA Document Guard") ||
		strings.HasPrefix(engineMode, conversationEngineMode)
}

// historyMessages menyusun riwayat percakapan sebagai pesan user/assistant untuk LLM eksternal (urut lama → baru).
// Bila isi dokumen tidak boleh dikirim (saklar R1), jawaban lama yang memuat kutipan isi dokumen diganti keterangan
// berisi judul dokumennya saja, agar riwayat tidak menjadi jalan bocor isi dokumen.
func historyMessages(history []ChatTurn, shareContent bool) []map[string]string {
	msgs := make([]map[string]string, 0, len(history)*2)
	for _, t := range history {
		reply := strings.TrimSpace(t.Reply)
		if reply == "" {
			reply = "(tidak ada jawaban tersimpan)"
		}
		if !shareContent && !replyFreeOfDocContent(t.EngineMode) {
			reply = "(Jawaban ini memuat kutipan isi dokumen sehingga tidak disertakan sesuai kebijakan data perusahaan."
			if len(t.DocTitles) > 0 {
				reply += " Dokumen yang dibahas: " + strings.Join(t.DocTitles, "; ") + "."
			}
			reply += ")"
		}
		msgs = append(msgs,
			map[string]string{"role": "user", "content": clip(t.Prompt, maxHistoryPromptRunes)},
			map[string]string{"role": "assistant", "content": clip(reply, maxHistoryReplyRunes)})
	}
	return msgs
}

// builtinConversationReply menjawab pertanyaan tentang percakapan tanpa LLM: daftar pertanyaan sebelumnya
// beserta dokumen yang ditemukan (hanya metadata).
func builtinConversationReply(history []ChatTurn) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Berikut %d pertanyaan terakhir Anda di percakapan ini, dari yang paling awal:\n\n", len(history)))
	for i, t := range history {
		sb.WriteString(fmt.Sprintf("%d. \"%s\"", i+1, clip(strings.Join(strings.Fields(t.Prompt), " "), 200)))
		if len(t.DocTitles) > 0 {
			sb.WriteString(" — dokumen yang ditemukan: " + strings.Join(t.DocTitles, "; "))
		}
		sb.WriteString("\n")
	}
	last := history[len(history)-1]
	sb.WriteString(fmt.Sprintf("\nPertanyaan tepat sebelum ini: **\"%s\"**.", clip(strings.Join(strings.Fields(last.Prompt), " "), 200)))
	return sb.String()
}

// noHistoryConversationReply: pertanyaan tentang percakapan saat riwayat tidak tersedia.
func noHistoryConversationReply(historyTurnsSetting int) string {
	if historyTurnsSetting == 0 {
		return "Saat ini Agentic AI diatur untuk **tidak membaca percakapan sebelumnya**, jadi saya tidak dapat melihat pertanyaan Anda yang lalu. " +
			"Super Admin dapat mengubahnya di menu **Docs → API Key** (Riwayat percakapan yang dibaca AI). " +
			"Riwayat lengkap tetap tersimpan dan bisa dibuka lewat tombol **Riwayat Chat**."
	}
	return "Belum ada pertanyaan sebelumnya di percakapan ini. Bila yang Anda maksud percakapan lain, bukalah lewat tombol **Riwayat Chat**."
}
