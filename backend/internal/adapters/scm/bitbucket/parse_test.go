package bitbucket

import "testing"

func TestParseRepository(t *testing.T) {
	p := newTestProvider(t, nil)
	cases := []struct {
		remote   string
		ok       bool
		owner    string
		fullName string
	}{
		{"git@bitbucket.org:dealhubio/aura-sre.git", true, "dealhubio", "dealhubio/aura-sre"},
		{"git@bitbucket.org:DealHubIO/aura-sre", true, "dealhubio", "dealhubio/aura-sre"},
		{"ssh://git@bitbucket.org/dealhubio/aura-sre.git", true, "dealhubio", "dealhubio/aura-sre"},
		{"https://angel@bitbucket.org/dealhubio/aura-sre.git", true, "dealhubio", "dealhubio/aura-sre"},
		{"https://bitbucket.org/dealhubio/aura-sre/", true, "dealhubio", "dealhubio/aura-sre"},
		{"https://BitBucket.org/DealHubIO/Aura-SRE.git/", true, "dealhubio", "dealhubio/aura-sre"},
		{"https://bitbucket.org.evil.com/dealhubio/aura-sre.git", false, "", ""},
		{"https://bitbucket.org@evil.com/dealhubio/aura-sre.git", false, "", ""},
		{"git@evil.com:bitbucket.org/aura-sre.git", false, "", ""},
		{"git@github.com:anhermon/x.git", false, "", ""},
		{"https://gitlab.com/a/b.git", false, "", ""},
		{"https://bitbucket.org/dealhubio", false, "", ""},
		{"https://bitbucket.org/dealhubio/aura-sre/extra", false, "", ""},
		{"https://bitbucket.org/dealhubio/.git", false, "", ""},
		{"", false, "", ""},
		{`C:\x`, false, "", ""},
		{"ssh://git@bitbucket.org:7999/dealhubio/aura-sre.git", false, "", ""},
		{"ssh://git@bitbucket.org:22/dealhubio/aura-sre.git", true, "dealhubio", "dealhubio/aura-sre"},
		{"https://bitbucket.org:8443/dealhubio/aura-sre.git", false, "", ""},
		{"https://bitbucket.org:443/dealhubio/aura-sre.git", true, "dealhubio", "dealhubio/aura-sre"},
		{"bitbucket.org:dealhubio/aura-sre.git", true, "dealhubio", "dealhubio/aura-sre"},
		{"https://bitbucket.org/dealhubio/aura-sre.git?foo=bar", true, "dealhubio", "dealhubio/aura-sre"},
		{"https://bitbucket.org/dealhubio/aura-sre#readme", true, "dealhubio", "dealhubio/aura-sre"},
		{"  git@bitbucket.org:dealhubio/aura-sre.git\n", true, "dealhubio", "dealhubio/aura-sre"},
		{"ftp://bitbucket.org/dealhubio/aura-sre.git", false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.remote, func(t *testing.T) {
			repo, ok := p.ParseRepository(c.remote)
			if ok != c.ok {
				t.Fatalf("ok=%v want %v", ok, c.ok)
			}
			if !ok {
				return
			}
			if repo.Provider != "bitbucket" || repo.Host != "bitbucket.org" ||
				repo.Owner != c.owner || repo.Repo != c.fullName ||
				repo.Name != c.fullName[len(c.owner)+1:] {
				t.Fatalf("got %+v", repo)
			}
		})
	}
}
