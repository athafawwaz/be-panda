package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"panda-backend/internal/models"
)

// RunChat is the shared use case for JSON and SSE: generate, then commit history and audit.
// The transport alone sends the terminal done/error event.
func RunChat(ctx context.Context, db *gorm.DB, user, actor models.User, impersonating bool, req AIChatRequest, emit AgentEmitter) (*AIChatResponse, error) {
	resp, err := AnswerAgenticQuery(ctx, db, user, req, actor.ID, emit)
	if err != nil {
		return nil, err
	}
	if err := recordChat(ctx, db, user, actor, impersonating, req, resp); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &AgentError{Message: "Jawaban telah dibuat, tetapi gagal menyimpan riwayat chat. Silakan coba lagi.", Cause: err}
	}
	return resp, nil
}

func UserTeamLabel(u models.User) string {
	switch u.Role {
	case models.RoleSuperAdmin:
		return "Super Admin (Semua Tim)"
	case models.RoleAdmin:
		return "Administrator (Semua Tim)"
	}
	if u.Team != nil && u.Branch != nil {
		return fmt.Sprintf("%s • %s", u.Team.TeamName, u.Branch.BranchName)
	}
	if u.Team != nil {
		return u.Team.TeamName
	}
	return "Pegawai TI"
}

func chatTitle(prompt string) string {
	title := []rune(strings.Join(strings.Fields(prompt), " "))
	if len(title) > 80 {
		return string(title[:80]) + "…"
	}
	return string(title)
}

func recordChat(ctx context.Context, db *gorm.DB, user, actor models.User, impersonating bool, req AIChatRequest, resp *AIChatResponse) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := time.Now().UTC()
	mode := ""
	if req.Thinking && resp.Trace != nil {
		mode = fmt.Sprintf(" [Thinking: %d pencarian, %d token]", resp.Trace.Searches, resp.Trace.Usage.TotalTokens)
	}
	detail := fmt.Sprintf("%s (Badge %s) mencari di Agentic AI: \"%s\" (%d dokumen ditemukan)%s", actor.FullName, actor.BadgeNumber, req.Prompt, len(resp.MatchedDocuments), mode)
	if impersonating {
		detail += fmt.Sprintf(" [impersonate sebagai %s]", UserTeamLabel(user))
	}
	if len(detail) > 1000 {
		end := 1000
		for !utf8.RuneStart(detail[end]) {
			end--
		}
		detail = detail[:end]
	}
	var sessionID string
	var messageID int64
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&models.UploadAuditLog{
			BadgeNumber: actor.BadgeNumber, UserName: actor.FullName, UserRole: actor.Role,
			TeamAndBranchLabel: UserTeamLabel(actor), ActionType: "AI_SEARCH", ModuleSource: "AGENTIC_AI",
			VisibilityLabel: resp.ScopeLabel, DetailDescription: detail, ActionTimestamp: now,
		}).Error; err != nil {
			return fmt.Errorf("record chat audit: %w", err)
		}
		// Impersonation is read-only: audit the real actor, but never mix segment answers into their history.
		if impersonating {
			return nil
		}
		var err error
		sessionID, messageID, err = saveChatTurn(tx, actor, req, resp, now)
		return err
	})
	if err != nil {
		return err
	}
	if !impersonating {
		resp.SessionID, resp.MessageID = sessionID, messageID
	}
	return nil
}

func saveChatTurn(tx *gorm.DB, owner models.User, req AIChatRequest, resp *AIChatResponse, now time.Time) (string, int64, error) {
	docsJSON, err := json.Marshal(resp.MatchedDocuments)
	if err != nil {
		return "", 0, fmt.Errorf("encode chat documents: %w", err)
	}
	traceJSON := ""
	if resp.Trace != nil {
		data, err := json.Marshal(resp.Trace)
		if err != nil {
			return "", 0, fmt.Errorf("encode chat trace: %w", err)
		}
		traceJSON = string(data)
	}
	replyJSON := ""
	if req.ReplyTo != nil {
		ref := *req.ReplyTo
		if text := []rune(strings.TrimSpace(ref.Text)); len(text) > 300 {
			ref.Text = string(text[:300]) + "…"
		}
		data, err := json.Marshal(ref)
		if err != nil {
			return "", 0, fmt.Errorf("encode chat reply: %w", err)
		}
		replyJSON = string(data)
	}
	var session models.AIChatSession
	if req.SessionID != "" {
		err := tx.First(&session, "id = ? AND user_id = ?", req.SessionID, owner.ID).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return "", 0, fmt.Errorf("load chat session: %w", err)
		}
	}
	if session.ID == "" {
		session = models.AIChatSession{ID: uuid.NewString(), UserID: owner.ID, Title: chatTitle(req.Prompt), CreatedAt: now, UpdatedAt: now}
		if err := tx.Create(&session).Error; err != nil {
			return "", 0, fmt.Errorf("create chat session: %w", err)
		}
	}
	msg := models.AIChatMessage{
		SessionID: session.ID, Prompt: req.Prompt, FilterModule: req.FilterModule,
		ReplyMarkdown: resp.ReplyMarkdown, EngineMode: resp.EngineMode, ScopeLabel: resp.ScopeLabel,
		MatchedDocumentsJSON: string(docsJSON), TraceJSON: traceJSON, ReplyToJSON: replyJSON,
		AnswerBasis: resp.AnswerBasis, CreatedAt: now,
	}
	if err := tx.Create(&msg).Error; err != nil {
		return "", 0, fmt.Errorf("create chat message: %w", err)
	}
	if err := tx.Model(&models.AIChatSession{}).Where("id = ? AND user_id = ?", session.ID, owner.ID).Updates(map[string]any{
		"updated_at": now, "message_count": gorm.Expr("message_count + 1"),
	}).Error; err != nil {
		return "", 0, fmt.Errorf("update chat session: %w", err)
	}
	return session.ID, msg.ID, nil
}
