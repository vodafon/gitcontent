package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

type Worker struct {
	outDir          string
	verbose         int
	maxCloneSeconds int
	maxOutputBytes  int64
	l               *slog.Logger
}

type repoSpec struct {
	URL      string
	Host     string
	Owner    string
	Repo     string
	SafeName string
}

var (
	plainCloneContext = git.PlainCloneContext
	makeTempDir       = os.MkdirTemp
)

func (obj *Worker) Repo(repoInput string) error {
	spec, err := parseRepoInput(repoInput)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(obj.outDir, os.ModePerm); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	cloneDir, err := makeTempDir("", "gitcontent-*")
	if err != nil {
		return fmt.Errorf("create clone temp directory: %w", err)
	}
	defer func() {
		if removeErr := os.RemoveAll(cloneDir); removeErr != nil {
			obj.log(1, "failed to remove temporary clone directory", "path", cloneDir, "error", removeErr)
		}
	}()

	repoDir := filepath.Join(cloneDir, uniqueRepoDirName(spec.URL, spec.SafeName))
	repo, err := obj.cloneRepo(spec.URL, repoDir)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			obj.log(1, "skipping repository because clone timeout exceeded", "repo", spec.URL, "max_clone_seconds", obj.maxCloneSeconds)
			return nil
		}
		return fmt.Errorf("clone repository: %w", err)
	}

	outputPath := filepath.Join(obj.outDir, spec.SafeName+"_content.txt")
	file, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}
	defer file.Close()

	writer := newBudgetWriter(file, obj.maxOutputBytes)

	if _, err := io.WriteString(writer, spec.URL+"\n"); err != nil {
		if errors.Is(err, errOutputLimitReached) {
			obj.log(1, "stopping repository because output limit reached", "repo", spec.URL, "max_output_bytes", obj.maxOutputBytes)
			return nil
		}
		return fmt.Errorf("write output header: %w", err)
	}

	if err := obj.processAllRefs(repo, writer); err != nil {
		if errors.Is(err, errOutputLimitReached) {
			obj.log(1, "stopping repository because output limit reached", "repo", spec.URL, "written_bytes", writer.Written(), "max_output_bytes", obj.maxOutputBytes)
			return nil
		}
		return fmt.Errorf("process refs: %w", err)
	}

	obj.log(2, "repository processed", "repo", spec.URL, "output", outputPath)
	return nil
}

func (obj *Worker) cloneRepo(repoURL, repoDir string) (*git.Repository, error) {
	obj.log(2, "cloning repository", "repo", repoURL)

	ctx := context.Background()
	if obj.maxCloneSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(obj.maxCloneSeconds)*time.Second)
		defer cancel()
	}

	return plainCloneContext(ctx, repoDir, false, &git.CloneOptions{
		URL:               repoURL,
		Progress:          os.Stdout,
		Depth:             0,
		SingleBranch:      false,
		Tags:              git.AllTags,
		RecurseSubmodules: git.NoRecurseSubmodules,
	})
}

func (obj *Worker) processAllRefs(repo *git.Repository, output io.Writer) error {
	refs, err := repo.References()
	if err != nil {
		return err
	}

	processedCommits := make(map[string]bool)
	processedBlobs := make(map[string]bool)

	return refs.ForEach(func(ref *plumbing.Reference) error {
		if ref == nil || ref.Hash().IsZero() {
			return nil
		}

		if ref.Type() != plumbing.HashReference {
			return nil
		}

		startHash, err := resolveReferenceCommitHash(repo, ref)
		if err != nil {
			obj.log(2, "skipping reference that cannot resolve to commit", "ref", ref.Name().String(), "hash", ref.Hash().String(), "error", err)
			return nil
		}

		obj.log(3, "processing reference", "ref", ref.Name().String(), "hash", startHash.String())
		return obj.processCommitsFromRef(repo, startHash, output, processedCommits, processedBlobs)
	})
}

func resolveReferenceCommitHash(repo *git.Repository, ref *plumbing.Reference) (plumbing.Hash, error) {
	if repo == nil || ref == nil {
		return plumbing.ZeroHash, errors.New("repository or reference is nil")
	}

	current := ref.Hash()
	if current.IsZero() {
		return plumbing.ZeroHash, errors.New("reference hash is zero")
	}

	for i := 0; i < 10; i++ {
		if _, err := repo.CommitObject(current); err == nil {
			return current, nil
		}

		tagObject, err := repo.TagObject(current)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("hash %s is neither commit nor tag object", current.String())
		}

		current = tagObject.Target
		if current.IsZero() {
			return plumbing.ZeroHash, fmt.Errorf("tag %s has zero target", tagObject.Name)
		}
	}

	return plumbing.ZeroHash, fmt.Errorf("tag chain too deep for reference %s", ref.Name().String())
}

func (obj *Worker) processCommitsFromRef(
	repo *git.Repository,
	from plumbing.Hash,
	output io.Writer,
	processedCommits map[string]bool,
	processedBlobs map[string]bool,
) error {
	commitIter, err := repo.Log(&git.LogOptions{From: from})
	if err != nil {
		return err
	}
	defer commitIter.Close()

	return commitIter.ForEach(func(commit *object.Commit) error {
		commitKey := commit.Hash.String()
		if processedCommits[commitKey] {
			return nil
		}
		processedCommits[commitKey] = true

		return obj.saveCommitFiles(commit, output, processedBlobs)
	})
}

func (obj *Worker) saveCommitFiles(commit *object.Commit, output io.Writer, processedBlobs map[string]bool) error {
	tree, err := commit.Tree()
	if err != nil {
		return err
	}

	return tree.Files().ForEach(func(f *object.File) error {
		blobHash := f.Hash.String()
		if processedBlobs[blobHash] {
			return nil
		}

		content, err := readBlobContent(&f.Blob)
		if err != nil {
			return fmt.Errorf("read blob %s (%s): %w", blobHash, f.Name, err)
		}

		if isBinary(content) {
			obj.log(3, "skipping binary file", "path", f.Name, "blob", blobHash)
			processedBlobs[blobHash] = true
			return nil
		}

		header := fmt.Sprintf("\n==== Blob %s | Commit %s | Path %s ====\n", blobHash, commit.Hash.String(), f.Name)
		if _, err := io.WriteString(output, header); err != nil {
			return err
		}
		if _, err := output.Write(content); err != nil {
			return err
		}
		if _, err := io.WriteString(output, "\n"); err != nil {
			return err
		}

		processedBlobs[blobHash] = true
		return nil
	})
}

func readBlobContent(blob *object.Blob) ([]byte, error) {
	reader, err := blob.Reader()
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	return io.ReadAll(reader)
}

func isBinary(content []byte) bool {
	if len(content) == 0 {
		return false
	}

	if bytes.IndexByte(content, 0x00) >= 0 {
		return true
	}

	if !utf8.Valid(content) {
		return true
	}

	var control int
	for _, b := range content {
		if b < 0x20 && b != '\n' && b != '\r' && b != '\t' {
			control++
		}
	}

	return float64(control)/float64(len(content)) > 0.3
}

func parseRepoInput(repoInput string) (repoSpec, error) {
	trimmed := strings.TrimSpace(repoInput)
	if trimmed == "" {
		return repoSpec{}, errors.New("empty repository input")
	}

	normalized := normalizeRepoURL(trimmed)
	parsed, err := url.Parse(normalized)
	if err != nil {
		return repoSpec{}, fmt.Errorf("parse repository input %q: %w", repoInput, err)
	}

	if parsed.Host == "" {
		return repoSpec{}, fmt.Errorf("repository input %q is missing host", repoInput)
	}

	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 2 {
		return repoSpec{}, fmt.Errorf("repository input %q must include owner and repo", repoInput)
	}

	owner := parts[0]
	repo := strings.TrimSuffix(parts[1], ".git")
	if owner == "" || repo == "" {
		return repoSpec{}, fmt.Errorf("repository input %q has invalid owner/repo", repoInput)
	}

	host := strings.ToLower(parsed.Host)
	canonical := fmt.Sprintf("https://%s/%s/%s", host, owner, repo)
	safeHost := sanitizeFilePart(host)
	safeName := fmt.Sprintf("%s_%s_%s", safeHost, sanitizeFilePart(owner), sanitizeFilePart(repo))

	return repoSpec{
		URL:      canonical,
		Host:     host,
		Owner:    owner,
		Repo:     repo,
		SafeName: safeName,
	}, nil
}

func normalizeRepoURL(raw string) string {
	if strings.HasPrefix(raw, "git@") {
		withoutPrefix := strings.TrimPrefix(raw, "git@")
		parts := strings.SplitN(withoutPrefix, ":", 2)
		if len(parts) == 2 {
			return "https://" + parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")
		}
	}

	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return raw
	}

	return "https://" + raw
}

func sanitizeFilePart(part string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "?", "_", "*", "_", "\"", "_", "<", "_", ">", "_", "|", "_", ".", "-")
	cleaned := replacer.Replace(strings.TrimSpace(part))
	if cleaned == "" {
		return "unknown"
	}
	return cleaned
}

func uniqueRepoDirName(repoURL, safeName string) string {
	sum := sha1.Sum([]byte(repoURL))
	return fmt.Sprintf("%s_%s", safeName, hex.EncodeToString(sum[:6]))
}

var errOutputLimitReached = errors.New("output limit reached")

type budgetWriter struct {
	w     io.Writer
	limit int64
	n     int64
}

func newBudgetWriter(w io.Writer, limit int64) *budgetWriter {
	return &budgetWriter{w: w, limit: limit}
}

func (w *budgetWriter) Write(p []byte) (int, error) {
	if w.limit > 0 && w.n+int64(len(p)) > w.limit {
		return 0, errOutputLimitReached
	}
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

func (w *budgetWriter) Written() int64 {
	return w.n
}

func (obj *Worker) log(level int, msg string, args ...any) {
	if obj == nil || obj.l == nil {
		return
	}
	if obj.verbose < level {
		return
	}
	obj.l.Info(msg, args...)
}
