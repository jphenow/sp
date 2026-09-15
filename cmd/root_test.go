package cmd

import (
	"reflect"
	"testing"

	"github.com/spf13/cobra"
)

// These run real cobra parsing, because the bug being guarded against lives
// there: cobra strips "--" from args, so the separator must be located with
// ArgsLenAtDash rather than by scanning args.
func TestSplitAtDashThroughCobra(t *testing.T) {
	cases := []struct {
		name        string
		argv        []string
		wantConnect []string
		wantCommand string
		wantErr     bool
	}{
		{name: "target and command", argv: []string{".", "--", "claude"}, wantConnect: []string{"."}, wantCommand: "claude"},
		{name: "variant and command with its own flags", argv: []string{"owner/repo", "blue", "--", "claude", "--continue"}, wantConnect: []string{"owner/repo", "blue"}, wantCommand: "claude --continue"},
		{name: "no separator", argv: []string{".", "scratch"}, wantConnect: []string{".", "scratch"}},
		{name: "separator with nothing after it", argv: []string{".", "--"}, wantConnect: []string{"."}},
		{name: "too many args before separator", argv: []string{"a", "b", "c", "--", "x"}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotConnect []string
			var gotCommand string
			cmd := &cobra.Command{
				Use:  "probe",
				Args: connectCmd.Args, // the real validator
				RunE: func(cmd *cobra.Command, args []string) error {
					gotConnect, gotCommand = splitAtDash(args, cmd.ArgsLenAtDash())
					return nil
				},
				SilenceErrors: true,
				SilenceUsage:  true,
			}
			cmd.SetArgs(c.argv)
			err := cmd.Execute()
			if c.wantErr {
				if err == nil {
					t.Fatal("expected an arg-count error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(gotConnect, c.wantConnect) || gotCommand != c.wantCommand {
				t.Errorf("got (%q, %q), want (%q, %q)", gotConnect, gotCommand, c.wantConnect, c.wantCommand)
			}
		})
	}
}
