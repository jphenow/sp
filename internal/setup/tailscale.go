package setup

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jphenow/sp/internal/sprite"
)

const (
	// TailscaleService is the sprite-env service that runs tailscaled. It is a
	// service rather than a backgrounded process so that a sprite which has
	// gone cold rejoins the tailnet on its own when it is next woken.
	TailscaleService = "sp-tailscaled"

	// tailscaleKeyPath is where the node authorization key lands on the
	// sprite. `tailscale up` is given the path rather than the key so the
	// secret never appears in the sprite's process list.
	tailscaleKeyPath = "/home/sprite/.tailscale-authkey"

	// defaultTailnetTags tags every sprite sp joins unless told otherwise.
	// Tagging is not decoration: an untagged node's key expires (180 days by
	// default) and the node then drops off the tailnet until someone logs in
	// again, which is precisely what cannot be done on a sprite with no
	// browser. Tagged nodes do not expire.
	//
	// It is only a default because owning a tag means editing the tailnet's
	// policy file, and somebody trying this out for the first time should not
	// have to. See TailscaleTags.
	defaultTailnetTags = "tag:sprite"
)

// tailnetHostname matches the characters a tailnet hostname may contain.
// Everything else is collapsed to a dash; sprite names carry '--' separators
// that are legal here, but a variant name need not be.
var tailnetHostname = regexp.MustCompile(`[^a-zA-Z0-9-]+`)

// TailnetOptions configures how a sprite joins the tailnet.
type TailnetOptions struct {
	// AuthKey is a tailnet node authorization key. It should be reusable and
	// pre-approved, because nothing here can answer a device-approval prompt.
	AuthKey string
	// Hostname is the name the sprite takes on the tailnet. Defaults to the
	// sprite's own name.
	Hostname string
	// Tags are the ACL tags to advertise, comma-separated, and are used
	// verbatim: empty means advertise none, which is a legitimate choice and
	// not a missing value. See TailscaleTags for where the default comes from.
	Tags string
}

// tailnetHostnameFor renders a sprite name as a tailnet hostname. Sprite
// names carry '--' separators, which are legal here and kept, but a variant
// name need not be: anything a hostname cannot hold is collapsed to a dash.
func tailnetHostnameFor(name string) string {
	return strings.Trim(tailnetHostname.ReplaceAllString(name, "-"), "-")
}

// TailscaleAuthKeyPath is where sp looks for the tailnet auth key.
func TailscaleAuthKeyPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "sprite", "tailscale.authkey")
}

// TailscaleTagsPath is where sp looks for the tags to advertise.
func TailscaleTagsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "sprite", "tailscale.tags")
}

// TailscaleTags returns the ACL tags sprites should advertise: TS_TAGS, else
// the tags file, else tag:sprite.
//
// "none" means advertise no tags, and is spelled out rather than left as an
// empty file because the two are easy to confuse and mean opposite things to
// everything downstream. An untagged node works fine; its key just expires.
func TailscaleTags() string {
	tags := strings.TrimSpace(os.Getenv("TS_TAGS"))
	if tags == "" {
		if data, err := os.ReadFile(TailscaleTagsPath()); err == nil {
			tags = strings.TrimSpace(string(data))
		}
	}
	if tags == "" {
		return defaultTailnetTags
	}
	if strings.EqualFold(tags, "none") {
		return ""
	}
	return tags
}

// TailscaleAuthKey returns the configured tailnet auth key, or "" when none is
// configured. An absent key is not an error: it is how a user who does not
// want their sprites on a tailnet opts out, so every caller treats "" as
// "skip".
func TailscaleAuthKey() string {
	if key := strings.TrimSpace(os.Getenv("TS_AUTHKEY")); key != "" {
		return key
	}
	data, err := os.ReadFile(TailscaleAuthKeyPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// JoinTailnet installs tailscale on a sprite, runs it as a restarting service
// and brings the node up on the tailnet. It is idempotent: a sprite already on
// the tailnet is left alone, so this is safe to call on every connect.
//
// Two decisions here are load-bearing and not obvious:
//
//   - tailscaled runs with --tun=userspace-networking. A sprite is a
//     container, and while the platform's device allowlist does know
//     /dev/net/tun, it is only granted to a sprite whose privileges policy
//     asks for it. Userspace mode needs no device and no CAP_NET_ADMIN, and
//     it still terminates inbound connections -- which is all that is wanted
//     here. The cost is that other processes on the sprite cannot route *out*
//     over the tailnet.
//
//   - --accept-dns=false, because the sprite's own runtime manages
//     /etc/resolv.conf. Letting tailscaled take DNS over would fight it, and
//     the loser would be every name the sprite already resolves.
func JoinTailnet(client *sprite.Client, spriteName, org string, opts TailnetOptions) error {
	if opts.AuthKey == "" {
		return fmt.Errorf("no tailnet auth key: write one to %s or set TS_AUTHKEY", TailscaleAuthKeyPath())
	}
	if opts.Hostname == "" {
		opts.Hostname = spriteName
	}
	opts.Hostname = tailnetHostnameFor(opts.Hostname)
	// Steady state is a sprite that is already on the tailnet, and connects
	// pay for every round trip to a sprite. One call answers "is there
	// anything to do here at all"; the install/service/up sequence below
	// costs several more and only runs when there is.
	if TailnetReady(client, spriteName, org) {
		return nil
	}

	if err := installTailscale(client, spriteName, org); err != nil {
		return err
	}
	if err := startTailscaled(client, spriteName, org); err != nil {
		return err
	}
	return tailscaleUp(client, spriteName, org, opts)
}

// installTailscale puts the tailscale binaries on the sprite if they are
// missing.
//
// The static tarball is used rather than the apt repository because the
// package expects systemd to own the daemon, and a sprite has no systemd --
// the daemon is a sprite service instead. Downloading nothing when the
// binaries are already present matters: this runs on every connect.
func installTailscale(client *sprite.Client, spriteName, org string) error {
	script := `set -e
if [ -x /usr/local/bin/tailscaled ] && [ -x /usr/local/bin/tailscale ]; then
  exit 0
fi
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "https://pkgs.tailscale.com/stable/tailscale_latest_${arch}.tgz" |
  tar xz -C "$tmp" --strip-components=1
sudo -n install -m 0755 "$tmp/tailscaled" /usr/local/bin/tailscaled
sudo -n install -m 0755 "$tmp/tailscale" /usr/local/bin/tailscale
`
	if out, err := client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: org, Command: []string{"sh", "-c", script},
	}); err != nil {
		return fmt.Errorf("installing tailscale on %s: %w\n%s", spriteName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// startTailscaled registers the daemon as a restarting sprite service.
//
// Recreated rather than reused, matching sp's other service actions: running
// this again should refresh the service instead of failing on the name. The
// state directory is left where tailscaled puts it by default, on the user
// volume, so the node keeps its identity and its tailnet address across a
// cold wake rather than appearing as a new machine every time.
func startTailscaled(client *sprite.Client, spriteName, org string) error {
	if running, err := tailnetBackendRunning(client, spriteName, org); err == nil && running {
		return nil
	}

	client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: org,
		Command: []string{"sprite-env", "services", "delete", TailscaleService},
	})

	run := "sudo -n mkdir -p /var/lib/tailscale /var/run/tailscale && " +
		"exec sudo -n /usr/local/bin/tailscaled " +
		"--state=/var/lib/tailscale/tailscaled.state " +
		"--socket=/var/run/tailscale/tailscaled.sock " +
		"--tun=userspace-networking"
	create := fmt.Sprintf("sprite-env services create %s --restart always -- /bin/sh -lc %s",
		TailscaleService, shellQuote(run))
	if out, err := client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: org, Command: []string{"sh", "-c", create},
	}); err != nil {
		return fmt.Errorf("starting %s on %s: %w\n%s", TailscaleService, spriteName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// tailscaleUp authorizes the node on the tailnet, unless it is already up.
//
// The key is written to a file and passed as a path because `tailscale up`
// accepts "file:" for exactly this reason: a key on the command line is
// readable in the sprite's process list for as long as the command runs.
func tailscaleUp(client *sprite.Client, spriteName, org string, opts TailnetOptions) error {
	if up, err := tailnetNodeUp(client, spriteName, org); err == nil && up {
		return nil
	}

	advertise := ""
	if opts.Tags != "" {
		advertise = " --advertise-tags " + shellQuote(opts.Tags)
	}

	script := fmt.Sprintf(`set -e
printf '%%s' '%s' | base64 -d > %s
chmod 600 %s
# tailscaled has just been (re)started as a service; give it a moment to
# create its socket before asking it to do anything.
for i in 1 2 3 4 5 6 7 8 9 10; do
  [ -S /var/run/tailscale/tailscaled.sock ] && break
  sleep 1
done
sudo -n /usr/local/bin/tailscale up \
  --auth-key %s \
  --hostname %s%s \
  --accept-dns=false
# Hand the socket to the sprite user so t3 (and tailscale serve) work without
# sudo for every call.
sudo -n /usr/local/bin/tailscale set --operator="$(id -un)"
rm -f %s
`,
		base64.StdEncoding.EncodeToString([]byte(opts.AuthKey)),
		tailscaleKeyPath, tailscaleKeyPath,
		shellQuote("file:"+tailscaleKeyPath),
		shellQuote(opts.Hostname),
		advertise,
		tailscaleKeyPath,
	)

	if out, err := client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: org, Command: []string{"sh", "-c", script},
	}); err != nil {
		return fmt.Errorf("joining %s to the tailnet: %w\n%s", spriteName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// TailnetReady reports whether the sprite is installed, running and logged in
// to the tailnet, in a single round trip. It answers false for every failure,
// including "tailscale is not installed", because the caller's next move is
// the same either way.
func TailnetReady(client *sprite.Client, spriteName, org string) bool {
	out, err := client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: org,
		Command: []string{"sh", "-lc",
			"test -x /usr/local/bin/tailscale && " +
				"/usr/local/bin/tailscale status --json 2>/dev/null | grep -q '\"BackendState\": \"Running\"' && " +
				"echo sp-tailnet-ready"},
	})
	return err == nil && strings.Contains(string(out), "sp-tailnet-ready")
}

// tailnetStatus is the part of `tailscale status --json` sp reads.
type tailnetStatus struct {
	BackendState string `json:"BackendState"`
	Self         struct {
		DNSName string `json:"DNSName"`
	} `json:"Self"`
}

// tailnetState returns the sprite's tailscale status, or an error when
// tailscale is not installed or not answering.
func tailnetState(client *sprite.Client, spriteName, org string) (*tailnetStatus, error) {
	out, err := client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: org,
		Command: []string{"sh", "-lc", "/usr/local/bin/tailscale status --json 2>/dev/null"},
	})
	if err != nil {
		return nil, err
	}
	// The sprite CLI writes advisories to stderr that arrive interleaved with
	// the output, so the JSON is found rather than assumed to start at byte 0.
	raw := string(out)
	start := strings.Index(raw, "{")
	if start < 0 {
		return nil, fmt.Errorf("no status from tailscale on %s", spriteName)
	}
	var status tailnetStatus
	if err := json.Unmarshal([]byte(raw[start:]), &status); err != nil {
		return nil, fmt.Errorf("reading tailscale status on %s: %w", spriteName, err)
	}
	return &status, nil
}

// tailnetBackendRunning reports whether tailscaled is up and answering,
// whether or not the node is logged in.
func tailnetBackendRunning(client *sprite.Client, spriteName, org string) (bool, error) {
	status, err := tailnetState(client, spriteName, org)
	if err != nil {
		return false, err
	}
	return status.BackendState != "" && status.BackendState != "NoState" && status.BackendState != "Stopped", nil
}

// tailnetNodeUp reports whether the node is logged in to the tailnet.
func tailnetNodeUp(client *sprite.Client, spriteName, org string) (bool, error) {
	status, err := tailnetState(client, spriteName, org)
	if err != nil {
		return false, err
	}
	return status.BackendState == "Running", nil
}

// TailnetName returns the sprite's MagicDNS name, which is the address
// everything else -- a browser, the T3 app, ssh -- uses to reach it.
func TailnetName(client *sprite.Client, spriteName, org string) (string, error) {
	status, err := tailnetState(client, spriteName, org)
	if err != nil {
		return "", err
	}
	name := strings.TrimSuffix(status.Self.DNSName, ".")
	if name == "" {
		return "", fmt.Errorf("%s has no tailnet name yet", spriteName)
	}
	return name, nil
}
