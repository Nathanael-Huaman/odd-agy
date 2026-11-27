package upstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Nathanael-Huaman/odd-agy/internal/assets"
)

const (
	LockSchemaVersion = 1

	CategoryPromptsAndSkills = "prompts-and-skills"
	CategoryEnforcementCode  = "enforcement-code"

	StatusUnchanged = "unchanged"
	StatusChanged   = "changed"
	StatusMissing   = "missing"
)

var sha256Pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var repoSegPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// RepoSource identifies a tracked upstream GitHub repository and ref.
type RepoSource struct {
	Name       string `json:"name"`
	Ref        string `json:"ref"`
	RawBaseURL string `json:"rawBaseUrl,omitempty"`
	APIBaseURL string `json:"apiBaseUrl,omitempty"`
}

// TrackedEntry records one upstream file or directory path and its baseline SHA-256 digest.
type TrackedEntry struct {
	ID           string            `json:"id"`
	Repo         string            `json:"repo"`
	Path         string            `json:"path"`
	Category     string            `json:"category"`
	SHA256       string            `json:"sha256"`
	Files        map[string]string `json:"files,omitempty"`
	LocalTargets []string          `json:"localTargets,omitempty"`
}

// LockFile is the persisted upstream tracking manifest at upstream/upstream.lock.json.
type LockFile struct {
	SchemaVersion int            `json:"schemaVersion"`
	UpdatedAt     string         `json:"updatedAt"`
	Repos         []RepoSource   `json:"repos"`
	Entries       []TrackedEntry `json:"entries"`
}

// CheckOptions configures offline (--source-dir) or remote HTTP upstream verification.
type CheckOptions struct {
	SourceDir  string
	Ref        string
	BaseURL    string
	HTTPClient *http.Client
}

// SyncOptions configures upstream lock updates and post-sync contract validation.
type SyncOptions struct {
	SourceDir    string
	Ref          string
	BaseURL      string
	HTTPClient   *http.Client
	ValidateFunc func() error
}

// EntryResult reports the check status of one tracked entry.
type EntryResult struct {
	ID           string   `json:"id"`
	Repo         string   `json:"repo"`
	Path         string   `json:"path"`
	Category     string   `json:"category"`
	Status       string   `json:"status"`
	ExpectedHash string   `json:"expectedSha256"`
	ActualHash   string   `json:"actualSha256,omitempty"`
	ChangedFiles []string `json:"changedFiles,omitempty"`
	MissingFiles []string `json:"missingFiles,omitempty"`
	LocalTargets []string `json:"localTargets,omitempty"`
}

// CheckReport summarizes drift across prompts/skills vs enforcement code.
type CheckReport struct {
	LockPath         string        `json:"lockPath"`
	Source           string        `json:"source"`
	Ref              string        `json:"ref,omitempty"`
	PromptsAndSkills []EntryResult `json:"promptsAndSkills"`
	EnforcementCode  []EntryResult `json:"enforcementCode"`
	ChangedCount     int           `json:"changedCount"`
	MissingCount     int           `json:"missingCount"`
	UnchangedCount   int           `json:"unchangedCount"`
}

// SyncReport summarizes a completed upstream lock sync.
type SyncReport struct {
	LockPath       string        `json:"lockPath"`
	Source         string        `json:"source"`
	Ref            string        `json:"ref,omitempty"`
	UpdatedCount   int           `json:"updatedCount"`
	UnchangedCount int           `json:"unchangedCount"`
	Validated      bool          `json:"validated"`
	Entries        []EntryResult `json:"entries"`
}

// LoadLock reads and validates an upstream lock file.
func LoadLock(lockPath string) (LockFile, error) {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return LockFile{}, fmt.Errorf("read lock %s: %w", lockPath, err)
	}
	var lock LockFile
	if err := json.Unmarshal(data, &lock); err != nil {
		return LockFile{}, fmt.Errorf("parse lock %s: %w", lockPath, err)
	}
	if err := ValidateLock(lock); err != nil {
		return LockFile{}, fmt.Errorf("validate lock %s: %w", lockPath, err)
	}
	return lock, nil
}

// SaveLock validates and atomically writes an upstream lock file.
func SaveLock(lockPath string, lock LockFile) error {
	if err := ValidateLock(lock); err != nil {
		return err
	}
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal lock: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(lockPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create lock dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-upstream-*.json")
	if err != nil {
		return fmt.Errorf("create temp lock file: %w", err)
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
		return fmt.Errorf("write temp lock: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp lock: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp lock: %w", err)
	}
	if err := os.Rename(tmpName, lockPath); err != nil {
		return fmt.Errorf("rename lock: %w", err)
	}
	cleanup = false
	return nil
}

// ValidateLock checks schema invariants on LockFile.
func ValidateLock(lock LockFile) error {
	if lock.SchemaVersion != LockSchemaVersion {
		return fmt.Errorf("invalid schemaVersion %d: must be %d", lock.SchemaVersion, LockSchemaVersion)
	}
	if len(lock.Repos) == 0 {
		return errors.New("lock must declare at least one repo")
	}
	repoSet := make(map[string]RepoSource, len(lock.Repos))
	for _, r := range lock.Repos {
		name := strings.TrimSpace(r.Name)
		if err := validateRepoName(r.Name); err != nil {
			return err
		}
		if strings.TrimSpace(r.Ref) == "" {
			return fmt.Errorf("repo %q ref must not be empty", name)
		}
		for _, raw := range []string{r.RawBaseURL, r.APIBaseURL} {
			if _, err := validateRemoteURL(raw, ""); raw != "" && err != nil {
				return fmt.Errorf("repo %q invalid base url: %w", name, err)
			}
		}
		repoSet[name] = r
	}
	if len(lock.Entries) == 0 {
		return errors.New("lock must declare at least one tracked entry")
	}
	seenID := make(map[string]bool, len(lock.Entries))
	for _, e := range lock.Entries {
		if strings.TrimSpace(e.ID) == "" {
			return errors.New("entry id must not be empty")
		}
		if seenID[e.ID] {
			return fmt.Errorf("duplicate entry id %q", e.ID)
		}
		seenID[e.ID] = true

		if _, ok := repoSet[e.Repo]; !ok {
			return fmt.Errorf("entry %q references unknown repo %q", e.ID, e.Repo)
		}
		if err := validateEntryPath(e.Path); err != nil {
			return fmt.Errorf("entry %q invalid path %q: %w", e.ID, e.Path, err)
		}
		switch e.Category {
		case CategoryPromptsAndSkills, CategoryEnforcementCode:
		default:
			return fmt.Errorf("entry %q invalid category %q (must be %q or %q)",
				e.ID, e.Category, CategoryPromptsAndSkills, CategoryEnforcementCode)
		}
		if !sha256Pattern.MatchString(e.SHA256) {
			return fmt.Errorf("entry %q invalid sha256 %q", e.ID, e.SHA256)
		}
		for rel, digest := range e.Files {
			if strings.TrimSpace(rel) == "" {
				return fmt.Errorf("entry %q has empty relative file key", e.ID)
			}
			if !sha256Pattern.MatchString(digest) {
				return fmt.Errorf("entry %q file %q invalid sha256 %q", e.ID, rel, digest)
			}
		}
	}
	return nil
}

// ComputeDirDigest computes a deterministic aggregate sha256:<hex> over sorted relative paths.
func ComputeDirDigest(files map[string]string) string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		_, _ = io.WriteString(h, k)
		_, _ = h.Write([]byte{0})
		_, _ = io.WriteString(h, files[k])
		_, _ = h.Write([]byte{'\n'})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func sha256Prefixed(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type inspectedEntry struct {
	result EntryResult
	files  map[string]string
}

// Check verifies tracked upstream entries against SourceDir or remote HTTP endpoints.
func Check(ctx context.Context, lockPath string, opts CheckOptions) (CheckReport, error) {
	lock, err := LoadLock(lockPath)
	if err != nil {
		return CheckReport{}, err
	}
	inspected, sourceDesc, err := inspectAll(ctx, lock, opts)
	if err != nil {
		return CheckReport{}, err
	}

	rep := CheckReport{
		LockPath: lockPath,
		Source:   sourceDesc,
		Ref:      strings.TrimSpace(opts.Ref),
	}
	for _, item := range inspected {
		res := item.result
		switch res.Status {
		case StatusUnchanged:
			rep.UnchangedCount++
		case StatusChanged:
			rep.ChangedCount++
		case StatusMissing:
			rep.MissingCount++
		}
		if res.Category == CategoryPromptsAndSkills {
			rep.PromptsAndSkills = append(rep.PromptsAndSkills, res)
		} else {
			rep.EnforcementCode = append(rep.EnforcementCode, res)
		}
	}
	return rep, nil
}

// Sync updates lock digests and ref from a verified SourceDir or remote endpoint,
// running contract validation before persisting the updated lock file.
func Sync(ctx context.Context, lockPath string, opts SyncOptions) (SyncReport, error) {
	lock, err := LoadLock(lockPath)
	if err != nil {
		return SyncReport{}, err
	}

	checkOpts := CheckOptions{
		SourceDir:  opts.SourceDir,
		Ref:        opts.Ref,
		BaseURL:    opts.BaseURL,
		HTTPClient: opts.HTTPClient,
	}
	inspected, sourceDesc, err := inspectAll(ctx, lock, checkOpts)
	if err != nil {
		return SyncReport{}, err
	}

	for _, item := range inspected {
		if item.result.Status == StatusMissing {
			return SyncReport{}, fmt.Errorf("cannot sync: upstream path %q (%s) is missing in %s",
				item.result.Path, item.result.Repo, sourceDesc)
		}
	}

	validateFn := opts.ValidateFunc
	if validateFn == nil {
		validateFn = assets.ValidateAll
	}
	if err := validateFn(); err != nil {
		return SyncReport{}, fmt.Errorf("post-sync contract validation failed (lock left untouched): %w", err)
	}

	updated := lock
	updated.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if ref := strings.TrimSpace(opts.Ref); ref != "" {
		repos := make([]RepoSource, len(lock.Repos))
		copy(repos, lock.Repos)
		for i := range repos {
			repos[i].Ref = ref
		}
		updated.Repos = repos
	}

	entries := make([]TrackedEntry, len(lock.Entries))
	copy(entries, lock.Entries)
	results := make([]EntryResult, 0, len(inspected))
	updatedCount := 0
	unchangedCount := 0

	for i, item := range inspected {
		results = append(results, item.result)
		if item.result.Status == StatusChanged {
			updatedCount++
		} else {
			unchangedCount++
		}
		entries[i].SHA256 = item.result.ActualHash
		if strings.HasSuffix(entries[i].Path, "/") {
			entries[i].Files = item.files
		} else {
			entries[i].Files = nil
		}
	}
	updated.Entries = entries

	if err := SaveLock(lockPath, updated); err != nil {
		return SyncReport{}, err
	}

	return SyncReport{
		LockPath:       lockPath,
		Source:         sourceDesc,
		Ref:            strings.TrimSpace(opts.Ref),
		UpdatedCount:   updatedCount,
		UnchangedCount: unchangedCount,
		Validated:      true,
		Entries:        results,
	}, nil
}

func inspectAll(ctx context.Context, lock LockFile, opts CheckOptions) ([]inspectedEntry, string, error) {
	reposByName := make(map[string]RepoSource, len(lock.Repos))
	for _, r := range lock.Repos {
		reposByName[r.Name] = r
	}

	sourceDir := strings.TrimSpace(opts.SourceDir)
	sourceDesc := "remote"
	if sourceDir != "" {
		abs, err := filepath.Abs(sourceDir)
		if err != nil {
			return nil, "", fmt.Errorf("resolve source-dir %q: %w", sourceDir, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, "", fmt.Errorf("stat source-dir %q: %w", abs, err)
		}
		if !info.IsDir() {
			return nil, "", fmt.Errorf("source-dir %q is not a directory", abs)
		}
		sourceDir = abs
		sourceDesc = abs
	} else if strings.TrimSpace(opts.BaseURL) != "" {
		sourceDesc = strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	out := make([]inspectedEntry, 0, len(lock.Entries))
	for _, entry := range lock.Entries {
		repo := reposByName[entry.Repo]
		ref := strings.TrimSpace(opts.Ref)
		if ref == "" {
			ref = repo.Ref
		}

		var item inspectedEntry
		var err error
		if sourceDir != "" {
			item, err = inspectLocalEntry(sourceDir, entry)
		} else {
			item, err = inspectRemoteEntry(ctx, httpClient, opts.BaseURL, repo, ref, entry)
		}
		if err != nil {
			return nil, "", err
		}
		out = append(out, item)
	}
	return out, sourceDesc, nil
}

func inspectLocalEntry(sourceDir string, entry TrackedEntry) (inspectedEntry, error) {
	if err := errors.Join(validateRepoName(entry.Repo), validateEntryPath(entry.Path)); err != nil {
		return inspectedEntry{}, err
	}
	sourceDir, err := filepath.EvalSymlinks(sourceDir)
	if err != nil {
		return inspectedEntry{}, fmt.Errorf("resolve sourceDir: %w", err)
	}
	repoShort := path.Base(entry.Repo)
	relClean := strings.TrimSuffix(filepath.FromSlash(entry.Path), string(filepath.Separator))

	candidateRepoScoped := filepath.Join(sourceDir, repoShort, relClean)
	candidateDirect := filepath.Join(sourceDir, relClean)

	target := candidateDirect
	if _, err := os.Lstat(candidateRepoScoped); err == nil {
		target = candidateRepoScoped
	}

	info, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return missingEntry(entry), nil
		}
		return inspectedEntry{}, fmt.Errorf("lstat %s: %w", target, err)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return inspectedEntry{}, fmt.Errorf("refusing symlink %s", target)
	}
	if rel, err := filepath.Rel(sourceDir, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return inspectedEntry{}, fmt.Errorf("path %s escapes sourceDir", target)
	}

	if strings.HasSuffix(entry.Path, "/") {
		if !info.IsDir() {
			return inspectedEntry{}, fmt.Errorf("expected directory for %s at %s", entry.Path, target)
		}
		files := make(map[string]string)
		err := filepath.WalkDir(target, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if linfo, lerr := os.Lstat(p); lerr != nil || d.Type()&os.ModeSymlink != 0 || linfo.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing symlink %s", p)
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(target, p)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return fmt.Errorf("read %s: %w", p, err)
			}
			files[filepath.ToSlash(rel)] = sha256Prefixed(data)
			return nil
		})
		if err != nil {
			return inspectedEntry{}, fmt.Errorf("walk %s: %w", target, err)
		}
		return buildDirEntryResult(entry, files), nil
	}

	if info.IsDir() {
		return inspectedEntry{}, fmt.Errorf("expected file for %s, found directory at %s", entry.Path, target)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return inspectedEntry{}, fmt.Errorf("read %s: %w", target, err)
	}
	return buildFileEntryResult(entry, sha256Prefixed(data)), nil
}

type githubContentItem struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Type        string `json:"type"`
	DownloadURL string `json:"download_url"`
}

func inspectRemoteEntry(
	ctx context.Context,
	client *http.Client,
	baseOverride string,
	repo RepoSource,
	ref string,
	entry TrackedEntry,
) (inspectedEntry, error) {
	rawBase := strings.TrimRight(repo.RawBaseURL, "/")
	if rawBase == "" {
		rawBase = "https://raw.githubusercontent.com/" + repo.Name
	}
	apiBase := strings.TrimRight(repo.APIBaseURL, "/")
	if apiBase == "" {
		apiBase = "https://api.github.com/repos/" + repo.Name
	}
	if strings.TrimSpace(baseOverride) != "" {
		b := strings.TrimRight(strings.TrimSpace(baseOverride), "/")
		rawBase = b + "/raw/" + repo.Name
		apiBase = b + "/repos/" + repo.Name
	}

	if strings.HasSuffix(entry.Path, "/") {
		rootRel := strings.TrimSuffix(entry.Path, "/")
		files := make(map[string]string)
		found, err := walkRemoteDir(ctx, client, baseOverride, rawBase, apiBase, ref, rootRel, rootRel, files)
		if err != nil {
			return inspectedEntry{}, err
		}
		if !found {
			return missingEntry(entry), nil
		}
		return buildDirEntryResult(entry, files), nil
	}

	fileURL := fmt.Sprintf("%s/%s/%s", rawBase, url.PathEscape(ref), strings.TrimPrefix(entry.Path, "/"))
	data, found, err := fetchBytes(ctx, client, fileURL, baseOverride)
	if err != nil {
		return inspectedEntry{}, err
	}
	if !found {
		return missingEntry(entry), nil
	}
	return buildFileEntryResult(entry, sha256Prefixed(data)), nil
}

func walkRemoteDir(
	ctx context.Context,
	client *http.Client,
	baseOverride, rawBase, apiBase, ref, rootRel, currentRel string,
	files map[string]string,
) (bool, error) {
	contentsURL := fmt.Sprintf("%s/contents/%s?ref=%s",
		apiBase, strings.TrimPrefix(currentRel, "/"), url.QueryEscape(ref))
	body, found, err := fetchBytes(ctx, client, contentsURL, baseOverride)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}

	var items []githubContentItem
	if err := json.Unmarshal(body, &items); err != nil {
		return false, fmt.Errorf("parse contents response for %s: %w", currentRel, err)
	}

	for _, item := range items {
		switch item.Type {
		case "dir":
			if _, err := walkRemoteDir(ctx, client, baseOverride, rawBase, apiBase, ref, rootRel, item.Path, files); err != nil {
				return false, err
			}
		case "file":
			dlURL := strings.TrimSpace(item.DownloadURL)
			if dlURL == "" {
				dlURL = fmt.Sprintf("%s/%s/%s", rawBase, url.PathEscape(ref), strings.TrimPrefix(item.Path, "/"))
			}
			data, ok, err := fetchBytes(ctx, client, dlURL, baseOverride)
			if err != nil {
				return false, err
			}
			if !ok {
				continue
			}
			rel := strings.TrimPrefix(item.Path, strings.TrimSuffix(rootRel, "/")+"/")
			if rel == item.Path && item.Name != "" {
				rel = item.Name
			}
			files[rel] = sha256Prefixed(data)
		}
	}
	return true, nil
}

func fetchBytes(ctx context.Context, client *http.Client, targetURL, baseOverride string) ([]byte, bool, error) {
	safeURL, err := validateRemoteURL(targetURL, baseOverride)
	if err != nil {
		return nil, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, false, fmt.Errorf("build request %s: invalid URL", safeURL)
	}
	req.Header.Set("User-Agent", "odd-agy-upstream-sync")
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("fetch %s failed", safeURL)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("fetch %s returned HTTP %d", safeURL, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", safeURL, err)
	}
	return data, true, nil
}

func validateRepoName(repo string) error {
	if p := strings.Split(repo, "/"); len(p) != 2 || p[0] == "." || p[0] == ".." || p[1] == "." || p[1] == ".." || !repoSegPattern.MatchString(p[0]) || !repoSegPattern.MatchString(p[1]) {
		return fmt.Errorf("invalid repo %q", repo)
	}
	return nil
}

func validateEntryPath(p string) error {
	if t := strings.TrimSuffix(p, "/"); p == "" || strings.TrimSpace(p) != p || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || t == "" || t == "." || t == ".." || strings.HasPrefix(t, "../") || path.Clean(t) != t {
		return errors.New("invalid relative path")
	}
	return nil
}

func validateRemoteURL(targetURL, baseOverride string) (string, error) {
	u, err := url.Parse(targetURL)
	if err != nil {
		return "", errors.New("invalid remote URL")
	}
	hasUser := u.User != nil
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	safe := u.String()
	if hasUser {
		return safe, fmt.Errorf("remote URL %s must not contain userinfo", safe)
	}
	if b := strings.TrimSpace(baseOverride); b != "" {
		if bu, err := url.Parse(b); err != nil || bu.User != nil || bu.Scheme == "" || bu.Host == "" || u.Scheme != bu.Scheme || u.Host != bu.Host {
			return safe, fmt.Errorf("remote URL %s outside baseURL", safe)
		}
		return safe, nil
	}
	if h := strings.ToLower(u.Host); !strings.EqualFold(u.Scheme, "https") || (h != "raw.githubusercontent.com" && h != "api.github.com" && h != "github.com") {
		return safe, fmt.Errorf("remote URL %s not allowlisted", safe)
	}
	return safe, nil
}

func missingEntry(entry TrackedEntry) inspectedEntry {
	return inspectedEntry{
		result: EntryResult{
			ID:           entry.ID,
			Repo:         entry.Repo,
			Path:         entry.Path,
			Category:     entry.Category,
			Status:       StatusMissing,
			ExpectedHash: entry.SHA256,
			LocalTargets: append([]string(nil), entry.LocalTargets...),
		},
	}
}

func buildFileEntryResult(entry TrackedEntry, actualHash string) inspectedEntry {
	status := StatusUnchanged
	if actualHash != entry.SHA256 {
		status = StatusChanged
	}
	return inspectedEntry{
		result: EntryResult{
			ID:           entry.ID,
			Repo:         entry.Repo,
			Path:         entry.Path,
			Category:     entry.Category,
			Status:       status,
			ExpectedHash: entry.SHA256,
			ActualHash:   actualHash,
			LocalTargets: append([]string(nil), entry.LocalTargets...),
		},
	}
}

func buildDirEntryResult(entry TrackedEntry, actualFiles map[string]string) inspectedEntry {
	actualHash := ComputeDirDigest(actualFiles)
	status := StatusUnchanged
	var changedFiles []string
	var missingFiles []string

	if actualHash != entry.SHA256 {
		status = StatusChanged
		for rel, expDigest := range entry.Files {
			actDigest, ok := actualFiles[rel]
			if !ok {
				missingFiles = append(missingFiles, rel)
			} else if actDigest != expDigest {
				changedFiles = append(changedFiles, rel)
			}
		}
		for rel := range actualFiles {
			if _, ok := entry.Files[rel]; !ok {
				changedFiles = append(changedFiles, rel)
			}
		}
		sort.Strings(changedFiles)
		sort.Strings(missingFiles)
	}

	return inspectedEntry{
		result: EntryResult{
			ID:           entry.ID,
			Repo:         entry.Repo,
			Path:         entry.Path,
			Category:     entry.Category,
			Status:       status,
			ExpectedHash: entry.SHA256,
			ActualHash:   actualHash,
			ChangedFiles: changedFiles,
			MissingFiles: missingFiles,
			LocalTargets: append([]string(nil), entry.LocalTargets...),
		},
		files: actualFiles,
	}
}
