package cmd

import "testing"

// Lines are shaped exactly like the entries the image's sprite-browser shim
// wrote to /tmp/xdg-open.log on a live sprite (state/challenge values elided).
func TestCallbackPortFromLogLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want int
	}{
		{
			name: "claude /login authorize URL (url-encoded redirect)",
			line: "2026-09-14 19:27:01 [PID:1,PPID:2] FOUND_URL: https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a&response_type=code&redirect_uri=http%3A%2F%2Flocalhost%3A33359%2Fcallback&scope=user%3Ainference&code_challenge=x&code_challenge_method=S256&state=y",
			want: 33359,
		},
		{
			name: "plain redirect form",
			line: "FOUND_URL: https://example.com/authorize?redirect_uri=http://localhost:43251/callback",
			want: 43251,
		},
		{
			name: "127.0.0.1 redirect",
			line: "FOUND_URL: https://example.com/a?redirect_uri=http%3A%2F%2F127.0.0.1%3A50123%2Fcb",
			want: 50123,
		},
		{
			name: "lowercase percent-encoding",
			line: "FOUND_URL: https://example.com/a?redirect_uri=http%3a%2f%2flocalhost%3a40000%2fcb",
			want: 40000,
		},
		{
			name: "non-login URL",
			line: "FOUND_URL: https://claude.ai/code/artifact/37a66604?via=auto_preview",
			want: 0,
		},
		{
			name: "remote redirect is not forwarded",
			line: "FOUND_URL: https://example.com/a?redirect_uri=https%3A%2F%2Fconsole.anthropic.com%2Foauth%2Fcode%2Fcallback",
			want: 0,
		},
		{
			name: "privileged port rejected",
			line: "FOUND_URL: https://example.com/a?redirect_uri=http://localhost:80/cb",
			want: 0,
		},
		{
			name: "non-URL log line",
			line: "2026-09-14 19:27:01 [PID:1,PPID:2] ESCAPE_SENT: TTY_STATUS=found_ancestor_tty",
			want: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := callbackPortFromLogLine(c.line); got != c.want {
				t.Errorf("callbackPortFromLogLine() = %d, want %d", got, c.want)
			}
		})
	}
}
