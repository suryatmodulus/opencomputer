package previewauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGenerateTokenUniqueAndCorrectLength(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 100; i++ {
		tok, err := GenerateToken()
		if err != nil {
			t.Fatalf("GenerateToken: %v", err)
		}
		// 32 bytes → ceil(32*4/3) = 43 chars (RawURLEncoding, no padding).
		if got := len(tok); got != 43 {
			t.Fatalf("token len = %d, want 43", got)
		}
		if _, dup := seen[tok]; dup {
			t.Fatalf("duplicate token in 100 draws — entropy is broken")
		}
		seen[tok] = struct{}{}
	}
}

func TestSHA256Hex(t *testing.T) {
	// Known vector: SHA-256 of "abc" = ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad
	if got, want := SHA256Hex("abc"), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"; got != want {
		t.Fatalf("SHA256Hex(\"abc\") = %q, want %q", got, want)
	}
}

func TestExtractToken(t *testing.T) {
	cases := []struct {
		name  string
		setup func(r *http.Request)
		want  string
	}{
		{
			name:  "Authorization: Bearer X",
			setup: func(r *http.Request) { r.Header.Set("Authorization", "Bearer my-token-123") },
			want:  "my-token-123",
		},
		{
			name:  "Authorization case-insensitive Bearer",
			setup: func(r *http.Request) { r.Header.Set("Authorization", "bearer my-token-123") },
			want:  "my-token-123",
		},
		{
			name:  "X-OC-Preview-Token wins over Authorization when both set",
			setup: func(r *http.Request) { r.Header.Set("X-OC-Preview-Token", "from-x"); r.Header.Set("Authorization", "Bearer from-auth") },
			want:  "from-x",
		},
		{
			name:  "trailing whitespace trimmed",
			setup: func(r *http.Request) { r.Header.Set("X-OC-Preview-Token", "  spaced-token  ") },
			want:  "spaced-token",
		},
		{
			name:  "Authorization: Basic — not Bearer, ignored",
			setup: func(r *http.Request) { r.Header.Set("Authorization", "Basic dXNlcjpwYXNz") },
			want:  "",
		},
		{
			name:  "no headers",
			setup: func(r *http.Request) {},
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			tc.setup(r)
			if got := ExtractToken(r); got != tc.want {
				t.Fatalf("ExtractToken = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConstantTimeEqualString(t *testing.T) {
	if !ConstantTimeEqualString("abc", "abc") {
		t.Fatalf("equal strings should compare equal")
	}
	if ConstantTimeEqualString("abc", "abd") {
		t.Fatalf("one-char different should not be equal")
	}
	if ConstantTimeEqualString("abc", "abcd") {
		t.Fatalf("different lengths should not be equal")
	}
}

func TestProcessRequest(t *testing.T) {
	// auto → generated
	pt, hash, scheme, status, err := ProcessRequest("", "auto")
	if err != nil || status != 0 {
		t.Fatalf("auto: err=%v status=%d", err, status)
	}
	if scheme != "bearer" || len(pt) != 43 || SHA256Hex(pt) != hash {
		t.Fatalf("auto: bad output pt=%q hash=%q scheme=%q", pt, hash, scheme)
	}

	// empty token → generated (same as "auto")
	pt2, _, _, _, err := ProcessRequest("bearer", "")
	if err != nil || pt2 == pt {
		t.Fatalf("empty token: pt2=%q err=%v (should differ from prior draw)", pt2, err)
	}

	// BYO ≥16 chars → echoed back
	byo := "my-supersecret-token-1234567890abcdef"
	pt3, hash3, scheme3, status3, err := ProcessRequest("bearer", byo)
	if err != nil || status3 != 0 || pt3 != byo || hash3 != SHA256Hex(byo) || scheme3 != "bearer" {
		t.Fatalf("BYO: pt=%q hash=%q scheme=%q status=%d err=%v", pt3, hash3, scheme3, status3, err)
	}

	// BYO <16 chars → 400
	_, _, _, status4, err := ProcessRequest("bearer", "short")
	if err == nil || status4 != http.StatusBadRequest || !strings.Contains(err.Error(), "at least 16") {
		t.Fatalf("short BYO: status=%d err=%v", status4, err)
	}

	// unsupported scheme → 400
	_, _, _, status5, err := ProcessRequest("hmac", "auto")
	if err == nil || status5 != http.StatusBadRequest {
		t.Fatalf("unsupported scheme: status=%d err=%v", status5, err)
	}
}
