package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// BranchState is the incremental indexing state of a single branch.
type BranchState struct {
	Tip            string    `json:"tip,omitempty"`
	IndexedCommits []string  `json:"indexed_commits,omitempty"`
	IndexedAt      time.Time `json:"indexed_at"`
}

// MirrorState is the mirror state: a sidecar file next to the mirror
// (data-model §2), written atomically (tmp+rename).
type MirrorState struct {
	LastFetchAt time.Time              `json:"last_fetch_at"`
	Branches    map[string]BranchState `json:"branches"`
	// Meta is the incremental metadata cache (T080): key → updated_at unix:
	// "issue:<id>", "mr:<id>", "ci:<pipeline>:<job>".
	Meta map[string]int64 `json:"meta,omitempty"`
	// MetaMiss counts consecutive metadata passes where an issue/MR key was
	// absent from the full listing (T140 entity reconcile): after 2 misses the
	// point is deleted and the Meta key dropped; a returned key resets the
	// counter. CI keys are owned by the existing prune and are never counted.
	MetaMiss map[string]int `json:"meta_miss,omitempty"`
	// MetaSyncedAt is the end of the last successful metadata pass (T135):
	// an "auto"-origin refresh skips the metadata phase when it is fresh.
	MetaSyncedAt time.Time `json:"meta_synced_at"`
}

// LoadState reads the state; a missing file yields an empty state.
func LoadState(path string) (*MirrorState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &MirrorState{Branches: map[string]BranchState{}, Meta: map[string]int64{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state %s: %w", path, err)
	}
	st := &MirrorState{}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("state %s: %w", path, err)
	}
	if st.Branches == nil {
		st.Branches = map[string]BranchState{}
	}
	if st.Meta == nil {
		st.Meta = map[string]int64{}
	}
	if st.MetaMiss == nil {
		st.MetaMiss = map[string]int{}
	}
	return st, nil
}

// SaveState writes the state atomically: tmp file + rename.
func SaveState(path string, st *MirrorState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// The metadata lane writes state before the mirror exists (no clone yet),
	// so the parent directory may be missing.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("state %s: %w", path, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("state %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("state %s: %w", path, err)
	}
	return nil
}

// stateMu serializes load-modify-save: there is one sidecar per repository.
// Single-writer invariant: each lane owns its section and writes it only
// through updateState — mirror is the only writer of last_fetch_at, index of
// branches, metadata of meta. A closure must touch the owning section only;
// then the concurrent lanes never clobber each other (T088/T089).
var stateMu sync.Mutex

// updateState applies a change to the state under the shared mutex.
func updateState(path string, fn func(*MirrorState)) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	st, err := LoadState(path)
	if err != nil {
		return err
	}
	fn(st)
	return SaveState(path, st)
}
