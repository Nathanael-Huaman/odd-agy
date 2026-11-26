package rdd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// emptyTreeHash is Git's well-known SHA-1 for the empty tree.
const emptyTreeHash = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

type candidateSnapshot struct {
	Files           []FrozenFile
	ChangedPaths    []string
	ChangedLines    int
	CandidateDigest string
	HasModeChange   bool
}

func resolveGitDir(repoRoot string) (string, error) {
	out, err := runGitCmd(repoRoot, "rev-parse", "--git-dir")
	if err != nil {
		return "", fmt.Errorf("not a git repository (%s): %w", repoRoot, err)
	}
	gitDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(repoRoot, gitDir)
	}
	return filepath.Clean(gitDir), nil
}

func gitHasHead(repoRoot string) bool {
	_, err := runGitCmd(repoRoot, "rev-parse", "--verify", "HEAD")
	return err == nil
}

func runGitCmd(repoRoot string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repoRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}

func collectCandidate(repoRoot, baseRef string, committedOnly bool) (candidateSnapshot, error) {
	if baseRef != "" && !committedOnly {
		return candidateSnapshot{}, fmt.Errorf("baseRef requires committedOnly=true")
	}

	var diffFrom string
	var diffTo string
	if committedOnly {
		if !gitHasHead(repoRoot) {
			return candidateSnapshot{}, fmt.Errorf("committedOnly requested on repository without HEAD")
		}
		diffTo = "HEAD"
		if baseRef != "" {
			if _, err := runGitCmd(repoRoot, "rev-parse", "--verify", baseRef+"^{commit}"); err != nil {
				return candidateSnapshot{}, fmt.Errorf("invalid baseRef %q: %w", baseRef, err)
			}
			diffFrom = baseRef
		} else if _, err := runGitCmd(repoRoot, "rev-parse", "--verify", "HEAD^"); err == nil {
			diffFrom = "HEAD^"
		} else {
			diffFrom = emptyTreeHash
		}
	} else {
		if gitHasHead(repoRoot) {
			diffFrom = "HEAD"
		} else {
			diffFrom = emptyTreeHash
		}
	}

	rawArgs := []string{"diff", "--raw", "--no-renames", "-z", diffFrom}
	numArgs := []string{"diff", "--numstat", "--no-renames", "-z", diffFrom}
	if committedOnly {
		rawArgs = append(rawArgs, diffTo)
		numArgs = append(numArgs, diffTo)
	}

	rawOut, err := runGitCmd(repoRoot, rawArgs...)
	if err != nil {
		return candidateSnapshot{}, err
	}
	numOut, err := runGitCmd(repoRoot, numArgs...)
	if err != nil {
		return candidateSnapshot{}, err
	}

	type rawEntry struct {
		oldMode string
		newMode string
		status  string
	}
	entries := make(map[string]rawEntry)
	parseRawDiffZ(rawOut, func(path, oldMode, newMode, status string) {
		entries[path] = rawEntry{oldMode: oldMode, newMode: newMode, status: status}
	})

	lineCounts := make(map[string]int)
	parseNumstatZ(numOut, func(path string, count int) {
		lineCounts[path] = count
	})

	filesMap := make(map[string]FrozenFile, len(entries))
	hasModeChange := false
	totalLines := 0

	for path, e := range entries {
		if isExecutableModeChange(e.oldMode, e.newMode, e.status) {
			hasModeChange = true
		}
		diffArgs := []string{"diff", "--no-color", "--no-ext-diff", diffFrom}
		if committedOnly {
			diffArgs = append(diffArgs, diffTo)
		}
		diffArgs = append(diffArgs, "--", path)
		diffBytes, err := runGitCmd(repoRoot, diffArgs...)
		if err != nil {
			return candidateSnapshot{}, err
		}

		var contentBytes []byte
		var contentHash string
		if e.status != "D" {
			if committedOnly {
				contentBytes, err = runGitCmd(repoRoot, "show", "HEAD:"+path)
				if err != nil {
					return candidateSnapshot{}, fmt.Errorf("read committed file %s: %w", path, err)
				}
			} else {
				contentBytes, err = readWorktreeEntry(filepath.Join(repoRoot, path), e.newMode)
				if err != nil {
					return candidateSnapshot{}, fmt.Errorf("read worktree file %s: %w", path, err)
				}
			}
			contentHash = sha256Hex(contentBytes)
		}

		lines := lineCounts[path]
		totalLines += lines
		filesMap[path] = FrozenFile{
			Path:          filepath.ToSlash(path),
			Status:        e.status,
			OldMode:       e.oldMode,
			NewMode:       e.newMode,
			Diff:          string(diffBytes),
			Content:       string(contentBytes),
			ContentSHA256: contentHash,
			ChangedLines:  lines,
		}
	}

	if !committedOnly {
		untrackedOut, err := runGitCmd(repoRoot, "ls-files", "--others", "--exclude-standard", "-z")
		if err != nil {
			return candidateSnapshot{}, err
		}
		for _, rawPath := range bytes.Split(untrackedOut, []byte{0}) {
			if len(rawPath) == 0 {
				continue
			}
			path := string(rawPath)
			if _, exists := filesMap[path]; exists {
				continue
			}
			fullPath := filepath.Join(repoRoot, path)
			info, err := os.Lstat(fullPath)
			if err != nil {
				return candidateSnapshot{}, fmt.Errorf("stat untracked file %s: %w", path, err)
			}
			mode := "100644"
			if info.Mode()&os.ModeSymlink != 0 {
				mode = "120000"
				hasModeChange = true
			} else if info.Mode()&0o111 != 0 {
				mode = "100755"
				hasModeChange = true
			}
			contentBytes, err := readWorktreeEntry(fullPath, mode)
			if err != nil {
				return candidateSnapshot{}, fmt.Errorf("read untracked file %s: %w", path, err)
			}
			lines := countTextLines(contentBytes)
			totalLines += lines
			filesMap[path] = FrozenFile{
				Path:          filepath.ToSlash(path),
				Status:        "A",
				OldMode:       "000000",
				NewMode:       mode,
				Diff:          buildAddedFileDiff(filepath.ToSlash(path), contentBytes),
				Content:       string(contentBytes),
				ContentSHA256: sha256Hex(contentBytes),
				ChangedLines:  lines,
			}
		}
	}

	paths := make([]string, 0, len(filesMap))
	for p := range filesMap {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	files := make([]FrozenFile, 0, len(paths))
	changedPaths := make([]string, 0, len(paths))
	for _, p := range paths {
		f := filesMap[p]
		files = append(files, f)
		changedPaths = append(changedPaths, f.Path)
	}

	digest, err := computeCandidateDigest(files)
	if err != nil {
		return candidateSnapshot{}, err
	}

	return candidateSnapshot{
		Files:           files,
		ChangedPaths:    changedPaths,
		ChangedLines:    totalLines,
		CandidateDigest: digest,
		HasModeChange:   hasModeChange,
	}, nil
}

func readWorktreeEntry(fullPath, mode string) ([]byte, error) {
	if mode == "120000" {
		target, err := os.Readlink(fullPath)
		if err != nil {
			return nil, err
		}
		return []byte(target), nil
	}
	return os.ReadFile(fullPath)
}

func parseRawDiffZ(raw []byte, fn func(path, oldMode, newMode, status string)) {
	tokens := bytes.Split(raw, []byte{0})
	for i := 0; i < len(tokens); i++ {
		tok := string(tokens[i])
		if tok == "" || !strings.HasPrefix(tok, ":") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(tok, ":"))
		if len(fields) < 5 || i+1 >= len(tokens) {
			continue
		}
		oldMode := fields[0]
		newMode := fields[1]
		status := fields[4]
		path := string(tokens[i+1])
		i++
		fn(path, oldMode, newMode, status)
	}
}

func parseNumstatZ(raw []byte, fn func(path string, count int)) {
	records := bytes.Split(raw, []byte{0})
	for _, rec := range records {
		if len(rec) == 0 {
			continue
		}
		parts := strings.SplitN(string(rec), "\t", 3)
		if len(parts) != 3 {
			continue
		}
		added, errA := strconv.Atoi(parts[0])
		deleted, errD := strconv.Atoi(parts[1])
		total := 0
		if errA == nil {
			total += added
		}
		if errD == nil {
			total += deleted
		}
		if parts[0] == "-" || parts[1] == "-" {
			total++
		}
		fn(parts[2], total)
	}
}

func isExecutableModeChange(oldMode, newMode, status string) bool {
	if newMode == "100755" && oldMode != "100755" {
		return true
	}
	if oldMode == "100755" && newMode == "100644" {
		return true
	}
	if newMode == "120000" || oldMode == "120000" {
		return status != ""
	}
	return false
}

func countTextLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte{'\n'})
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

func buildAddedFileDiff(path string, content []byte) string {
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	if len(content) == 0 {
		return fmt.Sprintf("diff --git a/%s b/%s\nnew file mode 100644\n--- /dev/null\n+++ b/%s\n", path, path, path)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\nnew file mode 100644\n--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", path, path, path, len(lines))
	for _, line := range lines {
		b.WriteByte('+')
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func computeCandidateDigest(files []FrozenFile) (string, error) {
	type digestEntry struct {
		Path          string `json:"path"`
		Status        string `json:"status"`
		OldMode       string `json:"oldMode,omitempty"`
		NewMode       string `json:"newMode,omitempty"`
		Diff          string `json:"diff"`
		ContentSHA256 string `json:"contentSha256,omitempty"`
	}
	items := make([]digestEntry, 0, len(files))
	for _, f := range files {
		items = append(items, digestEntry{
			Path:          f.Path,
			Status:        f.Status,
			OldMode:       f.OldMode,
			NewMode:       f.NewMode,
			Diff:          f.Diff,
			ContentSHA256: f.ContentSHA256,
		})
	}
	data, err := json.Marshal(items)
	if err != nil {
		return "", fmt.Errorf("marshal candidate digest input: %w", err)
	}
	return sha256Hex(data), nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func classifyRisk(snap candidateSnapshot) (string, string) {
	if len(snap.Files) == 0 {
		return RiskPassive, "no changed files"
	}
	if snap.HasModeChange {
		return RiskHigh, "executable or symlink mode change detected"
	}

	allPassive := true
	for _, f := range snap.Files {
		if isHighRiskPath(f.Path) {
			return RiskHigh, fmt.Sprintf("sensitive/config/workflow path changed: %s", f.Path)
		}
		if !isPassivePath(f.Path) {
			allPassive = false
		}
	}

	if snap.ChangedLines > 200 || len(snap.Files) > 10 {
		return RiskHigh, fmt.Sprintf("large change (%d lines across %d files)", snap.ChangedLines, len(snap.Files))
	}
	if allPassive {
		return RiskPassive, "documentation/passive-only changes"
	}
	return RiskMedium, fmt.Sprintf("code or behavior changes (%d lines across %d files)", snap.ChangedLines, len(snap.Files))
}

func isHighRiskPath(p string) bool {
	lower := strings.ToLower(filepath.ToSlash(p))
	base := filepath.Base(lower)
	if strings.HasPrefix(lower, ".github/workflows/") ||
		strings.HasPrefix(lower, ".agents/") ||
		strings.HasPrefix(lower, ".claude/") ||
		strings.HasPrefix(lower, ".gemini/") ||
		strings.Contains(lower, ".gitlab-ci") {
		return true
	}
	switch base {
	case "dockerfile", "docker-compose.yml", "docker-compose.yaml", "makefile",
		"go.mod", "go.sum", "package.json", "pnpm-lock.yaml", "package-lock.json",
		".env", ".env.local", ".env.example":
		return true
	}
	sensitiveKeywords := []string{
		"auth", "security", "secret", "token", "credential", "passwd",
		"crypto", "tls", "cert", "permission", "policy", "sandbox",
		"hook", "config", "mcp", "rdd", ".ssh",
	}
	for _, kw := range sensitiveKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func isPassivePath(p string) bool {
	lower := strings.ToLower(filepath.ToSlash(p))
	base := filepath.Base(lower)
	ext := filepath.Ext(lower)
	if base == "license" || base == "notice" {
		return true
	}
	if ext == ".md" || ext == ".txt" || ext == ".rst" {
		return true
	}
	return false
}
