package app

import (
	"context"
	"errors"
	"fmt"
	"os"
)

type ReadinessReport struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func (s *Service) Readiness(ctx context.Context) (ReadinessReport, error) {
	report := ReadinessReport{Status: "ready", Checks: map[string]string{}}
	var failures []error

	if err := s.Store.Ping(ctx); err != nil {
		report.Checks["store"] = err.Error()
		failures = append(failures, fmt.Errorf("store: %w", err))
	} else {
		report.Checks["store"] = "ok"
	}
	if err := s.Coordinator.Ping(ctx); err != nil {
		report.Checks["coordination"] = err.Error()
		failures = append(failures, fmt.Errorf("coordination: %w", err))
	} else {
		report.Checks["coordination"] = "ok"
	}
	providers, err := s.Store.ListProviders(ctx, true)
	if err != nil {
		report.Checks["providers"] = err.Error()
		failures = append(failures, fmt.Errorf("providers: %w", err))
	} else if len(providers) == 0 {
		// Admin must remain reachable so the first provider can be configured.
		report.Checks["providers"] = "0 enabled (configuration required)"
	} else {
		report.Checks["providers"] = fmt.Sprintf("%d enabled", len(providers))
	}
	if err := writableDirectory(s.Config.Server.MultipartStagingDir); err != nil {
		report.Checks["multipart_staging"] = err.Error()
		failures = append(failures, fmt.Errorf("multipart staging: %w", err))
	} else {
		report.Checks["multipart_staging"] = "ok"
	}

	if len(failures) > 0 {
		report.Status = "not_ready"
		return report, errors.Join(failures...)
	}
	return report, nil
}

func writableDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("path is not a directory")
	}
	file, err := os.CreateTemp(path, ".bucketmux-readiness-*")
	if err != nil {
		return fmt.Errorf("directory is not writable: %w", err)
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("close readiness probe: %w", err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("remove readiness probe: %w", err)
	}
	return nil
}
