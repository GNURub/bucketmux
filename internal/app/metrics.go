package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

func (s *Service) PrometheusMetrics(ctx context.Context) string {
	var b strings.Builder
	b.WriteString("# HELP bucketmux_provider_used_bytes Indexed bytes assigned to a provider.\n")
	b.WriteString("# TYPE bucketmux_provider_used_bytes gauge\n")
	providers, _ := s.Store.ListProviders(ctx, false)
	for _, provider := range providers {
		fmt.Fprintf(&b, "bucketmux_provider_used_bytes{provider=%q,kind=%q} %d\n", provider.ID, provider.Kind, provider.UsedBytes)
	}
	b.WriteString("# HELP bucketmux_provider_capacity_bytes Configured provider capacity in bytes.\n")
	b.WriteString("# TYPE bucketmux_provider_capacity_bytes gauge\n")
	for _, provider := range providers {
		fmt.Fprintf(&b, "bucketmux_provider_capacity_bytes{provider=%q,kind=%q} %d\n", provider.ID, provider.Kind, provider.CapacityBytes)
	}
	b.WriteString("# HELP bucketmux_bucket_provider_objects Indexed objects per provider and bucket.\n")
	b.WriteString("# TYPE bucketmux_bucket_provider_objects gauge\n")
	b.WriteString("# HELP bucketmux_bucket_provider_bytes Indexed bytes per provider and bucket.\n")
	b.WriteString("# TYPE bucketmux_bucket_provider_bytes gauge\n")
	usage, _ := s.Store.ListProviderBucketUsage(ctx)
	for _, row := range usage {
		fmt.Fprintf(&b, "bucketmux_bucket_provider_objects{provider=%q,bucket=%q} %d\n", row.ProviderAccountID, row.Bucket, row.ObjectCount)
		fmt.Fprintf(&b, "bucketmux_bucket_provider_bytes{provider=%q,bucket=%q} %d\n", row.ProviderAccountID, row.Bucket, row.Bytes)
	}
	writeStatusCounts(&b, "bucketmux_migration_jobs", "Migration jobs by status.", migrationStatusCounts(ctx, s))
	writeStatusCounts(&b, "bucketmux_hook_deliveries", "Webhook deliveries by status.", hookDeliveryStatusCounts(ctx, s))
	return b.String()
}

func migrationStatusCounts(ctx context.Context, s *Service) map[string]int {
	jobs, _ := s.Store.ListMigrationJobs(ctx, 100)
	counts := map[string]int{}
	for _, job := range jobs {
		counts[job.Status]++
	}
	return counts
}

func hookDeliveryStatusCounts(ctx context.Context, s *Service) map[string]int {
	deliveries, _ := s.Store.ListHookDeliveries(ctx, 200)
	counts := map[string]int{}
	for _, delivery := range deliveries {
		counts[delivery.Status]++
	}
	return counts
}

func writeStatusCounts(b *strings.Builder, metric, help string, counts map[string]int) {
	fmt.Fprintf(b, "# HELP %s %s\n", metric, help)
	fmt.Fprintf(b, "# TYPE %s gauge\n", metric)
	statuses := make([]string, 0, len(counts))
	for status := range counts {
		statuses = append(statuses, status)
	}
	sort.Strings(statuses)
	for _, status := range statuses {
		fmt.Fprintf(b, "%s{status=%q} %d\n", metric, status, counts[status])
	}
}
