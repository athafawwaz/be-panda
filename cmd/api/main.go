package main

import (
	"log"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"

	"panda-backend/internal/database"
	"panda-backend/internal/handler"
	"panda-backend/internal/service"
	"panda-backend/internal/storage"
)

func main() {
	_ = godotenv.Load(".env")
	_ = godotenv.Load("../.env")

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

	r := gin.Default()

	// CORS Middleware
	r.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, accept, origin, Cache-Control, X-Requested-With, X-Impersonate-Team, X-Impersonate-Branch")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT, PATCH, DELETE")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	})

	h := handler.NewHandler(db)

	api := r.Group("/api/v1")
	{
		// Public endpoints
		api.GET("/public/stats", h.GetPublicStats)
		api.POST("/auth/login", h.Login)
		// Preview & Download endpoints (support direct link click & iframe preview)
		api.GET("/documents/:id/preview", h.PreviewDocument)
		api.GET("/documents/:id/download", h.DownloadDocument)

		// Protected endpoints
		protected := api.Group("")
		protected.Use(h.AuthMiddleware())
		{
			protected.GET("/auth/me", h.GetMe)
			protected.GET("/dashboard/summary", h.GetDashboardSummary)

			// Master Data Tim, Branch & Master Akses (Badge RBAC)
			protected.GET("/master/teams", h.GetMasterTeams)
			protected.POST("/master/teams", h.CreateMasterTeam)
			protected.PUT("/master/teams/:id", h.UpdateMasterTeam)
			protected.DELETE("/master/teams/:id", h.DeleteMasterTeam)
			protected.POST("/master/branches", h.CreateMasterBranch)
			protected.PUT("/master/branches/:id", h.UpdateMasterBranch)
			protected.DELETE("/master/branches/:id", h.DeleteMasterBranch)
			protected.GET("/master/users", h.GetMasterUsers)
			protected.POST("/master/users", h.CreateMasterUser)
			protected.PUT("/master/users/:id", h.UpdateMasterUser)
			protected.DELETE("/master/users/:id", h.DeleteMasterUser)

			// Hirarki Aplikasi: Folder Tim -> Aplikasi -> Section Perubahan (Versioning)
			protected.GET("/folders", h.GetFolders)
			protected.POST("/folders", h.CreateFolder)
			protected.POST("/folders/:id/applications", h.CreateApplication)
			protected.GET("/applications/:id", h.GetApplicationDetail)
			protected.POST("/applications/:id/sections", h.CreateAppSection)

			// Menu Evidence & Document Upload
			protected.GET("/evidence", h.GetEvidenceList)
			protected.POST("/documents/upload", h.UploadDocument)

			// Log Aktivitas & Upload Detail
			protected.GET("/logs", h.GetUploadLogs)

			// Agentic AI Chat (Team-Scoped RAG + Dual Mode)
			protected.POST("/ai/chat", h.AgenticAIChat)
			protected.POST("/ai/chat/stream", h.AgenticAIChatStream)
			protected.GET("/ai/chat/shortcuts", h.GetAIChatShortcuts)
			// Riwayat chat Agentic AI (hanya milik user yang login)
			protected.GET("/ai/chat/sessions", h.GetChatSessions)
			protected.GET("/ai/chat/sessions/:id", h.GetChatSession)
			protected.DELETE("/ai/chat/sessions/:id", h.DeleteChatSession)
			// Status kuota AI untuk peringatan di halaman chat
			protected.GET("/ai/quota", h.GetAIQuota)

			// Upload via Agentic AI: analisis (AI memberi pilihan) -> konfirmasi user
			protected.POST("/ai/upload/analyze", h.AnalyzeAIUpload)
			protected.POST("/ai/upload/confirm", h.ConfirmAIUpload)
			protected.DELETE("/ai/upload/:id", h.CancelAIUpload)

			// Khusus Super Admin: Setting API Key Agentic AI (menu Docs)
			superAdmin := protected.Group("/admin")
			superAdmin.Use(h.RequireSuperAdmin())
			{
				superAdmin.GET("/settings/ai", h.GetAISettings)
				superAdmin.PUT("/settings/ai", h.UpdateAISettings)
				superAdmin.POST("/settings/ai/test", h.TestAISettings)
				superAdmin.GET("/settings/ai/quota", h.GetAIQuotaDetail)
				superAdmin.PUT("/settings/ai/quota", h.UpdateAIQuotaLimit)
				superAdmin.GET("/settings/rerank", h.GetRerankSettings)
				superAdmin.PUT("/settings/rerank", h.UpdateRerankSettings)
				superAdmin.POST("/settings/rerank/test", h.TestRerankSettings)
			}
		}
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("🐼 PANDA Backend Server running on http://localhost:%s", port)
	if err := r.Run(":" + port); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
