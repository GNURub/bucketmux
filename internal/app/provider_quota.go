package app

import (
	"context"
	"fmt"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
	"github.com/gnurub/bucketmux/internal/provider"
)

const quotaReconciliationInterval = 15 * time.Minute

func (s *Service) StartQuotaReconciliationWorker(ctx context.Context) {
	run := func() {
		acquired, err := s.Store.AcquireMaintenanceLease(ctx, "provider-quota", time.Now().UTC(), quotaReconciliationInterval)
		if err != nil {
			s.recordWorkerFailure("quota", err)
			return
		}
		if !acquired {
			return
		}
		defer func() { _ = s.Store.ReleaseMaintenanceLease(ctx, "provider-quota") }()
		if _, err := s.Store.RecoverExpiredProviderReservations(ctx, time.Now().UTC()); err != nil {
			s.recordWorkerFailure("quota", err)
			return
		}
		providers, err := s.Store.ListProviders(ctx, true)
		if err != nil {
			s.recordWorkerFailure("quota", err)
			return
		}
		var failures int
		for _, account := range providers {
			if _, err := s.ReconcileProviderQuota(ctx, account.ID); err != nil {
				failures++
			}
		}
		if failures > 0 {
			s.recordWorkerFailure("quota", fmt.Errorf("%d provider quota reconciliations failed", failures))
			return
		}
		s.recordWorkerSuccess("quota")
	}
	ticker := time.NewTicker(quotaReconciliationInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

func (s *Service) ReconcileProviderQuota(ctx context.Context, providerID string) (domain.ProviderQuota, error) {
	account, adapter, err := s.providerAccountAndAdapter(ctx, providerID)
	if err != nil {
		return domain.ProviderQuota{}, err
	}
	checkedAt := time.Now().UTC()
	if reporter, ok := adapter.(provider.QuotaReporter); ok {
		capacity, used, source, err := reporter.Quota(ctx, account)
		if err != nil {
			return domain.ProviderQuota{}, err
		}
		if err := s.Store.SetProviderRemoteQuota(ctx, account.ID, capacity, used, source, checkedAt); err != nil {
			return domain.ProviderQuota{}, err
		}
	}
	if lister, ok := adapter.(provider.ObjectLister); ok && account.Bucket != "" {
		var used int64
		token := ""
		for {
			page, err := lister.ListObjects(ctx, account, account.Bucket, "", token, 1000)
			if err != nil {
				return domain.ProviderQuota{}, err
			}
			for _, object := range page.Objects {
				used += object.Size
			}
			if page.NextContinuationToken == "" || page.NextContinuationToken == token {
				break
			}
			token = page.NextContinuationToken
		}
		if err := s.Store.SetProviderUsage(ctx, account.ID, used, "remote-inventory", checkedAt); err != nil {
			return domain.ProviderQuota{}, err
		}
	}
	account, err = s.Store.GetProvider(ctx, account.ID)
	if err != nil {
		return domain.ProviderQuota{}, err
	}
	quota := quotaForAccount(account, checkedAt)
	s.updateQuotaAlert(ctx, account, quota)
	return quota, nil
}

func (s *Service) ListProviderQuotas(ctx context.Context) ([]domain.ProviderQuota, error) {
	accounts, err := s.Store.ListProviders(ctx, false)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]domain.ProviderQuota, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, quotaForAccount(account, now))
	}
	return out, nil
}

func quotaForAccount(account domain.ProviderAccount, now time.Time) domain.ProviderQuota {
	period := now.UTC().Format("2006-01")
	monthlyUsed := account.MonthlyUploadedBytes
	if account.MonthlyPeriod != period {
		monthlyUsed = 0
	}
	available := int64(1<<62 - 1)
	capacity := account.CapacityBytes
	used := account.UsedBytes
	source := account.QuotaSource
	reliable := capacity > 0
	if capacity > 0 {
		available = capacity - used - account.ReservedBytes
	}
	if account.RemoteCapacityBytes > 0 {
		remoteAvailable := account.RemoteCapacityBytes - account.RemoteUsedBytes - account.ReservedBytes
		if remoteAvailable < available {
			available = remoteAvailable
		}
		if capacity <= 0 {
			capacity, used = account.RemoteCapacityBytes, account.RemoteUsedBytes
		}
		reliable = true
		if source == "" {
			source = "remote"
		}
	}
	if available == int64(1<<62-1) {
		available = -1
	}
	if available < -1 {
		available = 0
	}
	if source == "" && account.CapacityBytes > 0 {
		source = "configured-hard-limit"
	}
	return domain.ProviderQuota{ProviderAccountID: account.ID, CapacityBytes: capacity, UsedBytes: used, ReservedBytes: account.ReservedBytes, AvailableBytes: available, MonthlyLimitBytes: intSetting(account.Settings, "monthly_upload_quota_bytes"), MonthlyUploadedBytes: monthlyUsed, Period: period, Source: source, CheckedAt: account.QuotaCheckedAt, Reliable: reliable}
}

func (s *Service) updateQuotaAlert(ctx context.Context, account domain.ProviderAccount, quota domain.ProviderQuota) {
	dedupe := "quota:" + account.ID
	if quota.CapacityBytes <= 0 || quota.AvailableBytes < 0 {
		_ = s.Store.ResolveAlert(ctx, dedupe)
		return
	}
	threshold := intSetting(account.Settings, "quota_alert_threshold_percent")
	if threshold <= 0 || threshold > 100 {
		threshold = 85
	}
	usedPercent := int64(100)
	if quota.CapacityBytes > 0 {
		usedPercent = (quota.CapacityBytes - quota.AvailableBytes) * 100 / quota.CapacityBytes
	}
	if usedPercent < threshold {
		_ = s.Store.ResolveAlert(ctx, dedupe)
		return
	}
	severity := domain.AlertSeverityWarning
	typeName := domain.AlertTypeQuotaNearLimit
	if quota.AvailableBytes == 0 {
		severity, typeName = domain.AlertSeverityCritical, domain.AlertTypeQuotaExhausted
	}
	s.raiseAlert(ctx, domain.Alert{DedupeKey: dedupe, Type: typeName, Severity: severity, ProviderAccountID: account.ID, Message: fmt.Sprintf("provider quota is %d%% used (%d bytes available)", usedPercent, quota.AvailableBytes)})
}
