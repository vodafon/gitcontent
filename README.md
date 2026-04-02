# gitcontent

`gitcontent` is a small Go CLI that clones Git repositories and writes a single **grepable text file** per repository containing **non-binary blob contents** from commit history.

It is designed for repository-wide text inspection workflows (for example, searching for accidental secrets or sensitive patterns) while skipping binary data.

## What it does

- Reads repository inputs from **stdin** (one repository per line)
- Normalizes input to canonical HTTPS repository URLs
- Clones each repository into a temporary directory
- Walks references/commits and writes text blobs into one output file
- Skips likely binary blobs
- Applies safety limits:
  - clone timeout (`-max-clone-seconds`)
  - output file budget (`-max-output-bytes`)

## Installation

### Build locally

```bash
go build -o gitcontent .
```

### Run without building

```bash
go run .
```

## Usage

Pipe repositories into stdin:

```bash
printf '%s\n' \
  'https://github.com/octocat/Hello-World' \
  'git@github.com:owner/repo.git' \
  'github.com/owner/repo' \
| ./gitcontent
```

By default, output files are written to `./out`.

## Flags

```text
-v int
    verbose level (default 1)
-out string
    out dir (default "out")
-max-clone-seconds int
    skip repository when clone exceeds this number of seconds (default 300)
-max-output-bytes int
    maximum bytes per output file before stopping repository processing (default 1073741824)
```

## Input formats

Accepted repository forms:

- `https://github.com/owner/repo`
- `http://github.com/owner/repo`
- `git@github.com:owner/repo.git`
- `github.com/owner/repo`

Normalization behavior:

- `git@host:owner/repo.git` is converted to `https://host/owner/repo`
- host is lowercased in canonical output URL
- input must resolve to exactly `host/owner/repo`

## Output

For each repository, `gitcontent` creates:

```text
{outDir}/{safe_host}_{safe_owner}_{safe_repo}_content.txt
```

The file format is:

```text
https://github.com/owner/repo

==== Blob <blobHash> | Commit <commitHash> | Path <path> ====
<file content>

==== Blob <blobHash> | Commit <commitHash> | Path <path> ====
<file content>
```

The header gives enough metadata to trace matches back to a specific blob, commit, and path.

## Binary filtering

A blob is treated as binary and skipped if any of these are true:

- contains `0x00` byte
- invalid UTF-8
- more than 30% control bytes (excluding `\n`, `\r`, `\t`)

## Safety behavior

- **Clone timeout exceeded**: repository is skipped (logged), processing continues
- **Output byte budget reached**: repository processing stops early (logged), processing continues
- Temporary clone directories are removed after processing

## Notes and limitations

- Processes repositories sequentially (no parallel clone/scan)
- Produces one aggregated text file per repository
- Does not support incremental/resume mode
- Clone progress is sent to stdout by the Git library
- Deduplication avoids writing the same commit/blob repeatedly across refs

## Typical workflow for sensitive-data search

```bash
printf '%s\n' 'https://github.com/org/repo' | ./gitcontent -out out
grep -RniE '(api[_-]?key|secret|password|token)' out/
```

## Development

Run tests:

```bash
go test ./...
```
