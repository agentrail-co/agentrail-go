// Package handoff implements the authenticated Agentrail trace handoff protocol.
package handoff

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.opentelemetry.io/otel/trace"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// Refusal errors contain no request, identity, token, or key values.
var (
	ErrInput            = errors.New("handoff: invalid signing input")
	ErrMissing          = errors.New("handoff: missing token")
	ErrMalformed        = errors.New("handoff: malformed input")
	ErrKeyMismatch      = errors.New("handoff: key mismatch")
	ErrContextMismatch  = errors.New("handoff: context mismatch")
	ErrExpired          = errors.New("handoff: expired token")
	ErrFuture           = errors.New("handoff: future token")
	ErrSignatureInvalid = errors.New("handoff: invalid signature")
)

const maxSafeInteger int64 = 9007199254740991
const maxAge int64 = 60000

// KeyID is the first 16 lowercase hex characters of the key's SHA-256.
func KeyID(key string) string {
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:8])
}

// Hash is HMAC-SHA256, truncated to 32 lowercase hex characters.
func Hash(key, value string) string {
	digest := mac(key, []byte(value))
	return hex.EncodeToString(digest[:16])
}

// Sign binds a trace context to the exact method and origin-form wire target.
// issuedAt is an integer Unix timestamp in milliseconds, representable in JS.
func Sign(key, traceparent, method, target string, issuedAt int64) (string, error) {
	if !utf8.ValidString(key) || !validRequest(method, target) || issuedAt < 0 || issuedAt > maxSafeInteger || traceparent != trimJS(traceparent) {
		return "", ErrInput
	}
	if _, ok := ParseTraceparent(traceparent); !ok {
		return "", ErrInput
	}
	id := KeyID(key)
	signature := mac(key, payload(id, issuedAt, traceparent, method, target))
	return "ah1." + id + "." + strconv.FormatInt(issuedAt, 10) + "." + traceparent + "." + hex.EncodeToString(signature), nil
}

// Verify authenticates the token and returns its remote parent only on success.
// now is an integer Unix timestamp in milliseconds. No clock skew is accepted.
func Verify(key, token, traceparent, method, target string, now int64) (trace.SpanContext, error) {
	empty := trace.SpanContext{}
	if token == "" {
		return empty, ErrMissing
	}
	if len(token) > 256 || !utf8.ValidString(key) || !validRequest(method, target) || now < -maxSafeInteger || now > maxSafeInteger {
		return empty, ErrMalformed
	}
	parts := strings.Split(token, ".")
	if len(parts) != 5 || parts[0] != "ah1" || !lowerHex(parts[1], 16) || !timestamp(parts[2]) || !lowerHex(parts[4], 64) {
		return empty, ErrMalformed
	}
	issuedAt, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || issuedAt > maxSafeInteger {
		return empty, ErrMalformed
	}
	if parts[3] != trimJS(parts[3]) {
		return empty, ErrMalformed
	}
	sc, ok := ParseTraceparent(parts[3])
	if !ok {
		return empty, ErrMalformed
	}
	if parts[1] != KeyID(key) {
		return empty, ErrKeyMismatch
	}
	if parts[3] != traceparent {
		return empty, ErrContextMismatch
	}
	age := now - issuedAt
	if age < 0 {
		return empty, ErrFuture
	}
	if age > maxAge {
		return empty, ErrExpired
	}
	signature, err := hex.DecodeString(parts[4])
	if err != nil {
		return empty, ErrMalformed
	}
	if !hmac.Equal(signature, mac(key, payload(parts[1], issuedAt, parts[3], method, target))) {
		return empty, ErrSignatureInvalid
	}
	return sc, nil
}

// Target preserves escaped path spelling, the raw query, and an empty trailing
// query. Fragments never reach the origin and are excluded.
func Target(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.ForceQuery || u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return path
}

// ParseTraceparent matches core's version-00 parser, including ECMAScript
// whitespace trimming, lowercase hex, and rejection of zero IDs or duplicates.
func ParseTraceparent(header string) (trace.SpanContext, bool) {
	header = trimJS(header)
	if len(header) != 55 || header[:3] != "00-" || header[35] != '-' || header[52] != '-' || !lowerHex(header[3:35], 32) || !lowerHex(header[36:52], 16) || !lowerHex(header[53:], 2) {
		return trace.SpanContext{}, false
	}
	tid, err := trace.TraceIDFromHex(header[3:35])
	if err != nil {
		return trace.SpanContext{}, false
	}
	sid, err := trace.SpanIDFromHex(header[36:52])
	if err != nil {
		return trace.SpanContext{}, false
	}
	flags, err := strconv.ParseUint(header[53:], 16, 8)
	if err != nil {
		return trace.SpanContext{}, false
	}
	return trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.TraceFlags(flags), Remote: true}), true
}

// ValidMarker applies the collector's Crockford normalization and Luhn mod-32
// checksum to response and linked markers. Whitespace is intentionally retained.
func ValidMarker(raw string) bool {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	// Full Unicode case mapping mirrors String.toUpperCase, including expansions.
	text := cases.Upper(language.Und).String(raw)
	text = strings.NewReplacer("-", "", "I", "1", "L", "1", "O", "0").Replace(text)
	if len(text) != 14 && len(text) != 16 {
		return false
	}
	values := make([]int, len(text))
	for i := range len(text) {
		value := strings.IndexByte(alphabet, text[i])
		if value < 0 {
			return false
		}
		values[i] = value
	}
	sum, factor := 0, 2
	for i := len(values) - 2; i >= 0; i-- {
		addend := factor * values[i]
		sum += addend/32 + addend%32
		factor = 3 - factor
	}
	return values[len(values)-1] == (32-sum%32)%32
}

func mac(key string, bytes []byte) []byte {
	h := hmac.New(sha256.New, []byte(key))
	_, _ = h.Write(bytes) // hash.Hash.Write cannot return an error.
	return h.Sum(nil)
}

func lowerHex(s string, length int) bool {
	if len(s) != length {
		return false
	}
	for i := range len(s) {
		if !(s[i] >= '0' && s[i] <= '9' || s[i] >= 'a' && s[i] <= 'f') {
			return false
		}
	}
	return true
}

func timestamp(s string) bool {
	if len(s) == 0 || len(s) > 16 || len(s) > 1 && s[0] == '0' {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func validRequest(method, target string) bool {
	if !utf8.ValidString(method) || utf8.RuneCountInString(method) == 0 || utf8.RuneCountInString(method) > 64 || !utf8.ValidString(target) || !strings.HasPrefix(target, "/") || strings.ContainsAny(target, "\x00\r\n#") {
		return false
	}
	for _, c := range method {
		// JavaScript /[a-z]/iu also case-folds Kelvin sign and long s.
		if c == 'K' || c == 'ſ' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexRune("!#$%&'*+.^_`|~-", c) >= 0 {
			continue
		}
		return false
	}
	return true
}

func trimJS(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
			return true
		}
		return r >= 0x2000 && r <= 0x200a
	})
}

func payload(id string, at int64, parent, method, target string) []byte {
	b := []byte("agentrail.trace-handoff\x00[1,")
	b = appendJSONString(b, id)
	b = append(b, ',')
	b = strconv.AppendInt(b, at, 10)
	for _, value := range []string{parent, method, target} {
		b = append(b, ',')
		b = appendJSONString(b, value)
	}
	return append(b, ']')
}

// JSON.stringify emits HTML characters and U+2028/U+2029 literally. Go's JSON
// encoder always escapes the separators, even when HTML escaping is disabled.
// All callers validate UTF-8 before reaching this encoder.
func appendJSONString(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	for i := range len(s) {
		c := s[i]
		switch c {
		case '"', '\\':
			b = append(b, '\\', c)
		case '\b':
			b = append(b, '\\', 'b')
		case '\t':
			b = append(b, '\\', 't')
		case '\n':
			b = append(b, '\\', 'n')
		case '\f':
			b = append(b, '\\', 'f')
		case '\r':
			b = append(b, '\\', 'r')
		default:
			if c < 0x20 {
				b = append(b, '\\', 'u', '0', '0', hex[c>>4], hex[c&15])
			} else {
				b = append(b, c)
			}
		}
	}
	return append(b, '"')
}
