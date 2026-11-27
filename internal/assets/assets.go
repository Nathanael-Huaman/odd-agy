package assets

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strings"
)

//go:embed all:files
var embeddedFS embed.FS

const (
	CategoryRule  = "rules"
	CategoryAgent = "agents"
	CategorySkill = "skills"

	MaxRuleBytes            = 24000
	MaxCombinedAlwaysOnByte = 24000
)

// AllowedAntigravityTools is the verified Antigravity CLI tool allowlist.
// Any unknown or misspelled tool name causes Antigravity subagents to hang.
var AllowedAntigravityTools = []string{
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
	"call_mcp_tool",
}

var requiredRules = []string{
	"rules/odd-01-core.md",
	"rules/odd-02-orchestrator.md",
	"rules/odd-03-tracking.md",
	"rules/odd-04-verification.md",
	"rules/odd-05-rdd.md",
}

var requiredAgents = []string{
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

var requiredSkills = []string{
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

// AssetFile represents one embedded .agents/ workspace asset.
type AssetFile struct {
	RelPath  string
	Content  []byte
	Category string
}

// Frontmatter represents parsed YAML frontmatter fields across rules, agents, and skills.
type Frontmatter struct {
	Trigger               string
	Name                  string
	Description           string
	Model                 string
	MainAgent             *bool
	Subagent              *bool
	InheritCustomizations *bool
	CommandExecution      string
	Tools                 []string
	Extra                 map[string]string
}

// IsAllowedAntigravityTool reports whether tool is in AllowedAntigravityTools.
func IsAllowedAntigravityTool(tool string) bool {
	return slices.Contains(AllowedAntigravityTools, tool)
}

func rootFS() (fs.FS, error) {
	return fs.Sub(embeddedFS, "files")
}

// All returns all embedded rules, agents, and skills sorted by RelPath.
func All() ([]AssetFile, error) {
	sub, err := rootFS()
	if err != nil {
		return nil, fmt.Errorf("open embedded assets root: %w", err)
	}
	return readAllFromFS(sub)
}

// Rules returns all embedded rule files sorted by RelPath.
func Rules() ([]AssetFile, error) {
	return filterByCategory(CategoryRule)
}

// Agents returns all embedded agent files sorted by RelPath.
func Agents() ([]AssetFile, error) {
	return filterByCategory(CategoryAgent)
}

// Skills returns all embedded skill files sorted by RelPath.
func Skills() ([]AssetFile, error) {
	return filterByCategory(CategorySkill)
}

func filterByCategory(category string) ([]AssetFile, error) {
	all, err := All()
	if err != nil {
		return nil, err
	}
	var out []AssetFile
	for _, af := range all {
		if af.Category == category {
			out = append(out, af)
		}
	}
	return out, nil
}

func readAllFromFS(fsys fs.FS) ([]AssetFile, error) {
	var out []AssetFile
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		cat, err := classifyRelPath(p)
		if err != nil {
			return err
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		out = append(out, AssetFile{
			RelPath:  p,
			Content:  data,
			Category: cat,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].RelPath < out[j].RelPath
	})
	return out, nil
}

func classifyRelPath(p string) (string, error) {
	clean := path.Clean(p)
	parts := strings.Split(clean, "/")
	if len(parts) < 2 {
		return "", fmt.Errorf("unexpected asset path %q", p)
	}
	switch parts[0] {
	case CategoryRule:
		if len(parts) != 2 || !strings.HasSuffix(parts[1], ".md") {
			return "", fmt.Errorf("invalid rule path %q: must be rules/<name>.md", p)
		}
		return CategoryRule, nil
	case CategoryAgent:
		if len(parts) != 2 || !strings.HasSuffix(parts[1], ".md") {
			return "", fmt.Errorf("invalid agent path %q: must be agents/<name>.md", p)
		}
		return CategoryAgent, nil
	case CategorySkill:
		if len(parts) != 3 || parts[2] != "SKILL.md" {
			return "", fmt.Errorf("invalid skill path %q: must be 1-level depth skills/<name>/SKILL.md", p)
		}
		return CategorySkill, nil
	default:
		return "", fmt.Errorf("unknown asset category %q in %q", parts[0], p)
	}
}

// ValidateAll validates every embedded rule, agent, and skill against the odd-agy contract.
func ValidateAll() error {
	sub, err := rootFS()
	if err != nil {
		return fmt.Errorf("open embedded assets root: %w", err)
	}
	return ValidateFS(sub)
}

// ValidateFS validates all rules, agents, and skills contained in fsys.
func ValidateFS(fsys fs.FS) error {
	items, err := readAllFromFS(fsys)
	if err != nil {
		return err
	}

	seen := make(map[string]AssetFile, len(items))
	for _, it := range items {
		seen[it.RelPath] = it
	}

	for _, req := range requiredRules {
		if _, ok := seen[req]; !ok {
			return fmt.Errorf("missing required rule %q", req)
		}
	}
	for _, req := range requiredAgents {
		if _, ok := seen[req]; !ok {
			return fmt.Errorf("missing required agent %q", req)
		}
	}
	for _, req := range requiredSkills {
		if _, ok := seen[req]; !ok {
			return fmt.Errorf("missing required skill %q", req)
		}
	}

	var alwaysOnBytes int
	for _, it := range items {
		text := string(it.Content)
		for _, tok := range forbiddenPiTokens {
			if strings.Contains(text, tok) {
				return fmt.Errorf("asset %s contains forbidden Pi-only token %q", it.RelPath, tok)
			}
		}

		fm, body, err := ParseFrontmatter(it.Content)
		if err != nil {
			return fmt.Errorf("asset %s: %w", it.RelPath, err)
		}
		if strings.TrimSpace(body) == "" {
			return fmt.Errorf("asset %s has empty markdown body", it.RelPath)
		}

		switch it.Category {
		case CategoryRule:
			n, err := validateRule(it, fm)
			if err != nil {
				return err
			}
			alwaysOnBytes += n
		case CategoryAgent:
			if err := validateAgent(it, fm); err != nil {
				return err
			}
		case CategorySkill:
			if err := validateSkill(it, fm); err != nil {
				return err
			}
		}
	}

	if alwaysOnBytes >= MaxCombinedAlwaysOnByte {
		return fmt.Errorf("combined always_on rules size %d bytes must be < %d", alwaysOnBytes, MaxCombinedAlwaysOnByte)
	}

	return nil
}

func validateRule(it AssetFile, fm Frontmatter) (int, error) {
	if len(it.Content) >= MaxRuleBytes {
		return 0, fmt.Errorf("rule %s size %d bytes must be strictly < %d", it.RelPath, len(it.Content), MaxRuleBytes)
	}
	switch fm.Trigger {
	case "always_on":
		return len(it.Content), nil
	case "model_decision":
		if strings.TrimSpace(fm.Description) == "" {
			return 0, fmt.Errorf("rule %s with trigger model_decision must define non-empty description", it.RelPath)
		}
		return 0, nil
	case "glob", "manual":
		return 0, nil
	default:
		return 0, fmt.Errorf("rule %s invalid trigger %q (must be always_on, model_decision, glob, or manual)", it.RelPath, fm.Trigger)
	}
}

func validateAgent(it AssetFile, fm Frontmatter) error {
	expectedName := strings.TrimSuffix(path.Base(it.RelPath), ".md")
	if strings.TrimSpace(fm.Name) == "" {
		return fmt.Errorf("agent %s missing name", it.RelPath)
	}
	if fm.Name != expectedName {
		return fmt.Errorf("agent %s frontmatter name %q does not match file name %q", it.RelPath, fm.Name, expectedName)
	}
	if strings.TrimSpace(fm.Description) == "" {
		return fmt.Errorf("agent %s missing description", it.RelPath)
	}
	if len(fm.Tools) == 0 {
		return fmt.Errorf("agent %s must declare at least one tool", it.RelPath)
	}
	for _, tool := range fm.Tools {
		if !IsAllowedAntigravityTool(tool) {
			return fmt.Errorf("agent %s uses unknown/disallowed Antigravity tool %q", it.RelPath, tool)
		}
	}

	switch expectedName {
	case "odd-orchestrator":
		if fm.MainAgent == nil || !*fm.MainAgent {
			return fmt.Errorf("agent %s must set mainAgent: true", it.RelPath)
		}
		if fm.Subagent == nil || *fm.Subagent {
			return fmt.Errorf("agent %s must set subagent: false", it.RelPath)
		}
		if slices.Contains(fm.Tools, "replace_file_content") || slices.Contains(fm.Tools, "multi_replace_file_content") {
			return fmt.Errorf("agent %s must omit replace_file_content and multi_replace_file_content", it.RelPath)
		}
	case "gentle-ai-worker", "jd-fix-agent":
		if fm.Subagent == nil || !*fm.Subagent {
			return fmt.Errorf("agent %s must set subagent: true", it.RelPath)
		}
		if fm.InheritCustomizations == nil || *fm.InheritCustomizations {
			return fmt.Errorf("agent %s must set inheritCustomizations: false", it.RelPath)
		}
	default:
		if fm.Subagent == nil || !*fm.Subagent {
			return fmt.Errorf("agent %s must set subagent: true", it.RelPath)
		}
		for _, writeTool := range []string{"write_to_file", "replace_file_content", "multi_replace_file_content"} {
			if slices.Contains(fm.Tools, writeTool) {
				return fmt.Errorf("read-only agent %s must omit write/edit tool %q", it.RelPath, writeTool)
			}
		}
	}
	return nil
}

func validateSkill(it AssetFile, fm Frontmatter) error {
	parts := strings.Split(it.RelPath, "/")
	if len(parts) != 3 || parts[2] != "SKILL.md" {
		return fmt.Errorf("skill %s must have 1-level depth skills/<name>/SKILL.md", it.RelPath)
	}
	folderName := parts[1]
	if strings.TrimSpace(fm.Name) == "" {
		return fmt.Errorf("skill %s missing name", it.RelPath)
	}
	if fm.Name != folderName {
		return fmt.Errorf("skill %s frontmatter name %q does not match folder name %q", it.RelPath, fm.Name, folderName)
	}
	if strings.TrimSpace(fm.Description) == "" {
		return fmt.Errorf("skill %s missing description", it.RelPath)
	}
	return nil
}

// ParseFrontmatter extracts YAML frontmatter delimited by --- lines and returns the remaining body.
func ParseFrontmatter(data []byte) (Frontmatter, string, error) {
	normalized := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	text := string(normalized)
	if !strings.HasPrefix(text, "---\n") {
		return Frontmatter{}, "", errors.New("missing YAML frontmatter opening delimiter '---'")
	}
	rest := text[len("---\n"):]
	endIdx := strings.Index(rest, "\n---\n")
	if endIdx == -1 {
		if strings.HasSuffix(rest, "\n---") {
			endIdx = len(rest) - len("\n---")
		} else {
			return Frontmatter{}, "", errors.New("missing YAML frontmatter closing delimiter '---'")
		}
	}

	header := rest[:endIdx]
	bodyStart := endIdx + len("\n---\n")
	var body string
	if bodyStart <= len(rest) {
		body = rest[bodyStart:]
	}

	fm := Frontmatter{Extra: map[string]string{}}
	lines := strings.Split(header, "\n")
	var inListKey string
	for i := 0; i < len(lines); i++ {
		rawLine := lines[i]
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if inListKey != "" && strings.HasPrefix(trimmed, "- ") {
			item := unquoteYAML(strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
			if inListKey == "tools" && item != "" {
				fm.Tools = append(fm.Tools, item)
			}
			continue
		}
		inListKey = ""

		colon := strings.IndexByte(trimmed, ':')
		if colon <= 0 {
			return Frontmatter{}, "", fmt.Errorf("invalid frontmatter line %q", rawLine)
		}
		key := strings.TrimSpace(trimmed[:colon])
		val := strings.TrimSpace(trimmed[colon+1:])

		if val == "" {
			inListKey = key
			continue
		}

		switch key {
		case "trigger":
			fm.Trigger = unquoteYAML(val)
		case "name":
			fm.Name = unquoteYAML(val)
		case "description":
			fm.Description = unquoteYAML(val)
		case "model":
			fm.Model = unquoteYAML(val)
		case "commandExecutionPolicy":
			fm.CommandExecution = unquoteYAML(val)
		case "mainAgent":
			b, err := parseYAMLBool(key, val)
			if err != nil {
				return Frontmatter{}, "", err
			}
			fm.MainAgent = &b
		case "subagent":
			b, err := parseYAMLBool(key, val)
			if err != nil {
				return Frontmatter{}, "", err
			}
			fm.Subagent = &b
		case "inheritCustomizations":
			b, err := parseYAMLBool(key, val)
			if err != nil {
				return Frontmatter{}, "", err
			}
			fm.InheritCustomizations = &b
		case "tools":
			list, err := parseInlineYAMLList(val)
			if err != nil {
				return Frontmatter{}, "", fmt.Errorf("invalid tools list: %w", err)
			}
			fm.Tools = list
		default:
			fm.Extra[key] = unquoteYAML(val)
		}
	}

	return fm, body, nil
}

func parseYAMLBool(key, val string) (bool, error) {
	switch strings.ToLower(unquoteYAML(val)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("frontmatter key %q must be true or false, got %q", key, val)
	}
}

func parseInlineYAMLList(val string) ([]string, error) {
	if !strings.HasPrefix(val, "[") || !strings.HasSuffix(val, "]") {
		return nil, fmt.Errorf("expected [...] list, got %q", val)
	}
	inner := strings.TrimSpace(val[1 : len(val)-1])
	if inner == "" {
		return nil, nil
	}
	parts := strings.Split(inner, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		item := unquoteYAML(strings.TrimSpace(p))
		if item != "" {
			out = append(out, item)
		}
	}
	return out, nil
}

func unquoteYAML(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
