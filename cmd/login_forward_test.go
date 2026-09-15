package cmd

import "testing"

// Lines are shaped exactly like the entries the image's sprite-browser shim
// wrote to /tmp/xdg-open.log on a live sprite (state/challenge values elided).
func TestLoginURLFromLogLine(t *testing.T) {
	const authorize = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a&response_type=code&redirect_uri=http%3A%2F%2Flocalhost%3A33359%2Fcallback&scope=user%3Ainference&code_challenge=x&code_challenge_method=S256&state=y"
	cases := []struct {
		name     string
		line     string
		wantURL  string
		wantPort int
	}{
		{
			name:     "claude /login authorize URL (url-encoded redirect)",
			line:     "2026-09-14 19:27:11 [PID:10073,PPID:9385] FOUND_URL: " + authorize,
			wantURL:  authorize,
			wantPort: 33359,
		},
		{
			name:     "plain redirect form",
			line:     "FOUND_URL: https://example.com/authorize?redirect_uri=http://localhost:43251/callback",
			wantURL:  "https://example.com/authorize?redirect_uri=http://localhost:43251/callback",
			wantPort: 43251,
		},
		{
			name:     "127.0.0.1 redirect, lowercase encoding",
			line:     "FOUND_URL: https://example.com/a?redirect_uri=http%3a%2f%2f127.0.0.1%3a50123%2fcb",
			wantURL:  "https://example.com/a?redirect_uri=http%3a%2f%2f127.0.0.1%3a50123%2fcb",
			wantPort: 50123,
		},
		// The same URL also appears on the shim's START and ESCAPE_SENT lines;
		// only FOUND_URL counts, so each login opens the browser once.
		{name: "START line ignored", line: "2026-09-14 19:27:11 [PID:1,PPID:2] START: Args=(" + authorize + ") Parent=9385(claude)"},
		{name: "ESCAPE_SENT line ignored", line: "2026-09-14 19:27:11 [PID:1,PPID:2] ESCAPE_SENT: URL=" + authorize + " TTY_STATUS=found_ancestor_tty"},
		{name: "non-login URL", line: "FOUND_URL: https://claude.ai/code/artifact/37a66604?via=auto_preview"},
		{name: "remote redirect not forwarded", line: "FOUND_URL: https://example.com/a?redirect_uri=https%3A%2F%2Fconsole.anthropic.com%2Foauth%2Fcode%2Fcallback"},
		{name: "privileged port rejected", line: "FOUND_URL: https://example.com/a?redirect_uri=http://localhost:80/cb"},
		{name: "non-https URL never opened", line: "FOUND_URL: http://example.com/a?redirect_uri=http://localhost:43251/cb"},
		{name: "file URL never opened", line: "FOUND_URL: file:///etc/passwd?redirect_uri=http://localhost:43251/cb"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			url, port := loginURLFromLogLine(c.line)
			if url != c.wantURL || port != c.wantPort {
				t.Errorf("loginURLFromLogLine() = (%q, %d), want (%q, %d)", url, port, c.wantURL, c.wantPort)
			}
		})
	}
}
