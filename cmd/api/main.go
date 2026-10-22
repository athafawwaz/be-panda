package main

import (
	"log"
	"net/http"
	"os"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/logger"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/joho/godotenv"

	"panda-backend/internal/database"
	"panda-backend/internal/handler"
	"panda-backend/internal/service"
	"panda-backend/internal/storage"
)

func main() {
	_ = godotenv.Load(".env")
	_ = godotenv.Load("../.env")

	if err := service.ValidateAppSecret(); err != nil {
		log.Fatalf("Invalid JWT configuration: %v", err)
	}

	db, err := database.ConnectAndMigrate()
	if err != nil {
		log.Fatalf("Database connection failed: %v", err)
	}

	// Storage dokumen: MinIO bila MINIO_ENDPOINT/ACCESS_KEY/SECRET_KEY/BUCKET terisi, selain itu folder lokal uploads/
	if err := storage.Init(storage.ConfigFromEnv()); err != nil {
		log.Printf("[Storage] PERINGATAN: %v — upload & pratinjau dokumen di MinIO akan gagal sampai MinIO dapat dihubungi", err)
	}
	log.Printf("[Storage] File dokumen baru disimpan di %s", storage.Describe())

	// Catat setiap permintaan ke LLM eksternal untuk memantau kuota (menu Docs → Kuota AI)
	service.EnableLLMUsageTracking(db)
	// Baca isi file dokumen lama yang belum diindeks untuk Agentic AI (RAG)
	go service.ReindexPendingDocuments(db)
	// Bersihkan file staging upload via Agentic AI yang tidak dikonfirmasi (kedaluwarsa 1 jam)
	go service.RunStagingJanitor(db)

	r := newRouter(handler.NewHandler(db))
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("🐼 PANDA Backend Server running on http://localhost:%s", port)
	if err := r.Listen(":" + port); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

func newRouter(h *handler.Handler) *fiber.App {
	r := fiber.New(handler.HTTPConfig())
	r.Use(recover.New(), logger.New())
	r.Use(func(c fiber.Ctx) error {
		c.Set("Access-Control-Allow-Origin", "*")
		c.Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, accept, origin, Cache-Control, X-Requested-With, X-Impersonate-Team, X-Impersonate-Branch")
		c.Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT, PATCH, DELETE")
		if c.Method() == http.MethodOptions {
			return c.SendStatus(http.StatusNoContent)
		}
		return c.Next()
	})

	api := r.Group("/api/v1")
	{
		// Public endpoints
		api.Get("/public/stats", h.GetPublicStats)
		api.Post("/auth/login", h.Login)

		// Protected endpoints
		protected := api.Group("")
		{
			protected.Get("/auth/me", h.AuthMiddleware(), h.GetMe)
			protected.Get("/documents/:id/preview", h.AuthMiddleware(), h.PreviewDocument)
			protected.Get("/documents/:id/download", h.AuthMiddleware(), h.DownloadDocument)
			protected.Get("/dashboard/summary", h.AuthMiddleware(), h.GetDashboardSummary)

			// Master Data Tim, Branch & Master Akses (Badge RBAC)
			protected.Get("/master/teams", h.AuthMiddleware(), h.GetMasterTeams)
			protected.Post("/master/teams", h.AuthMiddleware(), h.CreateMasterTeam)
			protected.Put("/master/teams/:id", h.AuthMiddleware(), h.UpdateMasterTeam)
			protected.Delete("/master/teams/:id", h.AuthMiddleware(), h.DeleteMasterTeam)
			protected.Post("/master/branches", h.AuthMiddleware(), h.CreateMasterBranch)
			protected.Put("/master/branches/:id", h.AuthMiddleware(), h.UpdateMasterBranch)
			protected.Delete("/master/branches/:id", h.AuthMiddleware(), h.DeleteMasterBranch)
			protected.Get("/master/users", h.AuthMiddleware(), h.GetMasterUsers)
			protected.Post("/master/users", h.AuthMiddleware(), h.CreateMasterUser)
			protected.Put("/master/users/:id", h.AuthMiddleware(), h.UpdateMasterUser)
			protected.Delete("/master/users/:id", h.AuthMiddleware(), h.DeleteMasterUser)

			// Hirarki Aplikasi: Folder Tim -> Aplikasi -> Section Perubahan (Versioning)
			protected.Get("/folders", h.AuthMiddleware(), h.GetFolders)
			protected.Post("/folders", h.AuthMiddleware(), h.CreateFolder)
			protected.Post("/folders/:id/applications", h.AuthMiddleware(), h.CreateApplication)
			protected.Get("/applications/:id", h.AuthMiddleware(), h.GetApplicationDetail)
			protected.Post("/applications/:id/sections", h.AuthMiddleware(), h.CreateAppSection)

			// Menu Evidence & Document Upload
			protected.Get("/evidence", h.AuthMiddleware(), h.GetEvidenceList)
			protected.Post("/documents/upload", h.AuthMiddleware(), h.UploadDocument)

			// Log Aktivitas & Upload Detail
			protected.Get("/logs", h.AuthMiddleware(), h.GetUploadLogs)

			// Agentic AI Chat (Team-Scoped RAG + Dual Mode)
			protected.Post("/ai/chat", h.AuthMiddleware(), h.AgenticAIChat)
			protected.Post("/ai/chat/stream", h.AuthMiddleware(), h.AgenticAIChatStream)
			protected.Get("/ai/chat/shortcuts", h.AuthMiddleware(), h.GetAIChatShortcuts)
			// Riwayat chat Agentic AI (hanya milik user yang login)
			protected.Get("/ai/chat/sessions", h.AuthMiddleware(), h.GetChatSessions)
			protected.Get("/ai/chat/sessions/:id", h.AuthMiddleware(), h.GetChatSession)
			protected.Delete("/ai/chat/sessions/:id", h.AuthMiddleware(), h.DeleteChatSession)
			// Status kuota AI untuk peringatan di halaman chat
			protected.Get("/ai/quota", h.AuthMiddleware(), h.GetAIQuota)

			// Upload via Agentic AI: analisis (AI memberi pilihan) -> konfirmasi user
			protected.Post("/ai/upload/analyze", h.AuthMiddleware(), h.AnalyzeAIUpload)
			protected.Post("/ai/upload/confirm", h.AuthMiddleware(), h.ConfirmAIUpload)
			protected.Delete("/ai/upload/:id", h.AuthMiddleware(), h.CancelAIUpload)

			// Khusus Super Admin: Setting API Key Agentic AI (menu Docs)
			superAdmin := protected.Group("/admin")
			{
				superAdmin.Get("/settings/ai", h.AuthMiddleware(), h.RequireSuperAdmin(), h.GetAISettings)
				superAdmin.Put("/settings/ai", h.AuthMiddleware(), h.RequireSuperAdmin(), h.UpdateAISettings)
				superAdmin.Post("/settings/ai/test", h.AuthMiddleware(), h.RequireSuperAdmin(), h.TestAISettings)
				superAdmin.Get("/settings/ai/quota", h.AuthMiddleware(), h.RequireSuperAdmin(), h.GetAIQuotaDetail)
				superAdmin.Put("/settings/ai/quota", h.AuthMiddleware(), h.RequireSuperAdmin(), h.UpdateAIQuotaLimit)
				superAdmin.Get("/settings/rerank", h.AuthMiddleware(), h.RequireSuperAdmin(), h.GetRerankSettings)
				superAdmin.Put("/settings/rerank", h.AuthMiddleware(), h.RequireSuperAdmin(), h.UpdateRerankSettings)
				superAdmin.Post("/settings/rerank/test", h.AuthMiddleware(), h.RequireSuperAdmin(), h.TestRerankSettings)
			}
		}
	}

	r.Use(func(c fiber.Ctx) error {
		if path := c.Path(); len(path) > 1 && path[len(path)-1] == '/' {
			target := path[:len(path)-1]
			for _, route := range r.GetRoutes(true) {
				if route.Path == target && route.Method == c.Method() {
					return c.Redirect().Status(http.StatusMovedPermanently).To(target)
				}
			}
		}
		return c.Status(http.StatusNotFound).SendString("404 page not found")
	})
	return r
}
