package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"panda-backend/internal/models"
)

type AIChatRequest struct {
	Prompt       string        `json:"prompt"`
	FilterModule string        `json:"filter_module"`      // ALL | APPLICATION | EVIDENCE
	SessionID    string        `json:"session_id"`         // kosong = mulai percakapan baru di riwayat chat
	Thinking     bool          `json:"thinking"`           // mode Thinking: agen riset dengan pencarian berulang & rerank
	ReplyTo      *ChatReplyRef `json:"reply_to,omitempty"` // pesan yang dibalas / diteruskan user
}

// ChatReplyRef adalah pesan yang dibalas (Balas) atau diteruskan ke percakapan baru (Teruskan) oleh user.
type ChatReplyRef struct {
	MessageID int64  `json:"message_id,omitempty"` // id di riwayat chat; isi pesan dibaca ulang dari server bila ada
	Role      string `json:"role"`                 // question | answer
	Mode      string `json:"mode,omitempty"`       // reply | forward
	Text      string `json:"text,omitempty"`       // isi pesan dari klien, dipakai bila message_id tidak tersedia
}

type AIDocumentCitation struct {
	ID                  string    `json:"id"`
	ModuleSource        string    `json:"module_source"`
	DocCategory         string    `json:"doc_category"`
	Title               string    `json:"title"`
	StandardizedName    string    `json:"standardized_name"`
	VersionLabel        string    `json:"version_label"`
	FolderName          string    `json:"folder_name"`
	AppName             string    `json:"app_name"`
	SectionTitle        string    `json:"section_title"`
	VisibilityScope     string    `json:"visibility_scope"`
	UploadedByBadge     string    `json:"uploaded_by_badge"`
	UploadedByName      string    `json:"uploaded_by_name"`
	UploadedByTeamLabel string    `json:"uploaded_by_team_label"`
	UploadedAt          time.Time `json:"uploaded_at"`
	FileSizeBytes       int64     `json:"file_size_bytes"`
	AuditTags           string    `json:"audit_tags"`
	ValidUntil          string    `json:"valid_until"`
	AISummary           string    `json:"ai_summary"`
	MatchedExcerpt      string    `json:"matched_excerpt"` // cuplikan isi file yang paling relevan dengan pertanyaan
	PreviewURL          string    `json:"preview_url"`
	DownloadURL         string    `json:"download_url"`
}

type AIChatResponse struct {
	ReplyMarkdown    string               `json:"reply_markdown"`
	EngineMode       string               `json:"engine_mode"`
	ScopeLabel       string               `json:"scope_label"`
	MatchedDocuments []AIDocumentCitation `json:"matched_documents"`
	QueriedAt        time.Time            `json:"queried_at"`
	SessionID        string               `json:"session_id,omitempty"` // percakapan di riwayat chat tempat jawaban ini disimpan
	MessageID        int64                `json:"message_id,omitempty"` // id tanya-jawab di riwayat chat (untuk Balas/Teruskan)
	Trace            *AgentTrace          `json:"trace,omitempty"`      // langkah AI (pencarian, baca, rerank), token & durasi
	// AnswerBasis: DOCUMENTS (mengutip dokumen) | GENERAL (pengetahuan umum, tanpa dokumen) | MIXED; kosong = jawaban lama
	AnswerBasis string `json:"answer_basis,omitempty"`
}

// scopedDocuments mengembalikan query dokumen yang boleh diakses user. Selalu membuat chain baru
// karena chain GORM tidak aman dipakai ulang (dipakai juga sebagai subquery pencarian isi dokumen).
func scopedDocuments(db *gorm.DB, user models.User, filterModule string) *gorm.DB {
	query := ApplyDocumentScope(db.Model(&models.Document{}), user)

	if filterModule == "APPLICATION" || filterModule == "EVIDENCE" {
		query = query.Where("module_source = ?", filterModule)
	}
	return query
}

// RetrievalResult adalah hasil pencarian dokumen & potongan isi sebelum jawaban disusun.
type RetrievalResult struct {
	ScopeLabel      string
	Documents       []models.Document     // dokumen terpilih, urut relevansi (maks 4)
	ContextChunks   map[string][]chunkHit // potongan isi per dokumen yang dipakai sebagai konteks jawaban
	MatchedChunks   map[string][]chunkHit // potongan yang benar-benar cocok dengan kata kunci (untuk cuplikan kartu)
	Terms           []searchTerm          // kata kunci isi: dari pertanyaan, sinonim, dan konteks percakapan
	MissingTerms    []string              // kata dari pertanyaan yang tidak ada di dokumen mana pun
	GeneralQuestion bool                  // pertanyaan tanpa kata kunci spesifik → dokumen terbaru
	TotalAccessible int
	FollowUp        bool              // pertanyaan lanjutan yang dipahami memakai riwayat percakapan
	Rewritten       bool              // pertanyaan lanjutan ditulis ulang oleh LLM menjadi pertanyaan mandiri
	SearchQuery     string            // pertanyaan yang dipakai untuk pencarian (hasil tulis ulang bila pertanyaan lanjutan)
	Usage           LLMUsage          // token yang dipakai saat menulis ulang pertanyaan lanjutan
	Suggestions     []models.Document // dokumen terdekat dari metadata bila tidak ada dokumen relevan (saran untuk user)
}

// followUpDocBoost: tambahan bobot untuk dokumen yang dikutip pada jawaban sebelumnya saat pertanyaan lanjutan.
const followUpDocBoost = 1.5

func scopeLabelFor(user models.User) string {
	if user.IsAdmin() {
		return "Semua Tim & Seluruh Aplikasi (Full Admin Access)"
	}
	teamName := "Tim TI"
	if user.Team != nil {
		teamName = user.Team.TeamName
	}
	if user.Branch != nil {
		return fmt.Sprintf("Terisolasi: %s • %s (+ Dokumen All Tim)", teamName, user.Branch.BranchName)
	}
	return fmt.Sprintf("Terisolasi: %s (+ Dokumen All Tim)", teamName)
}

// loadCandidateDocs memuat semua dokumen dalam cakupan akses user beserta relasi untuk penilaian metadata.
func loadCandidateDocs(db *gorm.DB, user models.User, filterModule string) ([]models.Document, error) {
	var docs []models.Document
	err := scopedDocuments(db, user, filterModule).
		Preload("OwnerTeam").
		Preload("OwnerBranch").
		Preload("Application").
		Preload("Folder").
		Preload("Section").
		Omit("extracted_text").
		Order("uploaded_at DESC").
		Find(&docs).Error
	return docs, err
}

// RetrieveContext mencari dokumen & potongan isi yang relevan dalam cakupan akses user tanpa menyusun jawaban.
// Dipakai ProcessAgenticQuery dan alat evaluasi retrieval (cmd/rag-eval). history berisi tanya-jawab
// sebelumnya di percakapan yang sama (lama → baru); cfg menentukan apakah pertanyaan lanjutan ditulis ulang oleh LLM.
func RetrieveContext(db *gorm.DB, user models.User, req AIChatRequest, history []ChatTurn, cfg LLMConfig) (*RetrievalResult, error) {
	candidateDocs, err := loadCandidateDocs(db, user, req.FilterModule)
	if err != nil {
		return nil, err
	}
	res := &RetrievalResult{ScopeLabel: scopeLabelFor(user), TotalAccessible: len(candidateDocs), SearchQuery: req.Prompt}

	// Kata kunci isi dokumen, dengan kategori & kode aplikasi dalam cakupan sebagai prioritas
	known := knownSearchWords(candidateDocs)
	primary := contentSearchTerms(req.Prompt, known)
	terms := primary
	var previousDocs map[string]bool
	if isFollowUpQuestion(req.Prompt, len(primary), history) {
		res.FollowUp = true
		previousDocs = lastCitedDocuments(history)
		rewritten, usage, ok := rewriteFollowUpQuestion(db, cfg, history, req.Prompt)
		res.Usage.Add(usage)
		if ok {
			res.Rewritten = true
			res.SearchQuery = rewritten
			terms = contentSearchTerms(rewritten, known)
		} else {
			terms = append(terms, historyTerms(history, known, terms)...)
		}
	}
	terms = append(terms, synonymExpansions(res.SearchQuery, terms)...)
	res.Terms = terms

	// Score candidate documents using Built-in Smart Document Scorer
	promptLower := strings.ToLower(strings.TrimSpace(res.SearchQuery))
	tokens := tokenizePrompt(promptLower)

	// RAG: cari potongan isi file yang cocok, hanya dari dokumen dalam cakupan akses user
	chunkHits, contentTermsByDoc, err := retrieveRelevantChunks(db, func() *gorm.DB {
		return scopedDocuments(db, user, req.FilterModule).Select("id")
	}, terms)
	if err != nil {
		return nil, err
	}

	type scoredDoc struct {
		doc    models.Document
		score  int
		terms  []searchTerm // kata kunci yang ditemukan di metadata atau isi dokumen
		weight float64      // bobot kata kunci yang ditemukan (kata yang jarang muncul lebih penting)
	}

	termDocCount := map[string]int{}
	var scored []scoredDoc
	for _, d := range candidateDocs {
		s := scoreDocument(d, promptLower, tokens)
		if hits := chunkHits[d.ID]; len(hits) > 0 {
			s += int(math.Round(hits[0].Score * 6))
		}
		meta := metadataTermSet(d)
		var found []searchTerm
		for _, t := range terms {
			if contentTermsByDoc[d.ID][t.Term] || setHasTerm(meta, t.Term) {
				found = append(found, t)
				termDocCount[t.Term]++
			}
		}
		scored = append(scored, scoredDoc{doc: d, score: s, terms: found})
	}
	for i := range scored {
		for _, t := range scored[i].terms {
			scored[i].weight += t.Weight * math.Log(1+float64(len(candidateDocs))/float64(termDocCount[t.Term]))
		}
		// Pertanyaan lanjutan ("bagaimana dengan versi sebelumnya?") merujuk dokumen pada jawaban sebelumnya
		if previousDocs[scored[i].doc.ID] {
			scored[i].weight += followUpDocBoost
			scored[i].score += 40
		}
	}

	// Dokumen yang memuat kata kunci terpenting diutamakan, lalu skor, lalu yang terbaru
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].weight != scored[j].weight {
			return scored[i].weight > scored[j].weight
		}
		if scored[i].score == scored[j].score {
			return scored[i].doc.UploadedAt.After(scored[j].doc.UploadedAt)
		}
		return scored[i].score > scored[j].score
	})

	// Pertanyaan tanpa kata kunci spesifik (mis. "tampilkan dokumen terbaru") boleh dijawab dengan
	// dokumen terbaru. Selain itu hanya dokumen yang benar-benar memuat kata kunci yang ditampilkan,
	// dan yang bobotnya jauh di bawah dokumen terbaik dibuang, agar AI tidak menyodorkan dokumen lain
	// saat topik yang ditanyakan tidak ada.
	res.GeneralQuestion = len(terms) == 0
	minWeight := 0.0
	if len(scored) > 0 {
		minWeight = scored[0].weight * 0.5
	}
	for _, item := range scored {
		if len(res.Documents) >= 4 || (res.GeneralQuestion && len(res.Documents) >= 3) {
			break
		}
		if res.GeneralQuestion || (item.weight > 0 && item.weight >= minWeight) {
			res.Documents = append(res.Documents, item.doc)
		}
	}

	// Hanya kata yang ditulis user yang dilaporkan hilang; sinonim & konteks percakapan hanya pelengkap
	for _, t := range terms {
		if t.primary() && termDocCount[t.Term] == 0 {
			res.MissingTerms = append(res.MissingTerms, t.Display)
		}
	}

	if len(res.Documents) == 0 {
		res.Suggestions = closestDocuments(candidateDocs, res.SearchQuery, 3)
	}

	// Konteks isi per dokumen: potongan yang cocok, atau bagian awal dokumen bila tidak ada yang cocok
	res.ContextChunks = make(map[string][]chunkHit, len(res.Documents))
	res.MatchedChunks = make(map[string][]chunkHit, len(res.Documents))
	var needLeading []string
	for _, d := range res.Documents {
		if hits := chunkHits[d.ID]; len(hits) > 0 {
			res.MatchedChunks[d.ID] = hits
		} else {
			needLeading = append(needLeading, d.ID)
		}
	}
	if err := loadChunkTexts(db, res.MatchedChunks); err != nil {
		return nil, err
	}
	rerankExactMatches(res.MatchedChunks, terms)
	for id, hits := range res.MatchedChunks {
		res.ContextChunks[id] = hits
	}
	leading, err := loadLeadingChunks(db, needLeading)
	if err != nil {
		return nil, err
	}
	for id, hits := range leading {
		res.ContextChunks[id] = hits
	}
	return res, nil
}

// ProcessAgenticQuery menjawab pertanyaan Agentic AI tanpa streaming (lihat AnswerAgenticQuery).
func ProcessAgenticQuery(db *gorm.DB, user models.User, req AIChatRequest, historyOwnerID string) (*AIChatResponse, error) {
	return AnswerAgenticQuery(context.Background(), db, user, req, historyOwnerID, nil)
}

// AnswerAgenticQuery menjawab pertanyaan Agentic AI. historyOwnerID adalah pemilik riwayat chat (akun asli, juga saat
// impersonate): AI membaca maksimal cfg.ChatHistoryTurns tanya-jawab terakhir dari sesi req.SessionID milik akun itu.
// req.Thinking menjalankan agen riset (pencarian berulang, baca, rerank); emit (boleh nil) menerima langkah,
// potongan jawaban, dan pemakaian token untuk ditampilkan langsung. ctx dibatalkan bila user menghentikan jawaban.
func AnswerAgenticQuery(ctx context.Context, db *gorm.DB, user models.User, req AIChatRequest, historyOwnerID string, emit AgentEmitter) (*AIChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if db != nil {
		db = db.WithContext(ctx)
	}
	// Konfigurasi model dari Docs → AI atau .env; builtin hanya bila provider builtin dipilih.
	cfg := LoadLLMConfig(db)
	history, err := LoadRecentTurns(db, req.SessionID, historyOwnerID, cfg.ChatHistoryTurns)
	if err != nil {
		return nil, err
	}
	focus, replyNote, err := resolveReplyRef(db, historyOwnerID, req.ReplyTo, cfg.ShareDocContent)
	if err != nil {
		return nil, err
	}
	tr := newTracer(req.Thinking, emit)

	// Model terkonfigurasi selalu memegang kendali; kegagalan tidak mengaktifkan builtin.
	if cfg.Provider != ProviderBuiltin {
		if !cfg.UsesExternalLLM() {
			return nil, &AgentError{Message: "Agent AI belum siap: API key belum diisi. Atur koneksi di Docs → AI."}
		}
		a, err := newAgentRun(db, user, req, cfg, tr)
		if err != nil {
			return nil, err
		}
		a.rerankCfg = LoadRerankConfig(db)
		a.reranker = NewReranker(a.rerankCfg)
		return runToolAgent(ctx, a, history, replyNote)
	}

	// Builtin hanya aktif bila provider builtin dipilih (tidak ada model terhubung).
	conversational := isConversationQuestion(req.Prompt)
	if conversational {
		reply := noHistoryConversationReply(cfg.ChatHistoryTurns)
		if len(history) > 0 {
			reply = builtinConversationReply(history)
		}
		tr.delta(reply)
		return tr.finish(&AIChatResponse{ReplyMarkdown: reply, EngineMode: conversationEngineMode, ScopeLabel: scopeLabelFor(user)}), nil
	}

	// Mode builtin: sapaan & pertanyaan kemampuan dijawab langsung, bukan dicari di dokumen
	if isSmallTalkOrCapability(req.Prompt) {
		docs, err := loadCandidateDocs(db, user, req.FilterModule)
		if err != nil {
			return nil, err
		}
		st := tr.begin("answer", "Menjelaskan kemampuan PANDA AI")
		reply := builtinCapabilityReply(scopeLabelFor(user), len(docs))
		tr.delta(reply)
		tr.end(st)
		return tr.finish(&AIChatResponse{ReplyMarkdown: reply, EngineMode: "PANDA AI (Built-in) · " + generalEngineMarker,
			ScopeLabel: scopeLabelFor(user), AnswerBasis: AnswerBasisGeneral}), nil
	}

	// Pesan yang dibalas/diteruskan menjadi konteks pencarian: dokumen yang dikutipnya diutamakan
	retrievalHistory := history
	if focus != nil {
		retrievalHistory = append(append([]ChatTurn{}, history...), *focus)
	}
	if req.Thinking {
		return runThinking(ctx, db, user, req, cfg, retrievalHistory, tr)
	}
	return answerStandard(db, user, req, cfg, retrievalHistory, tr)
}

// buildCitation menyusun kartu dokumen yang ditampilkan di bawah jawaban.
func buildCitation(d models.Document, excerpt string) AIDocumentCitation {
	folderName := "-"
	if d.Folder != nil {
		folderName = d.Folder.FolderName
	}
	appName := "-"
	if d.Application != nil {
		appName = d.Application.AppName
	}
	secTitle := d.VersionLabel
	if d.Section != nil {
		secTitle = fmt.Sprintf("%s - %s", d.Section.VersionLabel, d.Section.SectionTitle)
	}
	return AIDocumentCitation{
		ID:                  d.ID,
		ModuleSource:        d.ModuleSource,
		DocCategory:         d.DocCategory,
		Title:               d.Title,
		StandardizedName:    d.StandardizedName,
		VersionLabel:        d.VersionLabel,
		FolderName:          folderName,
		AppName:             appName,
		SectionTitle:        secTitle,
		VisibilityScope:     d.VisibilityScope,
		UploadedByBadge:     d.UploadedByBadge,
		UploadedByName:      d.UploadedByName,
		UploadedByTeamLabel: d.UploadedByTeamLabel,
		UploadedAt:          d.UploadedAt,
		FileSizeBytes:       d.FileSizeBytes,
		AuditTags:           d.AuditTags,
		ValidUntil:          d.ValidUntil,
		AISummary:           d.AISummary,
		MatchedExcerpt:      excerpt,
		PreviewURL:          fmt.Sprintf("/api/v1/documents/%s/preview", d.ID),
		DownloadURL:         fmt.Sprintf("/api/v1/documents/%s/download", d.ID),
	}
}

// answerStandard menyusun jawaban builtin dari satu pencarian, tanpa memanggil model.
func answerStandard(db *gorm.DB, user models.User, req AIChatRequest, cfg LLMConfig,
	retrievalHistory []ChatTurn, tr *tracer) (*AIChatResponse, error) {
	search := tr.begin("search", "Mencari dokumen yang relevan")
	search.Queries = []string{req.Prompt}
	r, err := RetrieveContext(db, user, req, retrievalHistory, cfg)
	if err != nil {
		tr.fail(search, "Pencarian dokumen gagal.")
		return nil, err
	}
	tr.addUsage(search, r.Usage)
	scopeLabel, contextChunks, missingTerms := r.ScopeLabel, r.ContextChunks, r.MissingTerms
	keys := excerptKeys(r.Terms)

	citations := make([]AIDocumentCitation, 0, len(r.Documents))
	for i, d := range r.Documents {
		excerpt := ""
		if hits := r.MatchedChunks[d.ID]; len(hits) > 0 {
			excerpt = makeExcerpt(hits[0].Text, keys, 280)
		}
		citations = append(citations, buildCitation(d, excerpt))
		search.Results = append(search.Results, StepResult{DocumentID: d.ID, Title: d.Title, Category: d.DocCategory,
			Version: d.VersionLabel, Score: math.Round((1-float64(i)*0.15)*100) / 100, Snippet: clip(excerpt, 160), Selected: true})
	}
	if r.Rewritten {
		search.Queries = []string{r.SearchQuery}
		search.Detail = "Pertanyaan lanjutan dipahami sebagai: " + r.SearchQuery
	} else if len(r.Documents) == 0 {
		search.Detail = "Tidak ada dokumen yang cocok."
	} else {
		search.Detail = fmt.Sprintf("%d dokumen relevan dari %d dokumen yang dapat diakses.", len(r.Documents), r.TotalAccessible)
	}
	tr.trace.Searches, tr.trace.Findings = 1, len(r.Documents)
	tr.end(search)

	resp := &AIChatResponse{ScopeLabel: scopeLabel, MatchedDocuments: citations,
		EngineMode: "Built-in Go Smart Document Agent (Zero API Key)"}

	if len(citations) == 0 {
		resp.ReplyMarkdown = buildNotFoundReply(scopeLabel, missingTerms, r.TotalAccessible) + notFoundSuggestions(r.Suggestions)
		resp.EngineMode = "PANDA Document Guard (tidak ada dokumen relevan)"
		resp.AnswerBasis = AnswerBasisGeneral
	} else {
		resp.ReplyMarkdown = buildBuiltinSmartSummary(scopeLabel, citations, contextChunks, r.TotalAccessible, r.GeneralQuestion, missingTerms)
		resp.AnswerBasis = AnswerBasisDocuments
	}
	tr.delta(resp.ReplyMarkdown)
	return tr.finish(resp), nil
}

// stopWords: kata umum dalam pertanyaan yang tidak berguna sebagai kata kunci pencarian.
var stopWords = map[string]bool{
	"tolong": true, "carikan": true, "cari": true, "tampilkan": true, "dokumen": true,
	"yang": true, "untuk": true, "dari": true, "pada": true, "dan": true, "di": true,
	"apa": true, "saja": true, "ada": true, "apakah": true, "beserta": true, "ringkasan": true,
	"ringkasannya": true, "dong": true, "ya": true, "saya": true, "kami": true, "tim": true,
	"ini": true, "itu": true, "dengan": true, "dalam": true, "atau": true, "adalah": true,
	"bagaimana": true, "berapa": true, "siapa": true, "kapan": true, "mana": true,
	"jelaskan": true, "sebutkan": true, "tentang": true, "mengenai": true, "isi": true,
	"isinya": true, "ringkas": true, "rangkum": true,
	"dokumennya": true, "file": true, "filenya": true, "berkas": true, "berkasnya": true,
	"terbaru": true, "terakhir": true, "semua": true, "seluruh": true, "daftar": true,
	"lihat": true, "minta": true, "mohon": true, "bisa": true, "tersedia": true,
	"punya": true, "butuh": true, "perlu": true, "mau": true, "ingin": true,
	"buat": true, "membuat": true, "cara": true, "gimana": true, "kenapa": true, "mengapa": true,
	"dimana": true, "berikan": true, "tunjukkan": true, "yg": true, "dgn": true, "utk": true,
	"sudah": true, "belum": true, "juga": true,
}

// metadataTermSet: kata (bentuk tulisan & bentuk dasar) dari judul, kategori, nama file, aplikasi, folder, tim,
// tag, dan ringkasan dokumen, dihitung sekali per dokumen lalu dicocokkan dengan setiap kata kunci.
func metadataTermSet(d models.Document) map[string]bool {
	fields := []string{d.DocCategory, d.Title, d.StandardizedName, d.UploadedByTeamLabel, d.AuditTags, d.ITDomain, d.AISummary}
	if d.Application != nil {
		fields = append(fields, d.Application.AppName, d.Application.AppCode)
	}
	if d.Folder != nil {
		fields = append(fields, d.Folder.FolderName)
	}
	if d.Section != nil {
		fields = append(fields, d.Section.SectionTitle)
	}
	set := map[string]bool{}
	for _, f := range fields {
		for _, w := range splitWords(strings.ToLower(f)) {
			set[w] = true
			set[stemWord(w)] = true
		}
	}
	return set
}

// setHasTerm: kata kunci ada di himpunan kata, persis atau (kata ≥4 huruf) sebagai awalan, sama seperti termMatches.
func setHasTerm(set map[string]bool, term string) bool {
	if set[term] {
		return true
	}
	if len(term) < 4 {
		return false
	}
	for w := range set {
		if strings.HasPrefix(w, term) {
			return true
		}
	}
	return false
}

func tokenizePrompt(prompt string) []string {
	replacer := strings.NewReplacer(",", " ", ".", " ", "?", " ", "!", " ", "(", " ", ")", " ", "/", " ", "-", " ")
	cleaned := replacer.Replace(prompt)
	parts := strings.Fields(cleaned)
	var out []string
	for _, p := range parts {
		if len(p) >= 2 && !stopWords[p] {
			out = append(out, p)
		}
	}
	return out
}

func scoreDocument(d models.Document, fullPrompt string, tokens []string) int {
	score := 0
	catLower := strings.ToLower(d.DocCategory)
	titleLower := strings.ToLower(d.Title)
	stdLower := strings.ToLower(d.StandardizedName)
	sumLower := strings.ToLower(d.AISummary)
	teamLower := strings.ToLower(d.UploadedByTeamLabel)
	verLower := strings.ToLower(d.VersionLabel)
	appLower := ""
	if d.Application != nil {
		appLower = strings.ToLower(d.Application.AppName + " " + d.Application.AppCode)
	}

	// Category boost (per kata, agar kategori "UAT" tidak terpicu oleh kata "buat")
	if textHasTerm(fullPrompt, catLower) {
		score += 35
	}
	if strings.Contains(fullPrompt, "awal") && (verLower == "v1.0" || strings.Contains(titleLower, "awal")) {
		score += 25
	}
	if (strings.Contains(fullPrompt, "terbaru") || strings.Contains(fullPrompt, "perubahan")) && verLower != "v1.0" {
		score += 25
	}

	for _, tok := range tokens {
		if catLower == tok {
			score += 30
		}
		if strings.Contains(appLower, tok) {
			score += 28
		}
		if strings.Contains(titleLower, tok) {
			score += 20
		}
		if strings.Contains(stdLower, tok) {
			score += 18
		}
		if strings.Contains(teamLower, tok) {
			score += 15
		}
		if strings.Contains(sumLower, tok) {
			score += 10
		}
	}
	// Kecocokan dengan isi file dihitung terpisah lewat potongan isi (retrieveRelevantChunks)
	return score
}

func formatTermList(terms []string) string {
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = "**" + t + "**"
	}
	return strings.Join(quoted, ", ")
}

// buildNotFoundReply: jawaban bila tidak ada dokumen dalam cakupan akses yang memuat kata kunci pertanyaan.
func buildNotFoundReply(scopeLabel string, missingTerms []string, totalAccessible int) string {
	topic := "pertanyaan Anda"
	if len(missingTerms) > 0 {
		topic = formatTermList(missingTerms)
	}
	return fmt.Sprintf(
		"Tidak ditemukan dokumen yang membahas %s dalam cakupan akses Anda (**%s**, total %d dokumen).\n\n"+
			"PANDA AI hanya menjawab berdasarkan dokumen audit yang tersimpan di PANDA, sehingga pertanyaan di luar isi dokumen tidak dijawab. "+
			"Coba gunakan kata kunci lain, misalnya nama aplikasi atau jenis dokumen (PRD, UAT, BAST, SOP), atau unggah dokumennya terlebih dahulu di menu Evidence. "+
			"Jika dokumen tersebut milik tim lain, minta Admin mengatur visibilitasnya agar dapat diakses tim Anda.",
		topic, scopeLabel, totalAccessible,
	)
}

// truncateText memotong teks ±maxLen byte di batas baris/kata terdekat.
func truncateText(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	cut := maxLen
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	if i := strings.LastIndexAny(s[:cut], "\n "); i > maxLen/2 {
		cut = i
	}
	return strings.TrimSpace(s[:cut]) + " …"
}

// builtinContentChunks: maksimal potongan isi per dokumen yang ditampilkan mode built-in.
const builtinContentChunks = 2

func buildBuiltinSmartSummary(scopeLabel string, docs []AIDocumentCitation, contextChunks map[string][]chunkHit, totalAccessible int, generalQuestion bool, missingTerms []string) string {
	var sb strings.Builder
	switch {
	case generalQuestion:
		sb.WriteString(fmt.Sprintf("Berikut **%d dokumen terbaru** dari cakupan **%s** (total %d dokumen yang dapat diakses):\n\n", len(docs), scopeLabel, totalAccessible))
	case len(missingTerms) > 0:
		sb.WriteString(fmt.Sprintf("⚠️ Kata %s tidak ditemukan di dokumen mana pun dalam cakupan **%s**. "+
			"Dokumen di bawah hanya cocok dengan kata kunci lainnya, jadi pastikan memang dokumen ini yang Anda maksud:\n\n",
			formatTermList(missingTerms), scopeLabel))
	default:
		sb.WriteString(fmt.Sprintf("Berikut hasil analisis **PANDA Agentic AI** dari cakupan **%s** (ditemukan **%d dokumen relevan** dari total %d dokumen yang dapat diakses):\n\n", scopeLabel, len(docs), totalAccessible))
	}

	for i, d := range docs {
		sourceBadge := "📁 Modul Aplikasi"
		locationInfo := fmt.Sprintf("Aplikasi **%s** (%s)", d.AppName, d.SectionTitle)
		if d.ModuleSource == "EVIDENCE" {
			sourceBadge = "📂 Modul Evidence Lintas Tim"
			locationInfo = fmt.Sprintf("Unit/Visibilitas: **%s**", d.UploadedByTeamLabel)
		}

		sb.WriteString(fmt.Sprintf("### %d. [%s] %s (`%s`)\n", i+1, d.DocCategory, d.Title, d.VersionLabel))
		sb.WriteString(fmt.Sprintf("- **Sumber & Lokasi:** %s — %s\n", sourceBadge, locationInfo))
		sb.WriteString(fmt.Sprintf("- **Nama File Terstandar:** `%s`\n", d.StandardizedName))
		sb.WriteString(fmt.Sprintf("- **Ringkasan Eksekutif:** %s\n", d.AISummary))
		if chunks := contextChunks[d.ID]; len(chunks) > 0 {
			// Potongan isi paling relevan, ditampilkan sesuai urutan di dokumen
			shown := append([]chunkHit(nil), chunks...)
			if len(shown) > builtinContentChunks {
				shown = shown[:builtinContentChunks]
			}
			sort.Slice(shown, func(i, j int) bool { return shown[i].ChunkIndex < shown[j].ChunkIndex })
			sb.WriteString("- **Isi Dokumen yang Relevan:**\n")
			for _, c := range shown {
				for _, line := range strings.Split(truncateText(c.Text, 700), "\n") {
					if line = strings.TrimSpace(line); line != "" {
						sb.WriteString("> " + line + "\n")
					}
				}
				sb.WriteString(">\n")
			}
		} else if d.MatchedExcerpt != "" {
			sb.WriteString(fmt.Sprintf("- **Cuplikan Isi Dokumen:** “%s”\n", d.MatchedExcerpt))
		}
		sb.WriteString(fmt.Sprintf("- **Log Upload:** Diunggah oleh **%s** (Badge: `%s`) pada **%s WIB**\n",
			d.UploadedByName, d.UploadedByBadge, d.UploadedAt.Format("02 Jan 2006, 15:04:05")))
		if d.AuditTags != "" {
			sb.WriteString(fmt.Sprintf("- **Reusability Tag Audit:** `%s` | **Masa Berlaku:** %s\n", d.AuditTags, d.ValidUntil))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("💡 *Anda dapat langsung melakukan **Klik Lihat (Preview)** atau **Download Dokumen** melalui kartu dokumen di bawah ini tanpa harus mencari manual ke halaman aplikasi.*")
	return sb.String()
}

// TestLLMConnection mengirim prompt singkat untuk memastikan Base URL, Model, & API key valid.
func TestLLMConnection(cfg LLMConfig) (string, error) {
	return chatCompletion(cfg, []map[string]string{
		{"role": "user", "content": "Tes koneksi PANDA. Balas singkat dengan: OK"},
	}, 20*time.Second)
}

// LLMError: layanan LLM eksternal membalas dengan status gagal (non-2xx).
type LLMError struct {
	Status int
	Body   string
}

func (e *LLMError) Error() string {
	snippet := strings.TrimSpace(e.Body)
	if len(snippet) > 300 {
		snippet = snippet[:300] + "…"
	}
	return fmt.Sprintf("LLM status %d: %s", e.Status, snippet)
}

// describeLLMFailure menjelaskan kegagalan LLM eksternal dengan bahasa yang mudah dipahami user.
func describeLLMFailure(err error) string {
	var unsupported *ErrToolsUnsupported
	if errors.As(err, &unsupported) {
		return "model tidak mendukung tool calling; pilih model yang mendukung tools di Docs → AI"
	}
	var exhausted *ErrQuotaExhausted
	if errors.As(err, &exhausted) {
		return fmt.Sprintf("kuota layanan AI eksternal sudah habis (pulih sekitar %s lagi)", humanizeDuration(time.Until(exhausted.Until)))
	}
	var llmErr *LLMError
	if errors.As(err, &llmErr) {
		switch llmErr.Status {
		case http.StatusTooManyRequests:
			msg := "kuota layanan AI eksternal sudah habis"
			if strings.Contains(llmErr.Body, "PerDay") {
				msg = "kuota harian layanan AI eksternal sudah habis"
			}
			if d, ok := parseRetryDelay(llmErr.Body); ok {
				msg += fmt.Sprintf(" (pulih sekitar %s lagi)", humanizeDuration(d))
			}
			return msg
		case http.StatusUnauthorized, http.StatusForbidden:
			return "API key layanan AI eksternal ditolak"
		case http.StatusNotFound:
			return "model atau alamat layanan AI eksternal tidak ditemukan"
		}
		return fmt.Sprintf("layanan AI eksternal sedang bermasalah (status %d)", llmErr.Status)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "layanan AI eksternal tidak merespons tepat waktu"
	}
	return "layanan AI eksternal tidak dapat dihubungi"
}

func humanizeDuration(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%d jam %d menit", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%d menit", int(d.Minutes()))
	}
	return "1 menit"
}

func getEnvOrDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
