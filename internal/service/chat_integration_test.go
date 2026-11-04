//go:build integration

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"panda-backend/internal/models"
)

func TestIntegrationChatPersistence(t *testing.T) {
	db := openIntegrationDB(t)
	owner := models.User{ID: uuid.NewString(), BadgeNumber: uuid.NewString(), FullName: "Chat owner", Email: "chat@example.test", Role: models.RoleSuperAdmin}
	if err := db.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Where("id = ?", owner.ID).Delete(&models.User{}) })
	for _, failAt := range []string{"", "upload_audit_logs", "ai_chat_sessions", "ai_chat_messages", "update", "lookup"} {
		t.Run("failure="+failAt, func(t *testing.T) {
			tx := db.Begin()
			defer tx.Rollback()
			if tx.Error != nil {
				t.Fatal(tx.Error)
			}
			injected := errors.New("injected persistence failure")
			active := true
			name := "test:chat_failure"
			if failAt != "" {
				callbacks := tx.Callback().Create()
				if failAt == "update" {
					if err := tx.Callback().Update().Before("gorm:update").Register(name, func(q *gorm.DB) {
						if active {
							q.AddError(injected)
						}
					}); err != nil {
						t.Fatal(err)
					}
					defer tx.Callback().Update().Remove(name)
				} else if failAt == "lookup" {
					if err := tx.Callback().Query().Before("gorm:query").Register(name, func(q *gorm.DB) {
						if active {
							q.AddError(injected)
						}
					}); err != nil {
						t.Fatal(err)
					}
					defer tx.Callback().Query().Remove(name)
				} else {
					if err := callbacks.Before("gorm:create").Register(name, func(q *gorm.DB) {
						if active && q.Statement.Table == failAt {
							q.AddError(injected)
						}
					}); err != nil {
						t.Fatal(err)
					}
					defer callbacks.Remove(name)
				}
			}
			req := AIChatRequest{Prompt: strings.Repeat("界", 400), ReplyTo: &ChatReplyRef{Text: strings.Repeat("é", 301)}}
			if failAt == "lookup" {
				req.SessionID = "existing"
			}
			used := true
			resp := &AIChatResponse{ReplyMarkdown: "saved reply", Trace: &AgentTrace{DocContentUsed: &used}}
			err := recordChat(context.Background(), tx, owner, owner, false, req, resp)
			active = false
			if failAt != "" {
				if !errors.Is(err, injected) || resp.SessionID != "" || resp.MessageID != 0 {
					t.Fatalf("failure accepted: %+v %v", resp, err)
				}
			} else if err != nil || resp.SessionID == "" || resp.MessageID == 0 {
				t.Fatalf("save failed: %+v %v", resp, err)
			}
			want := int64(0)
			if failAt == "" {
				want = 1
			}
			for _, model := range []any{&models.AIChatSession{}, &models.AIChatMessage{}, &models.UploadAuditLog{}} {
				var count int64
				q := tx.Model(model)
				switch model.(type) {
				case *models.AIChatSession:
					q = q.Where("user_id = ?", owner.ID)
				case *models.AIChatMessage:
					q = q.Where("prompt = ?", req.Prompt)
				case *models.UploadAuditLog:
					q = q.Where("badge_number = ?", owner.BadgeNumber)
				}
				if err := q.Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				if count != want {
					t.Fatalf("%T count=%d want=%d", model, count, want)
				}
			}
			if failAt != "" {
				return
			}
			var msg models.AIChatMessage
			if err := tx.First(&msg, resp.MessageID).Error; err != nil {
				t.Fatal(err)
			}
			var trace AgentTrace
			if err := json.Unmarshal([]byte(msg.TraceJSON), &trace); err != nil || trace.DocContentUsed == nil || !*trace.DocContentUsed {
				t.Fatalf("privacy provenance lost: %s %v", msg.TraceJSON, err)
			}
			var audit models.UploadAuditLog
			if err := tx.Where("badge_number = ?", owner.BadgeNumber).First(&audit).Error; err != nil {
				t.Fatal(err)
			}
			if len(audit.DetailDescription) > 1000 || !utf8.ValidString(audit.DetailDescription) {
				t.Fatal("unsafe audit truncation")
			}
			// An unowned session never receives this actor's answer.
			req.SessionID = resp.SessionID
			other := owner
			other.ID = uuid.NewString()
			other.BadgeNumber = uuid.NewString()
			if err := tx.Create(&other).Error; err != nil {
				t.Fatal(err)
			}
			otherResp := &AIChatResponse{ReplyMarkdown: "other reply"}
			if err := recordChat(context.Background(), tx, other, other, false, req, otherResp); err != nil {
				t.Fatal(err)
			}
			if otherResp.SessionID == resp.SessionID {
				t.Fatal("cross-owner history write")
			}
		})
	}
	t.Run("run chat generates and persists before returning", func(t *testing.T) {
		tx := db.Begin()
		defer tx.Rollback()
		cfg := testLLMServer(t, func(w http.ResponseWriter, body map[string]any) {
			sseContent(w, "Secara umum, halo.")
		})
		key := cfg.APIKey
		if err := SaveLLMConfig(tx, LLMConfigUpdate{Provider: "custom", BaseURL: cfg.BaseURL, Model: cfg.Model, APIKey: &key}, owner.BadgeNumber); err != nil {
			t.Fatal(err)
		}
		resp, err := RunChat(context.Background(), tx, owner, owner, false, AIChatRequest{Prompt: "halo"}, nil)
		if err != nil || resp == nil || resp.SessionID == "" || resp.MessageID == 0 {
			t.Fatalf("run chat did not return committed IDs: %+v %v", resp, err)
		}
		var message models.AIChatMessage
		if err := tx.First(&message, resp.MessageID).Error; err != nil || message.ReplyMarkdown != resp.ReplyMarkdown {
			t.Fatalf("run result not persisted: %+v %v", message, err)
		}
	})
	t.Run("impersonation audit only", func(t *testing.T) {
		tx := db.Begin()
		defer tx.Rollback()
		resp := &AIChatResponse{}
		if err := recordChat(context.Background(), tx, models.User{Role: models.RolePegawaiTI}, owner, true, AIChatRequest{Prompt: "segment"}, resp); err != nil {
			t.Fatal(err)
		}
		var count int64
		if err := tx.Model(&models.AIChatSession{}).Where("user_id = ?", owner.ID).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 || resp.SessionID != "" || resp.MessageID != 0 {
			t.Fatal("impersonation persisted history")
		}
		if err := tx.Model(&models.UploadAuditLog{}).Where("badge_number = ?", owner.BadgeNumber).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatal("missing real actor audit")
		}
	})
}
