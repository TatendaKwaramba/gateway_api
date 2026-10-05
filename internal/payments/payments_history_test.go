package payments

import "testing"

func TestDigitsSuffix(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"+263771234567", 9, "771234567"},
		{"077 123 4567", 9, "771234567"},
		{"", 9, ""},
		{"123", 9, "123"},
	}
	for _, c := range cases {
		if got := digitsSuffix(c.in, c.n); got != c.want {
			t.Errorf("digitsSuffix(%q,%d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestNormalizePhoneDigits(t *testing.T) {
	cases := []struct{ in, want string }{
		{"+263 77 123 4567", "263771234567"},
		{"077-123-4567", "0771234567"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizePhoneDigits(c.in); got != c.want {
			t.Errorf("NormalizePhoneDigits(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDigitsSuffix_BridgesLocalAndE164 proves the last-9 rule used by the
// history endpoint matches the same phone stored in different formats.
func TestDigitsSuffix_BridgesLocalAndE164(t *testing.T) {
	local := digitsSuffix("0771234567", 9)
	e164 := digitsSuffix("+263771234567", 9)
	if local != e164 || local == "" {
		t.Fatalf("suffix matching failed: local=%q e164=%q", local, e164)
	}
}
