package setup

import "testing"

// TestClaudeCredsExpiry checks that expiresAt is extracted from the credential
// JSON shape, with 0 as the safe fallback for missing/garbage input.
func TestClaudeCredsExpiry(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int64
	}{
		{"valid", `{"claudeAiOauth":{"expiresAt":1785520326123,"refreshToken":"x"}}`, 1785520326123},
		{"missing field", `{"claudeAiOauth":{"refreshToken":"x"}}`, 0},
		{"missing object", `{"other":1}`, 0},
		{"empty", ``, 0},
		{"garbage", `not json`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := claudeCredsExpiry([]byte(c.in)); got != c.want {
				t.Errorf("claudeCredsExpiry(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}
