package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	pending    = "pending"
	queued     = "queued"
	running    = "running"
	submitting = "submitting"
	success    = "success"
	failed     = "failed"
	uncertain  = "uncertain"
	skipped    = "skipped"
)

type Plan struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Cluster     string  `json:"cluster"`
	Storage     string  `json:"storage"`
	StorageType string  `json:"storage_type"`
	TargetNode  string  `json:"target_node,omitempty"`
	Guests      []Guest `json:"guests"`
	Continue    bool    `json:"continue_on_error"`
}
type Step struct {
	Guest    Guest  `json:"guest"`
	State    string `json:"state"`
	Node     string `json:"node,omitempty"`
	UPID     string `json:"upid,omitempty"`
	Error    string `json:"error,omitempty"`
	LogError string `json:"log_error,omitempty"`
}
type Notice struct {
	Attempts int       `json:"attempts"`
	Next     time.Time `json:"next"`
	Status   string    `json:"status"`
}
type Run struct {
	ID          string     `json:"id"`
	Plan        Plan       `json:"plan"`
	State       string     `json:"state"`
	Due         time.Time  `json:"due"`
	Started     *time.Time `json:"started,omitempty"`
	Finished    *time.Time `json:"finished,omitempty"`
	Steps       []Step     `json:"steps"`
	EmailNotice *Notice    `json:"email_notice,omitempty"`
	Notice      *Notice    `json:"notice,omitempty"`
	Recurring   bool       `json:"recurring,omitempty"`
	Reviewed    bool       `json:"reviewed,omitempty"`
}
type State struct {
	Users     map[string]User     `json:"users,omitempty"`
	Plans     []Plan              `json:"plans"`
	Runs      []Run               `json:"runs"`
	Schedules map[string]Schedule `json:"schedules,omitempty"`
	// Store task logs separately in the same atomic snapshot, without returning
	// them in history responses.
	Logs map[string][]LogLine `json:"logs"`
}
type store struct {
	mu    sync.Mutex
	path  string
	lock  *os.File
	state State
}

func openStore(path string) (*store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("Datendatei bereits in Verwendung: %w", err)
	}
	s := &store{path: path, lock: lock, state: State{Plans: []Plan{}, Runs: []Run{}, Logs: map[string][]LogLine{}}}
	content, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(content, &s.state)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		lock.Close()
		return nil, err
	}
	if s.state.Logs == nil {
		s.state.Logs = map[string][]LogLine{}
	}
	return s, nil
}
func (s *store) snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Published snapshots are immutable. update clones before mutating.
	return s.state
}
func (s *store) update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	var next State
	if err := json.Unmarshal(data, &next); err != nil {
		return err
	}
	if err := fn(&next); err != nil {
		return err
	}
	data, err = json.Marshal(next)
	if err != nil {
		return err
	}
	if err := saveSnapshot(s.path, data); err != nil {
		return err
	}
	s.state = next
	return nil
}
func saveSnapshot(path string, data []byte) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	tmp, err := os.CreateTemp(dir.Name(), ".pve-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	// Once rename succeeds the running process must use the new snapshot.
	if err := dir.Sync(); err != nil {
		log.Fatalf("Datendatei konnte nicht synchronisiert werden: %v", err)
	}
	return nil
}
func (s *State) run(id string) *Run {
	for i := range s.Runs {
		if s.Runs[i].ID == id {
			return &s.Runs[i]
		}
	}
	return nil
}
func logKey(run string, index int) string { return fmt.Sprintf("%s/%d", run, index) }
