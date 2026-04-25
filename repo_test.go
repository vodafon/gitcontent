package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestParseRepoInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		wantErr   bool
		wantURL   string
		wantSafe  string
		wantOwner string
		wantRepo  string
	}{
		{
			name:      "https github",
			input:     "https://github.com/vodafon/gitcontent",
			wantURL:   "https://github.com/vodafon/gitcontent",
			wantSafe:  "github-com_vodafon_gitcontent",
			wantOwner: "vodafon",
			wantRepo:  "gitcontent",
		},
		{
			name:      "ssh input normalized",
			input:     "git@github.com:vodafon/gitcontent.git",
			wantURL:   "https://github.com/vodafon/gitcontent",
			wantSafe:  "github-com_vodafon_gitcontent",
			wantOwner: "vodafon",
			wantRepo:  "gitcontent",
		},
		{
			name:    "invalid short",
			input:   "foo",
			wantErr: true,
		},
		{
			name:    "invalid extra segments",
			input:   "https://github.com/vodafon/gitcontent/tree/main",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spec, err := parseRepoInput(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tt.input)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if spec.URL != tt.wantURL {
				t.Fatalf("URL mismatch: got %q want %q", spec.URL, tt.wantURL)
			}
			if spec.SafeName != tt.wantSafe {
				t.Fatalf("SafeName mismatch: got %q want %q", spec.SafeName, tt.wantSafe)
			}
			if spec.Owner != tt.wantOwner || spec.Repo != tt.wantRepo {
				t.Fatalf("owner/repo mismatch: got %s/%s want %s/%s", spec.Owner, spec.Repo, tt.wantOwner, tt.wantRepo)
			}
		})
	}
}

func TestIsBinary(t *testing.T) {
	t.Parallel()

	if isBinary([]byte("package main\nfunc main(){}\n")) {
		t.Fatal("expected text to be non-binary")
	}

	if !isBinary([]byte{0x00, 0x01, 0x02}) {
		t.Fatal("expected NUL bytes to be detected as binary")
	}

	if !isBinary([]byte{0xff, 0xfe, 0xfd}) {
		t.Fatal("expected invalid UTF-8 bytes to be detected as binary")
	}
}

func TestWorkerRepoRejectsMalformedInput(t *testing.T) {
	t.Parallel()

	worker := &Worker{
		outDir:          t.TempDir(),
		verbose:         1,
		maxCloneSeconds: 0,
		maxOutputBytes:  0,
		l:               slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	err := worker.Repo("foo")
	if err == nil {
		t.Fatal("expected error for malformed repo input")
	}

	if !strings.Contains(err.Error(), "must include owner and repo") && !strings.Contains(err.Error(), "missing host") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBudgetWriter(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	w := newBudgetWriter(&b, 5)

	n, err := w.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}
	if n != 5 {
		t.Fatalf("unexpected byte count: got %d want 5", n)
	}

	_, err = w.Write([]byte("!"))
	if !errors.Is(err, errOutputLimitReached) {
		t.Fatalf("expected output limit error, got: %v", err)
	}
	if w.Written() != 5 {
		t.Fatalf("unexpected written bytes after limit: got %d want 5", w.Written())
	}
}

func TestResolveReferenceCommitHashNil(t *testing.T) {
	t.Parallel()

	_, err := resolveReferenceCommitHash(nil, nil)
	if err == nil {
		t.Fatal("expected error for nil repo/ref")
	}
}

func TestWorkerRepoCloneTimeoutSkipsAndCleansTempDir(t *testing.T) {
	oldClone := plainCloneContext
	oldTemp := makeTempDir

	tempRoot := t.TempDir()
	tempPath := filepath.Join(tempRoot, "clone-temp")

	makeTempDir = func(dir, pattern string) (string, error) {
		return tempPath, os.MkdirAll(tempPath, os.ModePerm)
	}

	plainCloneContext = func(ctx context.Context, path string, isBare bool, o *git.CloneOptions) (*git.Repository, error) {
		<-ctx.Done()
		return nil, context.DeadlineExceeded
	}

	t.Cleanup(func() {
		plainCloneContext = oldClone
		makeTempDir = oldTemp
	})

	worker := &Worker{
		outDir:          t.TempDir(),
		verbose:         1,
		maxCloneSeconds: 1,
		maxOutputBytes:  1024,
		l:               slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	start := time.Now()
	err := worker.Repo("https://github.com/octocat/Hello-World")
	if err != nil {
		t.Fatalf("expected timeout skip without error, got: %v", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("timeout handling took too long: %s", time.Since(start))
	}

	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Fatalf("expected temp clone dir cleanup, stat err: %v", err)
	}
}

func TestWorkerRepoStopsOnOutputBudget(t *testing.T) {
	worker := &Worker{
		outDir:          t.TempDir(),
		verbose:         1,
		maxCloneSeconds: 300,
		maxOutputBytes:  64,
		l:               slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	err := worker.Repo("https://github.com/octocat/Hello-World")
	if err != nil {
		t.Fatalf("expected output budget stop without error, got: %v", err)
	}

	entries, err := os.ReadDir(worker.outDir)
	if err != nil {
		t.Fatalf("read out dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected single output file, got %d", len(entries))
	}

	outputPath := filepath.Join(worker.outDir, entries[0].Name())
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatalf("stat output: %v", err)
	}
	if info.Size() > worker.maxOutputBytes {
		t.Fatalf("output exceeded budget: got %d limit %d", info.Size(), worker.maxOutputBytes)
	}
}

func TestDetectRedactedMarker(t *testing.T) {
	repoDir := t.TempDir()
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	filePath := filepath.Join(repoDir, "secret.txt")
	if err := os.WriteFile(filePath, []byte("password = '***REMOVED***'"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if _, err := wt.Add("secret.txt"); err != nil {
		t.Fatalf("git add: %v", err)
	}

	commit, err := wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "test",
			Email: "test@test.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("git commit: %v", err)
	}

	origClone := plainCloneContext
	plainCloneContext = func(ctx context.Context, path string, isBare bool, o *git.CloneOptions) (*git.Repository, error) {
		return repo, nil
	}
	defer func() { plainCloneContext = origClone }()

	worker := &Worker{
		outDir:          t.TempDir(),
		verbose:         1,
		maxCloneSeconds: 300,
		maxOutputBytes:  1024 * 1024,
		resolveRedacted: true,
		l:               slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	err = worker.Repo("https://github.com/test/repo")
	if err != nil {
		t.Fatalf("worker.Repo: %v", err)
	}

	if len(worker.redactedBlobs) != 1 {
		t.Fatalf("expected 1 redacted blob, got %d", len(worker.redactedBlobs))
	}

	rb := worker.redactedBlobs[0]
	if rb.commitHash != commit.String() {
		t.Fatalf("commit hash mismatch: got %s want %s", rb.commitHash, commit.String())
	}
	if rb.path != "secret.txt" {
		t.Fatalf("path mismatch: got %s want secret.txt", rb.path)
	}
}

func TestDetectRedactedMarkerRedacted(t *testing.T) {
	repoDir := t.TempDir()
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	filePath := filepath.Join(repoDir, "secret.txt")
	if err := os.WriteFile(filePath, []byte("password = '***REDACTED***'"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if _, err := wt.Add("secret.txt"); err != nil {
		t.Fatalf("git add: %v", err)
	}

	commit, err := wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "test",
			Email: "test@test.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("git commit: %v", err)
	}

	origClone := plainCloneContext
	plainCloneContext = func(ctx context.Context, path string, isBare bool, o *git.CloneOptions) (*git.Repository, error) {
		return repo, nil
	}
	defer func() { plainCloneContext = origClone }()

	worker := &Worker{
		outDir:          t.TempDir(),
		verbose:         1,
		maxCloneSeconds: 300,
		maxOutputBytes:  1024 * 1024,
		resolveRedacted: true,
		l:               slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	err = worker.Repo("https://github.com/test/repo")
	if err != nil {
		t.Fatalf("worker.Repo: %v", err)
	}

	if len(worker.redactedBlobs) != 1 {
		t.Fatalf("expected 1 redacted blob for ***REDACTED*** marker, got %d", len(worker.redactedBlobs))
	}

	rb := worker.redactedBlobs[0]
	if rb.commitHash != commit.String() {
		t.Fatalf("commit hash mismatch: got %s want %s", rb.commitHash, commit.String())
	}
	if rb.path != "secret.txt" {
		t.Fatalf("path mismatch: got %s want secret.txt", rb.path)
	}
}

func TestDetectRedactedNoFalsePositive(t *testing.T) {
	repoDir := t.TempDir()
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	filePath := filepath.Join(repoDir, "normal.txt")
	if err := os.WriteFile(filePath, []byte("this is normal content without marker"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if _, err := wt.Add("normal.txt"); err != nil {
		t.Fatalf("git add: %v", err)
	}

	_, err = wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "test",
			Email: "test@test.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("git commit: %v", err)
	}

	origClone := plainCloneContext
	plainCloneContext = func(ctx context.Context, path string, isBare bool, o *git.CloneOptions) (*git.Repository, error) {
		return repo, nil
	}
	defer func() { plainCloneContext = origClone }()

	worker := &Worker{
		outDir:          t.TempDir(),
		verbose:         1,
		maxCloneSeconds: 300,
		maxOutputBytes:  1024 * 1024,
		resolveRedacted: true,
		l:               slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	err = worker.Repo("https://github.com/test/repo")
	if err != nil {
		t.Fatalf("worker.Repo: %v", err)
	}

	if len(worker.redactedBlobs) != 0 {
		t.Fatalf("expected 0 redacted blobs for content without marker, got %d", len(worker.redactedBlobs))
	}
}

func TestDetectRedactedDisabled(t *testing.T) {
	repoDir := t.TempDir()
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	filePath := filepath.Join(repoDir, "secret.txt")
	if err := os.WriteFile(filePath, []byte("password = '***REMOVED***'"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if _, err := wt.Add("secret.txt"); err != nil {
		t.Fatalf("git add: %v", err)
	}

	_, err = wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "test",
			Email: "test@test.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("git commit: %v", err)
	}

	origClone := plainCloneContext
	plainCloneContext = func(ctx context.Context, path string, isBare bool, o *git.CloneOptions) (*git.Repository, error) {
		return repo, nil
	}
	defer func() { plainCloneContext = origClone }()

	worker := &Worker{
		outDir:          t.TempDir(),
		verbose:         1,
		maxCloneSeconds: 300,
		maxOutputBytes:  1024 * 1024,
		resolveRedacted: false,
		l:               slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	err = worker.Repo("https://github.com/test/repo")
	if err != nil {
		t.Fatalf("worker.Repo: %v", err)
	}

	if len(worker.redactedBlobs) != 0 {
		t.Fatalf("expected 0 redacted blobs when resolveRedacted=false, got %d", len(worker.redactedBlobs))
	}
}

func TestDetectRedactedBinarySkip(t *testing.T) {
	repoDir := t.TempDir()
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	filePath := filepath.Join(repoDir, "binary.bin")
	binaryContent := append([]byte{0x00}, []byte("***REMOVED***")...)
	if err := os.WriteFile(filePath, binaryContent, 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if _, err := wt.Add("binary.bin"); err != nil {
		t.Fatalf("git add: %v", err)
	}

	_, err = wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "test",
			Email: "test@test.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("git commit: %v", err)
	}

	origClone := plainCloneContext
	plainCloneContext = func(ctx context.Context, path string, isBare bool, o *git.CloneOptions) (*git.Repository, error) {
		return repo, nil
	}
	defer func() { plainCloneContext = origClone }()

	worker := &Worker{
		outDir:          t.TempDir(),
		verbose:         1,
		maxCloneSeconds: 300,
		maxOutputBytes:  1024 * 1024,
		resolveRedacted: true,
		l:               slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	err = worker.Repo("https://github.com/test/repo")
	if err != nil {
		t.Fatalf("worker.Repo: %v", err)
	}

	if len(worker.redactedBlobs) != 0 {
		t.Fatalf("expected 0 redacted blobs for binary files, got %d", len(worker.redactedBlobs))
	}
}

func TestResolveFullPipeline(t *testing.T) {
	repoDir := t.TempDir()
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	filePath := filepath.Join(repoDir, "config.txt")
	if err := os.WriteFile(filePath, []byte("token = '***REMOVED***'"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := wt.Add("config.txt"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("git commit: %v", err)
	}

	head, _ := repo.Head()
	commit, _ := repo.CommitObject(head.Hash())
	localTree, _ := commit.Tree()
	localFile, _ := localTree.File("config.txt")
	cloneSHA := localFile.Hash.String()
	treeSHA := commit.TreeHash.String()
	apiSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	origClone := plainCloneContext
	plainCloneContext = func(ctx context.Context, path string, isBare bool, o *git.CloneOptions) (*git.Repository, error) {
		return repo, nil
	}
	t.Cleanup(func() { plainCloneContext = origClone })

	origFetchTree := fetchTree
	fetchTree = func(httpClient *http.Client, owner, r, tSHA, token string) (*githubTree, error) {
		if tSHA != treeSHA {
			t.Errorf("unexpected tree SHA: %s", tSHA)
		}
		_ = cloneSHA
		return &githubTree{
			SHA: treeSHA,
			Tree: []githubTreeEntry{
				{Path: "config.txt", SHA: apiSHA, Type: "blob"},
			},
			Truncated: false,
		}, nil
	}
	t.Cleanup(func() { fetchTree = origFetchTree })

	origFetchBlob := fetchBlob
	fetchBlob = func(httpClient *http.Client, owner, r, bSHA, token string) ([]byte, error) {
		if bSHA != apiSHA {
			t.Errorf("unexpected blob SHA: %s", bSHA)
		}
		return []byte("token = 'fake-resolved-content'\n"), nil
	}
	t.Cleanup(func() { fetchBlob = origFetchBlob })

	outDir := t.TempDir()
	worker := &Worker{
		outDir:          outDir,
		verbose:         1,
		maxCloneSeconds: 300,
		maxOutputBytes:  1024 * 1024,
		resolveRedacted: true,
		l:               slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	if err := worker.Repo("https://github.com/test/testrepo"); err != nil {
		t.Fatalf("worker.Repo: %v", err)
	}

	entries, _ := os.ReadDir(outDir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 output file, got %d", len(entries))
	}
	outputBytes, err := os.ReadFile(filepath.Join(outDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	output := string(outputBytes)
	if !strings.Contains(output, "***REMOVED***") {
		t.Errorf("output should contain original redacted content")
	}
	if !strings.Contains(output, "| Resolved") {
		t.Errorf("output should contain resolved header: %s", output)
	}
	if !strings.Contains(output, "fake-resolved-content") {
		t.Errorf("output should contain resolved content: %s", output)
	}
}

func TestResolveNonGitHubSkip(t *testing.T) {
	repoDir := t.TempDir()
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	filePath := filepath.Join(repoDir, "secret.txt")
	if err := os.WriteFile(filePath, []byte("key = '***REMOVED***'"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := wt.Add("secret.txt"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("git commit: %v", err)
	}

	origClone := plainCloneContext
	plainCloneContext = func(ctx context.Context, path string, isBare bool, o *git.CloneOptions) (*git.Repository, error) {
		return repo, nil
	}
	t.Cleanup(func() { plainCloneContext = origClone })

	fetchTreeCalled := false
	origFetchTree := fetchTree
	fetchTree = func(httpClient *http.Client, owner, r, tSHA, token string) (*githubTree, error) {
		fetchTreeCalled = true
		return nil, nil
	}
	t.Cleanup(func() { fetchTree = origFetchTree })

	worker := &Worker{
		outDir:          t.TempDir(),
		verbose:         1,
		maxCloneSeconds: 300,
		maxOutputBytes:  1024 * 1024,
		resolveRedacted: true,
		l:               slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	if err := worker.Repo("https://gitlab.com/test/testrepo"); err != nil {
		t.Fatalf("worker.Repo: %v", err)
	}

	if fetchTreeCalled {
		t.Fatal("fetchTree should not be called for non-github.com host")
	}
}

func TestResolveSHAMatchSkip(t *testing.T) {
	repoDir := t.TempDir()
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	filePath := filepath.Join(repoDir, "note.txt")
	if err := os.WriteFile(filePath, []byte("status = '***REMOVED***'"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := wt.Add("note.txt"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("git commit: %v", err)
	}

	head, _ := repo.Head()
	commit, _ := repo.CommitObject(head.Hash())
	localTree, _ := commit.Tree()
	localFile, _ := localTree.File("note.txt")
	cloneSHA := localFile.Hash.String()
	treeSHA := commit.TreeHash.String()

	origClone := plainCloneContext
	plainCloneContext = func(ctx context.Context, path string, isBare bool, o *git.CloneOptions) (*git.Repository, error) {
		return repo, nil
	}
	t.Cleanup(func() { plainCloneContext = origClone })

	origFetchTree := fetchTree
	fetchTree = func(httpClient *http.Client, owner, r, tSHA, token string) (*githubTree, error) {
		_ = treeSHA
		return &githubTree{
			SHA: tSHA,
			Tree: []githubTreeEntry{
				{Path: "note.txt", SHA: cloneSHA, Type: "blob"},
			},
		}, nil
	}
	t.Cleanup(func() { fetchTree = origFetchTree })

	fetchBlobCalled := false
	origFetchBlob := fetchBlob
	fetchBlob = func(httpClient *http.Client, owner, r, bSHA, token string) ([]byte, error) {
		fetchBlobCalled = true
		return nil, nil
	}
	t.Cleanup(func() { fetchBlob = origFetchBlob })

	outDir := t.TempDir()
	worker := &Worker{
		outDir:          outDir,
		verbose:         1,
		maxCloneSeconds: 300,
		maxOutputBytes:  1024 * 1024,
		resolveRedacted: true,
		l:               slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	if err := worker.Repo("https://github.com/test/testrepo"); err != nil {
		t.Fatalf("worker.Repo: %v", err)
	}

	if fetchBlobCalled {
		t.Fatal("fetchBlob should not be called when clone SHA matches API SHA (false positive)")
	}

	entries, _ := os.ReadDir(outDir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 output file, got %d", len(entries))
	}
	outputBytes, _ := os.ReadFile(filepath.Join(outDir, entries[0].Name()))
	if strings.Contains(string(outputBytes), "| Resolved") {
		t.Error("output should not contain resolved header for false positive")
	}
}

func TestResolveBudgetEnforcement(t *testing.T) {
	repoDir := t.TempDir()
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	filePath := filepath.Join(repoDir, "creds.txt")
	if err := os.WriteFile(filePath, []byte("pass = '***REMOVED***'"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := wt.Add("creds.txt"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("git commit: %v", err)
	}

	head, _ := repo.Head()
	commit, _ := repo.CommitObject(head.Hash())
	treeSHA := commit.TreeHash.String()
	apiSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	origClone := plainCloneContext
	plainCloneContext = func(ctx context.Context, path string, isBare bool, o *git.CloneOptions) (*git.Repository, error) {
		return repo, nil
	}
	t.Cleanup(func() { plainCloneContext = origClone })

	origFetchTree := fetchTree
	fetchTree = func(httpClient *http.Client, owner, r, tSHA, token string) (*githubTree, error) {
		_ = treeSHA
		return &githubTree{
			SHA: tSHA,
			Tree: []githubTreeEntry{
				{Path: "creds.txt", SHA: apiSHA, Type: "blob"},
			},
		}, nil
	}
	t.Cleanup(func() { fetchTree = origFetchTree })

	origFetchBlob := fetchBlob
	fetchBlob = func(httpClient *http.Client, owner, r, bSHA, token string) ([]byte, error) {
		return []byte("fake-resolved-value-for-budget-test\n"), nil
	}
	t.Cleanup(func() { fetchBlob = origFetchBlob })

	worker := &Worker{
		outDir:          t.TempDir(),
		verbose:         1,
		maxCloneSeconds: 300,
		maxOutputBytes:  80,
		resolveRedacted: true,
		l:               slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	err = worker.Repo("https://github.com/test/testrepo")
	if err != nil {
		t.Fatalf("expected no error even when budget exceeded, got: %v", err)
	}
}

func TestRateLimitStopsAll(t *testing.T) {
	repoDir := t.TempDir()
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "file1.txt"), []byte("val = '***REMOVED***'"), 0644); err != nil {
		t.Fatalf("write file1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "file2.txt"), []byte("key = '***REMOVED***'"), 0644); err != nil {
		t.Fatalf("write file2: %v", err)
	}
	if _, err := wt.Add("file1.txt"); err != nil {
		t.Fatalf("git add file1: %v", err)
	}
	if _, err := wt.Add("file2.txt"); err != nil {
		t.Fatalf("git add file2: %v", err)
	}
	if _, err := wt.Commit("initial", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t.com", When: time.Now()}}); err != nil {
		t.Fatalf("git commit: %v", err)
	}

	origClone := plainCloneContext
	plainCloneContext = func(ctx context.Context, path string, isBare bool, o *git.CloneOptions) (*git.Repository, error) {
		return repo, nil
	}
	t.Cleanup(func() { plainCloneContext = origClone })

	fetchTreeCallCount := 0
	fetchBlobCallCount := 0
	origFetchTree := fetchTree
	fetchTree = func(httpClient *http.Client, owner, r, tSHA, token string) (*githubTree, error) {
		fetchTreeCallCount++
		return nil, fmt.Errorf("GitHub API rate limited — set GITHUB_TOKEN env var for 5000 req/hr")
	}
	t.Cleanup(func() { fetchTree = origFetchTree })

	origFetchBlob := fetchBlob
	fetchBlob = func(httpClient *http.Client, owner, r, bSHA, token string) ([]byte, error) {
		fetchBlobCallCount++
		return []byte("should-not-be-called\n"), nil
	}
	t.Cleanup(func() { fetchBlob = origFetchBlob })

	outDir := t.TempDir()
	worker := &Worker{outDir: outDir, verbose: 1, maxCloneSeconds: 300, maxOutputBytes: 1024 * 1024, resolveRedacted: true, l: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	if err := worker.Repo("https://github.com/owner/testrepo"); err != nil {
		t.Fatalf("worker.Repo: %v", err)
	}
	if fetchTreeCallCount != 1 {
		t.Fatalf("expected one tree fetch before rate limit stop, got %d", fetchTreeCallCount)
	}
	if fetchBlobCallCount != 0 {
		t.Fatalf("expected no blob fetches after rate limit, got %d", fetchBlobCallCount)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read out dir: %v", err)
	}
	outputBytes, err := os.ReadFile(filepath.Join(outDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if strings.Contains(string(outputBytes), "| Resolved") {
		t.Fatalf("output should not contain resolved blobs after rate limit: %s", string(outputBytes))
	}
}

func TestEdgeBlobAPIPartialFailure(t *testing.T) {
	repoDir := t.TempDir()
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "first.txt"), []byte("val = '***REMOVED***'"), 0644); err != nil {
		t.Fatalf("write first: %v", err)
	}
	if _, err := wt.Add("first.txt"); err != nil {
		t.Fatalf("git add first: %v", err)
	}
	if _, err := wt.Commit("first", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t.com", When: time.Now()}}); err != nil {
		t.Fatalf("git commit first: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "second.txt"), []byte("key = '***REMOVED***'"), 0644); err != nil {
		t.Fatalf("write second: %v", err)
	}
	if _, err := wt.Add("second.txt"); err != nil {
		t.Fatalf("git add second: %v", err)
	}
	if _, err := wt.Commit("second", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t.com", When: time.Now()}}); err != nil {
		t.Fatalf("git commit second: %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("repo head: %v", err)
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("commit object: %v", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	firstFile, err := tree.File("first.txt")
	if err != nil {
		t.Fatalf("tree file first: %v", err)
	}
	secondFile, err := tree.File("second.txt")
	if err != nil {
		t.Fatalf("tree file second: %v", err)
	}
	apiFirstSHA := "1111111111111111111111111111111111111111"
	apiSecondSHA := "2222222222222222222222222222222222222222"

	origClone := plainCloneContext
	plainCloneContext = func(ctx context.Context, path string, isBare bool, o *git.CloneOptions) (*git.Repository, error) {
		return repo, nil
	}
	t.Cleanup(func() { plainCloneContext = origClone })

	origFetchTree := fetchTree
	fetchTree = func(httpClient *http.Client, owner, r, tSHA, token string) (*githubTree, error) {
		return &githubTree{SHA: tSHA, Tree: []githubTreeEntry{{Path: "first.txt", SHA: apiFirstSHA, Type: "blob"}, {Path: "second.txt", SHA: apiSecondSHA, Type: "blob"}}}, nil
	}
	t.Cleanup(func() { fetchTree = origFetchTree })

	fetchBlobCalls := 0
	origFetchBlob := fetchBlob
	fetchBlob = func(httpClient *http.Client, owner, r, bSHA, token string) ([]byte, error) {
		fetchBlobCalls++
		if fetchBlobCalls == 1 {
			return nil, fmt.Errorf("GitHub API server error (HTTP 500)")
		}
		return []byte("resolved-content\n"), nil
	}
	t.Cleanup(func() { fetchBlob = origFetchBlob })

	outDir := t.TempDir()
	worker := &Worker{outDir: outDir, verbose: 1, maxCloneSeconds: 300, maxOutputBytes: 1024 * 1024, resolveRedacted: true, l: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	if firstFile.Hash.String() == apiFirstSHA || secondFile.Hash.String() == apiSecondSHA {
		t.Fatal("test setup requires API SHAs to differ from clone SHAs")
	}
	if err := worker.Repo("https://github.com/owner/testrepo"); err != nil {
		t.Fatalf("worker.Repo: %v", err)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read out dir: %v", err)
	}
	outputBytes, err := os.ReadFile(filepath.Join(outDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	output := string(outputBytes)
	if strings.Contains(output, fmt.Sprintf("==== Blob %s | Commit %s | Path first.txt | Resolved ====", apiFirstSHA, worker.redactedBlobs[0].commitHash)) {
		t.Fatalf("first blob should not resolve after blob API failure: %s", output)
	}
	if !strings.Contains(output, "==== Blob "+apiSecondSHA) || !strings.Contains(output, "Path second.txt | Resolved") || !strings.Contains(output, "resolved-content") {
		t.Fatalf("second blob should resolve successfully: %s", output)
	}
}

func TestEdgeBlobDedup(t *testing.T) {
	repoDir := t.TempDir()
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	files := []string{"one.txt", "two.txt", "three.txt"}
	for i, name := range files {
		content := fmt.Sprintf("secret%d = '***REMOVED***'", i+1)
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if _, err := wt.Add(name); err != nil {
			t.Fatalf("git add %s: %v", name, err)
		}
	}
	if _, err := wt.Commit("initial", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t.com", When: time.Now()}}); err != nil {
		t.Fatalf("git commit: %v", err)
	}

	origClone := plainCloneContext
	plainCloneContext = func(ctx context.Context, path string, isBare bool, o *git.CloneOptions) (*git.Repository, error) {
		return repo, nil
	}
	t.Cleanup(func() { plainCloneContext = origClone })

	apiSHA := "cccccccccccccccccccccccccccccccccccccccc"
	origFetchTree := fetchTree
	fetchTree = func(httpClient *http.Client, owner, r, tSHA, token string) (*githubTree, error) {
		return &githubTree{SHA: tSHA, Tree: []githubTreeEntry{{Path: "one.txt", SHA: apiSHA, Type: "blob"}, {Path: "two.txt", SHA: apiSHA, Type: "blob"}, {Path: "three.txt", SHA: apiSHA, Type: "blob"}}}, nil
	}
	t.Cleanup(func() { fetchTree = origFetchTree })

	fetchBlobCallCount := 0
	origFetchBlob := fetchBlob
	fetchBlob = func(httpClient *http.Client, owner, r, bSHA, token string) ([]byte, error) {
		fetchBlobCallCount++
		return []byte("deduped-content\n"), nil
	}
	t.Cleanup(func() { fetchBlob = origFetchBlob })

	outDir := t.TempDir()
	worker := &Worker{outDir: outDir, verbose: 1, maxCloneSeconds: 300, maxOutputBytes: 1024 * 1024, resolveRedacted: true, l: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	if err := worker.Repo("https://github.com/owner/testrepo"); err != nil {
		t.Fatalf("worker.Repo: %v", err)
	}
	if fetchBlobCallCount != 1 {
		t.Fatalf("expected fetchBlob to be called once for dedup, got %d", fetchBlobCallCount)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read out dir: %v", err)
	}
	outputBytes, err := os.ReadFile(filepath.Join(outDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if count := strings.Count(string(outputBytes), "| Resolved ===="); count != 3 {
		t.Fatalf("expected 3 resolved sections, got %d: %s", count, string(outputBytes))
	}
}

func TestEdgeRedactedBlobsResetPerRepo(t *testing.T) {
	firstRepoDir := t.TempDir()
	firstRepo, err := git.PlainInit(firstRepoDir, false)
	if err != nil {
		t.Fatalf("git init first: %v", err)
	}
	firstWT, err := firstRepo.Worktree()
	if err != nil {
		t.Fatalf("worktree first: %v", err)
	}
	if err := os.WriteFile(filepath.Join(firstRepoDir, "secret.txt"), []byte("password = '***REMOVED***'"), 0644); err != nil {
		t.Fatalf("write first repo file: %v", err)
	}
	if _, err := firstWT.Add("secret.txt"); err != nil {
		t.Fatalf("git add first repo: %v", err)
	}
	if _, err := firstWT.Commit("first", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t.com", When: time.Now()}}); err != nil {
		t.Fatalf("git commit first repo: %v", err)
	}

	secondRepoDir := t.TempDir()
	secondRepo, err := git.PlainInit(secondRepoDir, false)
	if err != nil {
		t.Fatalf("git init second: %v", err)
	}
	secondWT, err := secondRepo.Worktree()
	if err != nil {
		t.Fatalf("worktree second: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secondRepoDir, "normal.txt"), []byte("normal content"), 0644); err != nil {
		t.Fatalf("write second repo file: %v", err)
	}
	if _, err := secondWT.Add("normal.txt"); err != nil {
		t.Fatalf("git add second repo: %v", err)
	}
	if _, err := secondWT.Commit("second", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@t.com", When: time.Now()}}); err != nil {
		t.Fatalf("git commit second repo: %v", err)
	}

	repos := []*git.Repository{firstRepo, secondRepo}
	repoIndex := 0
	origClone := plainCloneContext
	plainCloneContext = func(ctx context.Context, path string, isBare bool, o *git.CloneOptions) (*git.Repository, error) {
		repo := repos[repoIndex]
		repoIndex++
		return repo, nil
	}
	t.Cleanup(func() { plainCloneContext = origClone })

	fetchTreeCalls := 0
	origFetchTree := fetchTree
	fetchTree = func(httpClient *http.Client, owner, r, tSHA, token string) (*githubTree, error) {
		fetchTreeCalls++
		return nil, fmt.Errorf("GitHub API rate limited — set GITHUB_TOKEN env var for 5000 req/hr")
	}
	t.Cleanup(func() { fetchTree = origFetchTree })

	outDir := t.TempDir()
	worker := &Worker{outDir: outDir, verbose: 1, maxCloneSeconds: 300, maxOutputBytes: 1024 * 1024, resolveRedacted: true, l: slog.New(slog.NewTextHandler(os.Stderr, nil))}

	if err := worker.Repo("https://github.com/owner/first"); err != nil {
		t.Fatalf("first worker.Repo: %v", err)
	}
	if len(worker.redactedBlobs) == 0 {
		t.Fatal("expected first repo to populate redactedBlobs")
	}
	if err := worker.Repo("https://github.com/owner/second"); err != nil {
		t.Fatalf("second worker.Repo: %v", err)
	}
	if len(worker.redactedBlobs) != 0 {
		t.Fatalf("expected redactedBlobs reset for second repo, got %d", len(worker.redactedBlobs))
	}
	if fetchTreeCalls != 1 {
		t.Fatalf("expected fetchTree called only for first repo, got %d", fetchTreeCalls)
	}
}
