package review

import "testing"

func TestProviderFromPRURLBitbucket(t *testing.T) {
	if got := providerFromPRURL("https://bitbucket.org/dealhubio/aura-sre/pull-requests/12"); got != "bitbucket" {
		t.Fatalf("provider = %q", got)
	}
}
