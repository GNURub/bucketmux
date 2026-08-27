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
		"bucketmux_worker_failures_total",
		`worker="hooks"`,
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("metrics missing %q:\n%s", want, metrics)
		}
	}
	if got := strings.Count(metrics, `bucketmux_worker_failures_total{worker="hooks"}`); got != 1 {
		t.Fatalf("hook worker failure metric series count = %d, want 1", got)
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
