package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
	"github.com/gnurub/bucketmux/internal/provider"
)

func (s *Service) ValidateProvider(ctx context.Context, account domain.ProviderAccount) (domain.ProviderValidation, error) {
	if strings.TrimSpace(account.ID) == "" || account.Kind == "" || strings.TrimSpace(account.Bucket) == "" {
		return domain.ProviderValidation{}, fmt.Errorf("provider id, kind and bucket are required")
	}
	if account.CapacityBytes < 0 {
		return domain.ProviderValidation{}, fmt.Errorf("provider capacity_bytes cannot be negative")
	}
	if err := validateProviderLimits(account.Settings); err != nil {
		return domain.ProviderValidation{}, err
	}
	decrypted, err := s.decryptAccount(account)
	if err != nil {
		return domain.ProviderValidation{}, err
	}
	adapter, ok := s.Providers.Get(decrypted.Kind)
	if !ok {
		return domain.ProviderValidation{}, fmt.Errorf("provider kind %s is not registered", decrypted.Kind)
	}
	capabilities := domain.ProviderCapabilities{}
	if reporter, ok := adapter.(provider.CapabilityReporter); ok {
		capabilities = reporter.Capabilities(decrypted)
	} else {
		_, capabilities.ListObjects = adapter.(provider.ObjectLister)
		_, capabilities.DiscoverBuckets = adapter.(provider.BucketDiscoverer)
		_, capabilities.RemoteQuota = adapter.(provider.QuotaReporter)
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	health := adapter.Health(checkCtx, decrypted)
	return domain.ProviderValidation{ProviderAccountID: account.ID, Health: health, Capabilities: capabilities}, nil
}

func validateProviderLimits(settings map[string]string) error {
	for _, key := range []string{"max_object_size_bytes", "min_free_bytes", "quota_margin_bytes", "monthly_upload_quota_bytes"} {
		value := strings.TrimSpace(settings[key])
		if value == "" {
			continue
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			return fmt.Errorf("provider setting %s must be a non-negative integer", key)
		}
	}
	if value := strings.TrimSpace(settings["quota_alert_threshold_percent"]); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 100 {
			return fmt.Errorf("provider setting quota_alert_threshold_percent must be between 1 and 100")
		}
	}
	if value := strings.TrimSpace(settings["cost_per_gb_month"]); value != "" {
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || parsed < 0 {
			return fmt.Errorf("provider setting cost_per_gb_month must be a non-negative number")
		}
	}
	return nil
}

func (s *Service) recordProviderHealth(ctx context.Context, account domain.ProviderAccount, health domain.ProviderHealth) {
	degradedKey := "provider-degraded:" + account.ID
	credentialKey := "provider-credentials:" + account.ID
	if health.Status == domain.ProviderHealthHealthy {
		_ = s.Store.ClearProviderUnavailable(ctx, account.ID)
		_ = s.Store.ResolveAlert(ctx, degradedKey)
		_ = s.Store.ResolveAlert(ctx, credentialKey)
		return
	}
	status := "unavailable"
	alertType := domain.AlertTypeProviderDegraded
	alertKey := degradedKey
	if strings.Contains(strings.ToLower(health.Message), "credential") || strings.Contains(strings.ToLower(health.Message), "authentication") {
		status, alertType, alertKey = "credentials", domain.AlertTypeCredentialsInvalid, credentialKey
	}
	_ = s.Store.MarkProviderUnavailable(ctx, account.ID, status, health.Message, time.Time{})
	s.raiseAlert(ctx, domain.Alert{DedupeKey: alertKey, Type: alertType, Severity: domain.AlertSeverityCritical, ProviderAccountID: account.ID, Message: health.Message})
}

func (s *Service) recordProviderWriteFailure(ctx context.Context, account domain.ProviderAccount, cause error) {
	kind, retryAfter, ok := provider.Failure(cause)
	if !ok || kind == provider.FailurePermanent {
		return
	}
	status := string(kind)
	var until time.Time
	if kind == provider.FailureThrottled || kind == provider.FailureUnavailable {
		if retryAfter <= 0 {
			if kind == provider.FailureThrottled {
				retryAfter = time.Minute
			} else {
				retryAfter = 30 * time.Second
			}
		}
		until = time.Now().UTC().Add(retryAfter)
	}
	_ = s.Store.MarkProviderUnavailable(ctx, account.ID, status, cause.Error(), until)
	alertType, severity := domain.AlertTypeProviderDegraded, domain.AlertSeverityWarning
	switch kind {
	case provider.FailureQuota:
		alertType, severity = domain.AlertTypeQuotaExhausted, domain.AlertSeverityCritical
	case provider.FailureCredentials:
		alertType, severity = domain.AlertTypeCredentialsInvalid, domain.AlertSeverityCritical
	}
	s.raiseAlert(ctx, domain.Alert{DedupeKey: "provider-write:" + account.ID + ":" + string(kind), Type: alertType, Severity: severity, ProviderAccountID: account.ID, Message: cause.Error()})
}
