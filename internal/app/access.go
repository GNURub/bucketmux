package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

type AccessCredentialInput struct {
	ID             string
	Name           string
	Role           string
	Permissions    []string
	BucketPatterns []string
	PrefixPatterns []string
	Enabled        bool
	ExpiresAt      time.Time
}

type CreatedAccessCredential struct {
	Credential domain.AccessCredential `json:"credential"`
	SecretKey  string                  `json:"secret_key"`
}

func (s *Service) CreateAccessCredential(ctx context.Context, input AccessCredentialInput) (CreatedAccessCredential, error) {
	credential, err := normalizeAccessCredentialInput(input)
	if err != nil {
		return CreatedAccessCredential{}, err
	}
	if credential.ID == "" {
		credential.ID = randomIdentifier("cred-", 8)
	}
	credential.AccessKey = "BMUX" + strings.ToUpper(randomIdentifier("", 10))
	secret := randomSecret(32)
	credential.SecretEncrypted, err = s.Secrets.Encrypt(secret)
	if err != nil {
		return CreatedAccessCredential{}, err
	}
	if err := s.Store.UpsertAccessCredential(ctx, credential); err != nil {
		return CreatedAccessCredential{}, err
	}
	credential.SecretEncrypted = ""
	return CreatedAccessCredential{Credential: credential, SecretKey: secret}, nil
}

func (s *Service) UpdateAccessCredential(ctx context.Context, id string, input AccessCredentialInput) (domain.AccessCredential, error) {
	existing, err := s.Store.GetAccessCredential(ctx, id)
	if err != nil {
		return domain.AccessCredential{}, err
	}
	updated, err := normalizeAccessCredentialInput(input)
	if err != nil {
		return domain.AccessCredential{}, err
	}
	updated.ID = existing.ID
	updated.AccessKey = existing.AccessKey
	updated.SecretEncrypted = existing.SecretEncrypted
	updated.CreatedAt = existing.CreatedAt
	if err := s.Store.UpsertAccessCredential(ctx, updated); err != nil {
		return domain.AccessCredential{}, err
	}
	updated.SecretEncrypted = ""
	return updated, nil
}

func (s *Service) RotateAccessCredential(ctx context.Context, id string) (CreatedAccessCredential, error) {
	credential, err := s.Store.GetAccessCredential(ctx, id)
	if err != nil {
		return CreatedAccessCredential{}, err
	}
	secret := randomSecret(32)
	credential.SecretEncrypted, err = s.Secrets.Encrypt(secret)
	if err != nil {
		return CreatedAccessCredential{}, err
	}
	if err := s.Store.UpsertAccessCredential(ctx, credential); err != nil {
		return CreatedAccessCredential{}, err
	}
	credential.SecretEncrypted = ""
	return CreatedAccessCredential{Credential: credential, SecretKey: secret}, nil
}

func (s *Service) ResolveS3Credential(ctx context.Context, accessKey string) (domain.AccessCredential, error) {
	credential, err := s.Store.GetAccessCredentialByAccessKey(ctx, accessKey)
	if err != nil {
		return domain.AccessCredential{}, err
	}
	secret, err := s.Secrets.Decrypt(credential.SecretEncrypted)
	if err != nil {
		return domain.AccessCredential{}, fmt.Errorf("decrypt access credential: %w", err)
	}
	credential.SecretKey = secret
	return credential, nil
}

func normalizeAccessCredentialInput(input AccessCredentialInput) (domain.AccessCredential, error) {
	role := strings.TrimSpace(input.Role)
	if role == "" {
		role = domain.AccessRoleReadWrite
	}
	if role != domain.AccessRoleAdmin && role != domain.AccessRoleReadWrite && role != domain.AccessRoleReadOnly {
		return domain.AccessCredential{}, fmt.Errorf("role must be admin, read-write or read-only")
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return domain.AccessCredential{}, fmt.Errorf("name is required")
	}
	return domain.AccessCredential{
		ID:             strings.TrimSpace(input.ID),
		Name:           name,
		Role:           role,
		Permissions:    cleanPatterns(input.Permissions),
		BucketPatterns: cleanPatternsDefault(input.BucketPatterns),
		PrefixPatterns: cleanPatternsDefault(input.PrefixPatterns),
		Enabled:        input.Enabled,
		ExpiresAt:      input.ExpiresAt.UTC(),
	}, nil
}

func cleanPatterns(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func cleanPatternsDefault(values []string) []string {
	values = cleanPatterns(values)
	if len(values) == 0 {
		return []string{"*"}
	}
	return values
}

func randomIdentifier(prefix string, bytes int) string {
	buffer := make([]byte, bytes)
	_, _ = rand.Read(buffer)
	return prefix + hex.EncodeToString(buffer)
}

func randomSecret(bytes int) string {
	buffer := make([]byte, bytes)
	_, _ = rand.Read(buffer)
	return base64.RawURLEncoding.EncodeToString(buffer)
}
