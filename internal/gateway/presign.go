package gateway

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (a *Authenticator) PresignURL(method string, target url.URL, expires time.Duration) (string, bool) {
	if expires <= 0 || expires > 7*24*time.Hour {
		expires = 15 * time.Minute
	}
	now := a.now().UTC()
	dateStamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")
	query := target.Query()
	query.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	query.Set("X-Amz-Credential", a.accessKey+"/"+dateStamp+"/"+a.region+"/s3/aws4_request")
	query.Set("X-Amz-Date", amzDate)
	query.Set("X-Amz-Expires", strconv.Itoa(int(expires.Seconds())))
	query.Set("X-Amz-SignedHeaders", "host")
	target.RawQuery = query.Encode()
	req, err := http.NewRequest(method, target.String(), nil)
	if err != nil {
		return "", false
	}
	canonicalRequest, ok := a.presignedCanonicalRequest(req, req.URL.Query(), "host")
	if !ok {
		return "", false
	}
	scope := dateStamp + "/" + a.region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hexSHA256(canonicalRequest)}, "\n")
	signature := hexEncodeHMAC(deriveSigningKey(a.secretKey, dateStamp, a.region, "s3"), stringToSign)
	query = target.Query()
	query.Set("X-Amz-Signature", signature)
	target.RawQuery = query.Encode()
	return target.String(), true
}

func hexEncodeHMAC(key []byte, data string) string {
	return strings.ToLower(bytesToHex(hmacSHA256(key, data)))
}

func bytesToHex(bytes []byte) string {
	const hextable = "0123456789abcdef"
	out := make([]byte, len(bytes)*2)
	for i, b := range bytes {
		out[i*2] = hextable[b>>4]
		out[i*2+1] = hextable[b&0x0f]
	}
	return string(out)
}
