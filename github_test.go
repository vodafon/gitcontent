package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchGitHubTreeValid(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/octo/repo/git/trees/tree123" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("recursive"); got != "1" {
			t.Fatalf("unexpected recursive query: %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Fatalf("unexpected Accept header: %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("unexpected Authorization header: %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sha":"tree123","tree":[{"path":"a.txt","sha":"blob1","type":"blob","size":5}],"truncated":false}`))
	}))
	defer ts.Close()

	orig := githubAPIBaseURL
	githubAPIBaseURL = ts.URL
	t.Cleanup(func() { githubAPIBaseURL = orig })

	tree, err := fetchGitHubTree(ts.Client(), "octo", "repo", "tree123", "secret")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if tree.SHA != "tree123" {
		t.Fatalf("unexpected tree sha: %q", tree.SHA)
	}
	if len(tree.Tree) != 1 {
		t.Fatalf("unexpected tree entry count: %d", len(tree.Tree))
	}
	if tree.Tree[0].Path != "a.txt" || tree.Tree[0].SHA != "blob1" || tree.Tree[0].Type != "blob" || tree.Tree[0].Size != 5 {
		t.Fatalf("unexpected tree entry: %+v", tree.Tree[0])
	}
}

func TestFetchGitHubBlobDecode(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("hello github"))
	wrapped := encoded[:8] + "\n" + encoded[8:]

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/octo/repo/git/blobs/blob123" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Fatalf("unexpected Accept header: %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("expected no Authorization header, got %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, "{\"sha\":\"blob123\",\"content\":%q,\"encoding\":\"base64\"}", wrapped)
	}))
	defer ts.Close()

	orig := githubAPIBaseURL
	githubAPIBaseURL = ts.URL
	t.Cleanup(func() { githubAPIBaseURL = orig })

	blob, err := fetchGitHubBlob(ts.Client(), "octo", "repo", "blob123", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if string(blob) != "hello github" {
		t.Fatalf("unexpected blob contents: %q", string(blob))
	}
}

func TestFetchGitHubRateLimit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	orig := githubAPIBaseURL
	githubAPIBaseURL = ts.URL
	t.Cleanup(func() { githubAPIBaseURL = orig })

	_, err := fetchGitHubTree(ts.Client(), "octo", "repo", "tree123", "")
	if err == nil {
		t.Fatal("expected error")
	}
	if got, want := err.Error(), "GitHub API rate limited — set GITHUB_TOKEN env var for 5000 req/hr"; got != want {
		t.Fatalf("unexpected error: got %q want %q", got, want)
	}
}

func TestFetchGitHubNotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	orig := githubAPIBaseURL
	githubAPIBaseURL = ts.URL
	t.Cleanup(func() { githubAPIBaseURL = orig })

	_, err := fetchGitHubBlob(ts.Client(), "octo", "repo", "missing", "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got %q", err.Error())
	}
}

func TestFetchGitHub403RateLimit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()

	orig := githubAPIBaseURL
	githubAPIBaseURL = ts.URL
	t.Cleanup(func() { githubAPIBaseURL = orig })

	_, err := fetchGitHubTree(ts.Client(), "octo", "repo", "tree123", "")
	if err == nil {
		t.Fatal("expected error")
	}
	if got, want := err.Error(), "GitHub API rate limited — set GITHUB_TOKEN env var for 5000 req/hr"; got != want {
		t.Fatalf("unexpected error: got %q want %q", got, want)
	}
}

func TestFetchGitHubTreeTruncated(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sha":"tree123","tree":[],"truncated":true}`))
	}))
	defer ts.Close()

	orig := githubAPIBaseURL
	githubAPIBaseURL = ts.URL
	t.Cleanup(func() { githubAPIBaseURL = orig })

	tree, err := fetchGitHubTree(ts.Client(), "octo", "repo", "tree123", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !tree.Truncated {
		t.Fatal("expected truncated tree response")
	}
}
