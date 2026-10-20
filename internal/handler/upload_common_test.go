package handler

import (
	"net/http"
	"testing"
	"time"

	"panda-backend/internal/models"
)

func TestValidateUploadedFile(t *testing.T) {
	const limit = 1 << 20
	cases := []struct {
		name   string
		file   string
		data   []byte
		status int
	}{
		{"pdf asli", "UAT.pdf", []byte("%PDF-1.4\n1 0 obj"), 0},
		{"exe di-rename jadi pdf", "UAT.pdf", []byte("MZ\x90\x00\x03\x00\x00\x00binary"), http.StatusUnsupportedMediaType},
		{"ekstensi exe", "setup.exe", []byte("MZ\x90\x00"), http.StatusUnsupportedMediaType},
		{"docx (zip)", "PRD.DOCX", []byte("PK\x03\x04\x14\x00\x06\x00"), 0},
		{"doc lama (OLE)", "BAST.doc", append([]byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}, 0x00), 0},
		{"teks biasa", "catatan.txt", []byte("hasil uji 28 skenario"), 0},
		{"png palsu", "bukti.png", []byte("bukan gambar"), http.StatusUnsupportedMediaType},
		{"file kosong", "kosong.pdf", []byte{}, http.StatusBadRequest},
		{"terlalu besar", "besar.txt", make([]byte, limit+1), http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		f, status, msg := validateUploadedFile(tc.file, tc.data, limit)
		if status != tc.status {
			t.Errorf("%s: status %d (%s), harap %d", tc.name, status, msg, tc.status)
		}
		if status == 0 && (f.SHA256 == "" || f.Ext == "") {
			t.Errorf("%s: hash/ekstensi harus terisi", tc.name)
		}
	}
}

func TestValidateUploadedFileStripsPath(t *testing.T) {
	f, status, _ := validateUploadedFile(`..\..\etc\UAT.pdf`, []byte("%PDF-1.4"), 1<<20)
	if status != 0 || f.OriginalName != "UAT.pdf" {
		t.Fatalf("path harus dibuang dari nama file, dapat %q (status %d)", f.OriginalName, status)
	}
}

func TestCanAccessFolder(t *testing.T) {
	teamA, teamB, branchX, branchY := "team-a", "team-b", "branch-x", "branch-y"
	pegawaiAX := models.User{Role: models.RolePegawaiTI, TeamID: &teamA, BranchID: &branchX}
	pegawaiA := models.User{Role: models.RolePegawaiTI, TeamID: &teamA}
	admin := models.User{Role: models.RoleAdmin}

	cases := []struct {
		name   string
		user   models.User
		folder models.AppFolder
		want   bool
	}{
		{"branch sendiri", pegawaiAX, models.AppFolder{AssignedTeamID: teamA, AssignedBranchID: &branchX}, true},
		{"branch lain di tim sama", pegawaiAX, models.AppFolder{AssignedTeamID: teamA, AssignedBranchID: &branchY}, false},
		{"folder level tim", pegawaiAX, models.AppFolder{AssignedTeamID: teamA}, true},
		{"tim lain", pegawaiAX, models.AppFolder{AssignedTeamID: teamB}, false},
		{"pegawai tanpa branch ke branch mana pun di timnya", pegawaiA, models.AppFolder{AssignedTeamID: teamA, AssignedBranchID: &branchY}, true},
		{"admin", admin, models.AppFolder{AssignedTeamID: teamB}, true},
		{"pegawai tanpa tim", models.User{Role: models.RolePegawaiTI}, models.AppFolder{AssignedTeamID: teamA}, false},
	}
	for _, tc := range cases {
		if got := canAccessFolder(tc.user, tc.folder); got != tc.want {
			t.Errorf("%s: dapat %v, harap %v", tc.name, got, tc.want)
		}
	}
}

func TestParseDestinationKey(t *testing.T) {
	if tg, ok := parseDestinationKey("evidence"); !ok || tg.ModuleSource != "EVIDENCE" {
		t.Fatalf("evidence tidak terbaca")
	}
	if tg, ok := parseDestinationKey("app:abc:def"); !ok || tg.ApplicationID != "abc" || tg.SectionID != "def" {
		t.Fatalf("kunci aplikasi tidak terbaca: %+v", tg)
	}
	for _, bad := range []string{"", "app::x", "app:a", "folder:a:b", "app:a:b:c"} {
		if _, ok := parseDestinationKey(bad); ok {
			t.Fatalf("kunci %q seharusnya ditolak", bad)
		}
	}
}

func TestRateLimiter(t *testing.T) {
	l := &rateLimiter{hits: map[string][]time.Time{}, limit: 2, window: time.Minute}
	if !l.allow("u") || !l.allow("u") || l.allow("u") {
		t.Fatalf("limiter harus menolak request ke-3 dalam 1 window")
	}
	if !l.allow("lain") {
		t.Fatalf("limiter harus per user")
	}
}
