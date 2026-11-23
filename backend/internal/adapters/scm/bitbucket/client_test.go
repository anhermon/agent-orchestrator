package bitbucket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestClientSendsBasicAuthAndUserAgent(t *testing.T) {
	var gotUser, gotPass, gotUA, gotPath, gotQuery string
	p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		gotUA = r.UserAgent()
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		_, _ = w.Write([]byte(`{"username":"angel"}`))
	}))
	var out struct {
		Username string `json:"username"`
	}
	q := url.Values{"fields": {"username"}}
	if _, err := p.client.do(context.Background(), http.MethodGet, "/user", q, nil, &out); err != nil {
		t.Fatal(err)
	}
	if gotUser != testUser || gotPass != testToken {
		t.Fatalf("basic auth = %q/%q", gotUser, gotPass)
	}
	if gotUA != "ao-test" {
		t.Fatalf("user agent = %q", gotUA)
	}
	if gotPath != "/2.0/user" || gotQuery != "fields=username" {
		t.Fatalf("request = %s?%s", gotPath, gotQuery)
	}
	if out.Username != "angel" {
		t.Fatalf("decoded %+v", out)
	}
}

func TestClientSendsJSONBody(t *testing.T) {
	var gotCT string
	var got map[string]string
	p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
	}))
	if _, err := p.client.do(context.Background(), http.MethodPost, "/x", nil, map[string]string{"a": "b"}, nil); err != nil {
		t.Fatal(err)
	}
	if gotCT != "application/json" || got["a"] != "b" {
		t.Fatalf("content-type=%q body=%v", gotCT, got)
	}
}

func TestClientErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header map[string]string
		check  func(t *testing.T, err error)
	}{
		{"401", 401, nil, func(t *testing.T, err error) {
			if !errors.Is(err, ErrAuthFailed) {
				t.Fatalf("err=%v, want ErrAuthFailed", err)
			}
		}},
		{"403", 403, nil, func(t *testing.T, err error) {
			if !errors.Is(err, ports.ErrSCMForbidden) {
				t.Fatalf("err=%v, want ErrSCMForbidden", err)
			}
		}},
		{"429", 429, map[string]string{"Retry-After": "7"}, func(t *testing.T, err error) {
			var rl *RateLimitError
			if !errors.As(err, &rl) || rl.Err.Status != 429 {
				t.Fatalf("err=%v, want *RateLimitError 429", err)
			}
			if rl.GetRetryAfter() != 7*time.Second {
				t.Fatalf("RetryAfter=%v", rl.GetRetryAfter())
			}
			if !rl.GetResetAt().IsZero() {
				t.Fatalf("ResetAt=%v, want zero", rl.GetResetAt())
			}
		}},
		{"500", 500, nil, func(t *testing.T, err error) {
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != 500 || !strings.Contains(apiErr.Body, "boom") {
				t.Fatalf("err=%v, want *APIError 500 with body", err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range c.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(`{"error":"boom"}`))
			}))
			_, err := p.client.do(context.Background(), http.MethodGet, "/user", nil, nil, nil)
			if err == nil {
				t.Fatal("expected error")
			}
			c.check(t, err)
			if strings.Contains(err.Error(), testToken) {
				t.Fatalf("token leaked into error: %v", err)
			}
		})
	}
}

func TestClientNoCredentialsErrorDoesNotCallServer(t *testing.T) {
	called := false
	srv := newTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	p, err := NewProvider(ProviderOptions{Credentials: StaticCredentialSource{}, APIBase: srv.URL + "/2.0"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.client.do(context.Background(), http.MethodGet, "/user", nil, nil, nil)
	if !errors.Is(err, ErrNoCredentials) || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestPaginateFollowsNextAcrossPages(t *testing.T) {
	var base string
	p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		switch page {
		case "":
			fmt.Fprintf(w, `{"values":[1,2],"next":"%s/2.0/items?page=2"}`, base)
		case "2":
			fmt.Fprintf(w, `{"values":[3],"next":"%s/2.0/items?page=3"}`, base)
		default:
			fmt.Fprint(w, `{"values":[4,5]}`)
		}
	}))
	base = strings.TrimSuffix(p.client.apiBase, "/2.0")
	var got []int
	err := p.client.paginate(context.Background(), "/items", nil, func(raw json.RawMessage) error {
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			return err
		}
		got = append(got, n)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[1 2 3 4 5]" {
		t.Fatalf("got %v", got)
	}
}

func TestPaginateRefusesNextOnOtherHost(t *testing.T) {
	evilHits := 0
	evil := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evilHits++
		fmt.Fprint(w, `{"values":[99]}`)
	}))
	p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"values":[1],"next":"%s/2.0/items?page=2"}`, evil.URL)
	}))
	var got []int
	err := p.client.paginate(context.Background(), "/items", nil, func(raw json.RawMessage) error {
		var n int
		_ = json.Unmarshal(raw, &n)
		got = append(got, n)
		return nil
	})
	if err == nil {
		t.Fatal("expected error for cross-host next")
	}
	if evilHits != 0 {
		t.Fatalf("cross-host server was contacted %d times (credentials would leak)", evilHits)
	}
	if fmt.Sprint(got) != "[1]" {
		t.Fatalf("got %v", got)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token leaked: %v", err)
	}
}

func TestPaginateStopsWhenCallbackFails(t *testing.T) {
	hits := 0
	var base string
	p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprintf(w, `{"values":[1],"next":"%s/2.0/items?page=2"}`, base)
	}))
	base = strings.TrimSuffix(p.client.apiBase, "/2.0")
	stop := errors.New("stop")
	err := p.client.paginate(context.Background(), "/items", nil, func(json.RawMessage) error { return stop })
	if !errors.Is(err, stop) || hits != 1 {
		t.Fatalf("err=%v hits=%d", err, hits)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"seconds", "7", 7 * time.Second},
		{"padded", " 12 ", 12 * time.Second},
		{"http-date", now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{"past date", now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"huge seconds capped", "99999999", time.Hour},
		{"far future date capped", now.Add(48 * time.Hour).Format(http.TimeFormat), time.Hour},
		{"negative", "-5", 0},
		{"garbage", "soon", 0},
		{"empty", "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseRetryAfter(c.in, now); got != c.want {
				t.Fatalf("parseRetryAfter(%q)=%v want %v", c.in, got, c.want)
			}
		})
	}
}

func TestClientAuthErrorsKeepBodyAndRetryAfter(t *testing.T) {
	p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded"}}`))
	}))
	_, err := p.client.do(context.Background(), http.MethodGet, "/user", nil, nil, nil)
	if !errors.Is(err, ports.ErrSCMForbidden) {
		t.Fatalf("err=%v, want ErrSCMForbidden", err)
	}
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err=%v, want *RateLimitError for rate-limit detection", err)
	}
	if rl.GetRetryAfter() != 30*time.Second {
		t.Fatalf("RetryAfter=%v", rl.GetRetryAfter())
	}
	if !strings.Contains(err.Error(), "rate limit exceeded") {
		t.Fatalf("body excerpt missing from %v", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token leaked: %v", err)
	}
}

func TestClientAuthErrorBodyIsBounded(t *testing.T) {
	p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(strings.Repeat("x", 5000)))
	}))
	_, err := p.client.do(context.Background(), http.MethodGet, "/user", nil, nil, nil)
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("err=%v, want ErrAuthFailed", err)
	}
	if len(err.Error()) > 1024 {
		t.Fatalf("error length %d, want bounded", len(err.Error()))
	}
}

func TestPaginateStopsOnRepeatedNextURL(t *testing.T) {
	hits := 0
	var base string
	p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		fmt.Fprintf(w, `{"values":[1],"next":"%s%s"}`, base, r.URL.RequestURI())
	}))
	base = strings.TrimSuffix(p.client.apiBase, "/2.0")
	err := p.client.paginate(context.Background(), "/items", nil, func(json.RawMessage) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "repeated") {
		t.Fatalf("err=%v, want repeated-next error", err)
	}
	if hits != 1 {
		t.Fatalf("hits=%d, want 1", hits)
	}
}

func TestPaginateStopsOnNextLoop(t *testing.T) {
	hits := 0
	var base string
	p := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		other := "2"
		if r.URL.Query().Get("page") == "2" {
			other = "1"
		}
		fmt.Fprintf(w, `{"values":[1],"next":"%s/2.0/items?page=%s"}`, base, other)
	}))
	base = strings.TrimSuffix(p.client.apiBase, "/2.0")
	err := p.client.paginate(context.Background(), "/items", url.Values{"page": {"1"}}, func(json.RawMessage) error { return nil })
	if err == nil || hits > 2 {
		t.Fatalf("err=%v hits=%d, want error after 2 hits", err, hits)
	}
}

func TestNewProviderRejectsInvalidAPIBase(t *testing.T) {
	for _, base := range []string{"://bad", "http://[::1", "no-scheme-or-host", "/relative/path"} {
		t.Run(base, func(t *testing.T) {
			p, err := NewProvider(ProviderOptions{Credentials: StaticCredentialSource{Username: "u", Token: "t"}, APIBase: base})
			if err == nil || p != nil {
				t.Fatalf("NewProvider(%q)=%v,%v want error", base, p, err)
			}
		})
	}
}
