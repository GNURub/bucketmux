package app

import (
	"context"

	"github.com/gnurub/bucketmux/internal/domain"
)

func (s *Service) raiseAlert(ctx context.Context, alert domain.Alert) {
	if alert.ID == "" {
		alert.ID = randomIdentifier("alert-", 10)
	}
	if alert.DedupeKey == "" {
		alert.DedupeKey = alert.Type + ":" + alert.ProviderAccountID + ":" + alert.Bucket + ":" + alert.Key
	}
	_ = s.Store.UpsertAlert(ctx, alert)
}
