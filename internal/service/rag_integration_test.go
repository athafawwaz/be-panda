//go:build integration

package service

// Integration test terhadap PostgreSQL sungguhan: membuat database sementara dengan nama unik, menjalankan semua
// migrasi, mengindeks dokumen buatan, lalu menguji pencarian Agentic AI. Database dihapus setelah test selesai.
//
//	go test -tags integration ./internal/service -run Integration -v
//
// Kredensial dibaca dari file env backend (DB_HOST, DB_PORT, DB_USER, DB_PASSWORD); koneksi awal ke database "postgres".
// Bila user database tidak punya izin CREATEDB, siapkan database kosong khusus test lalu jalankan dengan
// PANDA_IT_DB_NAME=<nama>: migrasi diterapkan ke database itu dan data uji dihapus lagi setelah test (database tidak di-drop).
// Jangan arahkan PANDA_IT_DB_NAME ke database dev/produksi.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"gorm.io/gorm"

	"panda-backend/internal/database"
	"panda-backend/internal/models"
)

// integrationDocIDs: dokumen yang dibuat test, dihapus lagi saat memakai database test yang sudah ada.
var integrationDocIDs []string

func openIntegrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	_ = godotenv.Load(filepath.Join("..", "..", ".env"))
	_ = godotenv.Load(filepath.Join("..", "..", "..", ".env"))

	if existing := os.Getenv("PANDA_IT_DB_NAME"); existing != "" {
		t.Setenv("DB_NAME", existing)
		db, err := database.ConnectAndMigrate()
		if err != nil {
			t.Skipf("database test %s tidak tersedia: %v", existing, err)
		}
		t.Cleanup(func() {
			for _, id := range integrationDocIDs {
				for _, del := range []struct {
					model any
					col   string
				}{{&models.DocumentChunkTerm{}, "document_id"}, {&models.DocumentChunk{}, "document_id"}, {&models.Document{}, "id"}} {
					if err := db.Where(del.col+" = ?", id).Delete(del.model).Error; err != nil {
						t.Errorf("cleanup dokumen uji %s gagal: %v", id, err)
					}
				}
			}
			closeIntegrationDB(t, db)
		})
		return db
	}

	t.Setenv("DB_NAME", "postgres")
	admin, err := database.Connect()
	if err != nil {
		t.Skipf("PostgreSQL tidak tersedia: %v", err)
	}
	// Nama unik per run: tidak pernah menyentuh database lain yang sudah ada.
	name := "panda_ragit_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := admin.Exec(`CREATE DATABASE "` + name + `"`).Error; err != nil {
		closeIntegrationDB(t, admin)
		t.Skipf("tidak bisa membuat database sementara (izin CREATEDB?): %v — atau set PANDA_IT_DB_NAME ke database kosong khusus test", err)
	}
	t.Cleanup(func() {
		if err := admin.Exec(`DROP DATABASE "` + name + `" WITH (FORCE)`).Error; err != nil {
			t.Errorf("gagal menghapus database sementara %s: %v", name, err)
		}
		closeIntegrationDB(t, admin)
	})

	t.Setenv("DB_NAME", name)
	db, err := database.ConnectAndMigrate()
	if err != nil {
		t.Fatalf("migrasi database sementara gagal: %v", err)
	}
	t.Cleanup(func() { closeIntegrationDB(t, db) })
	return db
}

func closeIntegrationDB(t *testing.T, db *gorm.DB) {
	t.Helper()
	sqlDB, err := db.DB()
	if err == nil {
		err = sqlDB.Close()
	}
	if err != nil {
		t.Errorf("gagal menutup koneksi database: %v", err)
	}
}

func cleanLLMSettings(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Where("setting_key LIKE ?", "LLM\\_%").Delete(&models.AppSetting{}).Error; err != nil {
		t.Errorf("cleanup pengaturan LLM gagal: %v", err)
	}
}

func createIndexedDoc(t *testing.T, db *gorm.DB, title, category, ext, scope string, ownerTeam *string, data []byte) (models.Document, ContentIndexResult) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "file"+ext)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	doc := models.Document{
		ID: uuid.NewString(), ModuleSource: "EVIDENCE", DocCategory: category, Title: title,
		StandardizedName: strings.ToUpper(strings.ReplaceAll(title, " ", "_")), OriginalFilename: "file" + ext,
		FileExtension: ext, FilePath: path, FileSizeBytes: int64(len(data)), VersionLabel: "v1.0",
		VisibilityScope: scope, OwnerTeamID: ownerTeam, AISummary: "Ringkasan " + title,
		UploadedByBadge: "IT001", UploadedByName: "Integration Test", UploadedByRole: models.RoleAdmin,
	}
	if err := db.Create(&doc).Error; err != nil {
		t.Fatalf("gagal membuat dokumen %s: %v", title, err)
	}
	integrationDocIDs = append(integrationDocIDs, doc.ID)
	res := IndexDocumentContent(db, &doc, data)
	return doc, res
}

func TestIntegrationRetrieval(t *testing.T) {
	db := openIntegrationDB(t)
	admin := models.User{Role: models.RoleSuperAdmin}
	builtin := LLMConfig{Provider: ProviderBuiltin}
	teamA, teamB := "team-a", "team-b"
	for _, team := range []models.MasterTeam{
		{ID: teamA, TeamCode: "IT_TEAM_A", TeamName: "Integration Team A"},
		{ID: teamB, TeamCode: "IT_TEAM_B", TeamName: "Integration Team B"},
	} {
		if err := db.Create(&team).Error; err != nil {
			t.Fatal(err)
		}
		id := team.ID
		t.Cleanup(func() {
			// Dokumen harus dihapus sebelum tim karena AutoMigrate membuat foreign key.
			for _, docID := range integrationDocIDs {
				if err := db.Model(&models.Document{}).Where("id = ? AND owner_team_id = ?", docID, id).Update("owner_team_id", nil).Error; err != nil {
					t.Errorf("cleanup owner tim uji: %v", err)
				}
			}
			if err := db.Where("id = ?", id).Delete(&models.MasterTeam{}).Error; err != nil {
				t.Errorf("cleanup tim uji: %v", err)
			}
		})
	}

	bast, bastIdx := createIndexedDoc(t, db, "BAST CorePay v1.1", "BAST", ".pdf", "ALL_TIM", nil, buildTestPDF([]testPDFPage{
		{Lines: []string{"BERITA ACARA SERAH TERIMA", "Aplikasi CorePay versi 1.1 diserahterimakan kepada Tim Keuangan.", "Ditandatangani oleh Kepala Departemen TI."}},
		{Image: true},
	}))
	uat, _ := createIndexedDoc(t, db, "Laporan Uji SIMSKBDN", "UAT", ".docx", "ALL_TIM", nil, buildTestZip(map[string]string{
		"word/document.xml": `<w:document ` + wordNS + `><w:body>` +
			wordParagraph("Seluruh skenario modul pembayaran SIMSKBDN telah diuji oleh pengguna dan dinyatakan lulus.") +
			`</w:body></w:document>`,
	}))
	sop, _ := createIndexedDoc(t, db, "SOP Backup Harian", "SOP", ".txt", "ALL_TIM", nil,
		[]byte("Prosedur pencadangan data harian dilakukan setiap pukul 23.00 ke server cadangan."))
	secret, _ := createIndexedDoc(t, db, "Catatan Internal Tim A", "LAINNYA", ".txt", "TEAM_ONLY", &teamA,
		[]byte("Rencana migrasi gudang data rahasia milik tim A."))

	t.Run("indeks kata & halaman tak terbaca", func(t *testing.T) {
		if bastIdx.Source != "FILE" || bastIdx.TotalPages != 2 || bastIdx.UnreadPages != 1 || !strings.Contains(bastIdx.Note, "1 dari 2 halaman") {
			t.Fatalf("hasil indeks PDF campuran salah: %+v", bastIdx)
		}
		var stored models.Document
		db.First(&stored, "id = ?", bast.ID)
		if stored.ContentIndexVersion != contentIndexVersion || stored.ContentUnreadPageCount != 1 || stored.ContentPageCount == nil || *stored.ContentPageCount != 2 {
			t.Fatalf("kolom indeks dokumen tidak tersimpan: %+v", stored)
		}
		var terms int64
		db.Model(&models.DocumentChunkTerm{}).Where("document_id = ? AND term = ?", bast.ID, "serah").Count(&terms)
		if terms == 0 {
			t.Fatalf("indeks kata \"serah\" untuk BAST tidak terbentuk")
		}
	})

	retrieve := func(user models.User, prompt string, history []ChatTurn) *RetrievalResult {
		t.Helper()
		r, err := RetrieveContext(db, user, AIChatRequest{Prompt: prompt}, history, builtin)
		if err != nil {
			t.Fatalf("%q: %v", prompt, err)
		}
		return r
	}
	firstDoc := func(r *RetrievalResult) string {
		if len(r.Documents) == 0 {
			return ""
		}
		return r.Documents[0].ID
	}

	t.Run("pencarian lewat indeks kata", func(t *testing.T) {
		cases := []struct {
			prompt string
			want   string
		}{
			{"kapan serah terima CorePay?", bast.ID},
			{"hasil pengujian modul pembayaran SIMSKBDN", uat.ID}, // "pengujian" → "uji" cocok dengan "diuji"
			{"bagaimana prosedur pencadangan data?", sop.ID},
		}
		for _, c := range cases {
			r := retrieve(admin, c.prompt, nil)
			if firstDoc(r) != c.want {
				t.Errorf("%q: dokumen teratas salah, dapat %v", c.prompt, r.Documents)
				continue
			}
			if len(r.MatchedChunks[c.want]) == 0 || r.MatchedChunks[c.want][0].Text == "" {
				t.Errorf("%q: potongan isi yang cocok harus dimuat beserta teksnya", c.prompt)
			}
		}
	})

	t.Run("pertanyaan negatif & isolasi tim", func(t *testing.T) {
		if r := retrieve(admin, "klaim asuransi kendaraan dinas", nil); len(r.Documents) != 0 {
			t.Errorf("topik yang tidak ada harus berakhir di Document Guard, dapat %v", r.Documents)
		}
		pegawaiB := models.User{Role: models.RolePegawaiTI, TeamID: &teamB}
		for _, d := range retrieve(pegawaiB, "rencana migrasi gudang data rahasia", nil).Documents {
			if d.ID == secret.ID {
				t.Fatalf("dokumen TEAM_ONLY tim A bocor ke pegawai tim B")
			}
		}
		pegawaiA := models.User{Role: models.RolePegawaiTI, TeamID: &teamA}
		if firstDoc(retrieve(pegawaiA, "rencana migrasi gudang data rahasia", nil)) != secret.ID {
			t.Errorf("pegawai tim A harus menemukan dokumen timnya sendiri")
		}
	})

	t.Run("pertanyaan lanjutan", func(t *testing.T) {
		first := retrieve(admin, "kapan serah terima CorePay?", nil)
		history := []ChatTurn{{Prompt: "kapan serah terima CorePay?"}}
		for _, d := range first.Documents {
			history[0].DocumentIDs = append(history[0].DocumentIDs, d.ID)
		}
		r := retrieve(admin, "siapa yang menandatangani dokumen tersebut?", history)
		if !r.FollowUp || firstDoc(r) != bast.ID {
			t.Fatalf("pertanyaan lanjutan harus merujuk BAST sebelumnya: followUp=%v docs=%v", r.FollowUp, r.Documents)
		}
	})

	t.Run("jalur LIKE lama selama re-index", func(t *testing.T) {
		db.Model(&models.Document{}).Where("id = ?", sop.ID).Update("content_index_version", 0)
		r := retrieve(admin, "kapan serah terima CorePay?", nil)
		if firstDoc(r) != bast.ID || len(r.MatchedChunks[bast.ID]) == 0 {
			t.Fatalf("jalur LIKE lama harus tetap menemukan BAST, dapat %v", r.Documents)
		}
	})

	t.Run("re-index tanpa file asli mempertahankan potongan isi", func(t *testing.T) {
		db.Model(&models.Document{}).Where("id = ?", sop.ID).Updates(map[string]interface{}{"content_index_version": 0, "file_path": filepath.Join(t.TempDir(), "hilang.txt")})
		db.Where("document_id = ?", sop.ID).Delete(&models.DocumentChunkTerm{})
		ReindexPendingDocuments(db)

		var chunk models.DocumentChunk
		db.First(&chunk, "document_id = ?", sop.ID)
		var stored models.Document
		db.First(&stored, "id = ?", sop.ID)
		if !strings.Contains(chunk.ChunkText, "pencadangan data harian") || stored.ContentIndexVersion != contentIndexVersion || chunk.TokenCount == 0 {
			t.Fatalf("potongan isi lama harus dipertahankan & indeks dibangun ulang: chunk=%q version=%d", chunk.ChunkText, stored.ContentIndexVersion)
		}
		if firstDoc(retrieve(admin, "prosedur pencadangan data", nil)) != sop.ID {
			t.Fatalf("dokumen yang filenya hilang harus tetap bisa dicari dari indeks yang dibangun ulang")
		}
	})

	t.Run("thinking builtin", func(t *testing.T) {
		var events []string
		resp, err := AnswerAgenticQuery(context.Background(), db, admin,
			AIChatRequest{Prompt: "Apa hasil pengujian SIMSKBDN dan kapan serah terima CorePay?", Thinking: true}, "",
			func(event string, data any) { events = append(events, event) })
		if err != nil {
			t.Fatal(err)
		}
		kinds := map[string]int{}
		for _, s := range resp.Trace.Steps {
			kinds[s.Kind]++
		}
		if kinds["search"] < 2 || kinds["read"] != 1 || kinds["rerank"] != 1 || kinds["answer"] != 1 || !resp.Trace.Thinking {
			t.Fatalf("langkah Thinking builtin tidak lengkap: %v", kinds)
		}
		cited := map[string]bool{}
		for _, d := range resp.MatchedDocuments {
			cited[d.ID] = true
		}
		if !cited[uat.ID] || !cited[bast.ID] {
			t.Fatalf("pertanyaan majemuk harus menemukan UAT & BAST: %v", resp.MatchedDocuments)
		}
		if resp.Trace.Usage.TotalTokens != 0 || !strings.Contains(strings.Join(events, ","), "answer_delta") {
			t.Fatalf("builtin tanpa token & jawaban tetap dialirkan: usage=%+v events=%v", resp.Trace.Usage, events)
		}
	})

	t.Run("agent terkonfigurasi tidak digantikan builtin", func(t *testing.T) {
		for _, thinking := range []bool{false, true} {
			for _, prompt := range []string{"halo", "tadi saya tanya apa?", "cari BAST CorePay"} {
				t.Run(fmt.Sprintf("thinking=%t/%s", thinking, prompt), func(t *testing.T) {
					calls := 0
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls++
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["tools"] == nil {
							t.Error("model harus menerima native tools, bukan JSON planner")
						}
						w.WriteHeader(http.StatusBadRequest)
						fmt.Fprint(w, `{"error":"tools is not supported"}`)
					}))
					defer srv.Close()
					key := "fake-key"
					if err := SaveLLMConfig(db, LLMConfigUpdate{Provider: "custom", BaseURL: srv.URL, Model: "fake-model", APIKey: &key}, "IT001"); err != nil {
						t.Fatal(err)
					}
					defer cleanLLMSettings(t, db)
					resp, err := AnswerAgenticQuery(context.Background(), db, admin, AIChatRequest{Prompt: prompt, Thinking: thinking}, "", nil)
					if resp != nil || err == nil || calls != 1 {
						t.Fatalf("harus mengembalikan error model tanpa jalur pengganti: resp=%+v err=%v calls=%d", resp, err, calls)
					}
				})
			}
		}
	})

	t.Run("thinking otomatis berhenti pada batas keras", func(t *testing.T) {
		decisions := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if body["tool_choice"] == "none" {
				sseContent(w, "Jawaban dari bukti yang sudah ada.")
				return
			}
			decisions++
			sseToolCall(w, "", fmt.Sprintf("call_%d", decisions), "get_repository_stats", `{}`)
		}))
		defer srv.Close()
		key, auto := "fake-key", ThinkingStepsAuto
		if err := SaveLLMConfig(db, LLMConfigUpdate{Provider: "custom", BaseURL: srv.URL, Model: "fake-model", APIKey: &key, ThinkingMaxSteps: &auto}, "IT001"); err != nil {
			t.Fatal(err)
		}
		defer cleanLLMSettings(t, db)

		resp, err := AnswerAgenticQuery(context.Background(), db, admin, AIChatRequest{Prompt: "kapan serah terima CorePay?", Thinking: true}, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		stoppedByLimit := false
		for _, s := range resp.Trace.Steps {
			stoppedByLimit = stoppedByLimit || (s.Title == "Penelusuran dihentikan" && strings.Contains(s.Detail, "Batas"))
		}
		if !stoppedByLimit || decisions != toolAgentThinkingCalls || resp.ReplyMarkdown != "Jawaban dari bukti yang sudah ada." {
			t.Fatalf("agen yang berputar harus dihentikan pada batas %d tool call: decisions=%d", toolAgentThinkingCalls, decisions)
		}
	})

	t.Run("agent tool calling: isolasi tim, kebijakan isi dokumen, sitasi", func(t *testing.T) {
		var mu sync.Mutex
		var toolOutputs []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			var body struct {
				Messages []struct {
					Role    string  `json:"role"`
					Content *string `json:"content"`
				} `json:"messages"`
			}
			_ = json.Unmarshal(raw, &body)
			last := body.Messages[len(body.Messages)-1]
			w.Header().Set("Content-Type", "text/event-stream")
			if last.Role != "tool" {
				fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"search_documents","arguments":"{\"query\":\"rencana migrasi gudang data rahasia\"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\ndata: [DONE]\n\n")
				return
			}
			mu.Lock()
			toolOutputs = append(toolOutputs, *last.Content)
			mu.Unlock()
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"Temuan: [E1]."},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
		}))
		defer srv.Close()
		key, share := "fake-key", false
		if err := SaveLLMConfig(db, LLMConfigUpdate{Provider: "custom", BaseURL: srv.URL, Model: "tool-model", APIKey: &key, ShareDocContent: &share}, "IT001"); err != nil {
			t.Fatal(err)
		}
		defer cleanLLMSettings(t, db)

		pegawaiB := models.User{Role: models.RolePegawaiTI, TeamID: &teamB}
		resp, err := AnswerAgenticQuery(context.Background(), db, pegawaiB, AIChatRequest{Prompt: "rencana migrasi gudang data rahasia"}, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(toolOutputs) != 1 || strings.Contains(toolOutputs[0], secret.Title) {
			t.Fatalf("hasil tool untuk pegawai tim B tidak boleh memuat dokumen TEAM_ONLY tim A: %v", toolOutputs)
		}
		for _, d := range resp.MatchedDocuments {
			if d.ID == secret.ID {
				t.Fatalf("dokumen tim A bocor sebagai sitasi")
			}
		}

		pegawaiA := models.User{Role: models.RolePegawaiTI, TeamID: &teamA}
		resp, err = AnswerAgenticQuery(context.Background(), db, pegawaiA, AIChatRequest{Prompt: "rencana migrasi gudang data rahasia"}, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		out := toolOutputs[len(toolOutputs)-1]
		if !strings.Contains(out, secret.Title) || strings.Contains(out, "gudang data rahasia milik tim A") || !strings.Contains(out, "content_policy") {
			t.Fatalf("saklar isi mati: tim A melihat dokumennya tanpa isi file: %s", out)
		}
		if len(resp.MatchedDocuments) != 1 || resp.MatchedDocuments[0].ID != secret.ID || resp.AnswerBasis != AnswerBasisDocuments ||
			!strings.HasPrefix(resp.EngineMode, "PANDA Agent · tool-model · 1 tool call") {
			t.Fatalf("sitasi/label/engine salah: %+v", resp)
		}
		var tool *AgentStep
		for i := range resp.Trace.Steps {
			if resp.Trace.Steps[i].Kind == "tool" {
				tool = &resp.Trace.Steps[i]
			}
		}
		if tool == nil || tool.Tool.Name != "search_documents" || tool.Rerank == nil || tool.Rerank.Method != "heuristic" || len(tool.Results) == 0 {
			t.Fatalf("log langkah tool tidak lengkap: %+v", tool)
		}
	})

	t.Run("app code filter ignores case", func(t *testing.T) {
		tx := db.Begin()
		if tx.Error != nil {
			t.Fatal(tx.Error)
		}
		defer tx.Rollback()
		folder := models.AppFolder{ID: uuid.NewString(), FolderName: "Case filter", AssignedTeamID: teamA}
		if err := tx.Create(&folder).Error; err != nil {
			t.Fatal(err)
		}
		app := models.Application{ID: uuid.NewString(), FolderID: folder.ID, AppCode: "MiXeD_" + uuid.NewString(), AppName: "Unrelated title"}
		if err := tx.Create(&app).Error; err != nil {
			t.Fatal(err)
		}
		if err := tx.Model(&models.Document{}).Where("id = ?", sop.ID).Update("application_id", app.ID).Error; err != nil {
			t.Fatal(err)
		}
		a, err := newAgentRun(tx, admin, AIChatRequest{}, builtin, newTracer(false, nil))
		if err != nil {
			t.Fatal(err)
		}
		filter := docFilter{AppCode: strings.ToLower(app.AppCode)}
		var got []models.Document
		if err := tx.Where("id IN (?)", a.filteredScope(filter)()).Find(&got).Error; err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != sop.ID {
			t.Fatalf("lowercase app code must match mixed-case code: %+v", got)
		}
	})

	t.Run("canceled retrieval does not become not found", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := RetrieveContext(db.WithContext(ctx), admin, AIChatRequest{Prompt: "CorePay"}, nil, builtin); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected canceled retrieval, got %v", err)
		}
	})

	t.Run("alat evaluasi", func(t *testing.T) {
		report := RunRetrievalEval(db, admin, []EvalCase{
			{ID: "q1", Question: "kapan serah terima CorePay?", ExpectDocs: []string{bast.StandardizedName}, ExpectPhrases: []string{"serah terima"}},
			{ID: "q2", Question: "siapa yang menandatangani dokumen tersebut?", History: []string{"kapan serah terima CorePay?"}, ExpectDocs: []string{bast.StandardizedName}},
			{ID: "n1", Question: "klaim asuransi kendaraan dinas", Negative: true},
		})
		s := report.Summary
		if s.RecallAt4 != 1 || s.HitAt3 != 1 || s.GuardAccuracy != 1 || s.MRR != 1 {
			t.Fatalf("metrik evaluasi salah: %+v\n%+v", s, report.Results)
		}
	})
}
