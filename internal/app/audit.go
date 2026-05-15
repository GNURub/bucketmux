package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

func (s *Service) RecordAuditEvent(ctx context.Context, event domain.AuditEvent) {
	if event.ID == "" {
		event.ID = newAuditID()
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	_ = s.Store.CreateAuditEvent(ctx, event)
}

func AuditActorFromRequest(r *http.Request) string {
	if user, _, ok := r.BasicAuth(); ok && user != "" {
		return user
	}
	return "admin"
}

func newAuditID() string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return fmt.Sprintf("audit-%d", time.Now().UnixNano())
	}
	return "audit-" + hex.EncodeToString(bytes[:])
}
