package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/domain"
)

type CredentialResolver interface {
	ResolveS3Credential(context.Context, string) (domain.AccessCredential, error)
}

type Principal struct {
	ID             string
	AccessKey      string
	Role           string
	Permissions    []string
	BucketPatterns []string
	PrefixPatterns []string
}

type postPolicyDocument struct {
	Expiration time.Time `json:"expiration"`
	Conditions []any     `json:"conditions"`
}

func (p Principal) Allows(permission, bucket, key string) bool {
	return principalAllows(p, permission, bucket, key)
}

type Authenticator struct {
	accessKey string
	secretKey string
	region    string
	resolver  CredentialResolver
	now       func() time.Time
}

func NewAuthenticator(cfg config.S3Config) *Authenticator {
	region := cfg.Region
	if region == "" {
		region = "auto"
	}
	return &Authenticator{accessKey: cfg.AccessKey, secretKey: cfg.SecretKey, region: region, now: time.Now}
}

func NewAuthenticatorWithResolver(cfg config.S3Config, resolver CredentialResolver) *Authenticator {
	auth := NewAuthenticator(cfg)
	auth.resolver = resolver
	return auth
}

func (a *Authenticator) Authorize(r *http.Request) bool {
	_, ok := a.Authenticate(r)
	return ok
}

func (a *Authenticator) Authenticate(r *http.Request) (Principal, bool) {
	accessKey := requestAccessKey(r)
	credential, ok := a.resolveCredential(r.Context(), accessKey)
	if !ok {
		return Principal{}, false
	}
	if customSecret := r.Header.Get("X-S3LS-Secret-Key"); customSecret != "" {
		if !hmac.Equal([]byte(customSecret), []byte(credential.SecretKey)) {
			return Principal{}, false
		}
		return principalFromCredential(credential), true
	}
	if r.URL.Query().Get("X-Amz-Signature") != "" {
		if !a.authorizePresignedURL(r, credential) {
			return Principal{}, false
		}
		return principalFromCredential(credential), true
	}
	if !a.authorizeHeaderCredential(r, credential) {
		return Principal{}, false
	}
	return principalFromCredential(credential), true
}

func (a *Authenticator) AuthorizeAction(r *http.Request, permission, bucket, key string) (Principal, bool) {
	principal, ok := a.Authenticate(r)
	return principal, ok && principalAllows(principal, permission, bucket, key)
}

func (a *Authenticator) AuthorizePostPolicy(ctx context.Context, fields map[string]string, contentLength int64) (Principal, error) {
	credentialParts := strings.Split(fields["x-amz-credential"], "/")
	if fields["x-amz-algorithm"] != "AWS4-HMAC-SHA256" || len(credentialParts) != 5 || credentialParts[3] != "s3" || credentialParts[4] != "aws4_request" {
		return Principal{}, fmt.Errorf("invalid POST policy credential scope")
	}
	credential, ok := a.resolveCredential(ctx, credentialParts[0])
	if !ok {
		return Principal{}, fmt.Errorf("unknown or disabled access key")
	}
	encodedPolicy := fields["policy"]
	decodedPolicy, err := base64.StdEncoding.DecodeString(encodedPolicy)
	if err != nil {
		return Principal{}, fmt.Errorf("decode POST policy: %w", err)
	}
	var policy postPolicyDocument
	if err := json.Unmarshal(decodedPolicy, &policy); err != nil {
		return Principal{}, fmt.Errorf("decode POST policy JSON: %w", err)
	}
	if policy.Expiration.IsZero() || a.now().UTC().After(policy.Expiration.UTC()) {
		return Principal{}, fmt.Errorf("POST policy has expired")
	}
	dateStamp := credentialParts[1]
	region := credentialParts[2]
	signedAt, err := time.Parse("20060102T150405Z", fields["x-amz-date"])
	if err != nil || signedAt.Format("20060102") != dateStamp || a.now().UTC().Before(signedAt.Add(-15*time.Minute)) || a.now().UTC().After(policy.Expiration.UTC()) {
		return Principal{}, fmt.Errorf("invalid POST policy signing date")
	}
	signingKey := deriveSigningKey(credential.SecretKey, dateStamp, region, "s3")
	expected := hex.EncodeToString(hmacSHA256(signingKey, encodedPolicy))
	if !hmac.Equal([]byte(strings.ToLower(fields["x-amz-signature"])), []byte(expected)) {
		return Principal{}, fmt.Errorf("invalid POST policy signature")
	}
	if err := validatePostPolicyConditions(policy.Conditions, fields, contentLength); err != nil {
		return Principal{}, err
	}
	principal := principalFromCredential(credential)
	if !principal.Allows("s3:PutObject", fields["bucket"], fields["key"]) {
		return Principal{}, fmt.Errorf("credential cannot upload this object")
	}
	return principal, nil
}

func validatePostPolicyConditions(conditions []any, fields map[string]string, contentLength int64) error {
	for _, raw := range conditions {
		switch condition := raw.(type) {
		case map[string]any:
			for key, expected := range condition {
				if fields[strings.ToLower(key)] != fmt.Sprint(expected) {
					return fmt.Errorf("POST policy condition for %s was not met", key)
				}
			}
		case []any:
			if len(condition) < 3 {
				return fmt.Errorf("invalid POST policy condition")
			}
			operator := fmt.Sprint(condition[0])
			switch operator {
			case "starts-with", "eq":
				field := strings.TrimPrefix(strings.ToLower(fmt.Sprint(condition[1])), "$")
				expected := fmt.Sprint(condition[2])
				actual := fields[field]
				if (operator == "starts-with" && !strings.HasPrefix(actual, expected)) || (operator == "eq" && actual != expected) {
					return fmt.Errorf("POST policy %s condition for %s was not met", operator, field)
				}
			case "content-length-range":
				min, minOK := jsonNumberInt64(condition[1])
				max, maxOK := jsonNumberInt64(condition[2])
				if !minOK || !maxOK || contentLength < min || contentLength > max {
					return fmt.Errorf("POST upload size is outside the policy range")
				}
			default:
				return fmt.Errorf("unsupported POST policy condition %q", operator)
			}
		default:
			return fmt.Errorf("invalid POST policy condition")
		}
	}
	return nil
}

func jsonNumberInt64(value any) (int64, bool) {
	switch number := value.(type) {
	case float64:
		return int64(number), number == float64(int64(number))
	case json.Number:
		parsed, err := number.Int64()
		return parsed, err == nil
	default:
		parsed, err := strconv.ParseInt(fmt.Sprint(value), 10, 64)
		return parsed, err == nil
	}
}

func (a *Authenticator) authorizeHeaderCredential(r *http.Request, credential domain.AccessCredential) bool {
	parsed, ok := parseAuthorizationHeader(r.Header.Get("Authorization"))
	if !ok {
		return false
	}
	credentialParts := strings.Split(parsed.Credential, "/")
	if len(credentialParts) != 5 || credentialParts[0] != credential.AccessKey || credentialParts[3] != "s3" || credentialParts[4] != "aws4_request" {
		return false
	}
	dateStamp := credentialParts[1]
	region := credentialParts[2]
	if region == "" {
		region = a.region
	}
	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		amzDate = r.Header.Get("Date")
	}
	signedAt, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil || signedAt.Format("20060102") != dateStamp {
		return false
	}
	now := a.now().UTC()
	if now.Before(signedAt.Add(-15*time.Minute)) || now.After(signedAt.Add(15*time.Minute)) {
		return false
	}
	canonicalRequest, ok := a.headerCanonicalRequest(r, parsed.SignedHeaders)
	if !ok {
		return false
	}
	scope := strings.Join(credentialParts[1:], "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hexSHA256(canonicalRequest),
	}, "\n")
	signingKey := deriveSigningKey(credential.SecretKey, dateStamp, region, "s3")
	expected := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
	return hmac.Equal([]byte(strings.ToLower(parsed.Signature)), []byte(expected))
}

func (a *Authenticator) headerCanonicalRequest(r *http.Request, signedHeaders string) (string, bool) {
	canonicalHeaders, ok := canonicalHeadersForSignedList(r, signedHeaders)
	if !ok {
		return "", false
	}
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		payloadHash = emptyPayloadHash
	}
	return strings.Join([]string{
		r.Method,
		canonicalURI(r.URL.EscapedPath()),
		canonicalQueryString(r.URL.Query()),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n"), true
}

type authorizationHeader struct {
	Credential    string
	SignedHeaders string
	Signature     string
}

func parseAuthorizationHeader(header string) (authorizationHeader, bool) {
	const prefix = "AWS4-HMAC-SHA256 "
	if !strings.HasPrefix(header, prefix) {
		return authorizationHeader{}, false
	}
	fields := strings.Split(strings.TrimPrefix(header, prefix), ",")
	parsed := map[string]string{}
	for _, field := range fields {
		key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok || key == "" || value == "" {
			return authorizationHeader{}, false
		}
		parsed[key] = value
	}
	out := authorizationHeader{Credential: parsed["Credential"], SignedHeaders: parsed["SignedHeaders"], Signature: parsed["Signature"]}
	if out.Credential == "" || out.SignedHeaders == "" || out.Signature == "" {
		return authorizationHeader{}, false
	}
	return out, true
}

func (a *Authenticator) authorizePresignedURL(r *http.Request, resolved domain.AccessCredential) bool {
	query := r.URL.Query()
	algorithm := query.Get("X-Amz-Algorithm")
	credential := query.Get("X-Amz-Credential")
	amzDate := query.Get("X-Amz-Date")
	expiresRaw := query.Get("X-Amz-Expires")
	signedHeaders := query.Get("X-Amz-SignedHeaders")
	givenSignature := query.Get("X-Amz-Signature")
	if algorithm != "AWS4-HMAC-SHA256" || credential == "" || amzDate == "" || expiresRaw == "" || signedHeaders == "" || givenSignature == "" {
		return false
	}
	credentialParts := strings.Split(credential, "/")
	if len(credentialParts) != 5 || credentialParts[0] != resolved.AccessKey || credentialParts[3] != "s3" || credentialParts[4] != "aws4_request" {
		return false
	}
	dateStamp := credentialParts[1]
	region := credentialParts[2]
	if region == "" {
		region = a.region
	}
	expires, err := strconv.Atoi(expiresRaw)
	if err != nil || expires < 1 || expires > 604800 {
		return false
	}
	signedAt, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil || signedAt.Format("20060102") != dateStamp {
		return false
	}
	now := a.now().UTC()
	if now.Before(signedAt.Add(-15*time.Minute)) || now.After(signedAt.Add(time.Duration(expires)*time.Second)) {
		return false
	}
	canonicalRequest, ok := a.presignedCanonicalRequest(r, query, signedHeaders)
	if !ok {
		return false
	}
	scope := strings.Join(credentialParts[1:], "/")
	stringToSign := strings.Join([]string{
		algorithm,
		amzDate,
		scope,
		hexSHA256(canonicalRequest),
	}, "\n")
	signingKey := deriveSigningKey(resolved.SecretKey, dateStamp, region, "s3")
	expected := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
	return hmac.Equal([]byte(strings.ToLower(givenSignature)), []byte(expected))
}

func requestAccessKey(r *http.Request) string {
	if accessKey := strings.TrimSpace(r.Header.Get("X-S3LS-Access-Key")); accessKey != "" {
		return accessKey
	}
	if credential := r.URL.Query().Get("X-Amz-Credential"); credential != "" {
		return strings.SplitN(credential, "/", 2)[0]
	}
	parsed, ok := parseAuthorizationHeader(r.Header.Get("Authorization"))
	if !ok {
		return ""
	}
	return strings.SplitN(parsed.Credential, "/", 2)[0]
}

func (a *Authenticator) resolveCredential(ctx context.Context, accessKey string) (domain.AccessCredential, bool) {
	if accessKey == "" {
		return domain.AccessCredential{}, false
	}
	if hmac.Equal([]byte(accessKey), []byte(a.accessKey)) {
		return domain.AccessCredential{ID: "config-root", AccessKey: a.accessKey, SecretKey: a.secretKey, Role: domain.AccessRoleAdmin, Enabled: true, BucketPatterns: []string{"*"}, PrefixPatterns: []string{"*"}}, true
	}
	if a.resolver == nil {
		return domain.AccessCredential{}, false
	}
	credential, err := a.resolver.ResolveS3Credential(ctx, accessKey)
	if err != nil || !credential.Enabled || credential.SecretKey == "" || (!credential.ExpiresAt.IsZero() && time.Now().UTC().After(credential.ExpiresAt)) {
		return domain.AccessCredential{}, false
	}
	return credential, true
}

func principalFromCredential(credential domain.AccessCredential) Principal {
	return Principal{ID: credential.ID, AccessKey: credential.AccessKey, Role: credential.Role, Permissions: credential.Permissions, BucketPatterns: credential.BucketPatterns, PrefixPatterns: credential.PrefixPatterns}
}

func principalAllows(principal Principal, permission, bucket, key string) bool {
	if principal.Role == domain.AccessRoleAdmin {
		return true
	}
	if principal.Role == domain.AccessRoleReadOnly && !strings.HasPrefix(permission, "s3:Get") && permission != "s3:ListBucket" && permission != "s3:ListAllMyBuckets" {
		return false
	}
	if len(principal.Permissions) > 0 && !matchesAny(principal.Permissions, permission) {
		return false
	}
	if bucket != "" && !matchesAny(defaultPatterns(principal.BucketPatterns), bucket) {
		return false
	}
	if key != "" && !matchesAny(defaultPatterns(principal.PrefixPatterns), key) {
		return false
	}
	return true
}

func defaultPatterns(patterns []string) []string {
	if len(patterns) == 0 {
		return []string{"*"}
	}
	return patterns
}

func matchesAny(patterns []string, value string) bool {
	for _, pattern := range patterns {
		if pattern == "*" || pattern == value {
			return true
		}
		if matched, err := path.Match(pattern, value); err == nil && matched {
			return true
		}
		if strings.HasSuffix(pattern, "*") && strings.HasPrefix(value, strings.TrimSuffix(pattern, "*")) {
			return true
		}
	}
	return false
}

func (a *Authenticator) presignedCanonicalRequest(r *http.Request, query url.Values, signedHeaders string) (string, bool) {
	canonicalQuery := cloneQueryWithoutSignature(query)
	canonicalHeaders, ok := canonicalHeadersForSignedList(r, signedHeaders)
	if !ok {
		return "", false
	}
	return strings.Join([]string{
		r.Method,
		canonicalURI(r.URL.EscapedPath()),
		canonicalQueryString(canonicalQuery),
		canonicalHeaders,
		signedHeaders,
		"UNSIGNED-PAYLOAD",
	}, "\n"), true
}

func cloneQueryWithoutSignature(query url.Values) url.Values {
	cloned := url.Values{}
	for key, values := range query {
		if key == "X-Amz-Signature" {
			continue
		}
		for _, value := range values {
			cloned.Add(key, value)
		}
	}
	return cloned
}

func canonicalHeadersForSignedList(r *http.Request, signedHeaders string) (string, bool) {
	parts := strings.Split(signedHeaders, ";")
	if len(parts) == 0 {
		return "", false
	}
	for i := 1; i < len(parts); i++ {
		if parts[i-1] > parts[i] {
			return "", false
		}
	}
	values := map[string]string{"host": r.Host}
	for name, headers := range r.Header {
		lower := strings.ToLower(name)
		cleaned := make([]string, 0, len(headers))
		for _, header := range headers {
			cleaned = append(cleaned, strings.Join(strings.Fields(header), " "))
		}
		values[lower] = strings.Join(cleaned, ",")
	}
	var builder strings.Builder
	for _, part := range parts {
		if part == "" {
			return "", false
		}
		value, ok := values[part]
		if !ok {
			return "", false
		}
		builder.WriteString(part)
		builder.WriteByte(':')
		builder.WriteString(value)
		builder.WriteByte('\n')
	}
	return builder.String(), true
}

func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

func canonicalQueryString(values url.Values) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0)
	for _, key := range keys {
		vals := append([]string(nil), values[key]...)
		sort.Strings(vals)
		for _, value := range vals {
			parts = append(parts, awsQueryEscape(key)+"="+awsQueryEscape(value))
		}
	}
	return strings.Join(parts, "&")
}

func awsQueryEscape(value string) string {
	escaped := url.QueryEscape(value)
	escaped = strings.ReplaceAll(escaped, "+", "%20")
	escaped = strings.ReplaceAll(escaped, "%7E", "~")
	return escaped
}

const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func hexSHA256(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func deriveSigningKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}
