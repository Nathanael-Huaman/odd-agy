package upstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeFixtureFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", full, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
	return sha256Hex([]byte(content))
}

func makeTestLock(orchHash, agentDirHash string, agentFiles map[string]string, admissionHash string) LockFile {
	return LockFile{
		SchemaVersion: LockSchemaVersion,
		UpdatedAt:     "2026-04-20T00:00:00Z",
		Repos: []RepoSource{
			{
				Name: "Gentleman-Programming/gentle-shell",
				Ref:  "v5.0.0",
			},
			{
				Name: "Gentleman-Programming/gentle-ai",
				Ref:  "v5.0.0",
			},
		},
		Entries: []TrackedEntry{
			{
				ID:           "gentle-shell-orchestrator",
				Repo:         "Gentleman-Programming/gentle-shell",
				Path:         "assets/orchestrator.md",
				Category:     CategoryPromptsAndSkills,
				SHA256:       orchHash,
				LocalTargets: []string{"internal/assets/files/rules/odd-02-orchestrator.md"},
			},
			{
				ID:           "gentle-shell-agents",
				Repo:         "Gentleman-Programming/gentle-shell",
				Path:         "assets/agents/",
				Category:     CategoryPromptsAndSkills,
				SHA256:       agentDirHash,
				Files:        agentFiles,
				LocalTargets: []string{"internal/assets/files/agents/"},
			},
			{
				ID:           "gentle-shell-bounded-writer",
				Repo:         "Gentleman-Programming/gentle-shell",
				Path:         "lib/bounded-writer-admission.ts",
				Category:     CategoryEnforcementCode,
				SHA256:       admissionHash,
				LocalTargets: []string{"internal/hooks/admission.go"},
			},
		},
	}
}

func TestLoadAndSaveLockValidation(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "upstream.lock.json")

	// Missing file fails cleanly.
	if _, err := LoadLock(lockPath); err == nil {
		t.Fatal("expected error loading missing lock file")
	}

	// Invalid category rejected on SaveLock.
	bad := makeTestLock(sha256Hex([]byte("a")), sha256Hex([]byte("b")), nil, sha256Hex([]byte("c")))
	bad.Entries[0].Category = "unknown-category"
	if err := SaveLock(lockPath, bad); err == nil {
		t.Fatal("expected SaveLock to reject unknown category")
	}

	// Valid lock saves and round-trips cleanly.
	good := makeTestLock(
		sha256Hex([]byte("orch")),
		ComputeDirDigest(map[string]string{"worker.md": sha256Hex([]byte("worker"))}),
		map[string]string{"worker.md": sha256Hex([]byte("worker"))},
		sha256Hex([]byte("admission")),
	)
	if err := SaveLock(lockPath, good); err != nil {
		t.Fatalf("SaveLock: %v", err)
	}
	loaded, err := LoadLock(lockPath)
	if err != nil {
		t.Fatalf("LoadLock: %v", err)
	}
	if len(loaded.Entries) != 3 || loaded.Entries[0].SHA256 != good.Entries[0].SHA256 {
		t.Fatalf("round-trip mismatch: %+v", loaded)
	}
}

func TestCheckLocalSourceDirClassificationAndDrift(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "sources")
	gsRoot := filepath.Join(srcDir, "gentle-shell")

	orchHash := writeFixtureFile(t, gsRoot, "assets/orchestrator.md", "# orchestrator v1\n")
	workerHash := writeFixtureFile(t, gsRoot, "assets/agents/worker.md", "# worker v1\n")
	verifyHash := writeFixtureFile(t, gsRoot, "assets/agents/verify.md", "# verify v1\n")
	agentFiles := map[string]string{
		"verify.md": verifyHash,
		"worker.md": workerHash,
	}
	agentDirHash := ComputeDirDigest(agentFiles)
	admHash := writeFixtureFile(t, gsRoot, "lib/bounded-writer-admission.ts", "export const v = 1;\n")

	lockPath := filepath.Join(dir, "upstream.lock.json")
	if err := SaveLock(lockPath, makeTestLock(orchHash, agentDirHash, agentFiles, admHash)); err != nil {
		t.Fatalf("SaveLock: %v", err)
	}

	// 1. Exact match => all unchanged.
	rep, err := Check(context.Background(), lockPath, CheckOptions{SourceDir: srcDir})
	if err != nil {
		t.Fatalf("Check clean: %v", err)
	}
	if rep.ChangedCount != 0 || rep.MissingCount != 0 || rep.UnchangedCount != 3 {
		t.Fatalf("unexpected clean counts: %+v", rep)
	}
	if len(rep.PromptsAndSkills) != 2 || len(rep.EnforcementCode) != 1 {
		t.Fatalf("unexpected classification counts: prompts=%d enforcement=%d",
			len(rep.PromptsAndSkills), len(rep.EnforcementCode))
	}

	// 2. Mutate one prompt file and one enforcement file, delete verify.md inside dir.
	writeFixtureFile(t, gsRoot, "assets/orchestrator.md", "# orchestrator v2\n")
	writeFixtureFile(t, gsRoot, "lib/bounded-writer-admission.ts", "export const v = 2;\n")
	if err := os.Remove(filepath.Join(gsRoot, "assets/agents/verify.md")); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	rep, err = Check(context.Background(), lockPath, CheckOptions{SourceDir: srcDir})
	if err != nil {
		t.Fatalf("Check drifted: %v", err)
	}
	if rep.ChangedCount != 3 || rep.MissingCount != 0 || rep.UnchangedCount != 0 {
		t.Fatalf("expected 3 changed entries, got %+v", rep)
	}
	if rep.PromptsAndSkills[0].Status != StatusChanged || rep.EnforcementCode[0].Status != StatusChanged {
		t.Fatalf("expected changed classification in both buckets: %+v", rep)
	}
	if len(rep.PromptsAndSkills[1].MissingFiles) != 1 || rep.PromptsAndSkills[1].MissingFiles[0] != "verify.md" {
		t.Fatalf("expected missing file verify.md in dir entry, got %+v", rep.PromptsAndSkills[1])
	}

	// 3. Remove enforcement file completely => missing entry.
	if err := os.Remove(filepath.Join(gsRoot, "lib/bounded-writer-admission.ts")); err != nil {
		t.Fatalf("Remove admission: %v", err)
	}
	rep, err = Check(context.Background(), lockPath, CheckOptions{SourceDir: srcDir})
	if err != nil {
		t.Fatalf("Check missing: %v", err)
	}
	if rep.MissingCount != 1 || rep.EnforcementCode[0].Status != StatusMissing {
		t.Fatalf("expected missing enforcement entry, got %+v", rep.EnforcementCode[0])
	}
}

func TestCheckRemoteMockServerOffline(t *testing.T) {
	orchContent := "# remote orchestrator\n"
	workerContent := "# remote worker\n"
	admContent := "export const remote = true;\n"

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/raw/Gentleman-Programming/gentle-shell/v5.0.0/assets/orchestrator.md":
			_, _ = w.Write([]byte(orchContent))
		case "/raw/Gentleman-Programming/gentle-shell/v5.0.0/lib/bounded-writer-admission.ts":
			_, _ = w.Write([]byte(admContent))
		case "/repos/Gentleman-Programming/gentle-shell/contents/assets/agents":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"name":         "worker.md",
					"path":         "assets/agents/worker.md",
					"type":         "file",
					"download_url": srv.URL + "/raw/Gentleman-Programming/gentle-shell/v5.0.0/assets/agents/worker.md",
				},
			})
		case "/raw/Gentleman-Programming/gentle-shell/v5.0.0/assets/agents/worker.md":
			_, _ = w.Write([]byte(workerContent))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	lockPath := filepath.Join(dir, "upstream.lock.json")
	agentFiles := map[string]string{"worker.md": sha256Hex([]byte(workerContent))}
	lock := makeTestLock(
		sha256Hex([]byte(orchContent)),
		ComputeDirDigest(agentFiles),
		agentFiles,
		sha256Hex([]byte(admContent)),
	)
	if err := SaveLock(lockPath, lock); err != nil {
		t.Fatalf("SaveLock: %v", err)
	}

	rep, err := Check(context.Background(), lockPath, CheckOptions{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("Check remote: %v", err)
	}
	if rep.ChangedCount != 0 || rep.MissingCount != 0 || rep.UnchangedCount != 3 {
		t.Fatalf("expected 3 unchanged remote entries, got %+v", rep)
	}
}

func TestSyncUpdatesLockAndFailsClosedOnValidationError(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "sources")
	gsRoot := filepath.Join(srcDir, "gentle-shell")

	writeFixtureFile(t, gsRoot, "assets/orchestrator.md", "# v2 orchestrator\n")
	writeFixtureFile(t, gsRoot, "assets/agents/worker.md", "# v2 worker\n")
	writeFixtureFile(t, gsRoot, "lib/bounded-writer-admission.ts", "export const v = 2;\n")

	lockPath := filepath.Join(dir, "upstream.lock.json")
	initial := makeTestLock(
		sha256Hex([]byte("old-orch")),
		sha256Hex([]byte("old-agents")),
		map[string]string{"worker.md": sha256Hex([]byte("old-worker"))},
		sha256Hex([]byte("old-adm")),
	)
	if err := SaveLock(lockPath, initial); err != nil {
		t.Fatalf("SaveLock: %v", err)
	}
	beforeBytes, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("ReadFile before: %v", err)
	}

	// 1. Validation failure must fail closed and leave lock file untouched.
	if _, err := Sync(context.Background(), lockPath, SyncOptions{
		SourceDir: srcDir,
		Ref:       "v5.1.0",
		ValidateFunc: func() error {
			return errors.New("contract broken")
		},
	}); err == nil || !strings.Contains(err.Error(), "contract broken") {
		t.Fatalf("expected validation error, got %v", err)
	}
	afterFailBytes, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("ReadFile after fail: %v", err)
	}
	if string(afterFailBytes) != string(beforeBytes) {
		t.Fatal("lock file was clobbered despite validation failure")
	}

	// 2. Successful sync updates digests, ref, and passes Check cleanly afterwards.
	syncRep, err := Sync(context.Background(), lockPath, SyncOptions{
		SourceDir: srcDir,
		Ref:       "v5.1.0",
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if syncRep.UpdatedCount != 3 || !syncRep.Validated {
		t.Fatalf("unexpected sync report: %+v", syncRep)
	}

	loaded, err := LoadLock(lockPath)
	if err != nil {
		t.Fatalf("LoadLock after sync: %v", err)
	}
	if loaded.Repos[0].Ref != "v5.1.0" {
		t.Fatalf("expected ref v5.1.0, got %q", loaded.Repos[0].Ref)
	}

	chk, err := Check(context.Background(), lockPath, CheckOptions{SourceDir: srcDir})
	if err != nil {
		t.Fatalf("Check after sync: %v", err)
	}
	if chk.ChangedCount != 0 || chk.MissingCount != 0 || chk.UnchangedCount != 3 {
		t.Fatalf("expected all unchanged after sync, got %+v", chk)
	}
}

func TestRepoUpstreamLockValid(t *testing.T) {
	lockPath := filepath.Join("..", "..", "upstream", "upstream.lock.json")
	lock, err := LoadLock(lockPath)
	if err != nil {
		t.Fatalf("LoadLock(%s): %v", lockPath, err)
	}
	if len(lock.Repos) != 2 || len(lock.Entries) < 7 {
		t.Fatalf("unexpected repo/entry counts: repos=%d entries=%d", len(lock.Repos), len(lock.Entries))
	}
}

func TestR1SecurityGuards(t *testing.T) {
	h := sha256Hex([]byte("ok"))
	bad := makeTestLock(h, h, nil, h)
	bad.Repos, bad.Entries[0].Repo = []RepoSource{{Name: "../escape", Ref: "v1"}}, "../escape"
	if ValidateLock(bad) == nil {
		t.Fatal("expected ValidateLock to reject ../escape")
	}
	for _, p := range []string{"../../etc/passwd", "/abs", `a\b`, "."} {
		l := makeTestLock(h, h, nil, h)
		if l.Entries[0].Path = p; ValidateLock(l) == nil {
			t.Fatalf("expected ValidateLock to reject %q", p)
		}
	}
	for _, u := range []string{"http://127.0.0.1", "https://evil.example.com", "https://u:p@api.github.com"} {
		l := makeTestLock(h, h, nil, h)
		if l.Repos[0].RawBaseURL, l.Repos[0].APIBaseURL = u, u; ValidateLock(l) == nil {
			t.Fatalf("expected ValidateLock to reject %q", u)
		}
	}
	dir := t.TempDir()
	outside, gs, lockPath := filepath.Join(dir, "secret.txt"), filepath.Join(dir, "src", "gentle-shell"), filepath.Join(dir, "lock.json")
	_ = os.WriteFile(outside, []byte("TOP_SECRET"), 0o600)
	_ = os.MkdirAll(filepath.Join(gs, "assets", "agents"), 0o755)
	writeFixtureFile(t, gs, "lib/bounded-writer-admission.ts", "ok")
	_ = SaveLock(lockPath, makeTestLock(h, ComputeDirDigest(nil), nil, h))
	for _, target := range []string{filepath.Join(gs, "assets", "orchestrator.md"), filepath.Join(gs, "assets", "agents", "leak.md")} {
		if err := os.Symlink(outside, target); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		if _, err := Check(context.Background(), lockPath, CheckOptions{SourceDir: filepath.Join(dir, "src")}); err == nil {
			t.Fatalf("expected symlink rejection for %s", target)
		}
		_ = os.Remove(target)
		writeFixtureFile(t, gs, "assets/orchestrator.md", "ok")
	}
	evilFetched := false
	evilSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { evilFetched = true }))
	defer evilSrv.Close()
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "orchestrator.md") {
			_, _ = w.Write([]byte("ok"))
		} else if strings.HasSuffix(r.URL.Path, "agents") {
			_ = json.NewEncoder(w).Encode([]map[string]any{{"name": "w.md", "path": "assets/agents/w.md", "type": "file", "download_url": evilSrv.URL + "/meta"}})
		} else {
			http.Error(w, "SECRET_BODY_LEAK", http.StatusInternalServerError)
		}
	}))
	defer mockSrv.Close()
	if _, err := Check(context.Background(), lockPath, CheckOptions{BaseURL: mockSrv.URL, HTTPClient: mockSrv.Client()}); err == nil || evilFetched {
		t.Fatalf("expected cross-host download_url rejection without fetch (fetched=%v, err=%v)", evilFetched, err)
	}
	for i, u := range []string{mockSrv.URL + "/boom?token=topsecret", strings.Replace(mockSrv.URL, "http://", "http://alice:superpass@", 1) + "/boom?token=topsecret"} {
		if _, _, err := fetchBytes(context.Background(), mockSrv.Client(), u, mockSrv.URL); err == nil || strings.Contains(err.Error(), "SECRET_BODY_LEAK") || strings.Contains(err.Error(), "superpass") || strings.Contains(err.Error(), "topsecret") || (i == 0 && !strings.Contains(err.Error(), "HTTP 500")) {
			t.Fatalf("unexpected error for %s: %v", u, err)
		}
	}
}
