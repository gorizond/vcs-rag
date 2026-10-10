package gitengine

import (
	"context"
	"strconv"
	"strings"
)

// FileChange is a file change in a commit.
type FileChange struct {
	Path   string
	Status string // A (added), M (modified), D (deleted)
}

// DiffCommit lists the changes of a commit; --root makes the first commit
// yield the full list too. Renames/copies (R/C) are expanded into D(old) + A(new).
func (e *Engine) DiffCommit(ctx context.Context, mirror, sha string) ([]FileChange, error) {
	out, err := e.run(ctx, mirror, "diff-tree", "--no-commit-id", "--name-status", "-r", "-z", "--root", sha)
	if err != nil {
		return nil, err
	}
	tokens := splitNUL(out)
	var changes []FileChange
	for i := 0; i < len(tokens); {
		status := tokens[i]
		i++
		if status == "" {
			continue
		}
		switch status[0] {
		case 'R', 'C':
			// <status>\0<old>\0<new>\0
			if i+1 >= len(tokens) {
				return changes, nil
			}
			oldPath, newPath := tokens[i], tokens[i+1]
			i += 2
			changes = append(changes, FileChange{Path: oldPath, Status: "D"}, FileChange{Path: newPath, Status: "A"})
		default:
			if i >= len(tokens) {
				return changes, nil
			}
			path := tokens[i]
			i++
			changes = append(changes, FileChange{Path: path, Status: normalizeStatus(status)})
		}
	}
	return changes, nil
}

// normalizeStatus reduces a git status to A/M/D (T/U and others — as M).
func normalizeStatus(status string) string {
	switch status[0] {
	case 'A':
		return "A"
	case 'D':
		return "D"
	default:
		return "M"
	}
}

// TreeFile is a blob of a commit tree.
type TreeFile struct {
	Path string
	SHA  string
	Size int64
}

// TreeFiles lists tree blobs (ls-tree -r -l -z) and drops type!=blob and
// size>maxBytes — the filter is applied BEFORE reading contents.
func (e *Engine) TreeFiles(ctx context.Context, mirror, sha string, maxBytes int64) ([]TreeFile, error) {
	out, err := e.run(ctx, mirror, "ls-tree", "-r", "-l", "-z", sha)
	if err != nil {
		return nil, err
	}
	var files []TreeFile
	for _, rec := range splitNUL(out) {
		if rec == "" {
			continue
		}
		idx := strings.IndexByte(rec, '\t')
		if idx < 0 {
			continue
		}
		meta, path := rec[:idx], rec[idx+1:]
		fields := strings.Fields(meta) // mode type sha size (size is space-padded)
		if len(fields) < 4 || fields[1] != "blob" {
			continue
		}
		size, err := strconv.ParseInt(fields[len(fields)-1], 10, 64)
		if err != nil {
			continue
		}
		if maxBytes > 0 && size > maxBytes {
			continue
		}
		files = append(files, TreeFile{Path: path, SHA: fields[2], Size: size})
	}
	return files, nil
}

// splitNUL splits git -z output into tokens.
func splitNUL(out []byte) []string {
	raw := strings.Split(string(out), "\x00")
	if n := len(raw); n > 0 && raw[n-1] == "" {
		raw = raw[:n-1]
	}
	return raw
}
