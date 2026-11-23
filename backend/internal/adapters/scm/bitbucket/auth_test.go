package bitbucket

import (
	"context"
	"errors"
	"testing"
)

func clearBitbucketEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AO_BITBUCKET_USERNAME", "AO_BITBUCKET_API_TOKEN",
		"BITBUCKET_USERNAME", "BITBUCKET_API_TOKEN",
	} {
		t.Setenv(k, "")
	}
}

func TestEnvCredentialSourcePrecedence(t *testing.T) {
	clearBitbucketEnv(t)
	t.Setenv("BITBUCKET_USERNAME", "plain-user")
	t.Setenv("BITBUCKET_API_TOKEN", "plain-token")
	t.Setenv("AO_BITBUCKET_USERNAME", "ao-user")
	t.Setenv("AO_BITBUCKET_API_TOKEN", "ao-token")

	creds, err := DefaultEnvCredentialSource().Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if creds.Username != "ao-user" || creds.Token != "ao-token" {
		t.Fatalf("got %+v, want AO_ vars to win", creds)
	}

	t.Setenv("AO_BITBUCKET_USERNAME", "")
	t.Setenv("AO_BITBUCKET_API_TOKEN", "")
	creds, err = DefaultEnvCredentialSource().Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if creds.Username != "plain-user" || creds.Token != "plain-token" {
		t.Fatalf("got %+v, want fallback to BITBUCKET_ vars", creds)
	}
}

func TestEnvCredentialSourceUsernameWithoutToken(t *testing.T) {
	clearBitbucketEnv(t)
	t.Setenv("AO_BITBUCKET_USERNAME", "ao-user")
	_, err := DefaultEnvCredentialSource().Credentials(context.Background())
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("err=%v, want ErrNoCredentials", err)
	}
}

func TestFallbackCredentialSourceReturnsFirstSuccess(t *testing.T) {
	src := FallbackCredentialSource{
		StaticCredentialSource{},
		nil,
		StaticCredentialSource{Username: "u2", Token: "t2"},
		StaticCredentialSource{Username: "u3", Token: "t3"},
	}
	creds, err := src.Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if creds.Username != "u2" || creds.Token != "t2" {
		t.Fatalf("got %+v", creds)
	}
}

func TestFallbackCredentialSourceNoneYields(t *testing.T) {
	src := FallbackCredentialSource{StaticCredentialSource{}, StaticCredentialSource{}}
	if _, err := src.Credentials(context.Background()); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("err=%v, want ErrNoCredentials", err)
	}
}

func TestKeychainCredentialSource(t *testing.T) {
	clearBitbucketEnv(t)
	t.Setenv("BITBUCKET_USERNAME", "kc-user")

	var gotName string
	var gotArgs []string
	src := KeychainCredentialSource{
		Service: "ao-bitbucket",
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			gotName, gotArgs = name, args
			return []byte("kc-token\n"), nil
		},
	}
	creds, err := src.Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if creds.Username != "kc-user" || creds.Token != "kc-token" {
		t.Fatalf("got %+v", creds)
	}
	want := []string{"find-generic-password", "-s", "ao-bitbucket", "-a", "kc-user", "-w"}
	if gotName != "security" || len(gotArgs) != len(want) {
		t.Fatalf("ran %s %v", gotName, gotArgs)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Fatalf("args=%v want %v", gotArgs, want)
		}
	}
}

func TestKeychainCredentialSourceFailures(t *testing.T) {
	clearBitbucketEnv(t)
	fail := func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("item not found")
	}
	t.Setenv("BITBUCKET_USERNAME", "kc-user")
	if _, err := (KeychainCredentialSource{Service: "ao-bitbucket", run: fail}).Credentials(context.Background()); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("lookup failure: err=%v, want ErrNoCredentials", err)
	}
	t.Setenv("BITBUCKET_USERNAME", "")
	called := false
	noUser := KeychainCredentialSource{Service: "ao-bitbucket", run: func(context.Context, string, ...string) ([]byte, error) {
		called = true
		return []byte("x"), nil
	}}
	if _, err := noUser.Credentials(context.Background()); !errors.Is(err, ErrNoCredentials) || called {
		t.Fatalf("no username: err=%v called=%v, want ErrNoCredentials without keychain call", err, called)
	}
}

func TestSCMCredentialsAvailable(t *testing.T) {
	ctx := context.Background()
	empty, err := NewProvider(ProviderOptions{Credentials: StaticCredentialSource{}})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := empty.SCMCredentialsAvailable(ctx)
	if ok || err != nil {
		t.Fatalf("empty: ok=%v err=%v, want false,nil", ok, err)
	}
	ok, err = newTestProvider(t, nil).SCMCredentialsAvailable(ctx)
	if !ok || err != nil {
		t.Fatalf("static: ok=%v err=%v, want true,nil", ok, err)
	}
	boom := errors.New("boom")
	broken, err := NewProvider(ProviderOptions{Credentials: failingSource{boom}})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := broken.SCMCredentialsAvailable(ctx); ok || !errors.Is(err, boom) {
		t.Fatalf("broken: ok=%v err=%v, want false,boom", ok, err)
	}
}

type failingSource struct{ err error }

func (f failingSource) Credentials(context.Context) (Credentials, error) {
	return Credentials{}, f.err
}

func TestEnvCredentialSourceDoesNotMixPairs(t *testing.T) {
	clearBitbucketEnv(t)
	t.Setenv("AO_BITBUCKET_USERNAME", "ao-user")
	t.Setenv("BITBUCKET_API_TOKEN", "plain-token")
	_, err := DefaultEnvCredentialSource().Credentials(context.Background())
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("err=%v, want ErrNoCredentials for mixed AO_ user + BITBUCKET_ token", err)
	}

	// A complete lower-precedence pair is still used when the AO_ pair is partial.
	t.Setenv("BITBUCKET_USERNAME", "plain-user")
	creds, err := DefaultEnvCredentialSource().Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if creds.Username != "plain-user" || creds.Token != "plain-token" {
		t.Fatalf("got %+v, want the BITBUCKET_ pair", creds)
	}
}
