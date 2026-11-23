package relay

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const tailscaleStatus = `{"Self":{"DNSName":"relay-box.example.ts.net.","TailscaleIPs":["100.64.0.7","fd7a:115c:a1e0::7"]}}`

// TestParseProbe checks what a deploy learns from the relay machine, and the
// machines it refuses.
func TestParseProbe(t *testing.T) {
	got, err := parseProbe([]byte("Linux x86_64\n" + tailscaleStatus))
	if err != nil {
		t.Fatal(err)
	}
	if got != (Target{GOARCH: "amd64", DNSName: "relay-box.example.ts.net", Addr: "100.64.0.7"}) {
		t.Fatalf("got %+v", got)
	}
	if got, _ := parseProbe([]byte("Linux aarch64\n" + tailscaleStatus)); got.GOARCH != "arm64" {
		t.Errorf("aarch64 became %q, want arm64", got.GOARCH)
	}

	for name, out := range map[string]string{
		"not Linux":          "Darwin arm64\n" + tailscaleStatus,
		"unknown arch":       "Linux riscv64\n" + tailscaleStatus,
		"no tailscale":       "Linux x86_64\nsh: tailscale: not found\n",
		"logged out":         "Linux x86_64\n{\"Self\":{}}",
		"only an IPv6 route": "Linux x86_64\n" + `{"Self":{"DNSName":"a.ts.net.","TailscaleIPs":["fd7a::1"]}}`,
	} {
		if _, err := parseProbe([]byte(out)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestRenderEnvRefusesNewlines checks that a value can't add a variable of
// its own to the service's environment.
func TestRenderEnvRefusesNewlines(t *testing.T) {
	if got, err := renderEnv("my-org", "tok"); err != nil || got != "SP_ORG=my-org\nSPRITE_RELAY_TOKEN=tok\n" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, c := range [][2]string{{"my-org", "tok\nTAILNET_ADDR=0.0.0.0"}, {"my-org\n", "tok"}, {"", "tok"}, {"my-org", ""}} {
		if _, err := renderEnv(c[0], c[1]); err == nil {
			t.Errorf("accepted org %q token %q", c[0], c[1])
		}
	}
}

// TestInstallScriptParses checks the embedded script is valid shell.
func TestInstallScriptParses(t *testing.T) {
	if out, err := exec.Command("sh", "-n", "-c", string(installScript)).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
}

// TestDeploy runs a deploy against stand-ins for ssh, scp and go, and checks
// what reaches the machine: sp, the routes, and an environment file binding
// the tailnet address, followed by an install that cleans up after itself.
func TestDeploy(t *testing.T) {
	bin, remote, log := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "log")
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\necho \""+name+" $*\" >> '"+log+"'\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub("ssh", `case "$*" in
  *"uname -sm"*) printf 'Linux x86_64\n%s\n' '`+tailscaleStatus+`' ;;
  *"mktemp -d") echo '`+remote+`' ;;
esac`)
	// scp's last argument is host:dir/; copy everything before it into dir.
	stub("scp", `for a; do last=$a; done
for a; do [ "$a" = -q ] || [ "$a" = "$last" ] || cp "$a" '`+remote+`/'; done`)
	stub("go", `while [ "$1" != -o ]; do shift; done; echo "linux sp for $GOARCH" > "$2"`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	var out bytes.Buffer
	target, err := Deploy(DeployOptions{
		Host: "relay-box", Org: "my-org", Token: "tok", Src: t.TempDir(),
		List: []byte("8899 gh-a\n"), Out: &out,
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if target.DNSName != "relay-box.example.ts.net" {
		t.Errorf("DNSName = %q", target.DNSName)
	}

	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(remote, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := read("sp"); got != "linux sp for amd64\n" {
		t.Errorf("sp = %q, want the binary built for the machine", got)
	}
	if got := read("env"); got != "TAILNET_ADDR=100.64.0.7\nSP_ORG=my-org\nSPRITE_RELAY_TOKEN=tok\n" {
		t.Errorf("env = %q", got)
	}
	if got := read("relay.list"); got != "8899 gh-a\n" {
		t.Errorf("relay.list = %q", got)
	}
	read("install.sh")
	read("sp-relay.service")

	calls, _ := os.ReadFile(log)
	install := "ssh -t relay-box sudo sh " + remote + "/install.sh; rc=$?; rm -rf " + remote + "; exit $rc"
	if !strings.Contains(string(calls), install) {
		t.Errorf("no install that removes its files afterwards; calls:\n%s", calls)
	}
	if strings.Contains(string(calls), "tok") {
		t.Errorf("the credential appeared on a command line:\n%s", calls)
	}
}

// TestDeployRefusesBadRoutes checks a broken list stops the deploy before it
// touches the machine.
func TestDeployRefusesBadRoutes(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Deploy(DeployOptions{Host: "h", Org: "o", Token: "t", List: []byte("8899\n"), Out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "route list") {
		t.Fatalf("err = %v, want a route list error", err)
	}
}
