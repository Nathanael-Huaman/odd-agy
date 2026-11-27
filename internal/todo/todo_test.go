package todo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initTempGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test User")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func strPtr(s string) *string { return &s }

func TestStorePathsGitVsNonGit(t *testing.T) {
	gitDir := initTempGitRepo(t)
	gitStore := NewStore(gitDir)
	expectedGitPath := filepath.Join(gitDir, ".git", "odd-agy", "todo.json")
	if got := gitStore.Path(); got != expectedGitPath {
		t.Fatalf("git store path = %q, want %q", got, expectedGitPath)
	}

	nonGitDir := t.TempDir()
	nonGitStore := NewStore(nonGitDir)
	expectedFallback := filepath.Join(nonGitDir, ".agents", "odd", ".state", "todo.json")
	if got := nonGitStore.Path(); got != expectedFallback {
		t.Fatalf("non-git store path = %q, want %q", got, expectedFallback)
	}

	if _, err := nonGitStore.Add("fallback task", StatusPending, ""); err != nil {
		t.Fatalf("Add in non-git dir failed: %v", err)
	}
	if _, err := os.Stat(expectedFallback); err != nil {
		t.Fatalf("expected fallback file at %s: %v", expectedFallback, err)
	}
}

func TestStoreCRUDAndTurnCounters(t *testing.T) {
	dir := initTempGitRepo(t)
	store := NewStore(dir)

	// Empty list on missing file.
	st, err := store.List()
	if err != nil {
		t.Fatalf("List on empty store failed: %v", err)
	}
	if len(st.Tasks) != 0 {
		t.Fatalf("expected 0 tasks, got %d", len(st.Tasks))
	}

	// Add tasks.
	st, err = store.Add("First task", StatusPending, "")
	if err != nil {
		t.Fatalf("Add 1 failed: %v", err)
	}
	if len(st.Tasks) != 1 || st.Tasks[0].ID != 1 || st.NextID != 2 {
		t.Fatalf("unexpected state after Add 1: %+v", st)
	}

	st, err = store.Add("Second task", StatusInProgress, "working")
	if err != nil {
		t.Fatalf("Add 2 failed: %v", err)
	}
	if len(st.Tasks) != 2 || st.Tasks[1].ID != 2 || st.NextID != 3 {
		t.Fatalf("unexpected state after Add 2: %+v", st)
	}

	// Increment and reset tool turns.
	turns, err := store.IncrementToolTurns()
	if err != nil || turns != 1 {
		t.Fatalf("IncrementToolTurns() = (%d, %v), want (1, nil)", turns, err)
	}
	turns, err = store.IncrementToolTurns()
	if err != nil || turns != 2 {
		t.Fatalf("IncrementToolTurns() = (%d, %v), want (2, nil)", turns, err)
	}
	if err := store.ResetToolTurns(); err != nil {
		t.Fatalf("ResetToolTurns failed: %v", err)
	}
	st, err = store.List()
	if err != nil || st.ToolTurnsSinceUpdate != 0 {
		t.Fatalf("expected ToolTurnsSinceUpdate=0, got %d (err=%v)", st.ToolTurnsSinceUpdate, err)
	}

	// Update existing task (moving task 2 to done, then task 1 to in_progress).
	st, err = store.Update(2, nil, strPtr(StatusDone), nil)
	if err != nil {
		t.Fatalf("Update task 2 failed: %v", err)
	}
	st, err = store.Update(1, strPtr("First task updated"), strPtr(StatusInProgress), strPtr("active"))
	if err != nil {
		t.Fatalf("Update task 1 failed: %v", err)
	}
	if st.Tasks[0].Title != "First task updated" || st.Tasks[0].Status != StatusInProgress {
		t.Fatalf("unexpected updated task 1: %+v", st.Tasks[0])
	}

	// Write replaces whole list, keeping ID 1 and assigning ID 3 to omitted ID.
	st, err = store.Write([]TaskInput{
		{ID: 1, Title: "Kept first", Status: StatusDropped, Note: "superseded"},
		{Title: "Brand new", Status: StatusBlocked, Note: "waiting on API"},
	})
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if len(st.Tasks) != 2 {
		t.Fatalf("expected 2 tasks after Write, got %d", len(st.Tasks))
	}
	if st.Tasks[0].ID != 1 || st.Tasks[1].ID != 3 || st.NextID != 4 {
		t.Fatalf("unexpected IDs after Write: tasks=%+v nextID=%d", st.Tasks, st.NextID)
	}

	// Clear resets list.
	st, err = store.Clear()
	if err != nil {
		t.Fatalf("Clear failed: %v", err)
	}
	if len(st.Tasks) != 0 {
		t.Fatalf("expected 0 tasks after Clear, got %d", len(st.Tasks))
	}
}

func TestStoreValidationInvariantsLeaveStateUnchanged(t *testing.T) {
	dir := initTempGitRepo(t)
	store := NewStore(dir)

	if _, err := store.Add("Baseline task", StatusInProgress, "active"); err != nil {
		t.Fatalf("baseline Add failed: %v", err)
	}
	if _, err := store.IncrementToolTurns(); err != nil {
		t.Fatalf("IncrementToolTurns failed: %v", err)
	}
	beforeBytes, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatalf("ReadFile before failed: %v", err)
	}

	assertUnchanged := func(label string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: expected error, got nil", label)
		}
		afterBytes, readErr := os.ReadFile(store.Path())
		if readErr != nil {
			t.Fatalf("%s: ReadFile after failed: %v", label, readErr)
		}
		if string(afterBytes) != string(beforeBytes) {
			t.Fatalf("%s: stored file mutated on rejected input!\nbefore: %s\nafter: %s", label, string(beforeBytes), string(afterBytes))
		}
		st, listErr := store.List()
		if listErr != nil {
			t.Fatalf("%s: List failed: %v", label, listErr)
		}
		if st.ToolTurnsSinceUpdate != 1 {
			t.Fatalf("%s: ToolTurnsSinceUpdate = %d, want 1", label, st.ToolTurnsSinceUpdate)
		}
	}

	// Empty/whitespace title.
	_, err = store.Add("   \t\n ", StatusPending, "")
	assertUnchanged("empty title Add", err)

	_, err = store.Update(1, strPtr("  "), nil, nil)
	assertUnchanged("empty title Update", err)

	_, err = store.Write([]TaskInput{{Title: "", Status: StatusPending}})
	assertUnchanged("empty title Write", err)

	// Unknown status.
	_, err = store.Add("Task", "bogus", "")
	assertUnchanged("unknown status Add", err)

	_, err = store.Update(1, nil, strPtr(""), nil)
	assertUnchanged("empty status Update", err)

	_, err = store.Write([]TaskInput{{Title: "Task", Status: "invalid"}})
	assertUnchanged("unknown status Write", err)

	// Blocked requires non-empty note.
	_, err = store.Add("Blocked task", StatusBlocked, "   ")
	assertUnchanged("blocked without note Add", err)

	_, err = store.Update(1, nil, strPtr(StatusBlocked), strPtr(""))
	assertUnchanged("blocked without note Update", err)

	_, err = store.Write([]TaskInput{{Title: "Blocked task", Status: StatusBlocked, Note: ""}})
	assertUnchanged("blocked without note Write", err)

	// At most ONE in_progress task.
	_, err = store.Add("Second active", StatusInProgress, "")
	assertUnchanged("second in_progress Add", err)

	_, err = store.Write([]TaskInput{
		{ID: 1, Title: "First", Status: StatusInProgress},
		{Title: "Second", Status: StatusInProgress},
	})
	assertUnchanged("two in_progress Write", err)

	// Updating non-existent ID.
	_, err = store.Update(999, strPtr("Missing"), nil, nil)
	assertUnchanged("non-existent ID Update", err)

	// Write with unknown positive ID or duplicate IDs.
	_, err = store.Write([]TaskInput{{ID: 99, Title: "Ghost", Status: StatusPending}})
	assertUnchanged("unknown positive ID Write", err)

	_, err = store.Write([]TaskInput{
		{ID: 1, Title: "Dup 1", Status: StatusPending},
		{ID: 1, Title: "Dup 2", Status: StatusDone},
	})
	assertUnchanged("duplicate ID Write", err)
}
