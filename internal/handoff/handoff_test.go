package handoff

import (
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"os"
	"strings"
	"testing"
)

const key = "synthetic-workspace-key-for-handoff-tests"
const parent = "00-11111111111111111111111111111111-2222222222222222-01"
const issued int64 = 1793743200000
const target = "/api/mcp?x=1+2&x=%20"
const token = "ah1.65043bd0bbc7b543.1793743200000.00-11111111111111111111111111111111-2222222222222222-01.1928383719116ba2a719a91e8f53adcc305b165d0c12c79648587e86d0826aa8"

// Fails if encoding/json's HTML or separator escaping replaces JSON.stringify bytes.
func TestJavaScriptInteroperability(t *testing.T) {
	var vectors []struct {
		Name, HashKey, HashKeyID, Traceparent, Method, Target, Token, HashValue, Hash string
		IssuedAt                                                                      int64
	}
	readJSON(t, "../../testdata/interoperability/javascript.json", &vectors)
	for _, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			if got := KeyID(v.HashKey); got != v.HashKeyID {
				t.Fatalf("KeyID = %q, want %q", got, v.HashKeyID)
			}
			if got := Hash(v.HashKey, v.HashValue); got != v.Hash {
				t.Fatalf("Hash = %q, want %q", got, v.Hash)
			}
			got, err := Sign(v.HashKey, v.Traceparent, v.Method, v.Target, v.IssuedAt)
			if err != nil {
				t.Fatal(err)
			}
			if got != v.Token {
				t.Fatalf("Sign = %q, want %q", got, v.Token)
			}
			sc, err := Verify(v.HashKey, v.Token, v.Traceparent, v.Method, v.Target, v.IssuedAt)
			if err != nil {
				t.Fatal(err)
			}
			if sc.TraceID().String() != "11111111111111111111111111111111" || sc.SpanID().String() != "2222222222222222" || !sc.IsRemote() || !sc.IsSampled() {
				t.Fatalf("unexpected context: %v", sc)
			}
		})
	}
}

// Fails if any request field stops being authenticated or the validity window widens.
func TestVerifyBindsRequestAndContext(t *testing.T) {
	cases := []struct {
		name, k, tok, p, m, t string
		now                   int64
		want                  error
	}{
		{"valid", key, token, parent, "POST", target, issued, nil},
		{"last-valid-millisecond", key, token, parent, "POST", target, issued + 60000, nil},
		{"expired", key, token, parent, "POST", target, issued + 60001, ErrExpired},
		{"future", key, token, parent, "POST", target, issued - 1, ErrFuture},
		{"rotated-key", key + "-rotated", token, parent, "POST", target, issued, ErrKeyMismatch},
		{"different-context", key, token, strings.Replace(parent, "-01", "-00", 1), "POST", target, issued, ErrContextMismatch},
		{"duplicate-context", key, token, parent + ", " + parent, "POST", target, issued, ErrContextMismatch},
		{"trimmed-context", key, token, " " + parent, "POST", target, issued, ErrContextMismatch},
		{"method-case", key, token, parent, "post", target, issued, ErrSignatureInvalid},
		{"method", key, token, parent, "GET", target, issued, ErrSignatureInvalid},
		{"target-encoding", key, token, parent, "POST", "/api/mcp?x=1%202&x=%20", issued, ErrSignatureInvalid},
		{"tampered-mac", key, token[:len(token)-1] + "9", parent, "POST", target, issued, ErrSignatureInvalid},
		{"missing", key, "", parent, "POST", target, issued, ErrMissing},
		{"extra-part", key, token + ".extra", parent, "POST", target, issued, ErrMalformed},
		{"malformed", key, "caller-controlled-header", parent, "POST", target, issued, ErrMalformed},
		{"time-leading-zero", key, strings.Replace(token, ".1793743200000.", ".01793743200000.", 1), parent, "POST", target, issued, ErrMalformed},
		{"time-unsafe", key, strings.Replace(token, ".1793743200000.", ".9007199254740992.", 1), parent, "POST", target, issued, ErrMalformed},
		{"uppercase-mac", key, token[:len(token)-64] + strings.ToUpper(token[len(token)-64:]), parent, "POST", target, issued, ErrMalformed},
		{"bad-method", key, token, parent, "PO ST", target, issued, ErrMalformed},
		{"fragment", key, token, parent, "POST", "/path#fragment", issued, ErrMalformed},
		{"invalid-utf8-target", key, token, parent, "POST", "/\xff", issued, ErrMalformed},
		{"unsafe-now", key, token, parent, "POST", target, math.MaxInt64, ErrMalformed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sc, err := Verify(c.k, c.tok, c.p, c.m, c.t, c.now)
			if !errors.Is(err, c.want) {
				t.Fatalf("Verify error = %v, want %v", err, c.want)
			}
			if c.want != nil && sc.IsValid() {
				t.Fatal("refusal returned a valid context")
			}
		})
	}
}

// Fails if malformed requests can be signed and later exported as trusted handoffs.
func TestSignRejectsMalformedInputs(t *testing.T) {
	for _, c := range []struct {
		name, p, m, t string
		at            int64
	}{
		{"padded-parent", " " + parent, "POST", target, issued},
		{"duplicate-parent", parent + "," + parent, "POST", target, issued},
		{"uppercase-parent", "00-ABCDEFabcdefabcdefabcdefabcdefab-abcdefabcdefabcd-01", "POST", target, issued},
		{"negative-time", parent, "POST", target, -1},
		{"unsafe-time", parent, "POST", target, 9007199254740992},
		{"space-method", parent, "PO ST", target, issued},
		{"empty-method", parent, "", target, issued},
		{"long-method", parent, strings.Repeat("A", 65), target, issued},
		{"unicode-method", parent, "GÉT", target, issued},
		{"absolute-target", parent, "POST", "https://origin.test/path", issued},
		{"empty-target", parent, "POST", "", issued},
		{"fragment", parent, "POST", "/x#y", issued},
		{"newline", parent, "POST", "/x\ny", issued},
		{"return", parent, "POST", "/x\ry", issued},
		{"nul", parent, "POST", "/x\x00y", issued},
		{"invalid-utf8", parent, "POST", "/x\xff", issued},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := Sign(key, c.p, c.m, c.t, c.at)
			if !errors.Is(err, ErrInput) || got != "" {
				t.Fatalf("Sign returned %q, %v", got, err)
			}
		})
	}
}

// Fails if URL decoding, query rebuilding, or fragment forwarding changes the signed wire target.
func TestTargetPreservesWireRepresentation(t *testing.T) {
	for _, c := range []struct{ raw, want string }{
		{"https://origin.test/base/%2f?q=1+2&q=%20#not-sent", "/base/%2f?q=1+2&q=%20"},
		{"https://origin.test/path?", "/path?"},
		{"https://origin.test?", "/?"},
		{"https://origin.test", "/"},
		{"https://origin.test/caf%C3%A9?b=%26&a=1&a=2", "/caf%C3%A9?b=%26&a=1&a=2"},
	} {
		u, err := url.Parse(c.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := Target(u); got != c.want {
			t.Errorf("Target(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// Fails if duplicate headers, unsupported versions, uppercase hex, or zero IDs become parents.
func TestParseTraceparent(t *testing.T) {
	for _, c := range []struct {
		raw   string
		valid bool
	}{
		{parent, true}, {"\ufeff \t" + parent + "\r\n", true},
		{"00-abcdefabcdefabcdefabcdefabcdefab-abcdefabcdefabcd-ff", true},
		{"00-11111111111111111111111111111111-2222222222222222-00", true},
		{"", false}, {parent + "," + parent, false}, {parent + "-extra", false}, {"01" + parent[2:], false},
		{"00-00000000000000000000000000000000-2222222222222222-01", false},
		{"00-11111111111111111111111111111111-0000000000000000-01", false},
		{"00-ABCDEFabcdefabcdefabcdefabcdefab-abcdefabcdefabcd-01", false},
		{"\u0085" + parent, false}, {"\xff" + parent, false},
	} {
		sc, ok := ParseTraceparent(c.raw)
		if ok != c.valid || sc.IsValid() != c.valid {
			t.Errorf("ParseTraceparent(%q) validity = %v, want %v", c.raw, ok, c.valid)
		}
	}
}

// Fails if marker checksum, Crockford look-alikes, or linked markers diverge from the collector.
func TestContractFixtures(t *testing.T) {
	var c struct {
		TraceHandoff struct {
			Fixtures []struct {
				HashKey, Token string
				Input          struct {
					Traceparent, HashKeyID, Method, Target string
					IssuedAt                               int64
				}
			}
		}
		Markers struct {
			Minted []struct{ Marker string }
			Linked []struct{ Linked string }
			Parsed []struct {
				Raw    string
				Result struct{ Valid bool }
			}
		}
	}
	readJSON(t, "../../testdata/interoperability/contract.json", &c)
	for _, v := range c.TraceHandoff.Fixtures {
		got, err := Sign(v.HashKey, v.Input.Traceparent, v.Input.Method, v.Input.Target, v.Input.IssuedAt)
		if err != nil || got != v.Token {
			t.Fatalf("contract token mismatch: %q, %v", got, err)
		}
	}
	for _, v := range c.Markers.Minted {
		if !ValidMarker(v.Marker) {
			t.Errorf("minted marker refused: %q", v.Marker)
		}
	}
	for _, v := range c.Markers.Linked {
		if !ValidMarker(v.Linked) {
			t.Errorf("linked marker refused: %q", v.Linked)
		}
	}
	for _, v := range c.Markers.Parsed {
		if got := ValidMarker(v.Raw); got != v.Result.Valid {
			t.Errorf("ValidMarker(%q) = %v, want %v", v.Raw, got, v.Result.Valid)
		}
	}
	for _, raw := range []string{"0000000000000I", "0000000000000L", " 028T5CY4TQKFFX", "/028T5CY4TQKFFX", "028T5CY4TQKFFX ", "\xff"} {
		if ValidMarker(raw) {
			t.Errorf("invalid marker accepted: %q", raw)
		}
	}
}

// Fails if Unicode uppercase expansion is replaced by Go's simple rune mapping.
func TestMarkerUnicodeParity(t *testing.T) {
	var vectors []struct {
		Raw   string
		Valid bool
	}
	readJSON(t, "../../testdata/interoperability/markers.json", &vectors)
	for _, v := range vectors {
		if got := ValidMarker(v.Raw); got != v.Valid {
			t.Errorf("ValidMarker(%q) = %v, want %v", v.Raw, got, v.Valid)
		}
	}
}

func readJSON(t *testing.T, path string, dst any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, dst); err != nil {
		t.Fatal(err)
	}
}

func FuzzParseAndVerify(f *testing.F) {
	f.Add(token, parent, "POST", target, issued)
	f.Add("", "", "", "", int64(-1))
	f.Add(strings.Repeat("x", 257), "00-", "\xff", "/\xff", int64(math.MaxInt64))
	f.Fuzz(func(t *testing.T, tok, p, m, target string, now int64) {
		ParseTraceparent(p)
		ValidMarker(target)
		sc, err := Verify(key, tok, p, m, target, now)
		if err != nil && sc.IsValid() {
			t.Fatal("refusal returned valid context")
		}
	})
}
