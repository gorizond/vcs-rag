package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
}

// LoadState reads the state; a missing file yields an empty state.
func LoadState(path string) (*MirrorState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &MirrorState{Branches: map[string]BranchState{}}, nil
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
	return st, nil
}

// SaveState writes the state atomically: tmp file + rename.
func SaveState(path string, st *MirrorState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("state %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("state %s: %w", path, err)
	}
	return nil
}

// stateMu serializes load-modify-save: both lanes write the sidecar
// (mirror — last_fetch_at, index — branches), there is one file per repository.
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
