package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/domain"
)

const defaultHookTimeout = 5 * time.Second
const defaultHookMaxAttempts = 3
const hookWorkerInterval = time.Second
const hookHeartbeatInterval = 10 * time.Second
const hookWorkStaleAfter = 30 * time.Second

type HookPayload struct {
	Event             string    `json:"event"`
	Bucket            string    `json:"bucket"`
	Key               string    `json:"key"`
	ProviderAccountID string    `json:"providerAccountId"`
	RemoteBucket      string    `json:"remoteBucket"`
	RemoteKey         string    `json:"remoteKey"`
	Size              int64     `json:"size"`
	ContentType       string    `json:"contentType"`
	ETag              string    `json:"etag"`
	ChecksumSHA256    string    `json:"checksumSHA256"`
	ObjectUpdatedAt   time.Time `json:"objectUpdatedAt"`
	ReplicaStatus     string    `json:"replicaStatus"`
	Timestamp         time.Time `json:"timestamp"`
}

func (s *Service) UpsertHookFromAdmin(ctx context.Context, hook domain.Hook) error {
	hook.ID = strings.TrimSpace(hook.ID)
	hook.Name = strings.TrimSpace(hook.Name)
	hook.URL = strings.TrimSpace(hook.URL)
	hook.Method = strings.ToUpper(strings.TrimSpace(hook.Method))
	hook.Headers = normalizeHookHeaders(hook.Headers)
	if err := validateHook(hook); err != nil {
		return err
	}
	if hook.Name == "" {
		hook.Name = hook.ID
	}
	if hook.Kind == "" {
		hook.Kind = domain.HookKindHTTP
	}
	if hook.Method == "" {
		hook.Method = http.MethodPost
	}
	if len(hook.Events) == 0 {
		hook.Events = []string{domain.HookEventObjectCreated}
	}
	if len(hook.Headers) > 0 {
		headersJSON, err := json.Marshal(hook.Headers)
		if err != nil {
			return fmt.Errorf("encode hook headers: %w", err)
		}
		encrypted, err := s.Secrets.Encrypt(string(headersJSON))
		if err != nil {
			return fmt.Errorf("encrypt hook headers: %w", err)
		}
		hook.HeadersEncrypted = encrypted
	} else if existing, err := s.Store.GetHook(ctx, hook.ID); err == nil {
		hook.HeadersEncrypted = existing.HeadersEncrypted
	}
	return s.Store.UpsertHook(ctx, hook)
}

func validateHook(hook domain.Hook) error {
	if strings.TrimSpace(hook.ID) == "" {
		return fmt.Errorf("hook id is required")
	}
	if hook.Kind != "" && hook.Kind != domain.HookKindHTTP {
		return fmt.Errorf("only http hooks are supported")
	}
	parsed, err := url.Parse(strings.TrimSpace(hook.URL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("hook url must be an absolute http or https URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("hook url scheme must be http or https")
	}
	method := strings.ToUpper(strings.TrimSpace(hook.Method))
	if method == "" {
		method = http.MethodPost
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return fmt.Errorf("hook method must be GET, POST, PUT or PATCH")
	}
	for _, event := range hook.Events {
		switch event {
		case domain.HookEventObjectCreated, domain.HookEventObjectDeleted:
		default:
			return fmt.Errorf("unsupported hook event %q", event)
		}
	}
	for name := range hook.Headers {
		if !isValidHookHeaderName(name) {
			return fmt.Errorf("invalid hook header name %q", name)
		}
	}
	return nil
}

func (s *Service) dispatchObjectHook(ctx context.Context, event string, obj domain.ObjectRecord) {
	hooks, err := s.Store.ListHooks(ctx, true)
	if err != nil || len(hooks) == 0 {
		return
	}
	if event == domain.HookEventObjectCreated {
		// Store.PutObject owns the authoritative updated_at. External workers use
		// this value as an optimistic generation token when importing results.
		persisted, err := s.Store.GetObject(ctx, obj.Bucket, obj.Key)
		if err != nil {
			return
		}
		obj = persisted
	}
	payload := HookPayload{
		Event:             event,
		Bucket:            obj.Bucket,
		Key:               obj.Key,
		ProviderAccountID: obj.ProviderAccountID,
		RemoteBucket:      obj.RemoteBucket,
		RemoteKey:         obj.RemoteKey,
		Size:              obj.Size,
		ContentType:       obj.ContentType,
		ETag:              obj.ETag,
		ChecksumSHA256:    obj.ChecksumSHA256,
		ObjectUpdatedAt:   obj.UpdatedAt,
		ReplicaStatus:     obj.ReplicaStatus,
		Timestamp:         time.Now().UTC(),
	}
	notifications, _ := s.Store.ListBucketNotifications(ctx, obj.Bucket)
	for _, hook := range hooks {
		if hook.Kind != domain.HookKindHTTP || !hookMatchesEvent(hook, event) {
			continue
		}
		matching := matchingBucketNotifications(notifications, hook.ID, event, obj.Key)
		configuredForBucket := false
		for _, notification := range notifications {
			if notification.HookID == hook.ID {
				configuredForBucket = true
				break
			}
		}
		if configuredForBucket && len(matching) == 0 {
			continue
		}
		payloadJSON, err := json.Marshal(payload)
		if len(matching) > 0 {
			payloadJSON, err = json.Marshal(s3NotificationPayload(matching[0], event, obj))
		}
		if err != nil {
			continue
		}
		deliveryID, err := randomHookDeliveryID()
		if err != nil {
			continue
		}
		delivery := domain.HookDelivery{
			ID:            deliveryID,
			HookID:        hook.ID,
			Event:         event,
			Bucket:        obj.Bucket,
			Key:           obj.Key,
			PayloadJSON:   string(payloadJSON),
			Status:        domain.HookDeliveryStatusPending,
			MaxAttempts:   defaultHookMaxAttempts,
			NextAttemptAt: time.Now().UTC(),
		}
		if err := s.Store.CreateHookDelivery(ctx, delivery); err != nil {
			continue
		}
		signalWorker(s.hookWorkerWake)
	}
}

func matchingBucketNotifications(notifications []domain.BucketNotification, hookID, event, key string) []domain.BucketNotification {
	var result []domain.BucketNotification
	for _, notification := range notifications {
		if notification.HookID != hookID || notification.Event != event || !strings.HasPrefix(key, notification.Prefix) || !strings.HasSuffix(key, notification.Suffix) {
			continue
		}
		result = append(result, notification)
	}
	return result
}

func s3NotificationPayload(notification domain.BucketNotification, event string, object domain.ObjectRecord) map[string]any {
	eventName := "ObjectCreated:Put"
	if event == domain.HookEventObjectDeleted {
		eventName = "ObjectRemoved:Delete"
	}
	return map[string]any{"Records": []any{map[string]any{
		"eventVersion": "2.1",
		"eventSource":  "aws:s3",
		"awsRegion":    "auto",
		"eventTime":    time.Now().UTC().Format(time.RFC3339Nano),
		"eventName":    eventName,
		"userIdentity": map[string]any{"principalId": "bucketmux"},
		"s3": map[string]any{
			"s3SchemaVersion": "1.0",
			"configurationId": notification.ID,
			"bucket":          map[string]any{"name": object.Bucket, "arn": "arn:aws:s3:::" + object.Bucket},
			"object":          map[string]any{"key": url.QueryEscape(object.Key), "size": object.Size, "eTag": strings.Trim(object.ETag, `"`), "sequencer": strings.ToUpper(randomIdentifier("", 8))},
		},
	}}}
}

func hookMatchesEvent(hook domain.Hook, event string) bool {
	return len(hook.Events) == 0 || slices.Contains(hook.Events, event)
}

func (s *Service) deliverHookDelivery(ctx context.Context, delivery domain.HookDelivery) error {
	if delivery.Status != domain.HookDeliveryStatusRunning {
		return fmt.Errorf("hook delivery %s is not claimed", delivery.ID)
	}
	hook, err := s.Store.GetHook(ctx, delivery.HookID)
	if err != nil || !hook.Enabled || hook.Kind != domain.HookKindHTTP {
		delivery.Status = domain.HookDeliveryStatusFailed
		delivery.LastError = "hook not found, disabled or not http"
		return s.Store.UpdateHookDelivery(ctx, delivery)
	}
	statusCode, errText := s.deliverHTTPHook(ctx, delivery.ID, hook, []byte(delivery.PayloadJSON))
	delivery.Attempts++
	delivery.LastStatusCode = statusCode
	delivery.LastError = errText
	if errText == "" && statusCode >= 200 && statusCode < 300 {
		delivery.Status = domain.HookDeliveryStatusSucceeded
		delivery.NextAttemptAt = time.Now().UTC()
		return s.Store.UpdateHookDelivery(ctx, delivery)
	}
	if delivery.Attempts >= delivery.MaxAttempts {
		delivery.Status = domain.HookDeliveryStatusFailed
		delivery.NextAttemptAt = time.Now().UTC()
		return s.Store.UpdateHookDelivery(ctx, delivery)
	}
	delivery.Status = domain.HookDeliveryStatusPending
	delay := s.hookRetryDelay(delivery.Attempts)
	delivery.NextAttemptAt = time.Now().UTC().Add(delay)
	return s.Store.UpdateHookDelivery(ctx, delivery)
}

func (s *Service) deliverHTTPHook(parent context.Context, deliveryID string, hook domain.Hook, body []byte) (int, string) {
	client := s.HookHTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultHookTimeout}
	}
	headers, err := s.decryptHookHeaders(hook)
	if err != nil {
		return 0, err.Error()
	}
	method := strings.ToUpper(strings.TrimSpace(hook.Method))
	if method == "" {
		method = http.MethodPost
	}
	var reader *bytes.Reader
	if method == http.MethodGet {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}
	ctx, cancel := context.WithTimeout(parent, defaultHookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, hook.URL, reader)
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("User-Agent", "BucketMux-Hook/1")
	req.Header.Set("X-BucketMux-Delivery-ID", deliveryID)
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	var payload HookPayload
	if err := json.Unmarshal(body, &payload); err == nil && payload.Event != "" {
		req.Header.Set("X-BucketMux-Event", payload.Event)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 512))
	_ = res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return res.StatusCode, fmt.Sprintf("hook returned HTTP %d", res.StatusCode)
	}
	return res.StatusCode, ""
}

func (s *Service) hookRetryDelay(attempts int) time.Duration {
	if s.HookRetryDelay != nil {
		return s.HookRetryDelay(attempts)
	}
	if attempts < 1 {
		attempts = 1
	}
	delay := time.Duration(1<<min(attempts-1, 5)) * time.Second
	if delay > time.Minute {
		return time.Minute
	}
	return delay
}

func (s *Service) decryptHookHeaders(hook domain.Hook) (map[string]string, error) {
	if len(hook.Headers) > 0 {
		return normalizeHookHeaders(hook.Headers), nil
	}
	if hook.HeadersEncrypted == "" {
		return nil, nil
	}
	plain, err := s.Secrets.Decrypt(hook.HeadersEncrypted)
	if err != nil {
		return nil, fmt.Errorf("decrypt hook headers: %w", err)
	}
	headers := map[string]string{}
	if err := json.Unmarshal([]byte(plain), &headers); err != nil {
		return nil, fmt.Errorf("decode hook headers: %w", err)
	}
	return normalizeHookHeaders(headers), nil
}

func (s *Service) ListHooksForAdmin(ctx context.Context) ([]domain.Hook, error) {
	hooks, err := s.Store.ListHooks(ctx, false)
	if err != nil {
		return nil, err
	}
	for i := range hooks {
		headerNames, _ := s.hookHeaderNames(hooks[i])
		hooks[i].HeaderNames = headerNames
		hooks[i].Headers = nil
		hooks[i].HeadersEncrypted = ""
	}
	return hooks, nil
}

func (s *Service) StartHookDeliveryWorker(ctx context.Context) {
	s.runDurableWorker(ctx, durableWorker{
		name:              "hooks",
		interval:          hookWorkerInterval,
		heartbeatInterval: hookHeartbeatInterval,
		staleAfter:        hookWorkStaleAfter,
		wake:              s.hookWorkerWake,
		recover: func(ctx context.Context, cutoff time.Time) error {
			_, err := s.Store.RecoverStaleHookDeliveries(ctx, cutoff)
			return err
		},
		claim: func(ctx context.Context) (durableWorkItem, bool, error) {
			delivery, claimed, err := s.Store.ClaimNextHookDelivery(ctx, time.Now().UTC())
			if err != nil || !claimed {
				return durableWorkItem{}, claimed, err
			}
			return durableWorkItem{
				run: func(ctx context.Context) error {
					return s.deliverHookDelivery(ctx, delivery)
				},
				heartbeat: func(ctx context.Context) error {
					return s.Store.TouchHookDelivery(ctx, delivery.ID)
				},
			}, true, nil
		},
	})
}

func (s *Service) ProcessPendingHookDeliveries(ctx context.Context, limit int) error {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	for range limit {
		delivery, claimed, err := s.Store.ClaimNextHookDelivery(ctx, time.Now().UTC())
		if err != nil {
			return err
		}
		if !claimed {
			return nil
		}
		if err := s.deliverHookDelivery(ctx, delivery); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) hookHeaderNames(hook domain.Hook) ([]string, error) {
	headers, err := s.decryptHookHeaders(hook)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func normalizeHookHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := map[string]string{}
	for name, value := range headers {
		name = http.CanonicalHeaderKey(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		if name == "" || value == "" {
			continue
		}
		out[name] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func isValidHookHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, ch := range name {
		if ch >= '0' && ch <= '9' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || strings.ContainsRune("!#$%&'*+-.^_`|~", ch) {
			continue
		}
		return false
	}
	return true
}

func randomHookDeliveryID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate hook delivery id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
