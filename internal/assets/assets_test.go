package assets_test

import (
	"bytes"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Nathanael-Huaman/odd-agy/internal/assets"
)

var expectedRules = []string{
	"rules/odd-01-core.md",
	"rules/odd-02-orchestrator.md",
	"rules/odd-03-tracking.md",
	"rules/odd-04-verification.md",
	"rules/odd-05-rdd.md",
}

var expectedAgents = []string{
	"agents/odd-orchestrator.md",
	"agents/gentle-ai-explore.md",
	"agents/gentle-ai-worker.md",
	"agents/gentle-ai-verify.md",
	"agents/jd-judge-a.md",
	"agents/jd-judge-b.md",
	"agents/jd-fix-agent.md",
	"agents/review-risk.md",
	"agents/review-resilience.md",
	"agents/review-readability.md",
	"agents/review-reliability.md",
	"agents/review-refuter.md",
}

var expectedSkills = []string{
	"skills/odd-status/SKILL.md",
	"skills/odd-rdd/SKILL.md",
	"skills/odd-profile/SKILL.md",
	"skills/odd-doctor/SKILL.md",
	"skills/work-unit-commits/SKILL.md",
	"skills/judgment-day/SKILL.md",
	"skills/branch-pr/SKILL.md",
	"skills/chained-pr/SKILL.md",
	"skills/issue-creation/SKILL.md",
	"skills/comment-writer/SKILL.md",
	"skills/cognitive-doc-design/SKILL.md",
	"skills/gentle-ui-design/SKILL.md",
	"skills/skill-creator/SKILL.md",
	"skills/skill-improver/SKILL.md",
	"skills/rdd-defect-workflow/SKILL.md",
}

var expectedAllowedTools = []string{
	"view_file",
	"write_to_file",
	"replace_file_content",
	"multi_replace_file_content",
	"list_dir",
	"find_by_name",
	"grep_search",
	"run_command",
	"invoke_subagent",
	"send_message",
	"manage_subagents",
	"ask_question",
	"manage_task",
}

var forbiddenPiTokens = []string{
	"subagent_run",
	"subagent_continue",
	"gentle_review_run",
	"gentle_review_capture",
	"gentle_review",
	"gentle_design",
	"ask_user_choice",
	"ask_user_question",
	".atl/skill-registry.md",
}

func TestAllowedAntigravityToolsMatchesSpec(t *testing.T) {
	if len(assets.AllowedAntigravityTools) != len(expectedAllowedTools) {
		t.Fatalf("AllowedAntigravityTools len = %d, want %d: %v", len(assets.AllowedAntigravityTools), len(expectedAllowedTools), assets.AllowedAntigravityTools)
	}
	for _, want := range expectedAllowedTools {
		if !slices.Contains(assets.AllowedAntigravityTools, want) {
			t.Errorf("AllowedAntigravityTools missing %q", want)
		}
	}
}

func TestValidateAllEmbeddedAssets(t *testing.T) {
	if err := assets.ValidateAll(); err != nil {
		t.Fatalf("ValidateAll() unexpected error: %v", err)
	}
}

func TestAssetCountsAndPaths(t *testing.T) {
	rules, err := assets.Rules()
	if err != nil {
		t.Fatalf("Rules() error: %v", err)
	}
	if got := relPaths(rules); !slices.Equal(got, sortedCopy(expectedRules)) {
		t.Fatalf("Rules() paths = %v, want %v", got, sortedCopy(expectedRules))
	}
	for _, r := range rules {
		if r.Category != assets.CategoryRule {
			t.Errorf("rule %s Category = %q, want %q", r.RelPath, r.Category, assets.CategoryRule)
		}
	}

	agents, err := assets.Agents()
	if err != nil {
		t.Fatalf("Agents() error: %v", err)
	}
	if got := relPaths(agents); !slices.Equal(got, sortedCopy(expectedAgents)) {
		t.Fatalf("Agents() paths = %v, want %v", got, sortedCopy(expectedAgents))
	}
	for _, a := range agents {
		if a.Category != assets.CategoryAgent {
			t.Errorf("agent %s Category = %q, want %q", a.RelPath, a.Category, assets.CategoryAgent)
		}
	}

	skills, err := assets.Skills()
	if err != nil {
		t.Fatalf("Skills() error: %v", err)
	}
	if got := relPaths(skills); !slices.Equal(got, sortedCopy(expectedSkills)) {
		t.Fatalf("Skills() paths = %v, want %v", got, sortedCopy(expectedSkills))
	}
	for _, s := range skills {
		if s.Category != assets.CategorySkill {
			t.Errorf("skill %s Category = %q, want %q", s.RelPath, s.Category, assets.CategorySkill)
		}
	}

	all, err := assets.All()
	if err != nil {
		t.Fatalf("All() error: %v", err)
	}
	wantTotal := len(expectedRules) + len(expectedAgents) + len(expectedSkills)
	if len(all) != wantTotal {
		t.Fatalf("All() len = %d, want %d", len(all), wantTotal)
	}
}

func TestRulesContractAndBudget(t *testing.T) {
	rules, err := assets.Rules()
	if err != nil {
		t.Fatalf("Rules() error: %v", err)
	}

	var alwaysOnBytes int
	byPath := map[string]assets.AssetFile{}
	for _, r := range rules {
		byPath[r.RelPath] = r
		if len(r.Content) >= 24000 {
			t.Errorf("rule %s size = %d bytes, must be strictly < 24000", r.RelPath, len(r.Content))
		}
		fm, _, err := assets.ParseFrontmatter(r.Content)
		if err != nil {
			t.Fatalf("rule %s frontmatter parse error: %v", r.RelPath, err)
		}
		if fm.Trigger == "always_on" {
			alwaysOnBytes += len(r.Content)
		}
		if fm.Trigger == "model_decision" && strings.TrimSpace(fm.Description) == "" {
			t.Errorf("model_decision rule %s missing description", r.RelPath)
		}
	}

	if alwaysOnBytes >= 24000 {
		t.Errorf("combined always_on rules size = %d bytes, must be < 24000", alwaysOnBytes)
	}

	// Verify key semantic content in always_on rules
	core := string(byPath["rules/odd-01-core.md"].Content)
	for _, needle := range []string{"Authorize", "Explore", "Resolve", "Classify", "Track", "Implement", "Close", "odd/tasks/", "High-Risk"} {
		if !strings.Contains(core, needle) {
			t.Errorf("odd-01-core.md missing %q", needle)
		}
	}

	orch := string(byPath["rules/odd-02-orchestrator.md"].Content)
	for _, needle := range []string{"full-delegation", "organic", "gentle-ai-worker", "gentle-ai-explore", "gentle-ai-verify", "Allowed edit surfaces", "odd_external_run", "odd_external_continue", "profiles.json"} {
		if !strings.Contains(orch, needle) {
			t.Errorf("odd-02-orchestrator.md missing %q", needle)
		}
	}
}

func TestAgentsContractAndToolAllowlist(t *testing.T) {
	agents, err := assets.Agents()
	if err != nil {
		t.Fatalf("Agents() error: %v", err)
	}

	byPath := map[string]assets.AssetFile{}
	for _, a := range agents {
		byPath[a.RelPath] = a
		fm, _, err := assets.ParseFrontmatter(a.Content)
		if err != nil {
			t.Fatalf("agent %s frontmatter parse error: %v", a.RelPath, err)
		}
		if strings.TrimSpace(fm.Name) == "" {
			t.Errorf("agent %s missing name", a.RelPath)
		}
		if strings.TrimSpace(fm.Description) == "" {
			t.Errorf("agent %s missing description", a.RelPath)
		}
		if len(fm.Tools) == 0 {
			t.Errorf("agent %s has empty tools list", a.RelPath)
		}
		for _, tool := range fm.Tools {
			if !slices.Contains(assets.AllowedAntigravityTools, tool) {
				t.Errorf("agent %s uses disallowed tool %q", a.RelPath, tool)
			}
		}
	}

	orchFM, _, _ := assets.ParseFrontmatter(byPath["agents/odd-orchestrator.md"].Content)
	if orchFM.MainAgent == nil || !*orchFM.MainAgent {
		t.Errorf("odd-orchestrator must set mainAgent: true")
	}
	if orchFM.Subagent == nil || *orchFM.Subagent {
		t.Errorf("odd-orchestrator must set subagent: false")
	}
	if slices.Contains(orchFM.Tools, "replace_file_content") || slices.Contains(orchFM.Tools, "multi_replace_file_content") {
		t.Errorf("odd-orchestrator must omit replace_file_content and multi_replace_file_content")
	}

	for _, writerPath := range []string{"agents/gentle-ai-worker.md", "agents/jd-fix-agent.md"} {
		fm, _, _ := assets.ParseFrontmatter(byPath[writerPath].Content)
		if fm.InheritCustomizations == nil || *fm.InheritCustomizations {
			t.Errorf("%s must set inheritCustomizations: false", writerPath)
		}
	}

	readOnlyAgents := []string{
		"agents/gentle-ai-explore.md",
		"agents/gentle-ai-verify.md",
		"agents/jd-judge-a.md",
		"agents/jd-judge-b.md",
		"agents/review-risk.md",
		"agents/review-resilience.md",
		"agents/review-readability.md",
		"agents/review-reliability.md",
		"agents/review-refuter.md",
	}
	for _, roPath := range readOnlyAgents {
		fm, _, _ := assets.ParseFrontmatter(byPath[roPath].Content)
		for _, writeTool := range []string{"write_to_file", "replace_file_content", "multi_replace_file_content"} {
			if slices.Contains(fm.Tools, writeTool) {
				t.Errorf("read-only agent %s must not include %q", roPath, writeTool)
			}
		}
	}

	mcpAgents := []string{
		"agents/odd-orchestrator.md",
		"agents/gentle-ai-worker.md",
		"agents/review-risk.md",
		"agents/review-resilience.md",
		"agents/review-readability.md",
		"agents/review-reliability.md",
		"agents/review-refuter.md",
	}
	for _, mcpPath := range mcpAgents {
		fm, _, _ := assets.ParseFrontmatter(byPath[mcpPath].Content)
		if fm.InheritMcp == nil || !*fm.InheritMcp {
			t.Errorf("%s must declare inheritMcp: true", mcpPath)
		}
		if slices.Contains(fm.Tools, "call_mcp_tool") {
			t.Errorf("%s must not include call_mcp_tool in tools", mcpPath)
		}
	}
}

func TestZeroPiOnlyTokensAcrossAllAssets(t *testing.T) {
	all, err := assets.All()
	if err != nil {
		t.Fatalf("All() error: %v", err)
	}
	for _, af := range all {
		text := string(af.Content)
		for _, tok := range forbiddenPiTokens {
			if strings.Contains(text, tok) {
				t.Errorf("asset %s contains forbidden Pi-only token %q", af.RelPath, tok)
			}
		}
	}
}

func TestReviewAgentsAndSkillSchemaAlignment(t *testing.T) {
	agents, err := assets.Agents()
	if err != nil {
		t.Fatalf("Agents() error: %v", err)
	}
	skills, err := assets.Skills()
	if err != nil {
		t.Fatalf("Skills() error: %v", err)
	}

	var targets []assets.AssetFile
	for _, a := range agents {
		if strings.HasPrefix(a.RelPath, "agents/review-") {
			targets = append(targets, a)
		}
	}
	for _, s := range skills {
		if s.RelPath == "skills/rdd-defect-workflow/SKILL.md" {
			targets = append(targets, s)
		}
	}
	if len(targets) != 6 {
		t.Fatalf("expected 5 review-* agents + 1 rdd-defect-workflow skill, got %d", len(targets))
	}

	for _, af := range targets {
		text := string(af.Content)
		if !strings.Contains(text, "SEVERE") {
			t.Errorf("%s must reference uppercase SEVERE", af.RelPath)
		}
		if !strings.Contains(text, "causality") {
			t.Errorf("%s must reference causality field", af.RelPath)
		}
		if !strings.Contains(text, "pre_existing") {
			t.Errorf("%s must reference pre_existing causality value", af.RelPath)
		}
		if strings.Contains(text, "pre-existing") {
			t.Errorf("%s must not use hyphenated pre-existing", af.RelPath)
		}
		if strings.Contains(strings.ToLower(text), "origin") {
			t.Errorf("%s must use causality instead of origin", af.RelPath)
		}
		if strings.Contains(text, "severe") {
			t.Errorf("%s must not contain lowercase severe schema instructions", af.RelPath)
		}
	}
}

func TestValidateFSNegativeCases(t *testing.T) {
	baseFS := cloneEmbeddedToMapFS(t)

	t.Run("rejects agent declaring call_mcp_tool in tools", func(t *testing.T) {
		m := cloneMapFS(baseFS)
		m["agents/gentle-ai-explore.md"] = &fstest.MapFile{
			Data: []byte("---\nname: gentle-ai-explore\ndescription: Explore.\nsubagent: true\ntools: [view_file, call_mcp_tool]\n---\n\nExplore.\n"),
		}
		if err := assets.ValidateFS(m); err == nil || !strings.Contains(err.Error(), "call_mcp_tool") {
			t.Fatalf("ValidateFS() err = %v, want error mentioning call_mcp_tool", err)
		}
	})

	t.Run("rejects unknown tool name", func(t *testing.T) {
		m := cloneMapFS(baseFS)
		m["agents/gentle-ai-explore.md"] = &fstest.MapFile{
			Data: []byte("---\nname: gentle-ai-explore\ndescription: Explore.\nsubagent: true\ntools: [view_file, nonexistent_tool]\n---\n\nExplore.\n"),
		}
		if err := assets.ValidateFS(m); err == nil || !strings.Contains(err.Error(), "nonexistent_tool") {
			t.Fatalf("ValidateFS() err = %v, want error mentioning nonexistent_tool", err)
		}
	})

	t.Run("rejects oversized rule >= 24000 bytes", func(t *testing.T) {
		m := cloneMapFS(baseFS)
		bigBody := strings.Repeat("a", 24000)
		m["rules/odd-03-tracking.md"] = &fstest.MapFile{
			Data: []byte("---\ntrigger: model_decision\ndescription: Tracking.\n---\n\n" + bigBody),
		}
		if err := assets.ValidateFS(m); err == nil || !strings.Contains(err.Error(), "24000") {
			t.Fatalf("ValidateFS() err = %v, want size limit error", err)
		}
	})

	t.Run("rejects missing frontmatter", func(t *testing.T) {
		m := cloneMapFS(baseFS)
		m["skills/odd-status/SKILL.md"] = &fstest.MapFile{
			Data: []byte("# odd-status\n\nNo frontmatter here.\n"),
		}
		if err := assets.ValidateFS(m); err == nil || !strings.Contains(err.Error(), "frontmatter") {
			t.Fatalf("ValidateFS() err = %v, want frontmatter error", err)
		}
	})

	t.Run("rejects Pi-only token leak", func(t *testing.T) {
		m := cloneMapFS(baseFS)
		orig := m["skills/odd-status/SKILL.md"].Data
		m["skills/odd-status/SKILL.md"] = &fstest.MapFile{
			Data: append(append([]byte(nil), orig...), []byte("\nUse subagent_run here.\n")...),
		}
		if err := assets.ValidateFS(m); err == nil || !strings.Contains(err.Error(), "subagent_run") {
			t.Fatalf("ValidateFS() err = %v, want Pi token error", err)
		}
	})

	t.Run("rejects skill folder name mismatch", func(t *testing.T) {
		m := cloneMapFS(baseFS)
		m["skills/odd-status/SKILL.md"] = &fstest.MapFile{
			Data: []byte("---\nname: wrong-name\ndescription: Status.\n---\n\nBody.\n"),
		}
		if err := assets.ValidateFS(m); err == nil || !strings.Contains(err.Error(), "wrong-name") {
			t.Fatalf("ValidateFS() err = %v, want name mismatch error", err)
		}
	})

	t.Run("rejects orchestrator with replace_file_content", func(t *testing.T) {
		m := cloneMapFS(baseFS)
		data := bytes.ReplaceAll(m["agents/odd-orchestrator.md"].Data, []byte("view_file"), []byte("view_file, replace_file_content"))
		m["agents/odd-orchestrator.md"] = &fstest.MapFile{Data: data}
		if err := assets.ValidateFS(m); err == nil || !strings.Contains(err.Error(), "replace_file_content") {
			t.Fatalf("ValidateFS() err = %v, want orchestrator edit tool error", err)
		}
	})

	t.Run("rejects worker missing inheritCustomizations false", func(t *testing.T) {
		m := cloneMapFS(baseFS)
		data := bytes.ReplaceAll(m["agents/gentle-ai-worker.md"].Data, []byte("inheritCustomizations: false\n"), []byte(""))
		m["agents/gentle-ai-worker.md"] = &fstest.MapFile{Data: data}
		if err := assets.ValidateFS(m); err == nil || !strings.Contains(err.Error(), "inheritCustomizations") {
			t.Fatalf("ValidateFS() err = %v, want inheritCustomizations error", err)
		}
	})

	t.Run("rejects read-only agent with write_to_file", func(t *testing.T) {
		m := cloneMapFS(baseFS)
		data := bytes.ReplaceAll(m["agents/review-risk.md"].Data, []byte("view_file"), []byte("view_file, write_to_file"))
		m["agents/review-risk.md"] = &fstest.MapFile{Data: data}
		if err := assets.ValidateFS(m); err == nil || !strings.Contains(err.Error(), "write_to_file") {
			t.Fatalf("ValidateFS() err = %v, want read-only write_to_file error", err)
		}
	})

	t.Run("rejects model_decision rule missing description", func(t *testing.T) {
		m := cloneMapFS(baseFS)
		m["rules/odd-05-rdd.md"] = &fstest.MapFile{
			Data: []byte("---\ntrigger: model_decision\n---\n\nRDD body.\n"),
		}
		if err := assets.ValidateFS(m); err == nil || !strings.Contains(err.Error(), "description") {
			t.Fatalf("ValidateFS() err = %v, want missing description error", err)
		}
	})
}

func relPaths(items []assets.AssetFile) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.RelPath
	}
	return out
}

func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

func cloneEmbeddedToMapFS(t *testing.T) fstest.MapFS {
	t.Helper()
	all, err := assets.All()
	if err != nil {
		t.Fatalf("All() failed while building test fixture: %v", err)
	}
	m := fstest.MapFS{}
	for _, af := range all {
		m[af.RelPath] = &fstest.MapFile{
			Data: slices.Clone(af.Content),
			Mode: fs.FileMode(0o644),
		}
	}
	return m
}

func cloneMapFS(src fstest.MapFS) fstest.MapFS {
	dst := make(fstest.MapFS, len(src))
	for k, v := range src {
		dst[k] = &fstest.MapFile{
			Data: slices.Clone(v.Data),
			Mode: v.Mode,
		}
	}
	return dst
}
