// Tests for issue #7: JWT tokens carry exp claims, expired tokens are
// rejected with a clear error, legacy (no-exp) tokens are accepted with
// a warning, and rotation invalidates every prior token.

package server

import (
	"strings"
	"testing"
	"time"

	gjwt "github.com/gbrlsnchs/jwt/v3"

	"github.com/filecoin-project/go-jsonrpc/auth"

	"github.com/Reiers/lantern/api"
)

func TestMintedTokenCarriesExpAndIatAndJTI(t *testing.T) {
	a, err := LoadOrInitAuth(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := a.AuthNew([]auth.Permission{api.PermRead})
	if err != nil {
		t.Fatal(err)
	}
	info, err := a.Inspect(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if info.IssuedAt.IsZero() {
		t.Error("expected non-zero IssuedAt")
	}
	if info.Expires.IsZero() {
		t.Error("expected non-zero Expires")
	}
	if info.JTI == "" {
		t.Error("expected non-empty JTI")
	}
	// Read scope should be 365d.
	want := 365 * 24 * time.Hour
	got := info.Expires.Sub(info.IssuedAt)
	// Allow small drift from time of mint inside Inspect.
	if got < want-time.Hour || got > want+time.Hour {
		t.Errorf("read TTL = %v, want ~%v", got, want)
	}
}

func TestTTLPicksShortestScope(t *testing.T) {
	// Admin (30d) < sign (90d) < write (180d) < read (365d).
	cases := []struct {
		perms []auth.Permission
		want  time.Duration
	}{
		{[]auth.Permission{api.PermRead}, 365 * 24 * time.Hour},
		{[]auth.Permission{api.PermRead, api.PermWrite}, 180 * 24 * time.Hour},
		{[]auth.Permission{api.PermRead, api.PermWrite, api.PermSign}, 90 * 24 * time.Hour},
		{api.AllPerms, 30 * 24 * time.Hour},
	}
	for _, tc := range cases {
		if got := ttlFor(tc.perms); got != tc.want {
			t.Errorf("ttlFor(%v) = %v, want %v", tc.perms, got, tc.want)
		}
	}
}

func TestAuthVerifyRejectsExpiredToken(t *testing.T) {
	a, err := LoadOrInitAuth(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}

	// Hand-mint a token whose exp claim is in the past.
	hs := gjwt.NewHS256(a.secret)
	past := time.Now().Add(-1 * time.Hour)
	payload := authPayload{
		Payload: gjwt.Payload{
			IssuedAt:       gjwt.NumericDate(past.Add(-time.Hour)),
			ExpirationTime: gjwt.NumericDate(past),
		},
		Allow: []auth.Permission{api.PermRead},
	}
	tok, err := gjwt.Sign(payload, hs)
	if err != nil {
		t.Fatal(err)
	}

	_, err = a.AuthVerify(string(tok))
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("error should mention expiry, got: %v", err)
	}
	if !strings.Contains(err.Error(), "lantern auth rotate") {
		t.Errorf("error should point at the rotate command, got: %v", err)
	}
}

func TestAuthVerifyAcceptsLegacyTokenWithoutExp(t *testing.T) {
	a, err := LoadOrInitAuth(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}

	// Mint a token in the pre-#7 shape: no embedded Payload.
	type legacy struct {
		Allow []auth.Permission
	}
	hs := gjwt.NewHS256(a.secret)
	tok, err := gjwt.Sign(legacy{Allow: []auth.Permission{api.PermRead, api.PermWrite}}, hs)
	if err != nil {
		t.Fatal(err)
	}

	// Reset the once-flag so this test sees the warning path independent
	// of test ordering.
	legacyTokenWarned = false

	perms, err := a.AuthVerify(string(tok))
	if err != nil {
		t.Fatalf("legacy token should be accepted in grace window, got: %v", err)
	}
	if len(perms) != 2 {
		t.Errorf("got %d perms, want 2", len(perms))
	}
}

func TestRotateInvalidatesPriorTokens(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrInitAuth(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Capture a token under the original secret.
	originalAdminTok := a.Token(api.PermAdmin)
	if originalAdminTok == "" {
		t.Fatal("no admin token minted by LoadOrInitAuth")
	}
	// Verify it works pre-rotate.
	if _, err := a.AuthVerify(originalAdminTok); err != nil {
		t.Fatalf("admin token should verify pre-rotate: %v", err)
	}

	// Rotate.
	if err := a.Rotate(dir); err != nil {
		t.Fatal(err)
	}

	// Old token should no longer verify under the new secret.
	if _, err := a.AuthVerify(originalAdminTok); err == nil {
		t.Fatal("expected old admin token to fail after rotate")
	}

	// New admin token should verify.
	newAdminTok := a.Token(api.PermAdmin)
	if newAdminTok == "" {
		t.Fatal("rotate did not produce new admin token")
	}
	if newAdminTok == originalAdminTok {
		t.Fatal("rotate produced identical admin token (should be new)")
	}
	if _, err := a.AuthVerify(newAdminTok); err != nil {
		t.Fatalf("new admin token should verify post-rotate: %v", err)
	}
}

func TestInspectLegacyTokenReportsLegacy(t *testing.T) {
	a, err := LoadOrInitAuth(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}

	type legacy struct {
		Allow []auth.Permission
	}
	hs := gjwt.NewHS256(a.secret)
	tok, err := gjwt.Sign(legacy{Allow: []auth.Permission{api.PermRead}}, hs)
	if err != nil {
		t.Fatal(err)
	}

	info, err := a.Inspect(string(tok))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Legacy {
		t.Error("Inspect should flag tokens without exp as Legacy")
	}
	if !info.Expires.IsZero() {
		t.Error("Legacy tokens should report zero Expires")
	}
}
