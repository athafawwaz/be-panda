//go:build integration && linux

package service

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/sse"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"panda-backend/internal/httptransport"
	"panda-backend/internal/models"
)

func TestIntegrationTCPDisconnectRollsBackChat(t *testing.T) {
	db := openIntegrationDB(t)
	owner := models.User{ID: uuid.NewString(), BadgeNumber: uuid.NewString(), FullName: "TCP cancellation", Role: models.RoleSuperAdmin}
	if err := db.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Where("id = ?", owner.ID).Delete(&models.User{}) })
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			inserted := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			result := make(chan error, 1)
			callback := "test:tcp_chat_cancel"
			if err := db.Callback().Create().After("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table != "upload_audit_logs" {
					return
				}
				close(inserted)
				select {
				case <-tx.Statement.Context.Done():
				case <-release:
				}
			}); err != nil {
				t.Fatal(err)
			}
			defer db.Callback().Create().Remove(callback)
			app := fiber.New(fiber.Config{WriteTimeout: time.Second})
			persist := func(c fiber.Ctx) error {
				ctx, cleanup, err := httptransport.RequestContext(c.Context(), c.RequestCtx().Conn())
				if err != nil {
					result <- err
					return err
				}
				defer cleanup()
				response := &AIChatResponse{ReplyMarkdown: "generated before disconnect"}
				err = recordChat(ctx, db, owner, owner, false, AIChatRequest{Prompt: "TCP cancellation contract"}, response)
				if err == nil || ctx.Err() == nil || response.SessionID != "" || response.MessageID != 0 {
					result <- fmt.Errorf("cancellation contract violated: ctx=%v err=%v response=%+v", ctx.Err(), err, response)
				} else {
					result <- nil
				}
				return nil
			}
			if stream {
				app.Post("/chat", sse.New(sse.Config{HeartbeatInterval: 10 * time.Millisecond, Handler: func(c fiber.Ctx, s *sse.Stream) error {
					c.SetContext(s.Context())
					return persist(c)
				}}))
			} else {
				app.Post("/chat", persist)
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			serverDone := make(chan error, 1)
			go func() { serverDone <- app.Listener(ln) }()
			defer func() {
				unblock()
				if err := app.ShutdownWithTimeout(time.Second); err != nil {
					t.Error(err)
				}
				select {
				case err := <-serverDone:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(2 * time.Second):
					t.Error("server leaked")
				}
			}()
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := fmt.Fprint(conn, "POST /chat HTTP/1.1\r\nHost: localhost\r\nContent-Length: 0\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-inserted:
			case <-time.After(time.Second):
				t.Fatal("transaction did not reach audit insert")
			}
			if err := conn.(*net.TCPConn).SetLinger(0); err != nil {
				t.Fatal(err)
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("disconnect did not rollback transaction")
			}
			for _, query := range []*gorm.DB{
				db.Model(&models.UploadAuditLog{}).Where("badge_number = ?", owner.BadgeNumber),
				db.Model(&models.AIChatSession{}).Where("user_id = ?", owner.ID),
				db.Model(&models.AIChatMessage{}).Where("prompt = ?", "TCP cancellation contract"),
			} {
				var count int64
				if err := query.Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatalf("disconnect persisted %d rows", count)
				}
			}
		})
	}
}
