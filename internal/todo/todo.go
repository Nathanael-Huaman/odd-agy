package todo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusBlocked    = "blocked"
	StatusDone       = "done"
	StatusDropped    = "dropped"
)

// Task represents a single project todo item.
type Task struct {
	ID     int    `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

// TaskInput is used by Store.Write to replace or update the task list.
// When ID is 0 (omitted), a new sequential ID is assigned.
type TaskInput struct {
	ID     int    `json:"id,omitempty"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

// State is the persisted todo store state.
type State struct {
	Tasks                []Task `json:"tasks"`
	NextID               int    `json:"nextId"`
	ToolTurnsSinceUpdate int    `json:"toolTurnsSinceUpdate"`
	UpdatedAt            string `json:"updatedAt,omitempty"`
}

// Store manages project-scoped todo persistence using atomic file writes.
type Store struct {
	mu        sync.Mutex
	workspace string
	path      string
}

// NewStore creates a todo Store rooted at workspace.
// Inside a Git repository it stores state at <git-dir>/odd-agy/todo.json;
// outside Git it falls back to <workspace>/.agents/odd/.state/todo.json.
func NewStore(workspace string) *Store {
	return &Store{
		workspace: workspace,
		path:      resolveStorePath(workspace),
	}
}

// Path returns the resolved persistence file path for this store.
func (s *Store) Path() string {
	return s.path
}

func resolveStorePath(workspace string) string {
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = workspace
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		gitDir := strings.TrimSpace(stdout.String())
		if gitDir != "" {
			if !filepath.IsAbs(gitDir) {
				gitDir = filepath.Join(workspace, gitDir)
			}
			return filepath.Join(filepath.Clean(gitDir), "odd-agy", "todo.json")
		}
	}
	return filepath.Join(workspace, ".agents", "odd", ".state", "todo.json")
}

// List returns the current todo state (or an empty state if the file does not exist).
func (s *Store) List() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

// Write replaces the whole task list while preserving IDs of kept tasks and
// assigning sequential IDs for omitted IDs. Rejected inputs leave disk state
// and turn counters completely unchanged.
func (s *Store) Write(items []TaskInput) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur, err := s.loadLocked()
	if err != nil {
		return State{}, err
	}

	existingIDs := make(map[int]bool, len(cur.Tasks))
	for _, t := range cur.Tasks {
		existingIDs[t.ID] = true
	}

	nextID := cur.NextID
	if nextID < 1 {
		nextID = 1
	}

	seenIDs := make(map[int]bool, len(items))
	for _, item := range items {
		if item.ID < 0 {
			return State{}, fmt.Errorf("invalid negative task id %d", item.ID)
		}
		if item.ID > 0 {
			if !existingIDs[item.ID] {
				return State{}, fmt.Errorf("unknown task id %d", item.ID)
			}
			if seenIDs[item.ID] {
				return State{}, fmt.Errorf("duplicate task id %d", item.ID)
			}
			seenIDs[item.ID] = true
			if item.ID >= nextID {
				nextID = item.ID + 1
			}
		}
	}

	newTasks := make([]Task, 0, len(items))
	for _, item := range items {
		id := item.ID
		if id == 0 {
			id = nextID
			nextID++
		}
		newTasks = append(newTasks, Task{
			ID:     id,
			Title:  strings.TrimSpace(item.Title),
			Status: item.Status,
			Note:   item.Note,
		})
	}

	if err := validateTasks(newTasks); err != nil {
		return State{}, err
	}

	next := State{
		Tasks:                newTasks,
		NextID:               nextID,
		ToolTurnsSinceUpdate: 0,
		UpdatedAt:            nowUTC(),
	}
	if err := s.saveLocked(next); err != nil {
		return State{}, err
	}
	return cloneState(next), nil
}

// Add appends one task to the list. Rejected inputs leave disk state and
// turn counters completely unchanged.
func (s *Store) Add(title, status, note string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur, err := s.loadLocked()
	if err != nil {
		return State{}, err
	}

	nextID := cur.NextID
	if nextID < 1 {
		nextID = 1
	}

	candidate := append([]Task(nil), cur.Tasks...)
	candidate = append(candidate, Task{
		ID:     nextID,
		Title:  strings.TrimSpace(title),
		Status: status,
		Note:   note,
	})

	if err := validateTasks(candidate); err != nil {
		return State{}, err
	}

	next := State{
		Tasks:                candidate,
		NextID:               nextID + 1,
		ToolTurnsSinceUpdate: 0,
		UpdatedAt:            nowUTC(),
	}
	if err := s.saveLocked(next); err != nil {
		return State{}, err
	}
	return cloneState(next), nil
}

// Update modifies an existing task by ID. Rejected inputs or missing IDs leave
// disk state and turn counters completely unchanged.
func (s *Store) Update(id int, title, status, note *string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur, err := s.loadLocked()
	if err != nil {
		return State{}, err
	}

	idx := -1
	for i, t := range cur.Tasks {
		if t.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return State{}, fmt.Errorf("task id %d not found", id)
	}

	candidate := append([]Task(nil), cur.Tasks...)
	if title != nil {
		candidate[idx].Title = strings.TrimSpace(*title)
	}
	if status != nil {
		candidate[idx].Status = *status
	}
	if note != nil {
		candidate[idx].Note = *note
	}

	if err := validateTasks(candidate); err != nil {
		return State{}, err
	}

	next := State{
		Tasks:                candidate,
		NextID:               cur.NextID,
		ToolTurnsSinceUpdate: 0,
		UpdatedAt:            nowUTC(),
	}
	if next.NextID < 1 {
		next.NextID = 1
	}
	if err := s.saveLocked(next); err != nil {
		return State{}, err
	}
	return cloneState(next), nil
}

// Clear resets the task list and resets ToolTurnsSinceUpdate to 0.
func (s *Store) Clear() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := State{
		Tasks:                []Task{},
		NextID:               1,
		ToolTurnsSinceUpdate: 0,
		UpdatedAt:            nowUTC(),
	}
	if err := s.saveLocked(next); err != nil {
		return State{}, err
	}
	return cloneState(next), nil
}

// IncrementToolTurns increments ToolTurnsSinceUpdate and persists the change.
func (s *Store) IncrementToolTurns() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur, err := s.loadLocked()
	if err != nil {
		return 0, err
	}
	cur.ToolTurnsSinceUpdate++
	cur.UpdatedAt = nowUTC()
	if err := s.saveLocked(cur); err != nil {
		return 0, err
	}
	return cur.ToolTurnsSinceUpdate, nil
}

// ResetToolTurns sets ToolTurnsSinceUpdate back to 0 and persists if needed.
func (s *Store) ResetToolTurns() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur, err := s.loadLocked()
	if err != nil {
		return err
	}
	if cur.ToolTurnsSinceUpdate == 0 {
		return nil
	}
	cur.ToolTurnsSinceUpdate = 0
	cur.UpdatedAt = nowUTC()
	return s.saveLocked(cur)
}

func validateTasks(tasks []Task) error {
	inProgressCount := 0
	for _, t := range tasks {
		if strings.TrimSpace(t.Title) == "" {
			return errors.New("task title must not be empty")
		}
		switch t.Status {
		case StatusPending, StatusDone, StatusDropped:
		case StatusInProgress:
			inProgressCount++
			if inProgressCount > 1 {
				return errors.New("at most one task may be in_progress at a time")
			}
		case StatusBlocked:
			if strings.TrimSpace(t.Note) == "" {
				return errors.New("blocked task requires a non-empty note")
			}
		default:
			return fmt.Errorf("unknown task status %q: must be one of pending, in_progress, blocked, done, dropped", t.Status)
		}
	}
	return nil
}

func (s *Store) loadLocked() (State, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{
				Tasks:  []Task{},
				NextID: 1,
			}, nil
		}
		return State{}, fmt.Errorf("read todo store %s: %w", s.path, err)
	}

	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fmt.Errorf("parse todo store %s: %w", s.path, err)
	}
	if st.Tasks == nil {
		st.Tasks = []Task{}
	}
	if st.NextID < 1 {
		maxID := 0
		for _, t := range st.Tasks {
			if t.ID > maxID {
				maxID = t.ID
			}
		}
		st.NextID = maxID + 1
	}
	return st, nil
}

func (s *Store) saveLocked(st State) error {
	if st.Tasks == nil {
		st.Tasks = []Task{}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal todo state: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create todo directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-todo-*.json")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpName, s.path, err)
	}
	cleanup = false
	return nil
}

func cloneState(st State) State {
	out := st
	out.Tasks = append([]Task(nil), st.Tasks...)
	return out
}

func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}
