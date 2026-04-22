package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

var githubAPIBaseURL = "https://api.github.com"

type githubTreeEntry struct {
	Path string `json:"path"`
	SHA  string `json:"sha"`
	Type string `json:"type"`
	Size int    `json:"size"`
}

type githubTree struct {
	SHA       string            `json:"sha"`
	Tree      []githubTreeEntry `json:"tree"`
	Truncated bool              `json:"truncated"`
}

type githubBlob struct {
	SHA      string `json:"sha"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

var fetchTree func(httpClient *http.Client, owner, repo, treeSHA, token string) (*githubTree, error) = fetchGitHubTree
var fetchBlob func(httpClient *http.Client, owner, repo, blobSHA, token string) ([]byte, error) = fetchGitHubBlob
var fetchEvents func(httpClient *http.Client, owner, repo, token string, page int) ([]githubEvent, error) = fetchGitHubEvents
var fetchCommitsFromSHA func(httpClient *http.Client, owner, repo, sha, token string) ([]githubCommitListItem, error) = fetchGitHubCommitsFromSHA

type githubEvent struct {
	Type    string `json:"type"`
	Payload struct {
		Ref    string `json:"ref"`
		Head   string `json:"head"`
		Before string `json:"before"`
	} `json:"payload"`
}

type githubCommitListItem struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Date string `json:"date"`
		} `json:"author"`
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	} `json:"commit"`
}

func fetchGitHubTree(httpClient *http.Client, owner, repo, treeSHA, token string) (*githubTree, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/git/trees/%s?recursive=1", githubAPIBaseURL, owner, repo, treeSHA)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := handleGitHubAPIError(resp); err != nil {
		return nil, err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var tree githubTree
	if err := json.Unmarshal(body, &tree); err != nil {
		return nil, err
	}

	return &tree, nil
}

func fetchGitHubBlob(httpClient *http.Client, owner, repo, blobSHA, token string) ([]byte, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/git/blobs/%s", githubAPIBaseURL, owner, repo, blobSHA)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := handleGitHubAPIError(resp); err != nil {
		return nil, err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var blob githubBlob
	if err := json.Unmarshal(body, &blob); err != nil {
		return nil, err
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(blob.Content, "\n", ""))
	if err != nil {
		return nil, err
	}

	return decoded, nil
}

func fetchGitHubEvents(httpClient *http.Client, owner, repo, token string, page int) ([]githubEvent, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/events?per_page=100&page=%d", githubAPIBaseURL, owner, repo, page)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := handleGitHubAPIError(resp); err != nil {
		return nil, err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var events []githubEvent
	if err := json.Unmarshal(body, &events); err != nil {
		return nil, err
	}

	return events, nil
}

func fetchGitHubCommitsFromSHA(httpClient *http.Client, owner, repo, sha, token string) ([]githubCommitListItem, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/commits?per_page=100&sha=%s", githubAPIBaseURL, owner, repo, url.QueryEscape(sha))

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := handleGitHubAPIError(resp); err != nil {
		return nil, err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var commits []githubCommitListItem
	if err := json.Unmarshal(body, &commits); err != nil {
		return nil, err
	}

	return commits, nil
}

func handleGitHubAPIError(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("GitHub API: invalid token (HTTP 401)")
	case http.StatusForbidden:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return fmt.Errorf("GitHub API rate limited — set GITHUB_TOKEN env var for 5000 req/hr")
		}
		return fmt.Errorf("GitHub API: forbidden (check token permissions) (HTTP 403)")
	case http.StatusNotFound:
		return fmt.Errorf("GitHub API: repo/tree not found (HTTP 404)")
	case http.StatusTooManyRequests:
		return fmt.Errorf("GitHub API rate limited — set GITHUB_TOKEN env var for 5000 req/hr")
	}

	if resp.StatusCode >= 500 && resp.StatusCode < 600 {
		return fmt.Errorf("GitHub API server error (HTTP %d)", resp.StatusCode)
	}

	return fmt.Errorf("GitHub API request failed (HTTP %d)", resp.StatusCode)
}
