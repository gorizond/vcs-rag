package gitengine

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// BlobReader streams blob reads via a long-lived `git cat-file --batch`
// in the mirror directory. Not thread-safe: one reader per goroutine.
type BlobReader struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

// NewBlobReader starts cat-file --batch.
func (e *Engine) NewBlobReader(ctx context.Context, mirror string) (*BlobReader, error) {
	cmd := exec.CommandContext(ctx, e.GitBin, "cat-file", "--batch")
	cmd.Dir = mirror
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("cat-file: %w", err)
	}
	return &BlobReader{cmd: cmd, stdin: stdin, stdout: bufio.NewReaderSize(stdout, 1<<20)}, nil
}

// ReadBlob reads an object by cat-file spec: "<sha>" or "<sha>:<path>".
// Returns the content and a found flag (for a missing object — nil, false, nil).
func (r *BlobReader) ReadBlob(spec string) ([]byte, bool, error) {
	if _, err := io.WriteString(r.stdin, spec+"\n"); err != nil {
		return nil, false, fmt.Errorf("cat-file: write: %w", err)
	}
	header, err := r.stdout.ReadString('\n')
	if err != nil {
		return nil, false, fmt.Errorf("cat-file: read header: %w", err)
	}
	fields := strings.Fields(strings.TrimRight(header, "\n"))
	if len(fields) >= 2 && fields[1] == "missing" {
		return nil, false, nil
	}
	if len(fields) != 3 {
		return nil, false, fmt.Errorf("cat-file: unexpected header %q", strings.TrimSpace(header))
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return nil, false, fmt.Errorf("cat-file: invalid size %q", fields[2])
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r.stdout, buf); err != nil {
		return nil, false, fmt.Errorf("cat-file: read body: %w", err)
	}
	if _, err := r.stdout.Discard(1); err != nil { // trailing \n
		return nil, false, fmt.Errorf("cat-file: read separator: %w", err)
	}
	return buf, true, nil
}

// Close shuts the cat-file process down.
func (r *BlobReader) Close() error {
	_ = r.stdin.Close()
	_ = r.cmd.Wait()
	return nil
}

// IsBinary is a heuristic: a NUL byte within the first 8000 bytes (same as git).
func IsBinary(content []byte) bool {
	n := min(len(content), 8000)
	return bytes.IndexByte(content[:n], 0) >= 0
}
