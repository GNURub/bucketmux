package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gnurub/bucketmux/internal/config"
)

type Authenticator struct {
	accessKey string
	secretKey string
	region    string
	now       func() time.Time
}

func NewAuthenticator(cfg config.S3Config) *Authenticator {
	region := cfg.Region
	if region == "" {
		region = "auto"
	}
	return &Authenticator{accessKey: cfg.AccessKey, secretKey: cfg.SecretKey, region: region, now: time.Now}
}

func (a *Authenticator) Authorize(r *http.Request) bool {
	if r.Header.Get("X-S3LS-Access-Key") == a.accessKey && r.Header.Get("X-S3LS-Secret-Key") == a.secretKey {
		return true
	}
	if r.URL.Query().Get("X-Amz-Signature") != "" {
		return a.authorizePresignedURL(r)
	}
	return a.authorizeHeaderCredential(r)
}

func (a *Authenticator) authorizeHeaderCredential(r *http.Request) bool {
	parsed, ok := parseAuthorizationHeader(r.Header.Get("Authorization"))
	if !ok {
		return false
	}
	credentialParts := strings.Split(parsed.Credential, "/")
	if len(credentialParts) != 5 || credentialParts[0] != a.accessKey || credentialParts[3] != "s3" || credentialParts[4] != "aws4_request" {
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
	signingKey := deriveSigningKey(a.secretKey, dateStamp, region, "s3")
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

func (a *Authenticator) authorizePresignedURL(r *http.Request) bool {
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
	if len(credentialParts) != 5 || credentialParts[0] != a.accessKey || credentialParts[3] != "s3" || credentialParts[4] != "aws4_request" {
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
	signingKey := deriveSigningKey(a.secretKey, dateStamp, region, "s3")
	expected := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
	return hmac.Equal([]byte(strings.ToLower(givenSignature)), []byte(expected))
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
