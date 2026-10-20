package service

import (
	"strings"
	"testing"
)

var testCategories = []string{"PRD", "UAT", "BAST", "FSD", "SOP", "SLA", "DRC", "PENTEST", "LAINNYA"}

func testCandidates() []UploadCandidate {
	return []UploadCandidate{
		{Key: EvidenceDestinationKey, Kind: "EVIDENCE", Label: "Menu Evidence — Tim Digitalisasi"},
		{Key: "app:skbdn:s11", Kind: "APPLICATION", Label: "Korporat › SIMSKBDN › v1.1", AppCode: "SIMSKBDN",
			AppName: "Sistem Informasi Monitoring SKBDN Terintegrasi", VersionLabel: "v1.1", LatestSection: true},
		{Key: "app:skbdn:s10", Kind: "APPLICATION", Label: "Korporat › SIMSKBDN › v1.0", AppCode: "SIMSKBDN",
			AppName: "Sistem Informasi Monitoring SKBDN Terintegrasi", VersionLabel: "v1.0"},
		{Key: "app:plant:s10", Kind: "APPLICATION", Label: "Produksi › SMARTPLANT › v1.0", AppCode: "SMARTPLANT",
			AppName: "Pusri Smart Plant Telemetry", VersionLabel: "v1.0", LatestSection: true},
	}
}

func TestPromptExcludesContentWhenSharingDisabled(t *testing.T) {
	in := SuggestUploadInput{FileName: "UAT.pdf", ExtractedText: "RAHASIA-ISI-DOKUMEN-123", Categories: testCategories, Candidates: testCandidates()}

	msgs, _ := buildUploadSuggestPrompt(in, in.Candidates, false)
	for _, m := range msgs {
		if strings.Contains(m["content"], "RAHASIA-ISI-DOKUMEN-123") {
			t.Fatalf("isi dokumen ikut terkirim ke LLM padahal toggle mati")
		}
	}

	msgs, _ = buildUploadSuggestPrompt(in, in.Candidates, true)
	if !strings.Contains(msgs[1]["content"], "RAHASIA-ISI-DOKUMEN-123") {
		t.Fatalf("isi dokumen seharusnya disertakan saat toggle aktif")
	}
}

func TestPromptHidesInternalIDs(t *testing.T) {
	in := SuggestUploadInput{FileName: "x.pdf", Categories: testCategories, Candidates: testCandidates()}
	msgs, idMap := buildUploadSuggestPrompt(in, in.Candidates, false)
	if strings.Contains(msgs[1]["content"], "app:skbdn") {
		t.Fatalf("ID internal tujuan tidak boleh dikirim ke LLM")
	}
	if idMap["D2"] != "app:skbdn:s11" {
		t.Fatalf("pemetaan ID salah: %v", idMap)
	}
}

func TestParseLLMDropsUnknownIDsAndCategories(t *testing.T) {
	idMap := map[string]string{"D1": EvidenceDestinationKey, "D2": "app:skbdn:s11"}
	reply := "Berikut hasilnya:\n```json\n" + `{"destinations":[{"id":"D2","reason":"kode SIMSKBDN"},{"id":"D9","reason":"x"},{"id":"app:lain:rahasia","reason":"x"},{"id":"d2","reason":"duplikat"}],
	"categories":[{"value":"HACK","reason":"x"},{"value":"uat","reason":"nama file"}],"titles":["UAT SIMSKBDN"],"version":"versi 1.1","summary":"ok"}` + "\n```"

	got, ok := parseLLMUploadSuggestion(reply, idMap, testCategories)
	if !ok {
		t.Fatalf("jawaban valid gagal di-parse")
	}
	if len(got.Destinations) != 1 || got.Destinations[0].Value != "app:skbdn:s11" {
		t.Fatalf("hanya ID yang ada di daftar yang boleh lolos, dapat: %+v", got.Destinations)
	}
	if len(got.Categories) != 1 || got.Categories[0].Value != "UAT" {
		t.Fatalf("kategori di luar enum harus dibuang, dapat: %+v", got.Categories)
	}
	if got.Version != "v1.1" {
		t.Fatalf("versi harus dinormalisasi, dapat %q", got.Version)
	}
}

func TestParseLLMRejectsNonJSON(t *testing.T) {
	if _, ok := parseLLMUploadSuggestion("maaf saya tidak tahu", map[string]string{}, testCategories); ok {
		t.Fatalf("jawaban non-JSON harus ditolak")
	}
}

func TestBuiltinRanksMatchingApplicationAndVersion(t *testing.T) {
	s := SuggestUpload(LLMConfig{Provider: ProviderBuiltin}, SuggestUploadInput{
		FileName:   "SIMSKBDN_UAT_v1.1_final.pdf",
		Message:    "hasil pengujian perubahan SSO",
		Categories: testCategories,
		Candidates: testCandidates(),
	})
	if len(s.Destinations) == 0 || s.Destinations[0].Value != "app:skbdn:s11" {
		t.Fatalf("tujuan teratas harus SIMSKBDN v1.1, dapat %+v", s.Destinations)
	}
	for _, d := range s.Destinations {
		if d.Value == "app:plant:s10" {
			t.Fatalf("aplikasi yang tidak disebut tidak boleh disarankan: %+v", s.Destinations)
		}
		if d.Reason == "" {
			t.Fatalf("setiap saran wajib punya alasan: %+v", d)
		}
	}
	if len(s.Categories) == 0 || s.Categories[0].Value != "UAT" {
		t.Fatalf("kategori teratas harus UAT, dapat %+v", s.Categories)
	}
}

func TestBuiltinDoesNotGuessWithoutSignal(t *testing.T) {
	s := SuggestUpload(LLMConfig{Provider: ProviderBuiltin}, SuggestUploadInput{
		FileName:   "scan_0001.pdf",
		Categories: testCategories,
		Candidates: testCandidates(),
	})
	if len(s.Destinations) != 0 || len(s.Categories) != 0 {
		t.Fatalf("tanpa petunjuk, AI tidak boleh menebak: dest=%+v cat=%+v", s.Destinations, s.Categories)
	}
}

func TestCategoryKeywordUsesWordBoundary(t *testing.T) {
	src := []textSource{newTextSource("isi dokumen", "dokumen translasi bahasa", 10)}
	for _, c := range rankCategories(src, testCategories) {
		if c.Value == "SLA" {
			t.Fatalf("kata 'translasi' tidak boleh dianggap SLA")
		}
	}
}

func TestDetectVersions(t *testing.T) {
	if got := detectVersions("SIMSKBDN_UAT_v1.1_final", "rilis versi 2.0"); strings.Join(got, ",") != "v1.1,v2.0" {
		t.Fatalf("deteksi versi salah: %v", got)
	}
	if got := detectVersions("laporan 16.10.2026"); len(got) != 0 {
		t.Fatalf("tanggal tidak boleh dianggap versi: %v", got)
	}
}
