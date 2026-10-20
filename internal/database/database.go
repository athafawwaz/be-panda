package database

import (
	"embed"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/pressly/goose/v3"
	"gorm.io/driver/sqlserver"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"panda-backend/internal/models"
)

//go:embed migrations/*.sql
var embedMigrations embed.FS

// Connect membuka koneksi SQL Server tanpa migrasi & seeding (dipakai juga oleh alat seperti cmd/rag-eval).
func Connect() (*gorm.DB, error) {
	host := getEnv("DB_HOST", "localhost")
	port := getEnv("DB_PORT", "1433")
	user := getEnv("DB_USER", "panda_user")
	// Password tidak punya nilai default di kode (repo publik): wajib diisi lewat backend/.env
	pass := getEnv("DB_PASSWORD", "")
	if pass == "" {
		return nil, fmt.Errorf("DB_PASSWORD belum diisi di backend/.env (salin dari backend/.env.example)")
	}
	dbName := getEnv("DB_NAME", "PandaDB")
	encrypt := getEnv("DB_ENCRYPT", "disable")

	query := url.Values{}
	query.Add("database", dbName)
	query.Add("encrypt", encrypt)
	query.Add("TrustServerCertificate", "true")

	u := &url.URL{
		Scheme:   "sqlserver",
		User:     url.UserPassword(user, pass),
		Host:     fmt.Sprintf("%s:%s", host, port),
		RawQuery: query.Encode(),
	}

	dsn := u.String()
	db, err := gorm.Open(sqlserver.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to SQL Server: %w", err)
	}
	return db, nil
}

func ConnectAndMigrate() (*gorm.DB, error) {
	db, err := Connect()
	if err != nil {
		return nil, err
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}

	// 1. Run Goose Migrations
	goose.SetBaseFS(embedMigrations)
	if err := goose.SetDialect("mssql"); err != nil {
		log.Printf("[Goose] dialect warning: %v", err)
	}
	if err := goose.Up(sqlDB, "migrations"); err != nil {
		log.Printf("[Goose] migration status: %v", err)
	} else {
		log.Println("[Goose] SQL Server migrations applied successfully.")
	}

	// 2. Run GORM AutoMigrate
	err = db.AutoMigrate(
		&models.MasterTeam{},
		&models.MasterBranch{},
		&models.User{},
		&models.AppFolder{},
		&models.Application{},
		&models.AppChangeSection{},
		&models.Document{},
		&models.DocumentChunk{},
		&models.DocumentChunkTerm{},
		&models.UploadAuditLog{},
		&models.AppSetting{},
		&models.UploadStaging{},
		&models.AIChatSession{},
		&models.AIChatMessage{},
		&models.LLMUsageLog{},
	)
	if err != nil {
		log.Printf("[AutoMigrate] notice: %v", err)
	} else {
		log.Println("[AutoMigrate] SQL Server schema synchronized.")
	}

	// 3. Seed Master Tim/Branch (database baru) & akun Super Admin awal
	if err := SeedInitialData(db); err != nil {
		log.Printf("[Seeder] warning: %v", err)
	}

	return db, nil
}

// SeedInitialData hanya menyiapkan master data organisasi (Tim & Branch) pada database baru
// dan memastikan akun Super Admin awal tersedia. Tidak ada data contoh/dummy yang di-seed.
func SeedInitialData(db *gorm.DB) error {
	_ = os.MkdirAll(filepath.Join("uploads"), 0755)

	// 1. Master Tim & Branch hanya pada database baru (tabel tim masih kosong),
	// agar tim/branch yang sudah diubah atau dihapus Admin tidak muncul kembali saat restart.
	var teamCount int64
	db.Model(&models.MasterTeam{}).Count(&teamCount)
	if teamCount == 0 {
		teams := []models.MasterTeam{
			{ID: "team-digitalisasi", TeamCode: "TIM_DIGITALISASI", TeamName: "Tim Digitalisasi", HasBranches: true},
			{ID: "team-layanan", TeamCode: "TIM_LAYANAN_TI", TeamName: "Tim Layanan TI", HasBranches: false},
			{ID: "team-infrastruktur", TeamCode: "TIM_INFRASTRUKTUR_TI", TeamName: "Tim Infrastruktur TI", HasBranches: false},
			{ID: "team-tatakelola", TeamCode: "TIM_TATA_KELOLA_TI", TeamName: "Tim Tata Kelola TI", HasBranches: false},
		}
		for _, t := range teams {
			db.Create(&t)
		}

		// Branch Tim Digitalisasi (Non Produksi, Produksi, Data)
		branches := []models.MasterBranch{
			{ID: "branch-dig-nonprod", TeamID: "team-digitalisasi", BranchCode: "NON_PRODUKSI", BranchName: "Non Produksi"},
			{ID: "branch-dig-prod", TeamID: "team-digitalisasi", BranchCode: "PRODUKSI", BranchName: "Produksi"},
			{ID: "branch-dig-data", TeamID: "team-digitalisasi", BranchCode: "DATA", BranchName: "Data"},
		}
		for _, b := range branches {
			db.Create(&b)
		}
	}

	// 2. Akun Super Admin awal (Badge INITIAL_ADMIN_BADGE, default 2411249) selalu dipastikan ada
	adminBadge := getEnv("INITIAL_ADMIN_BADGE", "2411249")
	var existing models.User
	if err := db.Where("badge_number = ?", adminBadge).First(&existing).Error; err != nil {
		db.Create(&models.User{
			ID:           "user-admin-" + adminBadge,
			BadgeNumber:  adminBadge,
			FullName:     "Atha Fawwaz (Super Admin PANDA)",
			Email:        "atha.fawwaz@pusri.co.id",
			PasswordHash: getEnv("BYPASS_PASSWORD", "Panda"),
			Role:         models.RoleSuperAdmin,
			IsActive:     true,
		})
	}

	// Badge awal selalu Super Admin (migrasi dari role ADMIN lama)
	db.Model(&models.User{}).
		Where("badge_number = ? AND role <> ?", adminBadge, models.RoleSuperAdmin).
		Update("role", models.RoleSuperAdmin)

	return nil
}

// GenerateValidPDF creates a clean, standards-compliant 1-page PDF file so in-app PDF preview and download work out of the box.
func GenerateValidPDF(title, subtitle, body string) []byte {
	clean := func(s string) string {
		r := strings.NewReplacer("(", "[", ")", "]", "\\", "/", "\r", " ")
		return r.Replace(s)
	}

	wrapLines := func(text string, maxChars int) []string {
		words := strings.Fields(text)
		var lines []string
		var curr string
		for _, w := range words {
			if len(curr)+len(w)+1 > maxChars {
				lines = append(lines, curr)
				curr = w
			} else if curr == "" {
				curr = w
			} else {
				curr += " " + w
			}
		}
		if curr != "" {
			lines = append(lines, curr)
		}
		return lines
	}

	var contentBuilder strings.Builder
	contentBuilder.WriteString("BT\n")
	contentBuilder.WriteString("/F1 14 Tf\n50 770 Td\n")
	contentBuilder.WriteString(fmt.Sprintf("(PT PUPUK SRIWIDJAJA PALEMBANG - PANDA AUDIT REPOSITORY) Tj\n"))
	contentBuilder.WriteString("0 -26 Td\n/F1 12 Tf\n")
	for _, tl := range wrapLines(clean(title), 72) {
		contentBuilder.WriteString(fmt.Sprintf("(%s) Tj\n0 -18 Td\n", tl))
	}
	contentBuilder.WriteString("/F1 9 Tf\n")
	for _, sl := range wrapLines(clean(subtitle), 85) {
		contentBuilder.WriteString(fmt.Sprintf("(%s) Tj\n0 -15 Td\n", sl))
	}
	contentBuilder.WriteString("0 -12 Td\n/F1 10 Tf\n")
	for _, bl := range wrapLines(clean(body), 80) {
		contentBuilder.WriteString(fmt.Sprintf("(%s) Tj\n0 -16 Td\n", bl))
	}
	contentBuilder.WriteString("ET\n")

	stream := contentBuilder.String()

	obj1 := "1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n"
	obj2 := "2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n"
	obj3 := "3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 842] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>\nendobj\n"
	obj4 := fmt.Sprintf("4 0 obj\n<< /Length %d >>\nstream\n%sendstream\nendobj\n", len(stream), stream)
	obj5 := "5 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>\nendobj\n"

	header := "%PDF-1.4\n"
	o1 := len(header)
	o2 := o1 + len(obj1)
	o3 := o2 + len(obj2)
	o4 := o3 + len(obj3)
	o5 := o4 + len(obj4)
	xrefPos := o5 + len(obj5)

	xref := fmt.Sprintf("xref\n0 6\n0000000000 65535 f \n%010d 00000 n \n%010d 00000 n \n%010d 00000 n \n%010d 00000 n \n%010d 00000 n \n", o1, o2, o3, o4, o5)
	trailer := fmt.Sprintf("trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xrefPos)

	return []byte(header + obj1 + obj2 + obj3 + obj4 + obj5 + xref + trailer)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
