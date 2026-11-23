package relay

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// The relay machine's service definition and the script that installs it,
// shipped inside sp so a deploy needs nothing but sp and ssh.
var (
	//go:embed sp-relay.service
	serviceUnit []byte
	//go:embed install.sh
	installScript []byte
)

// probeCommand asks the relay machine for what a deploy needs to know: its
// platform, to build sp for it, and its tailnet identity, to bind to and to
// hand out to clients.
const probeCommand = "uname -sm; tailscale status --json"

// Target is what a deploy learned about the relay machine.
type Target struct {
	// GOARCH is the machine's architecture in Go's terms.
	GOARCH string
	// DNSName is the machine's tailnet name, which clients connect to.
	DNSName string
	// Addr is the machine's tailnet IPv4 address, which the relay binds.
	Addr string
}

// DeployOptions configures Deploy.
type DeployOptions struct {
	// Host is the ssh destination of the relay machine.
	Host string
	// Org is the organization the relay's sprites belong to.
	Org string
	// Token is the relay's Sprites credential.
	Token string
	// List is the route list, "<port> <sprite>" per line.
	List []byte
	// Src is an sp checkout to build from when the relay machine's platform
	// differs from this one's. Empty means the running binary is shipped,
	// which only works when the platforms match.
	Src string
	// Out receives progress and the install's output.
	Out io.Writer
}

// Deploy installs sp on the relay machine and runs sp relay serve there as a
// systemd service, replacing any earlier deploy. It needs ssh access to
// opts.Host and sudo there, and returns what it learned about the machine.
func Deploy(opts DeployOptions) (Target, error) {
	if _, err := ParseRoutes(strings.NewReader(string(opts.List))); err != nil {
		return Target{}, fmt.Errorf("route list: %w", err)
	}
	env, err := renderEnv(opts.Org, opts.Token)
	if err != nil {
		return Target{}, err
	}

	fmt.Fprintf(opts.Out, "Asking %s about itself...\n", opts.Host)
	out, err := exec.Command("ssh", opts.Host, probeCommand).Output()
	if err != nil {
		return Target{}, fmt.Errorf("ssh %s: %w", opts.Host, err)
	}
	target, err := parseProbe(out)
	if err != nil {
		return Target{}, fmt.Errorf("%s: %w", opts.Host, err)
	}

	local, err := os.MkdirTemp("", "sp-relay-deploy-")
	if err != nil {
		return Target{}, err
	}
	defer os.RemoveAll(local)

	bin, err := binaryFor(target, opts.Src, local, opts.Out)
	if err != nil {
		return Target{}, err
	}
	files := map[string][]byte{
		"relay.list":       opts.List,
		"env":              []byte("TAILNET_ADDR=" + target.Addr + "\n" + env),
		"sp-relay.service": serviceUnit,
		"install.sh":       installScript,
	}
	paths := []string{bin}
	for name, data := range files {
		p := filepath.Join(local, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			return Target{}, err
		}
		paths = append(paths, p)
	}

	out, err = exec.Command("ssh", opts.Host, "mktemp -d").Output()
	if err != nil {
		return Target{}, fmt.Errorf("ssh %s: %w", opts.Host, err)
	}
	remote := strings.TrimSpace(string(out))
	if !safeRemoteDir.MatchString(remote) {
		return Target{}, fmt.Errorf("%s: unexpected temporary directory %q", opts.Host, remote)
	}

	fmt.Fprintf(opts.Out, "Copying sp and the relay's configuration to %s...\n", opts.Host)
	scp := exec.Command("scp", append(append([]string{"-q"}, paths...), opts.Host+":"+remote+"/")...)
	scp.Stderr = opts.Out
	if err := scp.Run(); err != nil {
		exec.Command("ssh", opts.Host, "rm -rf "+remote).Run()
		return Target{}, fmt.Errorf("copying to %s: %w", opts.Host, err)
	}

	// -t gives sudo a terminal to ask for a password on. The copied files,
	// the credential among them, go whether or not the install succeeds.
	install := exec.Command("ssh", "-t", opts.Host,
		fmt.Sprintf("sudo sh %[1]s/install.sh; rc=$?; rm -rf %[1]s; exit $rc", remote))
	install.Stdin, install.Stdout, install.Stderr = os.Stdin, opts.Out, opts.Out
	if err := install.Run(); err != nil {
		return Target{}, fmt.Errorf("installing on %s: %w", opts.Host, err)
	}
	return target, nil
}

// safeRemoteDir matches what mktemp -d prints, and nothing that would mean
// something else once placed in a shell command.
var safeRemoteDir = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// renderEnv returns the org and credential lines of the service's
// environment file. A newline in either would smuggle in another variable.
func renderEnv(org, token string) (string, error) {
	if org == "" || token == "" {
		return "", fmt.Errorf("an organization and a token are both required")
	}
	if strings.ContainsAny(org+token, "\r\n") {
		return "", fmt.Errorf("the organization and token must each be a single line")
	}
	return "SP_ORG=" + org + "\nSPRITE_RELAY_TOKEN=" + token + "\n", nil
}

// parseProbe reads the output of probeCommand: a "uname -sm" line, then
// tailscale's status as JSON.
func parseProbe(out []byte) (Target, error) {
	line, rest, _ := strings.Cut(string(out), "\n")
	osName, machine, _ := strings.Cut(strings.TrimSpace(line), " ")
	if osName != "Linux" {
		return Target{}, fmt.Errorf("the relay runs as a systemd service, so it needs Linux; this is %q", line)
	}
	var t Target
	switch machine {
	case "x86_64":
		t.GOARCH = "amd64"
	case "aarch64", "arm64":
		t.GOARCH = "arm64"
	default:
		return Target{}, fmt.Errorf("unsupported architecture %q", machine)
	}

	var status struct {
		Self struct {
			DNSName      string
			TailscaleIPs []string
		}
	}
	if err := json.Unmarshal([]byte(rest), &status); err != nil {
		return Target{}, fmt.Errorf("reading tailscale status (is tailscale installed and up?): %w", err)
	}
	t.DNSName = strings.TrimSuffix(status.Self.DNSName, ".")
	for _, ip := range status.Self.TailscaleIPs {
		if !strings.Contains(ip, ":") {
			t.Addr = ip
			break
		}
	}
	if t.DNSName == "" || t.Addr == "" {
		return Target{}, fmt.Errorf("tailscale reports no name or IPv4 address; is it logged in?")
	}
	return t, nil
}

// binaryFor returns the path of an sp binary that runs on target: the running
// one when the platforms match, otherwise one built from src into dir.
func binaryFor(target Target, src, dir string, out io.Writer) (string, error) {
	bin := filepath.Join(dir, "sp")
	if src == "" {
		if runtime.GOOS == "linux" && runtime.GOARCH == target.GOARCH {
			// Copied rather than sent as is: the install script expects the
			// name sp, and the running binary may be called anything.
			self, err := os.Executable()
			if err != nil {
				return "", err
			}
			data, err := os.ReadFile(self)
			if err != nil {
				return "", err
			}
			return bin, os.WriteFile(bin, data, 0o755)
		}
		return "", fmt.Errorf("the relay machine is linux/%s and this sp is %s/%s; pass --src <sp checkout> to build one for it",
			target.GOARCH, runtime.GOOS, runtime.GOARCH)
	}
	fmt.Fprintf(out, "Building sp for linux/%s from %s...\n", target.GOARCH, src)
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = src
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+target.GOARCH, "CGO_ENABLED=0")
	build.Stdout, build.Stderr = out, out
	if err := build.Run(); err != nil {
		return "", fmt.Errorf("building sp: %w", err)
	}
	return bin, nil
}
