package hooks

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Nathanael-Huaman/odd-agy/internal/config"
	"github.com/Nathanael-Huaman/odd-agy/internal/rdd"
)

func handlePreToolUse(in PreToolUseInput) (PreToolUseOutput, error) {
	workspace, err := resolveWorkspace(in.CommonInput)
	if err != nil {
		return PreToolUseOutput{}, err
	}
	cfg, err := config.LoadConfig(workspace)
	if err != nil {
		return PreToolUseOutput{}, err
	}

	toolName := strings.ToLower(strings.TrimSpace(in.ToolCall.Name))

	// Rule A: Sensitive path and workspace-boundary protection.
	switch toolName {
	case "view_file", "write_to_file", "replace_file_content", "multi_replace_file_content":
		paths := extractToolPaths(in.ToolCall.Args)
		for _, p := range paths {
			if reason := checkSensitiveOrOutOfBoundsPath(workspace, p, in.ToolCall.Args); reason != "" {
				return PreToolUseOutput{Decision: DecisionDeny, Reason: reason}, nil
			}
		}
	case "run_command":
		cmdStr := getCommandLine(in.ToolCall.Args)
		if cwdArg := getStringArg(in.ToolCall.Args, "Cwd"); cwdArg != "" {
			if reason := checkPathBoundsOnly(workspace, cwdArg); reason != "" {
				return PreToolUseOutput{Decision: DecisionDeny, Reason: reason}, nil
			}
		}
		if reason := checkCommandSensitivePaths(workspace, cmdStr); reason != "" {
			return PreToolUseOutput{Decision: DecisionDeny, Reason: reason}, nil
		}
	}

	// Rule B: full-delegation parent write guard.
	if cfg.DelegationMode == config.DelegationModeFull {
		switch toolName {
		case "write_to_file", "replace_file_content", "multi_replace_file_content":
			paths := extractToolPaths(in.ToolCall.Args)
			for _, p := range paths {
				if reason := checkFullDelegationWriteTarget(workspace, p); reason != "" {
					return PreToolUseOutput{Decision: DecisionDeny, Reason: reason}, nil
				}
			}
		case "run_command":
			cmdStr := getCommandLine(in.ToolCall.Args)
			if reason := checkFullDelegationCommandMutation(workspace, cmdStr); reason != "" {
				return PreToolUseOutput{Decision: DecisionDeny, Reason: reason}, nil
			}
		}
	}

	// Rule C: invoke_subagent strict admission.
	if toolName == "invoke_subagent" {
		return checkInvokeSubagent(workspace, cfg, in)
	}

	// Rule D: destructive commands, conflict markers, and RDD pre-delivery gate.
	if toolName == "run_command" {
		cmdStr := getCommandLine(in.ToolCall.Args)
		if reason := checkDestructiveCommand(cmdStr); reason != "" {
			return PreToolUseOutput{Decision: DecisionDeny, Reason: reason}, nil
		}
		if deliveryCmd, isDelivery := detectDeliveryCommand(cmdStr); isDelivery {
			if reason := checkConflictMarkers(workspace); reason != "" {
				return PreToolUseOutput{Decision: DecisionDeny, Reason: reason}, nil
			}
			if cfg.RDDEnabled {
				if reason, err := checkRDDPreDeliveryGate(workspace, deliveryCmd); err != nil {
					return PreToolUseOutput{}, err
				} else if reason != "" {
					return PreToolUseOutput{Decision: DecisionDeny, Reason: reason}, nil
				}
			}
		}
	}

	return PreToolUseOutput{Decision: DecisionAllow}, nil
}

func extractToolPaths(args map[string]any) []string {
	if args == nil {
		return nil
	}
	keys := []string{"TargetFile", "AbsolutePath", "Path", "File"}
	var paths []string
	seen := make(map[string]bool)
	for _, k := range keys {
		if val := getStringArg(args, k); val != "" && !seen[val] {
			seen[val] = true
			paths = append(paths, val)
		}
	}
	return paths
}

func getCommandLine(args map[string]any) string {
	for _, k := range []string{"CommandLine", "Command", "Cmd"} {
		if val := getStringArg(args, k); val != "" {
			return val
		}
	}
	return ""
}

func checkSensitiveOrOutOfBoundsPath(workspace, rawPath string, args map[string]any) string {
	if reason := checkSensitivePathPattern(workspace, rawPath, args); reason != "" {
		return reason
	}
	return checkPathBoundsOnly(workspace, rawPath)
}

func checkPathBoundsOnly(workspace, rawPath string) string {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return ""
	}
	if trimmed == "~" || strings.HasPrefix(trimmed, "~/") || strings.HasPrefix(trimmed, "~\\") {
		return fmt.Sprintf("access denied: path %q escapes workspace root", rawPath)
	}

	absTarget := trimmed
	if !filepath.IsAbs(absTarget) {
		absTarget = filepath.Join(workspace, absTarget)
	}
	realTarget, err := evalSymlinksBestEffort(absTarget)
	if err != nil {
		return fmt.Sprintf("access denied: unable to resolve path %q: %v", rawPath, err)
	}
	realWorkspace, err := evalSymlinksBestEffort(workspace)
	if err != nil {
		return fmt.Sprintf("access denied: unable to resolve workspace root: %v", err)
	}

	if isWithinDir(realTarget, realWorkspace) {
		return ""
	}
	if !filepath.IsAbs(trimmed) {
		return fmt.Sprintf("access denied: relative path %q escapes workspace root", rawPath)
	}
	if isAllowedTempTarget(realWorkspace, realTarget) {
		return ""
	}
	return fmt.Sprintf("access denied: path %q escapes workspace root", rawPath)
}

func checkSensitivePathPattern(workspace, rawPath string, args map[string]any) string {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return ""
	}
	slash := filepath.ToSlash(trimmed)
	lower := strings.ToLower(slash)
	base := strings.ToLower(filepath.Base(slash))

	if base == ".env" || strings.HasPrefix(base, ".env.") {
		switch base {
		case ".env.example", ".env.sample", ".env.template":
		default:
			return fmt.Sprintf("sensitive path blocked: %q", rawPath)
		}
	}

	if base == "id_rsa" || base == "id_ed25519" || strings.HasPrefix(base, "id_rsa.") || strings.HasPrefix(base, "id_ed25519.") {
		return fmt.Sprintf("sensitive SSH key path blocked: %q", rawPath)
	}

	for _, seg := range strings.Split(lower, "/") {
		if seg == ".ssh" {
			return fmt.Sprintf("sensitive .ssh directory blocked: %q", rawPath)
		}
	}

	if strings.Contains(lower, ".aws/credentials") {
		return fmt.Sprintf("sensitive AWS credentials path blocked: %q", rawPath)
	}

	if strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") {
		return fmt.Sprintf("sensitive key/certificate file blocked: %q", rawPath)
	}

	if base == ".npmrc" {
		if npmrcHasAuth(workspace, trimmed, args) {
			return fmt.Sprintf("sensitive .npmrc with auth credentials blocked: %q", rawPath)
		}
	}
	return ""
}

func npmrcHasAuth(workspace, rawPath string, args map[string]any) bool {
	if args != nil {
		for _, k := range []string{"CodeContent", "ReplacementContent"} {
			if content := getStringArg(args, k); containsNPMAuth(content) {
				return true
			}
		}
		if rawChunks, ok := lookupArg(args, "ReplacementChunks"); ok {
			if chunks, ok := rawChunks.([]any); ok {
				for _, ch := range chunks {
					if m, ok := ch.(map[string]any); ok {
						if content := getStringArg(m, "ReplacementContent"); containsNPMAuth(content) {
							return true
						}
					}
				}
			}
		}
	}

	target := rawPath
	if !filepath.IsAbs(target) && !strings.HasPrefix(target, "~") {
		target = filepath.Join(workspace, target)
	}
	if data, err := os.ReadFile(target); err == nil {
		if containsNPMAuth(string(data)) {
			return true
		}
	}
	return false
}

func containsNPMAuth(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "_auth") ||
		strings.Contains(lower, "authtoken") ||
		strings.Contains(lower, "_password")
}

func checkCommandSensitivePaths(workspace, cmdStr string) string {
	for _, seg := range splitShellCommands(cmdStr) {
		tokens := splitShellTokens(seg)
		for _, tok := range tokens {
			clean := stripRedirectionPrefix(tok)
			if clean == "" || strings.HasPrefix(clean, "-") || isFDTarget(clean) || clean == "/dev/null" {
				continue
			}
			// Ignore Go package ellipsis patterns like ./... or ./internal/hooks/...
			if clean == "..." || strings.HasSuffix(clean, "/...") {
				continue
			}
			// Check obvious sensitive path references.
			if reason := checkSensitivePathPattern(workspace, clean, nil); reason != "" {
				return reason
			}
			// Check path escapes for tokens that look like filesystem paths.
			if looksLikeFilePathToken(clean) {
				if reason := checkPathBoundsOnly(workspace, clean); reason != "" {
					return reason
				}
			}
		}
	}
	return ""
}

func stripRedirectionPrefix(tok string) string {
	// Handle tokens attached to redirection operators like ">file" or "2>/tmp/x" or "<.env".
	for _, op := range []string{"&>>", "&>", ">>", ">|", ">", "<"} {
		if idx := strings.Index(tok, op); idx >= 0 {
			prefix := tok[:idx]
			if prefix == "" || isDigits(prefix) {
				return strings.TrimSpace(tok[idx+len(op):])
			}
		}
	}
	return tok
}

func looksLikeFilePathToken(tok string) bool {
	if strings.Contains(tok, "://") {
		return false
	}
	if tok == "~" || strings.HasPrefix(tok, "~/") || strings.HasPrefix(tok, "../") || strings.Contains(tok, "/../") || strings.HasSuffix(tok, "/..") || tok == ".." {
		return true
	}
	if strings.HasPrefix(tok, "/") {
		return true
	}
	return false
}

func checkFullDelegationWriteTarget(workspace, rawPath string) string {
	realRoot, err := evalSymlinksBestEffort(workspace)
	if err != nil {
		return fmt.Sprintf("full-delegation guard failed to resolve workspace: %v", err)
	}
	absTarget := strings.TrimSpace(rawPath)
	if !filepath.IsAbs(absTarget) {
		absTarget = filepath.Join(realRoot, absTarget)
	}
	realTarget, err := evalSymlinksBestEffort(absTarget)
	if err != nil {
		return fmt.Sprintf("full-delegation guard failed to resolve target %q: %v", rawPath, err)
	}

	if isOddTaskMarkdown(realRoot, realTarget) {
		return ""
	}

	rel := rawPath
	if r, err := filepath.Rel(realRoot, realTarget); err == nil {
		rel = filepath.ToSlash(r)
	}
	return fmt.Sprintf("full-delegation mode blocks direct parent edits to %q (only odd/tasks/*.md is allowed); delegate code, test, config, and doc changes to gentle-ai-worker (or external MCP worker) with a ## Allowed edit surfaces section", rel)
}

func isOddTaskMarkdown(realRoot, realTarget string) bool {
	rel, err := filepath.Rel(realRoot, realTarget)
	if err != nil {
		return false
	}
	slash := filepath.ToSlash(rel)
	if !strings.HasPrefix(slash, "odd/tasks/") {
		return false
	}
	name := strings.TrimPrefix(slash, "odd/tasks/")
	if name == "" || strings.Contains(name, "/") {
		return false
	}
	return strings.HasSuffix(strings.ToLower(name), ".md")
}

func checkFullDelegationCommandMutation(workspace, cmdStr string) string {
	realRoot, err := evalSymlinksBestEffort(workspace)
	if err != nil {
		return fmt.Sprintf("full-delegation guard failed to resolve workspace: %v", err)
	}

	for _, seg := range splitShellCommands(cmdStr) {
		tokens := splitShellTokens(seg)
		if len(tokens) == 0 {
			continue
		}

		// 1. Shell redirections: >, >>, >|, &>, &>>
		for i := 0; i < len(tokens); i++ {
			target, hasRedir, consumesNext := parseRedirectionToken(tokens[i])
			if !hasRedir {
				continue
			}
			if consumesNext {
				if i+1 >= len(tokens) {
					continue
				}
				target = tokens[i+1]
				i++
			}
			if reason := validateMutationTargetForFullDelegation(realRoot, target); reason != "" {
				return reason
			}
		}

		cmdName := filepath.Base(tokens[0])
		// 2. tee [-a] <file...>
		if cmdName == "tee" {
			for _, arg := range tokens[1:] {
				if strings.HasPrefix(arg, "-") {
					continue
				}
				if reason := validateMutationTargetForFullDelegation(realRoot, arg); reason != "" {
					return reason
				}
			}
		}

		// 3. sed -i / --in-place or perl -i / -pi
		if cmdName == "sed" && hasInPlaceFlag(tokens[1:], true) {
			if reason := checkInPlaceCommandTargets(realRoot, tokens[1:], true); reason != "" {
				return reason
			}
		}
		if cmdName == "perl" && hasInPlaceFlag(tokens[1:], false) {
			if reason := checkInPlaceCommandTargets(realRoot, tokens[1:], false); reason != "" {
				return reason
			}
		}
	}
	return ""
}

func parseRedirectionToken(tok string) (target string, hasRedir bool, consumesNext bool) {
	// Skip fd duplication or input-only operators.
	for _, op := range []string{"&>>", "&>", ">>", ">|", ">"} {
		idx := strings.Index(tok, op)
		if idx < 0 {
			continue
		}
		prefix := tok[:idx]
		if prefix != "" && !isDigits(prefix) {
			continue
		}
		rest := tok[idx+len(op):]
		if rest == "" {
			return "", true, true
		}
		return rest, true, false
	}
	return "", false, false
}

func hasInPlaceFlag(args []string, allowLongInPlace bool) bool {
	for _, a := range args {
		if allowLongInPlace && (a == "--in-place" || strings.HasPrefix(a, "--in-place=")) {
			return true
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a[1:], "i") {
			return true
		}
	}
	return false
}

func checkInPlaceCommandTargets(realRoot string, args []string, isSed bool) string {
	skipNext := false
	seenScript := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if skipNext {
			skipNext = false
			seenScript = true
			continue
		}
		if a == "-e" || a == "-f" {
			skipNext = true
			continue
		}
		// macOS BSD sed uses "-i ''" with an explicit empty backup extension before the script.
		if isSed && a == "-i" && i+1 < len(args) && args[i+1] == "" {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		if !seenScript {
			// First non-flag arg is the sed/perl expression unless -e/-f was used.
			seenScript = true
			if isSed || !strings.Contains(a, "/") {
				continue
			}
		}
		if reason := validateMutationTargetForFullDelegation(realRoot, a); reason != "" {
			return reason
		}
	}
	return ""
}

func validateMutationTargetForFullDelegation(realRoot, target string) string {
	clean := strings.TrimSpace(target)
	if clean == "" || clean == "/dev/null" || isFDTarget(clean) {
		return ""
	}
	if strings.HasPrefix(clean, "$TMPDIR/") || clean == "$TMPDIR" {
		return ""
	}
	absTarget := clean
	if !filepath.IsAbs(absTarget) {
		absTarget = filepath.Join(realRoot, absTarget)
	}
	realTarget, err := evalSymlinksBestEffort(absTarget)
	if err != nil {
		return fmt.Sprintf("full-delegation mode blocks shell mutation to %q; delegate to gentle-ai-worker with ## Allowed edit surfaces", target)
	}
	if isWithinDir(realTarget, realRoot) {
		if isOddTaskMarkdown(realRoot, realTarget) {
			return ""
		}
		return fmt.Sprintf("full-delegation mode blocks shell mutation to %q outside odd/tasks/*.md; delegate to gentle-ai-worker with ## Allowed edit surfaces", target)
	}
	if !filepath.IsAbs(clean) {
		return fmt.Sprintf("full-delegation mode blocks relative shell mutation to %q outside workspace", target)
	}
	if isAllowedTempTarget(realRoot, realTarget) {
		return ""
	}
	return fmt.Sprintf("full-delegation mode blocks shell mutation to %q outside odd/tasks/*.md; delegate to gentle-ai-worker with ## Allowed edit surfaces", target)
}

func checkDestructiveCommand(cmdStr string) string {
	for _, seg := range splitShellCommands(cmdStr) {
		tokens := splitShellTokens(seg)
		if len(tokens) == 0 {
			continue
		}
		cmd := filepath.Base(tokens[0])

		if cmd == "rm" {
			hasR, hasF := false, false
			var targets []string
			for _, a := range tokens[1:] {
				if strings.HasPrefix(a, "--") {
					if a == "--recursive" {
						hasR = true
					} else if a == "--force" {
						hasF = true
					}
					continue
				}
				if strings.HasPrefix(a, "-") && len(a) > 1 {
					if strings.ContainsAny(a[1:], "rR") {
						hasR = true
					}
					if strings.Contains(a[1:], "f") {
						hasF = true
					}
					continue
				}
				targets = append(targets, a)
			}
			if hasR && hasF {
				for _, tgt := range targets {
					clean := strings.TrimRight(tgt, "/")
					if clean == "" && strings.HasPrefix(tgt, "/") {
						return fmt.Sprintf("destructive command blocked: %q", seg)
					}
					if tgt == "/" || tgt == "/*" || tgt == "~" || tgt == "~/" || tgt == "~/*" || tgt == "$HOME" || tgt == "$HOME/" || tgt == "$HOME/*" {
						return fmt.Sprintf("destructive command blocked: %q", seg)
					}
				}
			}
		}

		if cmd == "git" {
			sub, subArgs := extractGitSubcommand(tokens[1:])
			switch sub {
			case "reset":
				for _, a := range subArgs {
					if a == "--hard" {
						return fmt.Sprintf("destructive git command blocked: %q", seg)
					}
				}
			case "push":
				for _, a := range subArgs {
					if a == "--force" || a == "-f" {
						return fmt.Sprintf("destructive git command blocked: %q", seg)
					}
				}
			case "clean":
				hasF, hasD := false, false
				for _, a := range subArgs {
					if a == "--force" {
						hasF = true
						continue
					}
					if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") {
						if strings.Contains(a[1:], "f") {
							hasF = true
						}
						if strings.Contains(a[1:], "d") {
							hasD = true
						}
					}
				}
				if hasF && hasD {
					return fmt.Sprintf("destructive git command blocked: %q", seg)
				}
			}
		}
	}
	return ""
}

func detectDeliveryCommand(cmdStr string) (string, bool) {
	for _, seg := range splitShellCommands(cmdStr) {
		tokens := splitShellTokens(seg)
		if len(tokens) == 0 {
			continue
		}
		cmd := filepath.Base(tokens[0])
		if cmd == "git" {
			sub, _ := extractGitSubcommand(tokens[1:])
			if sub == "push" {
				return "git push", true
			}
		}
		if cmd == "gh" && len(tokens) >= 3 && tokens[1] == "pr" && (tokens[2] == "create" || tokens[2] == "merge") {
			return "gh pr " + tokens[2], true
		}
	}
	return "", false
}

func checkConflictMarkers(workspace string) string {
	files, err := listChangedAndStagedFiles(workspace)
	if err != nil {
		return ""
	}
	for _, rel := range files {
		full := filepath.Join(workspace, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		if containsConflictMarkers(data) {
			return fmt.Sprintf("delivery blocked: Git conflict markers found in %s", rel)
		}
	}
	return ""
}

func listChangedAndStagedFiles(workspace string) ([]string, error) {
	seen := make(map[string]bool)
	var out []string
	collect := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = workspace
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		if err := cmd.Run(); err != nil {
			return
		}
		for _, raw := range bytes.Split(stdout.Bytes(), []byte{0}) {
			if len(raw) == 0 {
				continue
			}
			p := filepath.ToSlash(string(raw))
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}

	collect("diff", "--name-only", "-z")
	collect("diff", "--cached", "--name-only", "-z")
	collect("ls-files", "--others", "--exclude-standard", "-z")
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func containsConflictMarkers(data []byte) bool {
	lines := strings.Split(string(data), "\n")
	hasOpen := false
	hasClose := false
	for _, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		if strings.HasPrefix(trimmed, "<<<<<<<") {
			hasOpen = true
		} else if strings.HasPrefix(trimmed, ">>>>>>>") {
			hasClose = true
		}
		if hasOpen || hasClose {
			return true
		}
	}
	return false
}

func checkRDDPreDeliveryGate(workspace, deliveryCmd string) (string, error) {
	assess, err := assessWorkspaceForHook(workspace)
	if err != nil {
		// If not a git repo, do not block command with an RDD error.
		if strings.Contains(err.Error(), "not a git repository") {
			return "", nil
		}
		return "", err
	}
	if assess.ReviewDue && !assess.AlreadyApproved {
		return fmt.Sprintf("RDD pre-delivery gate blocked %q: unreviewed %s-risk candidate (%s). Run odd_review_assess / odd_review_start before delivery, or disable RDD per project with 'odd-agy rdd disable'.", deliveryCmd, assess.Risk, assess.ReviewDueReason), nil
	}
	return "", nil
}

func assessWorkspaceForHook(workspace string) (rdd.AssessResult, error) {
	eng := rdd.NewEngine(workspace)
	res, err := eng.Assess(rdd.AssessRequest{})
	if err != nil {
		return rdd.AssessResult{}, err
	}
	if len(res.ChangedPaths) == 0 && gitHasHead(workspace) {
		committedRes, err := eng.Assess(rdd.AssessRequest{CommittedOnly: true})
		if err == nil && len(committedRes.ChangedPaths) > 0 {
			return committedRes, nil
		}
	}
	return res, nil
}

func gitHasHead(workspace string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "HEAD")
	cmd.Dir = workspace
	return cmd.Run() == nil
}

func extractGitSubcommand(args []string) (string, []string) {
	skipNext := false
	for i, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a, args[i+1:]
	}
	return "", nil
}

func splitShellCommands(cmd string) []string {
	var parts []string
	var cur strings.Builder
	inSingle := false
	inDouble := false
	escaped := false

	for i := 0; i < len(cmd); i++ {
		ch := cmd[i]
		if escaped {
			cur.WriteByte(ch)
			escaped = false
			continue
		}
		if ch == '\\' && !inSingle {
			cur.WriteByte(ch)
			escaped = true
			continue
		}
		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			cur.WriteByte(ch)
			continue
		}
		if ch == '"' && !inSingle {
			inDouble = !inDouble
			cur.WriteByte(ch)
			continue
		}
		if !inSingle && !inDouble {
			if ch == ';' || ch == '\n' || ch == '|' {
				if ch == '|' && i+1 < len(cmd) && cmd[i+1] == '|' {
					i++
				}
				if s := strings.TrimSpace(cur.String()); s != "" {
					parts = append(parts, s)
				}
				cur.Reset()
				continue
			}
			if ch == '&' && i+1 < len(cmd) && cmd[i+1] == '&' {
				i++
				if s := strings.TrimSpace(cur.String()); s != "" {
					parts = append(parts, s)
				}
				cur.Reset()
				continue
			}
		}
		cur.WriteByte(ch)
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		parts = append(parts, s)
	}
	return parts
}

func splitShellTokens(seg string) []string {
	var tokens []string
	var cur strings.Builder
	inSingle := false
	inDouble := false
	escaped := false
	hasToken := false

	for i := 0; i < len(seg); i++ {
		ch := seg[i]
		if escaped {
			cur.WriteByte(ch)
			escaped = false
			hasToken = true
			continue
		}
		if ch == '\\' && !inSingle {
			escaped = true
			hasToken = true
			continue
		}
		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			hasToken = true
			continue
		}
		if ch == '"' && !inSingle {
			inDouble = !inDouble
			hasToken = true
			continue
		}
		if !inSingle && !inDouble && (ch == ' ' || ch == '\t' || ch == '\r') {
			if hasToken {
				tokens = append(tokens, cur.String())
				cur.Reset()
				hasToken = false
			}
			continue
		}
		cur.WriteByte(ch)
		hasToken = true
	}
	if hasToken {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

func isFDTarget(s string) bool {
	return strings.HasPrefix(s, "&") && isDigits(s[1:])
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isAllowedTempTarget(realWorkspace, realTarget string) bool {
	if !isAllowedTempPath(realTarget) {
		return false
	}
	// If the workspace itself lives under a test/temp directory (e.g., t.TempDir()),
	// block sibling/parent escapes like filepath.Join(workspace, "..", "escape.txt").
	wsParent := filepath.Dir(realWorkspace)
	tgtParent := filepath.Dir(realTarget)
	if wsParent != realWorkspace && tgtParent == wsParent {
		if !isExactTempRoot(wsParent) {
			return false
		}
	}
	return true
}

func isExactTempRoot(dir string) bool {
	for _, c := range []string{os.TempDir(), "/tmp", "/var/tmp"} {
		if strings.TrimSpace(c) == "" {
			continue
		}
		if realTmp, err := evalSymlinksBestEffort(c); err == nil && dir == realTmp {
			return true
		}
	}
	return false
}

func isAllowedTempPath(realTarget string) bool {
	candidates := []string{os.TempDir(), "/tmp", "/var/tmp"}
	for _, c := range candidates {
		if strings.TrimSpace(c) == "" {
			continue
		}
		realTmp, err := evalSymlinksBestEffort(c)
		if err != nil {
			continue
		}
		if isWithinDir(realTarget, realTmp) {
			return true
		}
	}
	return false
}

func evalSymlinksBestEffort(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}

	var tail []string
	cur := abs
	for {
		parent := filepath.Dir(cur)
		base := filepath.Base(cur)
		if parent == cur {
			return abs, nil
		}
		tail = append([]string{base}, tail...)
		cur = parent
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			parts := append([]string{resolved}, tail...)
			return filepath.Join(parts...), nil
		}
	}
}

func isWithinDir(target, dir string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func lookupArg(args map[string]any, key string) (any, bool) {
	if args == nil {
		return nil, false
	}
	if v, ok := args[key]; ok {
		return v, true
	}
	for k, v := range args {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return nil, false
}

func getStringArg(args map[string]any, key string) string {
	v, ok := lookupArg(args, key)
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}
