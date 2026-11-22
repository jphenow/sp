package cmd

import "testing"

// TestOrgFor covers the precedence that keeps a one-off organization switch
// from disturbing a directory that is already bound to another one.
func TestOrgFor(t *testing.T) {
	t.Cleanup(func() { globalOrg = "" })

	tests := []struct {
		name          string
		flag          string
		spriteFileOrg string
		want          string
	}{
		{"nothing set leaves the CLI default alone", "", "", ""},
		{"a .sprite file decides on its own", "", "bound-org", "bound-org"},
		{"the flag wins over a bound directory", "other-org", "bound-org", "other-org"},
		{"the flag alone is enough", "other-org", "", "other-org"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			globalOrg = tt.flag
			if got := orgFor(tt.spriteFileOrg); got != tt.want {
				t.Errorf("orgFor(%q) with --org %q = %q, want %q",
					tt.spriteFileOrg, tt.flag, got, tt.want)
			}
		})
	}
}

// TestOrgFlagFoldsEnv checks that SP_ORG reaches globalOrg, and that an
// explicit flag is not overwritten by it.
func TestOrgFlagFoldsEnv(t *testing.T) {
	t.Cleanup(func() { globalOrg = "" })

	t.Setenv("SP_ORG", "env-org")

	globalOrg = ""
	rootCmd.PersistentPreRun(rootCmd, nil)
	if globalOrg != "env-org" {
		t.Errorf("SP_ORG not picked up: globalOrg = %q", globalOrg)
	}

	globalOrg = "flag-org"
	rootCmd.PersistentPreRun(rootCmd, nil)
	if globalOrg != "flag-org" {
		t.Errorf("SP_ORG overrode the flag: globalOrg = %q", globalOrg)
	}
}
