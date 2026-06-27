package proxy

import (
	"strings"
	"testing"
)

func TestClientProfilesUseLatestCodexCLI(t *testing.T) {
	if len(clientProfiles) == 0 {
		t.Fatal("clientProfiles should not be empty")
	}

	wantUA := "codex_cli_rs/" + latestCodexCLIVersion
	for _, profile := range clientProfiles {
		if profile.Version != latestCodexCLIVersion {
			t.Fatalf("clientProfiles should only use latest Codex CLI %s, got %s", latestCodexCLIVersion, profile.Version)
		}
		if !strings.Contains(profile.UserAgent, wantUA) {
			t.Fatalf("%s profile has mismatched User-Agent: %q", latestCodexCLIVersion, profile.UserAgent)
		}
	}
}

func TestDefaultClientProfileUsesLatestCodexCLI(t *testing.T) {
	profile := ProfileForAccount(1)
	if profile.Version != latestCodexCLIVersion {
		t.Fatalf("ProfileForAccount returned Codex CLI version %s, want %s", profile.Version, latestCodexCLIVersion)
	}
}

func TestIsCodexOfficialClientByHeaders(t *testing.T) {
	tests := []struct {
		name       string
		userAgent  string
		originator string
		want       bool
	}{
		{name: "cli ua", userAgent: "codex_cli_rs/0.128.0", want: true},
		{name: "vscode ua", userAgent: "codex_vscode/1.2.3", want: true},
		{name: "desktop originator", originator: "codex_chatgpt_desktop", want: true},
		{name: "opencode ua", userAgent: "opencode/0.5.0", want: true},
		{name: "opencode originator", originator: "opencode", want: true},
		{name: "legacy contains codex token", userAgent: "Mozilla/5.0 codex_cli_rs/0.128.0", want: true},
		{name: "non official", userAgent: "curl/8.0", originator: "random-client", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCodexOfficialClientByHeaders(tc.userAgent, tc.originator); got != tc.want {
				t.Fatalf("IsCodexOfficialClientByHeaders() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsCodexStrictOfficialClientByHeaders(t *testing.T) {
	tests := []struct {
		name       string
		userAgent  string
		originator string
		want       bool
	}{
		{name: "cli ua", userAgent: "codex_cli_rs/0.128.0", want: true},
		{name: "vscode ua", userAgent: "codex_vscode/1.2.3", want: true},
		{name: "desktop originator", originator: "codex_chatgpt_desktop", want: true},
		{name: "unknown codex-like originator rejected", originator: "codex_random", want: false},
		{name: "codex spaced ua", userAgent: "codex 0.136.0", want: true},
		{name: "codex spaced without version rejected", userAgent: "codex ", want: false},
		{name: "embedded cli token rejected", userAgent: "Mozilla/5.0 codex_cli_rs/0.128.0", want: false},
		{name: "random codex token rejected", userAgent: "random-codex-client", want: false},
		{name: "opencode kept out of strict official", userAgent: "opencode/0.5.0", originator: "opencode", want: false},
		{name: "non official", userAgent: "curl/8.0", originator: "random-client", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCodexStrictOfficialClientByHeaders(tc.userAgent, tc.originator); got != tc.want {
				t.Fatalf("IsCodexStrictOfficialClientByHeaders() = %v, want %v", got, tc.want)
			}
		})
	}
}
