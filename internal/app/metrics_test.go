package app

import (
	"context"
	"strings"
	"testing"

	"github.com/gnurub/bucketmux/internal/domain"
)

func TestPrometheusMetricsExposeProviderUsage(t *testing.T) {
	svc, cleanup := newMigrationTestService(t)
	defer cleanup()

	mustPutMigrationObject(t, svc, "metrics/demo.txt", "hello")
	metrics := svc.PrometheusMetrics(context.Background())
	for _, want := range []string{
		"bucketmux_provider_used_bytes",
		`provider="local-source"`,
		"bucketmux_provider_capacity_bytes",
		"bucketmux_bucket_provider_objects",
		"bucketmux_migration_jobs",
		"bucketmux_hook_deliveries",
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("metrics missing %q:\n%s", want, metrics)
		}
	}

	_, err := svc.CreateMigrationJob(context.Background(), CreateMigrationJobInput{Bucket: "images", SourceProviderID: "local-source", TargetProviderID: "local-target", Mode: domain.MigrationModeCopy})
	if err != nil {
		t.Fatalf("CreateMigrationJob() error = %v", err)
	}
	metrics = svc.PrometheusMetrics(context.Background())
	if !strings.Contains(metrics, `bucketmux_migration_jobs{status="pending"} 1`) {
		t.Fatalf("metrics missing pending migration count:\n%s", metrics)
	}
}
