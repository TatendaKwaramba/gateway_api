package identity

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	return NewService(nil, nil, nil, "test-secret-0123456789abcdef0123456789abcdef", "noreply@example.com")
}

func TestIssueAndValidateToken(t *testing.T) {
	s := newTestService(t)

	token, err := s.IssueToken(Identity{Phone: "+263771234567"})
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	got, err := s.ValidateToken(token)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	if got.Phone != "+263771234567" || got.Email != "" {
		t.Fatalf("unexpected identity: %+v", got)
	}
}

func TestValidateTokenTampered(t *testing.T) {
	s := newTestService(t)

	token, err := s.IssueToken(Identity{Email: "user@example.com"})
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	parts := strings.Split(token, ".")
	tampered := parts[0] + ".deadbeef"
	if _, err := s.ValidateToken(tampered); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken, got %v", err)
	}

	// Flipped body char (re-signed with wrong sig is caught by HMAC compare)
	body := parts[0] + "x"
	if _, err := s.ValidateToken(body + "." + parts[1]); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken for modified body, got %v", err)
	}
}

func TestValidateTokenExpired(t *testing.T) {
	s := newTestService(t)
	s.tokenTTL = -1 * time.Minute

	token, err := s.IssueToken(Identity{Phone: "+263771234567"})
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if _, err := s.ValidateToken(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken for expired token, got %v", err)
	}
}

func TestIssueTokenEmptyIdentity(t *testing.T) {
	s := newTestService(t)
	if _, err := s.IssueToken(Identity{}); err == nil {
		t.Fatal("expected error for empty identity")
	}
}

func TestDisabledService(t *testing.T) {
	s := NewService(nil, nil, nil, "", "")
	if s.Enabled() {
		t.Fatal("service without secret must be disabled")
	}
	if _, err := s.ValidateToken("a.b"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("expected ErrDisabled, got %v", err)
	}
}

func TestNormalizePhone(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"+263 77 123 4567", "+263771234567"},
		{"0771 234 567", "+0771234567"},
		{"+263-77-123-4567", "+263771234567"},
		{"abc", ""},
		{"", ""},
		{"+123", ""},
	}
	for _, c := range cases {
		if got := NormalizePhone(c.in); got != c.want {
			t.Errorf("NormalizePhone(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestValidateRecipient(t *testing.T) {
	if _, err := validateRecipient(channelSMS, "+263 77 123 4567"); err != nil {
		t.Errorf("valid phone rejected: %v", err)
	}
	if _, err := validateRecipient(channelSMS, "not-a-phone!"); err == nil {
		t.Error("invalid phone accepted")
	}
	if _, err := validateRecipient(channelEmail, "User@Example.com"); err != nil {
		t.Errorf("valid email rejected: %v", err)
	}
	if got, _ := validateRecipient(channelEmail, "User@Example.COM"); got != "user@example.com" {
		t.Errorf("email not lowercased: %q", got)
	}
	if _, err := validateRecipient(otpChannel("pigeon"), "x"); !errors.Is(err, ErrUnsupportedChannel) {
		t.Errorf("expected ErrUnsupportedChannel, got %v", err)
	}
}
