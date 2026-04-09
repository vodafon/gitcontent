package main

import (
	"bufio"
	"flag"
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
)

func main() {
	flag.Parse()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	worker := &Worker{
		outDir:          *flagOut,
		verbose:         *flagV,
		maxCloneSeconds: *flagMaxCloneSeconds,
		maxOutputBytes:  *flagMaxOutputBytes,
		insecure:        *flagInsecure,
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
