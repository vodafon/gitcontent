package main

import (
	"bufio"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

var (
	flagV               = flag.Int("v", 1, "verbose level")
	flagOut             = flag.String("out", "out", "out dir")
	flagMaxCloneSeconds = flag.Int("max-clone-seconds", 300, "skip repository when clone exceeds this number of seconds")
	flagMaxOutputBytes  = flag.Int64("max-output-bytes", 1073741824, "maximum bytes per output file before stopping repository processing")
	flagInsecure        = flag.Bool("insecure", true, "skip TLS certificate verification")
	flagResolveRedacted = flag.Bool("resolve-redacted", false, "resolve GitHub-redacted secrets (***REMOVED***) via API; optionally set GITHUB_TOKEN env var for higher rate limits")
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: gitcontent [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Reads repository URLs from stdin (one per line), clones each, and writes\n")
		fmt.Fprintf(os.Stderr, "non-binary blob contents to a grepable text file per repository.\n\n")
		fmt.Fprintf(os.Stderr, "Example:\n")
		fmt.Fprintf(os.Stderr, "  printf 'https://github.com/owner/repo' | gitcontent\n\n")
		fmt.Fprintf(os.Stderr, "Example with secret redaction bypass:\n")
		fmt.Fprintf(os.Stderr, "  export GITHUB_TOKEN=ghp_xxx\n")
		fmt.Fprintf(os.Stderr, "  printf 'https://github.com/owner/repo' | gitcontent -resolve-redacted\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	worker := &Worker{
		outDir:          *flagOut,
		verbose:         *flagV,
		maxCloneSeconds: *flagMaxCloneSeconds,
		maxOutputBytes:  *flagMaxOutputBytes,
		insecure:        *flagInsecure,
		resolveRedacted: *flagResolveRedacted,
		githubToken:     os.Getenv("GITHUB_TOKEN"),
		l:               logger,
	}

	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		worker.Process(sc.Text())
	}

	if err := sc.Err(); err != nil {
		logger.Error("failed to read stdin", "error", err)
		os.Exit(1)
	}
}

func (obj *Worker) Process(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}

	err := obj.Repo(line)
	if err != nil {
		obj.l.Error("failed to process repository", "input", line, "error", err)
	}
}
