//go:build integration

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"panda-backend/internal/models"
)

func TestIntegrationChatCancellationRollsBack(t *testing.T) {
	db := openIntegrationDB(t)
	owner := models.User{ID: uuid.NewString(), BadgeNumber: uuid.NewString(), FullName: "Cancellation contract", Role: models.RoleSuperAdmin}
	if err := db.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Where("id = ?", owner.ID).Delete(&models.User{}) })
	for _, cancelAt := range []string{"before transaction", "upload_audit_logs", "ai_chat_sessions", "ai_chat_messages"} {
		t.Run(cancelAt, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			callback := "test:chat_cancel"
			if cancelAt == "before transaction" {
				cancel()
			} else {
				if err := db.Callback().Create().After("gorm:create").Register(callback, func(tx *gorm.DB) {
					if tx.Statement.Table == cancelAt {
						cancel()
					}
				}); err != nil {
					t.Fatal(err)
				}
				defer db.Callback().Create().Remove(callback)
			}
			response := &AIChatResponse{ReplyMarkdown: "must not persist"}
			err := recordChat(ctx, db, owner, owner, false, AIChatRequest{Prompt: "cancel contract"}, response)
			if err == nil || !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatalf("cancellation accepted: %v", err)
			}
			if response.SessionID != "" || response.MessageID != 0 {
				t.Fatal("uncommitted IDs exposed")
			}
			for _, query := range []*gorm.DB{
				db.Model(&models.UploadAuditLog{}).Where("badge_number = ?", owner.BadgeNumber),
				db.Model(&models.AIChatSession{}).Where("user_id = ?", owner.ID),
				db.Model(&models.AIChatMessage{}).Where("prompt = ?", "cancel contract"),
			} {
				var count int64
				if err := query.Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatalf("canceled transaction persisted %d rows", count)
				}
			}
		})
	}
}
