package hooks

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Nathanael-Huaman/odd-agy/internal/config"
	"github.com/Nathanael-Huaman/odd-agy/internal/rdd"
)

type subagentEntry struct {
	Prompt    string
	Role      string
	TypeName  string
	Workspace string
}

var (
	specLinePattern   = regexp.MustCompile(`(?i)\bSpec:\s*(odd/tasks/[^\s,)\]]+\.md)\s*,\s*T\d+\b`)
	riskLinePattern   = regexp.MustCompile(`(?i)\bRisk:\s*item\s+([1-6])\b`)
	reReviewLineageID = regexp.MustCompile(`\breview-[0-9a-f]+\b`)
)

func parseSubagents(args map[string]any) []subagentEntry {
	if args == nil {
		return nil
	}
	raw, ok := lookupArg(args, "Subagents")
	if !ok || raw == nil {
		return nil
	}
	slice, ok := raw.([]any)
	if !ok {
		return nil
	}

	out := make([]subagentEntry, 0, len(slice))
	for _, item := range slice {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, subagentEntry{
			Prompt:    getStringArg(m, "Prompt"),
			Role:      getStringArg(m, "Role"),
			TypeName:  getStringArg(m, "TypeName"),
			Workspace: getStringArg(m, "Workspace"),
		})
	}
	return out
}

func (s subagentEntry) effectiveRole() string {
	if r := strings.ToLower(strings.TrimSpace(s.Role)); r != "" {
		return r
	}
	return strings.ToLower(strings.TrimSpace(s.TypeName))
}

func isWriterRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "gentle-ai-worker", "worker", "jd-fix-agent":
		return true
	default:
		return false
	}
}

func isVerifierRole(role string) bool {
	return strings.ToLower(strings.TrimSpace(role)) == "gentle-ai-verify"
}

func profileRoleForSubagent(agentName string) string {
	switch strings.ToLower(strings.TrimSpace(agentName)) {
	case "gentle-ai-explore", "explore":
		return "explore"
	case "gentle-ai-worker", "worker":
		return "worker"
	case "gentle-ai-verify", "verify":
		return "verify"
	case "jd-judge-a", "jd-judge-b", "jd-fix-agent",
		"review-risk", "review-resilience", "review-readability", "review-reliability", "review-refuter":
		return strings.ToLower(strings.TrimSpace(agentName))
	default:
		return ""
	}
}

func isBranchWorkspace(ws string) bool {
	return strings.EqualFold(strings.TrimSpace(ws), "branch")
}

func admissionKey(conversationID string, stepIdx int) string {
	conv := strings.TrimSpace(conversationID)
	if conv == "" {
		conv = "default"
	}
	return fmt.Sprintf("%s#%d", conv, stepIdx)
}

func checkInvokeSubagent(workspace string, cfg config.Config, in PreToolUseInput) (PreToolUseOutput, error) {
	entries := parseSubagents(in.ToolCall.Args)
	if len(entries) == 0 {
		return PreToolUseOutput{
			Decision: DecisionDeny,
			Reason:   "invoke_subagent requires a non-empty Subagents array",
		}, nil
	}

	if profs, err := config.LoadProfiles(workspace); err == nil {
		if pf, ok := profs.Profiles[profs.ActiveProfile]; ok {
			for _, entry := range entries {
				profileRole := profileRoleForSubagent(entry.effectiveRole())
				if profileRole == "" {
					continue
				}
				ra, ok := pf.Roles[profileRole]
				if !ok || ra.Runtime != config.RuntimeExternalMCP {
					continue
				}
				if strings.Contains(strings.ToLower(entry.Prompt), "fallback_to_native") {
					continue
				}
				model := ra.Model
				if model == "" {
					if prov, hasProv := profs.Providers[ra.Provider]; hasProv {
						model = prov.DefaultModel
					}
				}
				return PreToolUseOutput{
					Decision: DecisionDeny,
					Reason:   fmt.Sprintf("Active profile %q routes role %q to external-mcp (%s/%s). Call MCP tool odd-agy/odd_external_run with role=%q instead of invoke_subagent (only fall back to native invoke_subagent after odd_external_run returns fallback_to_native and mention fallback_to_native in the prompt).", profs.ActiveProfile, profileRole, ra.Provider, model, profileRole),
				}, nil
			}
		}
	}

	if denyReason, err := checkNativeReviewBatch(workspace, entries); err != nil {
		return PreToolUseOutput{}, err
	} else if denyReason != "" {
		return PreToolUseOutput{
			Decision: DecisionDeny,
			Reason:   denyReason,
		}, nil
	}

	type parsedWriter struct {
		entry    subagentEntry
		surfaces []string
		hasSpec  bool
	}
	var writers []parsedWriter

	for idx, entry := range entries {
		role := entry.effectiveRole()
		if isVerifierRole(role) {
			if err := validateVerifierPrompt(workspace, entry.Prompt); err != nil {
				return PreToolUseOutput{
					Decision: DecisionDeny,
					Reason:   fmt.Sprintf("subagent[%d] (%s): %v", idx, role, err),
				}, nil
			}
			continue
		}
		if !isWriterRole(role) {
			continue
		}

		surfaces, err := extractAllowedEditSurfaces(entry.Prompt)
		if err != nil {
			return PreToolUseOutput{
				Decision: DecisionDeny,
				Reason:   fmt.Sprintf("subagent[%d] (%s): %v", idx, role, err),
			}, nil
		}

		hasSpec, specErr := validateOptionalSpec(workspace, entry.Prompt)
		if specErr != nil {
			return PreToolUseOutput{
				Decision: DecisionDeny,
				Reason:   fmt.Sprintf("subagent[%d] (%s): %v", idx, role, specErr),
			}, nil
		}

		writers = append(writers, parsedWriter{
			entry:    entry,
			surfaces: surfaces,
			hasSpec:  hasSpec,
		})
	}

	// Organic mode: single generic writer requires Spec: odd/tasks/<existing>.md, T<n>.
	// 2+ disjoint writers in the same Subagents array are allowed without Spec:.
	if cfg.DelegationMode == config.DelegationModeOrganic && len(writers) == 1 && !writers[0].hasSpec {
		return PreToolUseOutput{
			Decision: DecisionDeny,
			Reason:   fmt.Sprintf("organic mode requires single writer (%s) to include 'Spec: odd/tasks/<existing>.md, T<n>' (or invoke 2+ disjoint parallel writers)", writers[0].entry.effectiveRole()),
		}, nil
	}

	// Check intra-batch overlap among non-branch writers.
	for i := 0; i < len(writers); i++ {
		if isBranchWorkspace(writers[i].entry.Workspace) {
			continue
		}
		for j := i + 1; j < len(writers); j++ {
			if isBranchWorkspace(writers[j].entry.Workspace) {
				continue
			}
			if a, b, overlap := findSurfaceOverlap(writers[i].surfaces, writers[j].surfaces); overlap {
				return PreToolUseOutput{
					Decision: DecisionDeny,
					Reason:   fmt.Sprintf("overlapping Allowed edit surfaces in invoke_subagent batch (%q vs %q); split into disjoint surfaces or set Workspace: \"branch\"", a, b),
				}, nil
			}
		}
	}

	// Check against currently active non-branch writer surfaces, and record if allowed.
	var nonBranchSurfaces []string
	for _, w := range writers {
		if !isBranchWorkspace(w.entry.Workspace) {
			nonBranchSurfaces = append(nonBranchSurfaces, w.surfaces...)
		}
	}

	if len(nonBranchSurfaces) > 0 {
		var denyReason string
		key := admissionKey(in.ConversationID, in.StepIdx)
		if _, err := updateHookState(workspace, func(st *HookState) error {
			for activeKey, activeSurfaces := range st.ActiveWriterSurfaces {
				if a, b, overlap := findSurfaceOverlap(nonBranchSurfaces, activeSurfaces); overlap {
					denyReason = fmt.Sprintf("writer surface %q overlaps active writer lock %s (%q); wait for completion or set Workspace: \"branch\"", a, activeKey, b)
					return nil
				}
			}
			st.ActiveWriterSurfaces[key] = append(st.ActiveWriterSurfaces[key], nonBranchSurfaces...)
			return nil
		}); err != nil {
			return PreToolUseOutput{}, err
		}
		if denyReason != "" {
			return PreToolUseOutput{
				Decision: DecisionDeny,
				Reason:   denyReason,
			}, nil
		}
	}

	return PreToolUseOutput{Decision: DecisionAllow}, nil
}

func isRDDLensRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "review-risk", "review-resilience", "review-readability", "review-reliability":
		return true
	default:
		return false
	}
}

func checkNativeReviewBatch(workspace string, entries []subagentEntry) (string, error) {
	hasLensRole := false
	for _, entry := range entries {
		if isRDDLensRole(entry.effectiveRole()) {
			hasLensRole = true
			break
		}
	}
	if !hasLensRole {
		return "", nil
	}

	// Never block a single-lens fallback_to_native call.
	if len(entries) == 1 && strings.Contains(strings.ToLower(entries[0].Prompt), "fallback_to_native") {
		return "", nil
	}

	// Determine target lineageId from entry prompts or active collecting_lenses lineage.
	foundIDs := make([]string, 0, 1)
	seenIDs := make(map[string]bool)
	for _, entry := range entries {
		for _, id := range reReviewLineageID.FindAllString(entry.Prompt, -1) {
			if !seenIDs[id] {
				seenIDs[id] = true
				foundIDs = append(foundIDs, id)
			}
		}
	}
	if len(foundIDs) > 1 {
		// Ambiguous batch targeting multiple distinct lineages; skip batch guard.
		return "", nil
	}

	var st rdd.LineageStatus
	if len(foundIDs) == 1 {
		loaded, err := rdd.NewEngine(workspace).Status(foundIDs[0])
		if err != nil {
			return "", nil
		}
		st = loaded
	} else {
		active, ok := findActiveCollectingLineage(workspace)
		if !ok {
			return "", nil
		}
		st = active
	}

	if st.Status != rdd.StatusCollectingLenses {
		return "", nil
	}

	var pendingLenses []string
	if len(st.PendingLenses) > 0 {
		pendingLenses = append(pendingLenses, st.PendingLenses...)
	} else {
		completed := make(map[string]bool, len(st.CompletedLenses))
		for _, c := range st.CompletedLenses {
			completed[c] = true
		}
		for _, l := range st.Lenses {
			if !completed[l] {
				pendingLenses = append(pendingLenses, l)
			}
		}
	}

	// Filter to pending lenses routed to agy-native (if profiles.json missing, treat all as native).
	var pf config.ProfileConfig
	hasPF := false
	if profs, err := config.LoadProfiles(workspace); err == nil {
		pf, hasPF = profs.Profiles[profs.ActiveProfile]
	}

	pendingNativeRoles := make([]string, 0, len(pendingLenses))
	for _, lens := range pendingLenses {
		role := "review-" + lens
		isNative := true
		if hasPF {
			if ra, ok := pf.Roles[role]; ok && ra.Runtime == config.RuntimeExternalMCP {
				isNative = false
			}
		}
		if isNative {
			pendingNativeRoles = append(pendingNativeRoles, role)
		}
	}

	if len(pendingNativeRoles) < 2 {
		return "", nil
	}

	suppliedCounts := make(map[string]int, len(entries))
	var dupRoles []string
	for _, entry := range entries {
		role := entry.effectiveRole()
		if !isRDDLensRole(role) {
			continue
		}
		suppliedCounts[role]++
		if suppliedCounts[role] == 2 {
			dupRoles = append(dupRoles, role)
		}
	}

	var missingRoles []string
	for _, reqRole := range pendingNativeRoles {
		if suppliedCounts[reqRole] == 0 {
			missingRoles = append(missingRoles, reqRole)
		}
	}

	if len(missingRoles) == 0 && len(dupRoles) == 0 {
		return "", nil
	}

	var details []string
	if len(missingRoles) > 0 {
		details = append(details, fmt.Sprintf("missing: [%s]", strings.Join(missingRoles, ", ")))
	}
	if len(dupRoles) > 0 {
		details = append(details, fmt.Sprintf("duplicate: [%s]", strings.Join(dupRoles, ", ")))
	}
	return fmt.Sprintf(
		"RDD lineage %q has %d pending native review lenses ([%s]; %s). Launch ALL pending lenses together in a single parallel invoke_subagent call instead of one by one.",
		st.LineageID,
		len(pendingNativeRoles),
		strings.Join(pendingNativeRoles, ", "),
		strings.Join(details, "; "),
	), nil
}

func findActiveCollectingLineage(workspace string) (rdd.LineageStatus, bool) {
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = workspace
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return rdd.LineageStatus{}, false
	}
	gitDir := strings.TrimSpace(stdout.String())
	if gitDir == "" {
		return rdd.LineageStatus{}, false
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(workspace, gitDir)
	}
	lineagesDir := filepath.Join(filepath.Clean(gitDir), "odd-agy", "rdd", "lineages")
	dirEntries, err := os.ReadDir(lineagesDir)
	if err != nil {
		return rdd.LineageStatus{}, false
	}

	eng := rdd.NewEngine(workspace)
	var active []rdd.LineageStatus
	for _, de := range dirEntries {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		if !strings.HasPrefix(name, "review-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		lineageID := strings.TrimSuffix(name, ".json")
		st, err := eng.Status(lineageID)
		if err != nil {
			continue
		}
		if st.Status == rdd.StatusCollectingLenses {
			active = append(active, st)
			if len(active) > 1 {
				return rdd.LineageStatus{}, false
			}
		}
	}
	if len(active) == 1 {
		return active[0], true
	}
	return rdd.LineageStatus{}, false
}

func validateVerifierPrompt(workspace, prompt string) error {
	if riskLinePattern.MatchString(prompt) {
		return nil
	}
	hasSpec, err := validateOptionalSpec(workspace, prompt)
	if err != nil {
		return err
	}
	if hasSpec {
		return nil
	}
	return fmt.Errorf("gentle-ai-verify prompt must include 'Risk: item [1-6]' or 'Spec: odd/tasks/<existing>.md, T<n>'")
}

func validateOptionalSpec(workspace, prompt string) (bool, error) {
	m := specLinePattern.FindStringSubmatch(prompt)
	if len(m) < 2 {
		return false, nil
	}
	specRel := filepath.ToSlash(strings.TrimSpace(m[1]))
	if !strings.HasPrefix(specRel, "odd/tasks/") || strings.Contains(specRel, "..") {
		return false, fmt.Errorf("invalid Spec path %q", specRel)
	}
	realRoot, err := evalSymlinksBestEffort(workspace)
	if err != nil {
		return false, fmt.Errorf("resolve workspace root: %w", err)
	}
	realTarget, err := evalSymlinksBestEffort(filepath.Join(realRoot, filepath.FromSlash(specRel)))
	if err != nil {
		return false, fmt.Errorf("resolve Spec file %q: %w", specRel, err)
	}
	if !isWithinDir(realTarget, filepath.Join(realRoot, "odd", "tasks")) {
		return false, fmt.Errorf("Spec path %q escapes odd/tasks/", specRel)
	}
	info, err := os.Stat(realTarget)
	if err != nil || !info.Mode().IsRegular() {
		return false, fmt.Errorf("Spec file %q does not exist", specRel)
	}
	return true, nil
}

func extractAllowedEditSurfaces(prompt string) ([]string, error) {
	lines := strings.Split(strings.ReplaceAll(prompt, "\r\n", "\n"), "\n")
	inSection := false
	var surfaces []string

	for _, rawLine := range lines {
		trimmed := strings.TrimSpace(rawLine)
		if !inSection {
			if strings.EqualFold(trimmed, "## Allowed edit surfaces") {
				inSection = true
			}
			continue
		}

		if strings.HasPrefix(trimmed, "#") {
			break
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "<!--") {
			continue
		}

		candidate := stripListPrefix(trimmed)
		candidate = strings.Trim(candidate, "`\"'")
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		normalized, err := validateSurfacePattern(candidate)
		if err != nil {
			return nil, err
		}
		surfaces = append(surfaces, normalized)
	}

	if !inSection {
		return nil, fmt.Errorf("writer prompt must include a '## Allowed edit surfaces' heading")
	}
	if len(surfaces) == 0 {
		return nil, fmt.Errorf("'## Allowed edit surfaces' must list at least 1 narrow repo-relative path or glob")
	}
	return surfaces, nil
}

func stripListPrefix(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "* ") {
		return strings.TrimSpace(s[2:])
	}
	// Handle numbered list items like "1. path".
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			continue
		}
		if i > 0 && s[i] == '.' && i+1 < len(s) && s[i+1] == ' ' {
			return strings.TrimSpace(s[i+2:])
		}
		break
	}
	return s
}

func validateSurfacePattern(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("empty edit surface entry")
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "\\") || strings.HasPrefix(s, "~") {
		return "", fmt.Errorf("edit surface %q must be repository-relative, not absolute or home-relative", raw)
	}
	if len(s) >= 2 && s[1] == ':' && ((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z')) {
		return "", fmt.Errorf("edit surface %q must be repository-relative", raw)
	}

	s = strings.ReplaceAll(s, "\\", "/")
	for strings.HasPrefix(s, "./") {
		s = strings.TrimPrefix(s, "./")
	}
	if s == "" || s == "." || s == ".." || s == "*" || s == "**" {
		return "", fmt.Errorf("edit surface %q is too broad or invalid", raw)
	}

	if strings.HasSuffix(s, "/") {
		s += "**"
	}

	segments := strings.Split(s, "/")
	for _, seg := range segments {
		if seg == ".." || seg == "." || seg == "" {
			return "", fmt.Errorf("edit surface %q contains invalid path segment %q", raw, seg)
		}
	}
	if segments[0] == "*" || segments[0] == "**" {
		return "", fmt.Errorf("edit surface %q is too broad; scope it under a repository directory", raw)
	}
	return strings.Join(segments, "/"), nil
}

func findSurfaceOverlap(aList, bList []string) (string, string, bool) {
	for _, a := range aList {
		for _, b := range bList {
			if surfacesOverlap(a, b) {
				return a, b, true
			}
		}
	}
	return "", "", false
}

func surfacesOverlap(a, b string) bool {
	segA := strings.Split(a, "/")
	segB := strings.Split(b, "/")
	return segmentsOverlap(segA, segB)
}

func segmentsOverlap(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) == 0 || len(b) == 0 {
		// Treat directory prefix (no glob extension) as overlapping deeper paths under it,
		// and trailing ** as matching zero segments.
		if len(a) == 1 && a[0] == "**" {
			return true
		}
		if len(b) == 1 && b[0] == "**" {
			return true
		}
		return false
	}

	if a[0] == "**" {
		if segmentsOverlap(a[1:], b) {
			return true
		}
		return segmentsOverlap(a, b[1:])
	}
	if b[0] == "**" {
		if segmentsOverlap(a, b[1:]) {
			return true
		}
		return segmentsOverlap(a[1:], b)
	}

	if !singleSegmentOverlaps(a[0], b[0]) {
		return false
	}

	// If one pattern ends here and has no extension/glob while the other continues deeper,
	// treat directory path "pkg/foo" as overlapping "pkg/foo/bar.go".
	if len(a) == 1 && len(b) > 1 && !strings.ContainsAny(a[0], "*?.") {
		return true
	}
	if len(b) == 1 && len(a) > 1 && !strings.ContainsAny(b[0], "*?.") {
		return true
	}

	return segmentsOverlap(a[1:], b[1:])
}

func singleSegmentOverlaps(p1, p2 string) bool {
	g1 := strings.ContainsAny(p1, "*?")
	g2 := strings.ContainsAny(p2, "*?")
	if !g1 && !g2 {
		return p1 == p2
	}
	if !g1 {
		ok, err := path.Match(p2, p1)
		return err == nil && ok
	}
	if !g2 {
		ok, err := path.Match(p1, p2)
		return err == nil && ok
	}

	// Both contain wildcards: check literal prefix and suffix compatibility.
	pre1, suf1 := literalBounds(p1)
	pre2, suf2 := literalBounds(p2)
	minPre := len(pre1)
	if len(pre2) < minPre {
		minPre = len(pre2)
	}
	if pre1[:minPre] != pre2[:minPre] {
		return false
	}
	minSuf := len(suf1)
	if len(suf2) < minSuf {
		minSuf = len(suf2)
	}
	if suf1[len(suf1)-minSuf:] != suf2[len(suf2)-minSuf:] {
		return false
	}
	return true
}

func literalBounds(pat string) (prefix, suffix string) {
	i := strings.IndexAny(pat, "*?")
	if i < 0 {
		return pat, pat
	}
	j := strings.LastIndexAny(pat, "*?")
	return pat[:i], pat[j+1:]
}
