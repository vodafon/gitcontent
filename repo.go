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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

type redactedBlob struct {
	blobHash   string
	commitHash string
	path       string
}

type Worker struct {
	outDir          string
	verbose         int
	maxCloneSeconds int
	maxOutputBytes  int64
	insecure        bool
	l               *slog.Logger
	resolveRedacted bool
	githubToken     string
	split           bool
	splitDir        string
	redactedBlobs   []redactedBlob
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
	obj.redactedBlobs = nil

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
	if obj.split {
		obj.splitDir = filepath.Join(obj.outDir, spec.SafeName)
		if err := os.RemoveAll(obj.splitDir); err != nil {
			return fmt.Errorf("remove split directory: %w", err)
		}
		if err := os.MkdirAll(obj.splitDir, os.ModePerm); err != nil {
			return fmt.Errorf("create split directory: %w", err)
		}
	}
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

	httpClient := &http.Client{Timeout: 30 * time.Second}
	if err := obj.resolveRedactedBlobs(repo, spec, writer, httpClient); err != nil {
		if errors.Is(err, errOutputLimitReached) {
			obj.log(1, "stopping resolution because output limit reached", "repo", spec.URL)
			return nil
		}
		return fmt.Errorf("resolve redacted blobs: %w", err)
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

	cloneOpts := &git.CloneOptions{
		URL:               repoURL,
		Progress:          os.Stdout,
		Depth:             0,
		SingleBranch:      false,
		Tags:              git.AllTags,
		RecurseSubmodules: git.NoRecurseSubmodules,
	}

	if obj.insecure {
		cloneOpts.InsecureSkipTLS = true
	}

	return plainCloneContext(ctx, repoDir, false, cloneOpts)
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

		if (bytes.Contains(content, []byte("***REMOVED***")) || bytes.Contains(content, []byte("***REDACTED***"))) && obj.resolveRedacted {
			obj.redactedBlobs = append(obj.redactedBlobs, redactedBlob{
				blobHash:   blobHash,
				commitHash: commit.Hash.String(),
				path:       f.Name,
			})
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

		if obj.split {
			shortHash := blobHash[:12]
			splitFileName := shortHash + "_" + filepath.Base(f.Name)
			splitFilePath, err := safeSplitPath(obj.splitDir, filepath.Join(filepath.Dir(f.Name), splitFileName))
			if err != nil {
				obj.log(1, "skipping split file due to unsafe path", "path", f.Name, "error", err)
			} else {
				if err := os.MkdirAll(filepath.Dir(splitFilePath), os.ModePerm); err != nil {
					return fmt.Errorf("create split directory: %w", err)
				}
				if err := os.WriteFile(splitFilePath, content, 0644); err != nil {
					return fmt.Errorf("write split file: %w", err)
				}
			}
		}

		processedBlobs[blobHash] = true
		return nil
	})
}

func (obj *Worker) resolveRedactedBlobs(repo *git.Repository, spec repoSpec, output io.Writer, httpClient *http.Client) error {
	if !obj.resolveRedacted || len(obj.redactedBlobs) == 0 || spec.Host != "github.com" {
		return nil
	}

	resolved, err := obj.resolveRedactedViaOriginalHistory(repo, spec, output, httpClient)
	if err != nil {
		return err
	}
	if resolved {
		return nil
	}

	return obj.resolveRedactedViaCurrentTreeAPI(repo, spec, output, httpClient)
}

func (obj *Worker) resolveRedactedViaOriginalHistory(repo *git.Repository, spec repoSpec, output io.Writer, httpClient *http.Client) (bool, error) {
	rewriteBaseSHA, err := obj.findRewriteBaseSHA(repo, spec, httpClient)
	if err != nil {
		if strings.Contains(err.Error(), "rate limited") {
			obj.log(1, "GitHub API rate limited — set GITHUB_TOKEN env var for 5000 req/hr", "repo", spec.URL)
			return true, nil
		}
		obj.log(1, "failed to discover rewrite base", "repo", spec.URL, "error", err)
		return false, nil
	}

	if rewriteBaseSHA == "" {
		obj.log(2, "no rewrite base found, using tree/blob API resolution", "repo", spec.URL)
		return false, nil
	}

	originalCommits, err := fetchCommitsFromSHA(httpClient, spec.Owner, spec.Repo, rewriteBaseSHA, obj.githubToken)
	if err != nil {
		if strings.Contains(err.Error(), "rate limited") {
			obj.log(1, "GitHub API rate limited — set GITHUB_TOKEN env var for 5000 req/hr", "repo", spec.URL)
			return true, nil
		}
		obj.log(1, "failed to fetch original commit chain", "repo", spec.URL, "base_sha", rewriteBaseSHA, "error", err)
		return false, nil
	}

	originalByIdentity := make(map[string]string)
	for _, c := range originalCommits {
		if c.SHA == "" || c.Commit.Message == "" || c.Commit.Author.Date == "" {
			continue
		}
		tm, parseErr := time.Parse(time.RFC3339, c.Commit.Author.Date)
		if parseErr != nil {
			continue
		}
		identity := commitIdentityKey(c.Commit.Message, tm)
		if _, exists := originalByIdentity[identity]; !exists {
			originalByIdentity[identity] = c.SHA
		}
	}

	blobCache := make(map[string][]byte)
	commitBlobMap := make(map[string][]redactedBlob)
	for _, rb := range obj.redactedBlobs {
		commitBlobMap[rb.commitHash] = append(commitBlobMap[rb.commitHash], rb)
	}

	for cloneCommitSHA, blobs := range commitBlobMap {
		cloneCommitHash := plumbing.NewHash(cloneCommitSHA)
		cloneCommit, cloneErr := repo.CommitObject(cloneCommitHash)
		if cloneErr != nil {
			obj.log(1, "cannot get local commit for redaction resolution", "commit", cloneCommitSHA, "error", cloneErr)
			continue
		}

		identity := commitIdentityKey(cloneCommit.Message, cloneCommit.Author.When)
		originalCommitSHA := originalByIdentity[identity]
		if originalCommitSHA == "" {
			obj.log(2, "no original commit match for redacted commit", "clone_commit", cloneCommitSHA)
			continue
		}

		originalCommitHash := plumbing.NewHash(originalCommitSHA)
		originalCommit, origErr := repo.CommitObject(originalCommitHash)
		if origErr != nil {
			if fetchErr := obj.fetchOriginalCommitBySHA(repo, originalCommitSHA); fetchErr != nil {
				obj.log(1, "failed to fetch original commit by SHA", "sha", originalCommitSHA, "error", fetchErr)
				continue
			}
			originalCommit, origErr = repo.CommitObject(originalCommitHash)
			if origErr != nil {
				obj.log(1, "fetched original commit but cannot read commit object", "sha", originalCommitSHA, "error", origErr)
				continue
			}
		}

		originalTree, treeErr := originalCommit.Tree()
		if treeErr != nil {
			obj.log(1, "cannot get original tree for commit", "commit", originalCommitSHA, "error", treeErr)
			continue
		}

		originalPathSHA := make(map[string]string)
		_ = originalTree.Files().ForEach(func(f *object.File) error {
			originalPathSHA[f.Name] = f.Hash.String()
			return nil
		})

		for _, rb := range blobs {
			originalBlobSHA := originalPathSHA[rb.path]
			if originalBlobSHA == "" {
				obj.log(2, "path not found in original tree", "path", rb.path, "original_commit", originalCommitSHA)
				continue
			}

			if originalBlobSHA == rb.blobHash {
				obj.log(2, "SHA match — marker is legitimate content, skipping resolution", "path", rb.path)
				continue
			}

			content, cached := blobCache[originalBlobSHA]
			if !cached {
				blobObj, blobErr := repo.BlobObject(plumbing.NewHash(originalBlobSHA))
				if blobErr != nil {
					obj.log(1, "original blob not present locally after commit fetch", "sha", originalBlobSHA, "path", rb.path, "error", blobErr)
					continue
				}
				readContent, readErr := readBlobContent(blobObj)
				if readErr != nil {
					obj.log(1, "failed to read original blob content", "sha", originalBlobSHA, "path", rb.path, "error", readErr)
					continue
				}
				blobCache[originalBlobSHA] = readContent
				content = readContent
			}

			header := fmt.Sprintf("\n==== Blob %s | Commit %s | Path %s | Resolved ====\n", originalBlobSHA, rb.commitHash, rb.path)
			if _, writeErr := io.WriteString(output, header); writeErr != nil {
				return true, writeErr
			}
			if _, writeErr := output.Write(content); writeErr != nil {
				return true, writeErr
			}
			if _, writeErr := io.WriteString(output, "\n"); writeErr != nil {
				return true, writeErr
			}
		}
	}

	return true, nil
}

func (obj *Worker) resolveRedactedViaCurrentTreeAPI(repo *git.Repository, spec repoSpec, output io.Writer, httpClient *http.Client) error {

	blobCache := make(map[string][]byte)

	commitBlobMap := make(map[string][]redactedBlob)
	for _, rb := range obj.redactedBlobs {
		commitBlobMap[rb.commitHash] = append(commitBlobMap[rb.commitHash], rb)
	}

	for commitHashStr, blobs := range commitBlobMap {
		commitHash := plumbing.NewHash(commitHashStr)
		commit, err := repo.CommitObject(commitHash)
		if err != nil {
			obj.log(1, "cannot get commit object for resolution", "commit", commitHashStr, "error", err)
			continue
		}

		treeSHA := commit.TreeHash.String()
		apiTree, err := fetchTree(httpClient, spec.Owner, spec.Repo, treeSHA, obj.githubToken)
		if err != nil {
			if strings.Contains(err.Error(), "rate limited") {
				obj.log(1, "GitHub API rate limited — set GITHUB_TOKEN env var for 5000 req/hr", "repo", spec.URL)
				return nil
			}
			obj.log(1, "failed to fetch GitHub tree for resolution", "commit", commitHashStr, "error", err)
			continue
		}

		if apiTree.Truncated {
			obj.log(1, "GitHub API tree is truncated — some blobs may not be resolved", "commit", commitHashStr)
		}

		apiPathSHA := make(map[string]string)
		for _, entry := range apiTree.Tree {
			if entry.Type == "blob" {
				apiPathSHA[entry.Path] = entry.SHA
			}
		}

		localTree, err := commit.Tree()
		if err != nil {
			obj.log(1, "cannot get local tree for commit", "commit", commitHashStr, "error", err)
			continue
		}
		clonePathSHA := make(map[string]string)
		_ = localTree.Files().ForEach(func(f *object.File) error {
			clonePathSHA[f.Name] = f.Hash.String()
			return nil
		})

		for _, rb := range blobs {
			apiSHA, hasAPIEntry := apiPathSHA[rb.path]
			if !hasAPIEntry {
				obj.log(1, "path not found in API tree, skipping", "path", rb.path)
				continue
			}

			cloneSHA := clonePathSHA[rb.path]
			if cloneSHA == apiSHA {
				obj.log(2, "SHA match — marker is legitimate content, skipping resolution", "path", rb.path)
				continue
			}

			content, cached := blobCache[apiSHA]
			if !cached {
				fetched, err := fetchBlob(httpClient, spec.Owner, spec.Repo, apiSHA, obj.githubToken)
				if err != nil {
					if strings.Contains(err.Error(), "rate limited") {
						obj.log(1, "GitHub API rate limited — set GITHUB_TOKEN env var for 5000 req/hr", "repo", spec.URL)
						return nil
					}
					obj.log(1, "failed to fetch blob for resolution", "path", rb.path, "sha", apiSHA, "error", err)
					continue
				}
				blobCache[apiSHA] = fetched
				content = fetched
			}

			header := fmt.Sprintf("\n==== Blob %s | Commit %s | Path %s | Resolved ====\n", apiSHA, rb.commitHash, rb.path)
			if _, err := io.WriteString(output, header); err != nil {
				return err
			}
			if _, err := output.Write(content); err != nil {
				return err
			}
			if _, err := io.WriteString(output, "\n"); err != nil {
				return err
			}
		}
	}

	return nil
}

func (obj *Worker) findRewriteBaseSHA(repo *git.Repository, spec repoSpec, httpClient *http.Client) (string, error) {
	for page := 1; page <= 3; page++ {
		events, err := fetchEvents(httpClient, spec.Owner, spec.Repo, obj.githubToken, page)
		if err != nil {
			if strings.Contains(err.Error(), "invalid token") {
				return "", fmt.Errorf("GitHub API: invalid token (HTTP 401)")
			}
			return "", err
		}
		if len(events) == 0 {
			return "", nil
		}

		for _, evt := range events {
			if evt.Type != "PushEvent" {
				continue
			}
			if evt.Payload.Ref != "refs/heads/main" {
				continue
			}

			headSHA := strings.TrimSpace(evt.Payload.Head)
			beforeSHA := strings.TrimSpace(evt.Payload.Before)
			if len(headSHA) != 40 || len(beforeSHA) != 40 {
				continue
			}

			if _, headErr := repo.CommitObject(plumbing.NewHash(headSHA)); headErr != nil {
				continue
			}
			if _, beforeErr := repo.CommitObject(plumbing.NewHash(beforeSHA)); beforeErr == nil {
				continue
			}

			return beforeSHA, nil
		}
	}

	return "", nil
}

func (obj *Worker) fetchOriginalCommitBySHA(repo *git.Repository, commitSHA string) error {
	if len(commitSHA) != 40 {
		return fmt.Errorf("invalid commit SHA length: %s", commitSHA)
	}

	shortSHA := commitSHA[:12]
	refSpec := config.RefSpec("+" + commitSHA + ":refs/gitcontent/original/" + shortSHA)
	err := repo.Fetch(&git.FetchOptions{
		RemoteName:      "origin",
		RefSpecs:        []config.RefSpec{refSpec},
		InsecureSkipTLS: obj.insecure,
		Tags:            git.NoTags,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return err
	}

	return nil
}

func commitIdentityKey(message string, authorTime time.Time) string {
	normalizedMessage := strings.TrimSpace(strings.SplitN(message, "\n", 2)[0])
	return normalizedMessage + "|" + authorTime.UTC().Format(time.RFC3339)
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

// safeSplitPath validates that joining splitDir and blobPath produces a path that
// stays within splitDir and contains no symlink components.
// Returns the clean absolute path on success, or an error if the path would escape
// or if any existing component is a symlink.
func safeSplitPath(splitDir, blobPath string) (string, error) {
	if blobPath == "" {
		return "", fmt.Errorf("empty blob path")
	}
	cleanDir := filepath.Clean(splitDir) + string(os.PathSeparator)
	full := filepath.Clean(filepath.Join(splitDir, blobPath))
	if !strings.HasPrefix(full, cleanDir) {
		return "", fmt.Errorf("path escapes split directory: %q", blobPath)
	}
	// Check each existing path component for symlinks
	// Only check components under splitDir (the blobPath portion)
	rel, err := filepath.Rel(filepath.Clean(splitDir), full)
	if err != nil {
		return "", fmt.Errorf("failed to compute relative path: %w", err)
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	current := filepath.Clean(splitDir)
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				// Component doesn't exist yet — safe (will be created by MkdirAll)
				break
			}
			return "", fmt.Errorf("lstat %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlink in path component: %q", current)
		}
	}
	return full, nil
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
