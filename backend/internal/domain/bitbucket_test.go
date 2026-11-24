package domain

import (
	"errors"
	"testing"
)

func TestParseChangeRequestURLBitbucket(t *testing.T) {
	for _, suffix := range []string{"", "/diff", "/commits", "/overview", "/"} {
		ref, err := ParseChangeRequestURL("https://bitbucket.org/dealhubio/aura-sre/pull-requests/12" + suffix)
		if err != nil {
			t.Fatalf("suffix %q: %v", suffix, err)
		}
		want := ChangeRequestReference{
			URL: "https://bitbucket.org/dealhubio/aura-sre/pull-requests/12", Provider: "bitbucket",
			Host: "bitbucket.org", Repository: "dealhubio/aura-sre", Number: 12,
		}
		if ref != want {
			t.Fatalf("suffix %q: ref = %+v, want %+v", suffix, ref, want)
		}
	}
	for _, raw := range []string{
		"https://bitbucket.org/dealhubio/aura-sre/pull-requests/abc",
		"https://bitbucket.org/dealhubio/aura-sre/pull-requests/0",
		"https://bitbucket.org/dealhubio/aura-sre/pull-requests/12/unknown",
		"https://bitbucket.org/dealhubio/pull-requests/12",
		"https://bitbucket.org/a/b/c/pull-requests/12",
		"https://bitbucket.org:8443/dealhubio/aura-sre/pull-requests/12",
		"https://bitbucket.org/dealhubio/aura-sre/pull/12",
	} {
		if _, err := ParseChangeRequestURL(raw); !errors.Is(err, ErrInvalidChangeRequestURL) {
			t.Fatalf("%s: err = %v, want ErrInvalidChangeRequestURL", raw, err)
		}
	}
}

func TestBitbucketRepositoryIdentity(t *testing.T) {
	for _, raw := range []string{
		"git@bitbucket.org:dealhubio/aura-sre.git",
		"https://bitbucket.org/dealhubio/aura-sre",
		"ssh://git@bitbucket.org/dealhubio/aura-sre.git",
	} {
		id, err := ParseRepositoryIdentity(raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		want := RepositoryIdentity{Provider: "bitbucket", Host: "bitbucket.org", Namespace: "dealhubio", Name: "aura-sre"}
		if id != want {
			t.Fatalf("%s: id = %+v, want %+v", raw, id, want)
		}
	}
	if _, err := ParseRepositoryIdentity("https://bitbucket.org/dealhubio/sub/aura-sre"); err == nil {
		t.Fatal("nested bitbucket namespace must be rejected")
	}
	if got := RepositoryProvider("Bitbucket.org:443"); got != "bitbucket" {
		t.Fatalf("provider = %q", got)
	}
	if got := RepositoryProvider("gitlab.example.com"); got != "gitlab" {
		t.Fatalf("provider = %q", got)
	}
}

func TestInferTrackerProviderBitbucketHasNoTracker(t *testing.T) {
	for _, raw := range []string{"git@bitbucket.org:dealhubio/aura-sre.git", "https://bitbucket.org/dealhubio/aura-sre"} {
		if got := InferTrackerProvider(raw); got != "" {
			t.Fatalf("%s: provider = %q, want empty", raw, got)
		}
	}
	if got := InferTrackerProvider("https://git.example.com/a/b"); got != TrackerProviderGitLab {
		t.Fatalf("provider = %q", got)
	}
}

func TestParseChangeRequestURLBitbucketNormalises(t *testing.T) {
	ref, err := ParseChangeRequestURL("http://bitbucket.org/DealHubIO/Aura-SRE/pull-requests/12/diff")
	if err != nil {
		t.Fatal(err)
	}
	if ref.URL != "https://bitbucket.org/dealhubio/aura-sre/pull-requests/12" || ref.Repository != "dealhubio/aura-sre" {
		t.Fatalf("ref = %+v", ref)
	}
	if got, want := ref.Key(), "bitbucket|bitbucket.org|dealhubio/aura-sre|12"; got != want {
		t.Fatalf("key = %q, want %q", got, want)
	}
}

func TestBitbucketRepositoryIdentityPorts(t *testing.T) {
	for _, raw := range []string{"ssh://git@bitbucket.org:22/ws/repo.git", "https://bitbucket.org:443/ws/repo"} {
		id, err := ParseRepositoryIdentity(raw)
		if err != nil || id.Host != "bitbucket.org" || id.Provider != "bitbucket" {
			t.Fatalf("%s: id = %+v, err = %v", raw, id, err)
		}
	}
	for _, raw := range []string{
		"ssh://git@bitbucket.org:7999/ws/repo.git",
		"https://bitbucket.org:8443/ws/repo",
		"ssh://git@bitbucket.org:443/ws/repo.git",
		"https://bitbucket.org:22/ws/repo",
	} {
		if _, err := ParseRepositoryIdentity(raw); err == nil {
			t.Fatalf("%s: want error", raw)
		}
	}
}
