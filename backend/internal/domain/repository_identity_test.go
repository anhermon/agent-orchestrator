package domain

import "testing"

// The Bitbucket adapter tolerates these remotes; the identity parser does not.
func TestParseRepositoryIdentityRejectsAmbiguousRemotes(t *testing.T) {
	for _, raw := range []string{
		"https://bitbucket.org/dealhubio/aura-sre.git?foo=bar",
		"https://bitbucket.org/dealhubio/aura-sre#readme",
		"https://github.com/acme/repo?",
		"bitbucket.org:dealhubio/aura-sre.git",
		"github.com:acme/repo.git",
	} {
		if _, err := ParseRepositoryIdentity(raw); err == nil {
			t.Fatalf("%q: want error", raw)
		}
	}
}

func TestCanonicalRepositoryValidation(t *testing.T) {
	for _, tc := range []struct {
		name, origin, canonical string
		valid                   bool
	}{
		{"legacy", "", "", true},
		{"authenticated checkout origin", "https://git-user:token@github.com/alice/repo.git", "https://github.com/acme/repo", true},
		{"github fork", "git@github.com:alice/repo.git", "https://github.com/acme/repo", true},
		{"gitlab nested", "ssh://git@gitlab.example.com/alice/team/repo.git", "https://gitlab.example.com/group/subgroup/repo", true},
		{"cross host", "https://gitlab.com/alice/repo", "https://gitlab.example.com/group/repo", false},
		{"cross provider", "https://github.com/alice/repo", "https://gitlab.com/group/repo", false},
		{"no origin", "", "https://github.com/acme/repo", false},
		{"remote name", "https://github.com/alice/repo", "upstream", false},
		{"PR URL", "https://github.com/alice/repo", "https://github.com/acme/repo/pull/7", false},
		{"MR URL", "https://gitlab.com/alice/repo", "https://gitlab.com/acme/repo/-/merge_requests/7", false},
		{"query", "https://github.com/alice/repo", "https://github.com/acme/repo?x=y", false},
		{"credentials", "https://github.com/alice/repo", "https://user:secret@github.com/acme/repo", false},
		{"traversal", "https://gitlab.com/alice/repo", "https://gitlab.com/acme/../repo", false},
		{"encoded namespace", "https://gitlab.com/alice/repo", "https://gitlab.com/acme%2fsub/repo", false},
		{"custom authority", "https://gitlab.example.com:8443/alice/repo", "https://gitlab.example.com:8443/group/sub/repo", true},
		{"port", "https://github.com/alice/repo", "https://github.com:8443/acme/repo", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ProjectConfig{CanonicalRepoURL: tc.canonical}
			if err := cfg.ValidateCanonicalRepository(tc.origin); (err == nil) != tc.valid {
				t.Fatalf("validation = %v, valid=%v", err, tc.valid)
			}
		})
	}
}

func TestParseRepositoryIdentityLowercasesBitbucketNames(t *testing.T) {
	for _, raw := range []string{
		"git@bitbucket.org:DealHubIO/Aura-SRE.git",
		"https://bitbucket.org/DealHubIO/Aura-SRE.git",
	} {
		got, err := ParseRepositoryIdentity(raw)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if got.Namespace != "dealhubio" || got.Name != "aura-sre" {
			t.Fatalf("%q: namespace/name = %q/%q, want dealhubio/aura-sre (as the SCM adapter and PR URL parser produce)", raw, got.Namespace, got.Name)
		}
	}
	// GitHub keeps the case it was given.
	got, err := ParseRepositoryIdentity("https://github.com/Acme/Repo.git")
	if err != nil || got.Namespace != "Acme" || got.Name != "Repo" {
		t.Fatalf("github identity = %+v, %v; want case preserved", got, err)
	}
}
