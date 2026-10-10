package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// BranchState — состояние инкрементальной индексации одной ветки.
type BranchState struct {
	Tip            string    `json:"tip,omitempty"`
	IndexedCommits []string  `json:"indexed_commits,omitempty"`
	IndexedAt      time.Time `json:"indexed_at"`
}

// MirrorState — состояние зеркала: sidecar-файл рядом с зеркалом
// (data-model §2), запись атомарная (tmp+rename).
type MirrorState struct {
	LastFetchAt time.Time              `json:"last_fetch_at"`
	Branches    map[string]BranchState `json:"branches"`
}

// LoadState читает состояние; отсутствие файла — пустое состояние.
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

// SaveState атомарно пишет состояние: tmp-файл + rename.
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

// stateMu сериализует load-modify-save: sidecar пишут обе линии (mirror —
// last_fetch_at, index — branches), файл у репозитория один.
var stateMu sync.Mutex

// updateState применяет изменение к состоянию под общим мьютексом.
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
