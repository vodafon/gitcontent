package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
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
