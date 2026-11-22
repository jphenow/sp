package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jphenow/sp/internal/setup"
	"github.com/jphenow/sp/internal/sprite"
)

var (
	tailscaleTags string
)

// tailscaleCmd groups the tailnet commands.
var tailscaleCmd = &cobra.Command{
	Use:   "tailscale",
	Short: "Put a sprite on your tailnet",
}

// tailscaleSetupCmd joins one sprite to the tailnet.
var tailscaleSetupCmd = &cobra.Command{
	Use:   "setup <sprite>",
	Short: "Install tailscale on a sprite and join it to your tailnet",
	Long: `setup installs tailscale on a sprite, runs it as a restarting service and
brings the node up on your tailnet under the sprite's own name.

It needs a reusable auth key, read from TS_AUTHKEY or from
~/.config/sprite/tailscale.authkey. Reusable because every sprite joins with
the same key, and pre-approved if your tailnet requires device approval --
nothing here can answer an approval prompt.

  sp tailscale setup .
  sp tailscale setup jphenow/sp
  sp tailscale setup gameservers --tags tag:dev
  sp tailscale setup gameservers --tags none

Tags default to tag:sprite, which your tailnet's policy file has to own. Point
--tags at a tag you already own, or pass "none" to skip tagging entirely. Set
the default for every sprite in ~/.config/sprite/tailscale.tags.

Tagging is worth doing eventually: an untagged node's key expires (180 days by
default) and the node then drops off the tailnet until someone logs in again,
which is the one thing a sprite with no browser cannot do. Tagged nodes do not
expire.

This is idempotent, so it is also what runs on every connect once a key is
configured: a sprite already on the tailnet is left alone.

The node keeps its address across a cold wake, because tailscaled's state lives
on the sprite's own volume. What it cannot do is answer while the sprite is
paused -- tailscale, like any relay the sprite dials out to, needs the sprite
awake first.`,
	Args: cobra.ExactArgs(1),
	RunE: runTailscaleSetup,
}

func init() {
	tailscaleSetupCmd.Flags().StringVar(&tailscaleTags, "tags", "", `ACL tags to advertise, comma-separated; "none" for untagged (default: tag:sprite)`)
	tailscaleCmd.AddCommand(tailscaleSetupCmd)
	rootCmd.AddCommand(tailscaleCmd)
}

// resolvedTailnetTags returns the tags for this run: the flag when it was
// given, the configured default otherwise.
//
// The flag is consulted through Changed rather than for emptiness because
// --tags "" is a real instruction ("advertise nothing"), and an unset flag is
// not. Collapsing the two would make it impossible to ask for an untagged
// node from the command line.
func resolvedTailnetTags(cmd *cobra.Command) string {
	if cmd.Flags().Changed("tags") {
		if strings.EqualFold(strings.TrimSpace(tailscaleTags), "none") {
			return ""
		}
		return strings.TrimSpace(tailscaleTags)
	}
	return setup.TailscaleTags()
}

// resolveSpriteTarget turns whatever the user typed into the name of a sprite
// that exists, accepting the same targets sp itself does: ".", "owner/repo" or
// a bare sprite name.
//
// Resolving through the connect path rather than taking the argument at face
// value is what makes "owner/repo" work, and it is also the only thing that
// finds a sprite created under the older "--" naming: ResolveRepo reports
// today's name and yesterday's, and adoptExistingSprite prefers whichever one
// is really there. Taking the argument literally instead produces a 404 whose
// text ("sprite not found") says nothing about the name being the problem.
// It also settles the organization for the rest of the run: with neither --org
// nor SP_ORG given, a .sprite file in the target directory decides, exactly as
// it does for connect. Resolving the sprite in one organization and then
// talking to it in another is the mixed state this avoids.
func resolveSpriteTarget(arg string) (string, error) {
	resolved, err := resolveTarget([]string{arg})
	if err != nil {
		return "", err
	}
	globalOrg = resolved.Org

	client := sprite.NewClient(resolved.Org)
	_, exists, err := adoptExistingSprite(client, resolved)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf(
			"no sprite found for %q (looked for %q).\nRun 'sp status' to list your sprites, or 'sp %s' to create one",
			arg, resolved.SpriteName, arg)
	}
	return resolved.SpriteName, nil
}

// runTailscaleSetup joins a sprite to the tailnet and prints its address.
func runTailscaleSetup(cmd *cobra.Command, args []string) error {
	spriteName, err := resolveSpriteTarget(args[0])
	if err != nil {
		return err
	}
	client := sprite.NewClient(globalOrg)

	key := setup.TailscaleAuthKey()
	if key == "" {
		return fmt.Errorf(
			"no tailnet auth key found.\n"+
				"Create a reusable, pre-approved, tagged key in the Tailscale admin console\n"+
				"(Settings -> Keys -> Generate auth key) and save it to:\n  %s",
			setup.TailscaleAuthKeyPath())
	}

	fmt.Printf("Waking %s...\n", spriteName)
	if _, err := client.Exec(sprite.ExecOptions{
		Sprite: spriteName, Org: globalOrg, Command: []string{"true"},
	}); err != nil {
		return fmt.Errorf("waking %s: %w", spriteName, err)
	}

	if err := ensureRestartableServices(client, spriteName, globalOrg); err != nil {
		return err
	}

	fmt.Printf("Joining %s to the tailnet...\n", spriteName)
	if err := setup.JoinTailnet(client, spriteName, globalOrg, setup.TailnetOptions{
		AuthKey: key,
		Tags:    resolvedTailnetTags(cmd),
	}); err != nil {
		return err
	}

	name, err := setup.TailnetName(client, spriteName, globalOrg)
	if err != nil {
		return err
	}
	fmt.Printf("\n%s is on the tailnet as %s\n", spriteName, name)
	return nil
}
