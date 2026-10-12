// Package awsauth implements the AWS authentication and event framing needed by
// the Bedrock transport, without linking an AWS SDK.
package awsauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SignV4 signs req using the AWS SDK's default SigV4 HTTP signing semantics.
// payloadSHA256Hex is the caller-computed payload hash (including for empty bodies).
// The request headers, Host and RawQuery are modified in place. This is the
// non-S3 variant: the already escaped URI path is escaped a second time.
func SignV4(req *http.Request, payloadSHA256Hex string, creds Credentials, service, region string, now time.Time) error {
	if req == nil || req.URL == nil {
		return errors.New("sign AWS request: request and URL are required")
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	now = now.UTC()
	date, stamp := now.Format("20060102"), now.Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", stamp)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}
	host := signingHost(req)
	query := req.URL.Query()
	for k := range query {
		sort.Strings(query[k])
	}
	req.URL.RawQuery = strings.ReplaceAll(query.Encode(), "+", "%20")
	signedHeaders, canonicalHeaders := signingHeaders(req, host)
	canonical := strings.Join([]string{req.Method, escapeSigningPath(signingPath(req.URL)), req.URL.RawQuery, canonicalHeaders, signedHeaders, payloadSHA256Hex}, "\n")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	hash := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hex.EncodeToString(hash[:])
	key := signingHMAC([]byte("AWS4"+creds.SecretAccessKey), date)
	key = signingHMAC(key, region)
	key = signingHMAC(key, service)
	key = signingHMAC(key, "aws4_request")
	signature := hex.EncodeToString(signingHMAC(key, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+creds.AccessKeyID+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
	return nil
}

func signingHMAC(key []byte, value string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(value))
	return h.Sum(nil)
}

func signingHost(req *http.Request) string {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	scheme, port := strings.ToLower(req.URL.Scheme), signingPort(host)
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		// Match the SDK's sanitization, including unbracketed IPv6 at default ports.
		req.Host = stripSigningPort(host)
		return req.Host
	}
	return host
}

func signingPort(host string) string {
	colon := strings.IndexByte(host, ':')
	if colon < 0 {
		return ""
	}
	if i := strings.Index(host, "]:"); i >= 0 {
		return host[i+2:]
	}
	if strings.Contains(host, "]") {
		return ""
	}
	return host[colon+1:]
}

func stripSigningPort(host string) string {
	colon := strings.IndexByte(host, ':')
	if colon < 0 {
		return host
	}
	if i := strings.IndexByte(host, ']'); i >= 0 {
		return strings.TrimPrefix(host[:i], "[")
	}
	return host[:colon]
}

// The SDK's ignore list matches canonical header keys exactly, not case-folded.
var unsignedSigningHeaders = map[string]bool{
	"Authorization": true, "User-Agent": true, "X-Amzn-Trace-Id": true,
	"Expect": true, "Transfer-Encoding": true,
}

func signingHeaders(req *http.Request, host string) (string, string) {
	values := map[string][]string{"host": {host}}
	if req.ContentLength > 0 {
		values["content-length"] = []string{strconv.FormatInt(req.ContentLength, 10)}
	}
	for name, v := range req.Header {
		if unsignedSigningHeaders[name] || strings.EqualFold(name, "content-length") {
			continue
		}
		key := strings.ToLower(name)
		values[key] = append(values[key], v...)
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		b.WriteString(name)
		b.WriteByte(':')
		if name == "host" {
			b.WriteString(stripSigningSpaces(host))
		} else {
			for i, v := range values[name] {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(strings.TrimSpace(stripSigningSpaces(v)))
			}
		}
		b.WriteByte('\n')
	}
	return strings.Join(names, ";"), b.String()
}

func stripSigningSpaces(s string) string {
	s = strings.Trim(s, " ")
	var b strings.Builder
	space := false
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' && space {
			continue
		}
		b.WriteByte(s[i])
		space = s[i] == ' '
	}
	return b.String()
}

func signingPath(u *url.URL) string {
	path := u.EscapedPath()
	if u.Opaque != "" {
		opaque, _, _ := strings.Cut(u.Opaque, "?")
		opaque = strings.TrimPrefix(opaque, "//")
		path = ""
		if i := strings.IndexByte(opaque, '/'); i >= 0 {
			path = opaque[i:]
		}
	}
	if path == "" {
		return "/"
	}
	return path
}

func escapeSigningPath(path string) string {
	const digits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		c := path[i]
		if signingPathSafe(c) {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(digits[c>>4])
			b.WriteByte(digits[c&15])
		}
	}
	return b.String()
}

func signingPathSafe(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~/", rune(c))
}
