package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Nathanael-Huaman/odd-agy/internal/config"
	"github.com/Nathanael-Huaman/odd-agy/internal/hooks"
	"github.com/Nathanael-Huaman/odd-agy/internal/installer"
	"github.com/Nathanael-Huaman/odd-agy/internal/llm"
	"github.com/Nathanael-Huaman/odd-agy/internal/mcp"
	"github.com/Nathanael-Huaman/odd-agy/internal/tui"
	"github.com/Nathanael-Huaman/odd-agy/internal/upstream"
	"github.com/Nathanael-Huaman/odd-agy/internal/version"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

var (
	runTUI         = tui.Run
	lookPath       = exec.LookPath
	runExternalCmd = func(dir string, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) int {
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Stdin = stdin
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				code := exitErr.ExitCode()
				if code < 0 {
					fmt.Fprintf(stderr, "odd-agy: %s terminated by signal: %v\n", name, err)
					return exitError
				}
				return code
			}
			fmt.Fprintf(stderr, "odd-agy launch: %v\n", err)
			return exitError
		}
		return exitOK
	}
)

// Run executes the CLI using os.Stdin as standard input.
func Run(args []string, stdout, stderr io.Writer, cwd string) int {
	return RunWithIO(args, os.Stdin, stdout, stderr, cwd)
}

// RunWithIO executes the CLI with explicit stdin/stdout/stderr streams.
func RunWithIO(args []string, stdin io.Reader, stdout, stderr io.Writer, cwd string) int {
	pos, targetCwd, err := extractCwdFlag(args, cwd)
	if err != nil {
		fmt.Fprintf(stderr, "odd-agy: %v\n", err)
		return exitUsage
	}

	if len(pos) == 0 {
		if err := runTUI(targetCwd); err != nil {
			fmt.Fprintf(stderr, "odd-agy ui: %v\n", err)
			return exitError
		}
		return exitOK
	}

	cmd := pos[0]
	rest := pos[1:]

	switch cmd {
	case "--version", "-v", "version":
		if len(rest) > 0 {
			fmt.Fprintf(stderr, "odd-agy %s: unexpected arguments %v\n", cmd, rest)
			return exitUsage
		}
		fmt.Fprintf(stdout, "odd-agy %s (%s)\n", version.Version, version.Repo)
		return exitOK
	case "--help", "-h", "help":
		printHelp(stdout)
		return exitOK
	case "ui":
		if len(rest) > 0 {
			fmt.Fprintf(stderr, "odd-agy ui: unexpected arguments %v\n", rest)
			return exitUsage
		}
		if err := runTUI(targetCwd); err != nil {
			fmt.Fprintf(stderr, "odd-agy ui: %v\n", err)
			return exitError
		}
		return exitOK
	case "init":
		return runInit(rest, stdout, stderr, targetCwd)
	case "sync":
		return runSync(rest, stdout, stderr, targetCwd)
	case "doctor":
		return runDoctor(rest, stdout, stderr, targetCwd)
	case "uninstall":
		return runUninstall(rest, stdout, stderr, targetCwd)
	case "launch":
		return runLaunch(rest, stdin, stdout, stderr, targetCwd)
	case "permissions":
		return runPermissions(rest, stdout, stderr)
	case "hook":
		return runHook(rest, stdin, stdout, stderr)
	case "mcp":
		return runMCP(rest, stdin, stdout, stderr, targetCwd)
	case "mode":
		return runMode(rest, stdout, stderr, targetCwd)
	case "rdd":
		return runRDD(rest, stdout, stderr, targetCwd)
	case "profile":
		return runProfile(rest, stdout, stderr, targetCwd)
	case "upstream":
		return runUpstream(rest, stdout, stderr, targetCwd)
	default:
		fmt.Fprintf(stderr, "odd-agy: unknown command %q (run 'odd-agy --help' for usage)\n", cmd)
		return exitUsage
	}
}

func extractCwdFlag(args []string, defaultCwd string) ([]string, string, error) {
	cwd := defaultCwd
	pos := make([]string, 0, len(args))
	explicit := false

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			pos = append(pos, args[i:]...)
			break
		}
		if arg == "--cwd" {
			if i+1 >= len(args) {
				return nil, "", fmt.Errorf("--cwd requires a directory argument")
			}
			cwd = args[i+1]
			explicit = true
			i++
			continue
		}
		if strings.HasPrefix(arg, "--cwd=") {
			cwd = strings.TrimPrefix(arg, "--cwd=")
			if cwd == "" {
				return nil, "", fmt.Errorf("--cwd requires a non-empty directory")
			}
			explicit = true
			continue
		}
		pos = append(pos, arg)
	}

	if explicit {
		info, err := os.Stat(cwd)
		if err != nil {
			return nil, "", fmt.Errorf("invalid --cwd %q: %w", cwd, err)
		}
		if !info.IsDir() {
			return nil, "", fmt.Errorf("invalid --cwd %q: not a directory", cwd)
		}
	}
	return pos, cwd, nil
}

func parseValueFlag(args []string, idx int, name string) (string, int, bool, error) {
	arg := args[idx]
	if arg == name {
		if idx+1 >= len(args) {
			return "", idx, true, fmt.Errorf("%s requires a value", name)
		}
		return args[idx+1], idx + 1, true, nil
	}
	prefix := name + "="
	if strings.HasPrefix(arg, prefix) {
		return strings.TrimPrefix(arg, prefix), idx, true, nil
	}
	return "", idx, false, nil
}

func runInit(args []string, stdout, stderr io.Writer, cwd string) int {
	var opts installer.InitOptions
	allowMCP := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--force" {
			opts.Force = true
			continue
		}
		if arg == "--allow-mcp" {
			allowMCP = true
			continue
		}
		if val, nextIdx, matched, err := parseValueFlag(args, i, "--mode"); matched {
			if err != nil {
				fmt.Fprintf(stderr, "odd-agy init: %v\n", err)
				return exitUsage
			}
			opts.InstallMode = val
			i = nextIdx
			continue
		}
		if val, nextIdx, matched, err := parseValueFlag(args, i, "--delegation"); matched {
			if err != nil {
				fmt.Fprintf(stderr, "odd-agy init: %v\n", err)
				return exitUsage
			}
			opts.DelegationMode = val
			i = nextIdx
			continue
		}
		if val, nextIdx, matched, err := parseValueFlag(args, i, "--profile"); matched {
			if err != nil {
				fmt.Fprintf(stderr, "odd-agy init: %v\n", err)
				return exitUsage
			}
			opts.Profile = val
			i = nextIdx
			continue
		}
		if val, nextIdx, matched, err := parseValueFlag(args, i, "--binary"); matched {
			if err != nil {
				fmt.Fprintf(stderr, "odd-agy init: %v\n", err)
				return exitUsage
			}
			opts.BinaryCommand = val
			i = nextIdx
			continue
		}
		fmt.Fprintf(stderr, "odd-agy init: unknown or unexpected argument %q\n", arg)
		return exitUsage
	}

	res, err := installer.Init(cwd, opts)
	if err != nil {
		fmt.Fprintf(stderr, "odd-agy init: %v\n", err)
		return exitError
	}
	if allowMCP {
		permRes, err := installer.GrantGlobalMCPPermissions("")
		if err != nil {
			fmt.Fprintf(stderr, "odd-agy init --allow-mcp: %v\n", err)
			return exitError
		}
		fmt.Fprintf(stdout, "initialized %d asset(s) under .agents/ (manifest=%s preservedConfig=%t engramMCP=%t globalMCP=enabled settings=%s)\n",
			len(res.InstalledFiles), res.ManifestPath, res.PreservedConfig, res.AddedEngramMCP, permRes.SettingsPath)
		return exitOK
	}
	fmt.Fprintf(stdout, "initialized %d asset(s) under .agents/ (manifest=%s preservedConfig=%t engramMCP=%t)\n",
		len(res.InstalledFiles), res.ManifestPath, res.PreservedConfig, res.AddedEngramMCP)
	return exitOK
}

func runSync(args []string, stdout, stderr io.Writer, cwd string) int {
	var opts installer.SyncOptions
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--force" {
			opts.Force = true
			continue
		}
		if val, nextIdx, matched, err := parseValueFlag(args, i, "--binary"); matched {
			if err != nil {
				fmt.Fprintf(stderr, "odd-agy sync: %v\n", err)
				return exitUsage
			}
			opts.BinaryCommand = val
			i = nextIdx
			continue
		}
		fmt.Fprintf(stderr, "odd-agy sync: unknown or unexpected argument %q\n", arg)
		return exitUsage
	}

	res, err := installer.Sync(cwd, opts)
	if err != nil {
		fmt.Fprintf(stderr, "odd-agy sync: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "sync complete: %d created, %d updated, %d unchanged, %d removed, %d backed up\n",
		len(res.Created), len(res.Updated), len(res.Unchanged), len(res.Removed), len(res.BackedUp))
	return exitOK
}

func runDoctor(args []string, stdout, stderr io.Writer, cwd string) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "odd-agy doctor: unexpected arguments %v\n", args)
		return exitUsage
	}
	report, err := installer.Doctor(cwd, installer.DoctorOptions{})
	if err != nil {
		fmt.Fprintf(stderr, "odd-agy doctor: %v\n", err)
		return exitError
	}
	for _, c := range report.Checks {
		fmt.Fprintf(stdout, "%-6s %-20s %s\n", "["+c.Status+"]", c.Name, c.Detail)
	}
	if report.HasFailures() {
		return exitError
	}
	return exitOK
}

func runUninstall(args []string, stdout, stderr io.Writer, cwd string) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "odd-agy uninstall: unexpected arguments %v\n", args)
		return exitUsage
	}
	res, err := installer.Uninstall(cwd)
	if err != nil {
		fmt.Fprintf(stderr, "odd-agy uninstall: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "uninstall complete: %d removed, %d updated, %d preserved\n",
		len(res.Removed), len(res.Updated), len(res.Preserved))
	return exitOK
}

func runLaunch(args []string, stdin io.Reader, stdout, stderr io.Writer, cwd string) int {
	agentName := "odd-orchestrator"
	agentExplicit := false
	noAgent := false
	var extraArgs []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			extraArgs = append(extraArgs, args[i+1:]...)
			break
		}
		if arg == "--no-agent" {
			noAgent = true
			continue
		}
		if val, nextIdx, matched, err := parseValueFlag(args, i, "--agent"); matched {
			if err != nil {
				fmt.Fprintf(stderr, "odd-agy launch: %v\n", err)
				return exitUsage
			}
			if strings.TrimSpace(val) == "" {
				fmt.Fprintf(stderr, "odd-agy launch: --agent requires a non-empty name\n")
				return exitUsage
			}
			agentName = val
			agentExplicit = true
			i = nextIdx
			continue
		}
		fmt.Fprintf(stderr, "odd-agy launch: unknown or unexpected argument %q (pass extra agy args after '--')\n", arg)
		return exitUsage
	}

	if noAgent && agentExplicit {
		fmt.Fprintf(stderr, "odd-agy launch: --agent and --no-agent cannot be used together\n")
		return exitUsage
	}

	settingsPath := installer.SessionSettingsPath(cwd)
	info, err := os.Stat(settingsPath)
	if err != nil || info.IsDir() {
		fmt.Fprintf(stderr, "odd-agy launch: missing %s (run 'odd-agy init' first)\n", installer.SessionSettingsRelPath)
		return exitError
	}
	absSettings, err := filepath.Abs(settingsPath)
	if err != nil {
		fmt.Fprintf(stderr, "odd-agy launch: resolve settings path: %v\n", err)
		return exitError
	}

	agyBin, err := lookPath("agy")
	if err != nil {
		fmt.Fprintf(stderr, "odd-agy launch: agy binary not found on PATH: %v\n", err)
		return exitError
	}

	var orchestratorModel string
	var orchestratorEffort string
	if !noAgent && agentName == "odd-orchestrator" && (!hasCLIFlag(extraArgs, "--model") || !hasCLIFlag(extraArgs, "--effort")) {
		if _, err := os.Stat(config.ProfilesPath(cwd)); err == nil {
			profs, err := config.LoadProfiles(cwd)
			if err != nil {
				fmt.Fprintf(stderr, "odd-agy launch: %v\n", err)
				return exitError
			}
			if prof, ok := profs.Profiles[profs.ActiveProfile]; ok {
				ra := prof.Roles[config.RoleOrchestrator]
				if ra.Runtime == config.RuntimeAgyNative {
					model := strings.TrimSpace(ra.Model)
					if !hasCLIFlag(extraArgs, "--model") && model != "" && model != "inherit" {
						orchestratorModel = model
					}
					if effort := strings.TrimSpace(ra.Effort); !hasCLIFlag(extraArgs, "--effort") && effort != "" && !modelHasEffortSuffix(model) {
						orchestratorEffort = effort
					}
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "odd-agy launch: stat profiles: %v\n", err)
			return exitError
		}
	}

	agyArgs := make([]string, 0, 8+len(extraArgs))
	if !noAgent {
		agyArgs = append(agyArgs, "--agent", agentName)
		if orchestratorModel != "" {
			agyArgs = append(agyArgs, "--model", orchestratorModel)
		}
		if orchestratorEffort != "" {
			agyArgs = append(agyArgs, "--effort", orchestratorEffort)
		}
	}
	agyArgs = append(agyArgs, "--settings", absSettings)
	agyArgs = append(agyArgs, extraArgs...)

	return runExternalCmd(cwd, stdin, stdout, stderr, agyBin, agyArgs...)
}

func hasCLIFlag(args []string, flagName string) bool {
	prefix := flagName + "="
	for _, a := range args {
		if a == flagName || strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

func modelHasEffortSuffix(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	for _, suffix := range []string{"-low", "-medium", "-high", "-xhigh", "-max"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

func runPermissions(args []string, stdout, stderr io.Writer) int {
	sub := "status"
	if len(args) > 1 {
		fmt.Fprintf(stderr, "odd-agy permissions: expected at most 1 argument, got %d\n", len(args))
		return exitUsage
	}
	if len(args) == 1 {
		sub = args[0]
	}

	switch sub {
	case "status":
		enabled, err := installer.CheckGlobalMCPPermissions("")
		if err != nil {
			fmt.Fprintf(stderr, "odd-agy permissions status: %v\n", err)
			return exitError
		}
		state := "disabled"
		if enabled {
			state = "enabled"
		}
		fmt.Fprintf(stdout, "global MCP permissions: %s (%s)\n", state, installer.GlobalSettingsPath(""))
		return exitOK

	case "allow-mcp":
		res, err := installer.GrantGlobalMCPPermissions("")
		if err != nil {
			fmt.Fprintf(stderr, "odd-agy permissions allow-mcp: %v\n", err)
			return exitError
		}
		if res.BackupPath != "" {
			fmt.Fprintf(stdout, "global MCP permissions enabled (settings=%s changed=%t backup=%s)\n",
				res.SettingsPath, res.Changed, res.BackupPath)
		} else {
			fmt.Fprintf(stdout, "global MCP permissions enabled (settings=%s changed=%t)\n",
				res.SettingsPath, res.Changed)
		}
		return exitOK

	case "revoke-mcp":
		res, err := installer.RevokeGlobalMCPPermissions("")
		if err != nil {
			fmt.Fprintf(stderr, "odd-agy permissions revoke-mcp: %v\n", err)
			return exitError
		}
		fmt.Fprintf(stdout, "global MCP permissions revoked (settings=%s changed=%t)\n",
			res.SettingsPath, res.Changed)
		return exitOK

	default:
		fmt.Fprintf(stderr, "odd-agy permissions: invalid subcommand %q (expected \"status\", \"allow-mcp\", or \"revoke-mcp\")\n", sub)
		return exitUsage
	}
}

func runHook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(stderr, "odd-agy hook: expected <SessionStart|PreToolUse|PostToolUse|PreInvocation|Stop>\n")
		return exitUsage
	}
	if err := hooks.Dispatch(args[0], stdin, stdout); err != nil {
		fmt.Fprintf(stderr, "odd-agy hook %s: %v\n", args[0], err)
		if strings.Contains(err.Error(), "unknown hook event") {
			return exitUsage
		}
		return exitError
	}
	return exitOK
}

func runMCP(args []string, stdin io.Reader, stdout, stderr io.Writer, cwd string) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "odd-agy mcp: unexpected arguments %v\n", args)
		return exitUsage
	}
	srv := mcp.NewServer(mcp.ServerConfig{Workspace: cwd})
	if err := srv.Serve(context.Background(), stdin, stdout); err != nil {
		fmt.Fprintf(stderr, "odd-agy mcp: %v\n", err)
		return exitError
	}
	return exitOK
}

func runMode(args []string, stdout, stderr io.Writer, cwd string) int {
	sub := "status"
	if len(args) > 1 {
		fmt.Fprintf(stderr, "odd-agy mode: expected at most 1 argument, got %d\n", len(args))
		return exitUsage
	}
	if len(args) == 1 {
		sub = args[0]
	}

	cfg, err := config.LoadConfig(cwd)
	if err != nil {
		fmt.Fprintf(stderr, "odd-agy mode: %v\n", err)
		return exitError
	}

	switch sub {
	case "status":
		fmt.Fprintf(stdout, "delegationMode: %s\n", cfg.DelegationMode)
		return exitOK
	case config.DelegationModeFull, config.DelegationModeOrganic:
		cfg.DelegationMode = sub
		if err := config.SaveConfig(cwd, cfg); err != nil {
			fmt.Fprintf(stderr, "odd-agy mode: %v\n", err)
			return exitError
		}
		fmt.Fprintf(stdout, "delegationMode set to %s (%s)\n", cfg.DelegationMode, config.ConfigPath(cwd))
		return exitOK
	default:
		fmt.Fprintf(stderr, "odd-agy mode: invalid mode %q (expected %q, %q, or \"status\")\n", sub, config.DelegationModeFull, config.DelegationModeOrganic)
		return exitUsage
	}
}

func runRDD(args []string, stdout, stderr io.Writer, cwd string) int {
	sub := "status"
	if len(args) > 1 {
		fmt.Fprintf(stderr, "odd-agy rdd: expected at most 1 argument, got %d\n", len(args))
		return exitUsage
	}
	if len(args) == 1 {
		sub = args[0]
	}

	cfg, err := config.LoadConfig(cwd)
	if err != nil {
		fmt.Fprintf(stderr, "odd-agy rdd: %v\n", err)
		return exitError
	}

	switch sub {
	case "status":
		state := "disabled"
		if cfg.RDDEnabled {
			state = "enabled"
		}
		fmt.Fprintf(stdout, "rdd: %s\n", state)
		return exitOK
	case "enable":
		cfg.RDDEnabled = true
		if err := config.SaveConfig(cwd, cfg); err != nil {
			fmt.Fprintf(stderr, "odd-agy rdd: %v\n", err)
			return exitError
		}
		fmt.Fprintf(stdout, "rdd enabled (%s)\n", config.ConfigPath(cwd))
		return exitOK
	case "disable":
		cfg.RDDEnabled = false
		if err := config.SaveConfig(cwd, cfg); err != nil {
			fmt.Fprintf(stderr, "odd-agy rdd: %v\n", err)
			return exitError
		}
		fmt.Fprintf(stdout, "rdd disabled (%s)\n", config.ConfigPath(cwd))
		return exitOK
	default:
		fmt.Fprintf(stderr, "odd-agy rdd: invalid subcommand %q (expected \"status\", \"enable\", or \"disable\")\n", sub)
		return exitUsage
	}
}

func runProfile(args []string, stdout, stderr io.Writer, cwd string) int {
	sub := "list"
	var rest []string
	if len(args) > 0 {
		sub = args[0]
		rest = args[1:]
	}

	cfg, err := config.LoadProfiles(cwd)
	if err != nil {
		fmt.Fprintf(stderr, "odd-agy profile: %v\n", err)
		return exitError
	}

	switch sub {
	case "list":
		if len(rest) > 0 {
			fmt.Fprintf(stderr, "odd-agy profile list: unexpected arguments %v\n", rest)
			return exitUsage
		}
		names := make([]string, 0, len(cfg.Profiles))
		for name := range cfg.Profiles {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			marker := " "
			if name == cfg.ActiveProfile {
				marker = "*"
			}
			desc := cfg.Profiles[name].Description
			if desc != "" {
				fmt.Fprintf(stdout, "%s %s - %s\n", marker, name, desc)
			} else {
				fmt.Fprintf(stdout, "%s %s\n", marker, name)
			}
		}
		return exitOK

	case "show":
		if len(rest) > 1 {
			fmt.Fprintf(stderr, "odd-agy profile show: expected at most 1 profile name\n")
			return exitUsage
		}
		target := cfg.ActiveProfile
		if len(rest) == 1 {
			target = rest[0]
		}
		prof, ok := cfg.Profiles[target]
		if !ok {
			fmt.Fprintf(stderr, "odd-agy profile show: unknown profile %q\n", target)
			return exitUsage
		}
		activeLabel := ""
		if target == cfg.ActiveProfile {
			activeLabel = " (active)"
		}
		fmt.Fprintf(stdout, "Profile: %s%s\n", target, activeLabel)
		if prof.Description != "" {
			fmt.Fprintf(stdout, "Description: %s\n", prof.Description)
		}
		for _, role := range config.AllRoles {
			route := prof.Roles[role]
			if route.Provider != "" {
				fmt.Fprintf(stdout, "  %-20s runtime=%s provider=%s model=%s", role, route.Runtime, route.Provider, route.Model)
			} else {
				fmt.Fprintf(stdout, "  %-20s runtime=%s model=%s", role, route.Runtime, route.Model)
			}
			if route.Effort != "" {
				fmt.Fprintf(stdout, " effort=%s", route.Effort)
			}
			if len(route.Fallback) > 0 {
				fb := route.Fallback[0]
				if fb.Provider != "" {
					fmt.Fprintf(stdout, " fallback=%s/%s:%s", fb.Runtime, fb.Provider, fb.Model)
				} else {
					fmt.Fprintf(stdout, " fallback=%s:%s", fb.Runtime, fb.Model)
				}
			}
			fmt.Fprintln(stdout)
		}
		return exitOK

	case "use":
		if len(rest) != 1 {
			fmt.Fprintf(stderr, "odd-agy profile use: expected exactly 1 profile name\n")
			return exitUsage
		}
		name := rest[0]
		if _, ok := cfg.Profiles[name]; !ok {
			fmt.Fprintf(stderr, "odd-agy profile use: unknown profile %q\n", name)
			return exitUsage
		}
		cfg.ActiveProfile = name
		if err := config.ValidateProfiles(cfg); err != nil {
			fmt.Fprintf(stderr, "odd-agy profile use: %v\n", err)
			return exitError
		}
		if _, err := installer.ApplyProfileModels(cwd, cfg); err != nil {
			fmt.Fprintf(stderr, "odd-agy profile use: %v\n", err)
			return exitError
		}
		if err := config.SaveProfiles(cwd, cfg); err != nil {
			_, _ = installer.ApplyActiveProfileModels(cwd)
			fmt.Fprintf(stderr, "odd-agy profile use: %v\n", err)
			return exitError
		}
		fmt.Fprintf(stdout, "active profile set to %s (%s)\n", name, config.ProfilesPath(cwd))
		return exitOK

	case "providers":
		if len(rest) > 0 {
			fmt.Fprintf(stderr, "odd-agy profile providers: unexpected arguments %v\n", rest)
			return exitUsage
		}
		names := make([]string, 0, len(cfg.Providers))
		for name := range cfg.Providers {
			names = append(names, name)
		}
		sort.Strings(names)
		hasInvalid := false
		for _, name := range names {
			p := cfg.Providers[name]
			ep, err := config.ResolveProviderEndpoint(p, os.LookupEnv)
			if err != nil {
				hasInvalid = true
				fmt.Fprintf(stdout, "%-18s invalid (%s)\n", name, sanitizeProviderErr(p, err))
				continue
			}
			fmt.Fprintf(stdout, "%-18s url=%s compat=%s keyEnv=%s keyStatus=%s defaultModel=%s\n",
				name, llm.RedactURL(ep.BaseURL), ep.Compatibility, p.APIKeyEnv, ep.KeySource, p.DefaultModel)
		}
		if hasInvalid {
			return exitError
		}
		return exitOK

	case "test":
		if len(rest) != 1 {
			fmt.Fprintf(stderr, "odd-agy profile test: expected exactly 1 provider name\n")
			return exitUsage
		}
		name := rest[0]
		prov, ok := cfg.Providers[name]
		if !ok {
			fmt.Fprintf(stderr, "odd-agy profile test: unknown provider %q\n", name)
			return exitUsage
		}
		res := llm.ProbeProvider(context.Background(), name, prov, os.LookupEnv)
		if res.Error != "" {
			fmt.Fprintf(stdout, "provider=%s reachable=%t status=%d latency=%dms url=%s keySource=%s error=%s\n",
				res.Provider, res.Reachable, res.StatusCode, res.LatencyMS, res.RedactedURL, res.KeySource, res.Error)
		} else {
			fmt.Fprintf(stdout, "provider=%s reachable=%t status=%d latency=%dms url=%s keySource=%s\n",
				res.Provider, res.Reachable, res.StatusCode, res.LatencyMS, res.RedactedURL, res.KeySource)
		}
		if !res.Reachable {
			return exitError
		}
		return exitOK

	default:
		fmt.Fprintf(stderr, "odd-agy profile: unknown subcommand %q (expected \"list\", \"show\", \"use\", \"providers\", or \"test\")\n", sub)
		return exitUsage
	}
}

func runUpstream(args []string, stdout, stderr io.Writer, cwd string) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, "odd-agy upstream: expected subcommand \"check\" or \"sync\"\n")
		return exitUsage
	}
	sub := args[0]
	rest := args[1:]

	lockPath := filepath.Join(cwd, "upstream", "upstream.lock.json")
	var sourceDir, ref string

	for i := 0; i < len(rest); i++ {
		if val, nextIdx, matched, err := parseValueFlag(rest, i, "--lock"); matched {
			if err != nil {
				fmt.Fprintf(stderr, "odd-agy upstream %s: %v\n", sub, err)
				return exitUsage
			}
			if filepath.IsAbs(val) {
				lockPath = val
			} else {
				lockPath = filepath.Join(cwd, val)
			}
			i = nextIdx
			continue
		}
		if val, nextIdx, matched, err := parseValueFlag(rest, i, "--source-dir"); matched {
			if err != nil {
				fmt.Fprintf(stderr, "odd-agy upstream %s: %v\n", sub, err)
				return exitUsage
			}
			if filepath.IsAbs(val) {
				sourceDir = val
			} else {
				sourceDir = filepath.Join(cwd, val)
			}
			i = nextIdx
			continue
		}
		if val, nextIdx, matched, err := parseValueFlag(rest, i, "--ref"); matched {
			if err != nil {
				fmt.Fprintf(stderr, "odd-agy upstream %s: %v\n", sub, err)
				return exitUsage
			}
			ref = val
			i = nextIdx
			continue
		}
		fmt.Fprintf(stderr, "odd-agy upstream %s: unknown or unexpected argument %q\n", sub, rest[i])
		return exitUsage
	}

	switch sub {
	case "check":
		rep, err := upstream.Check(context.Background(), lockPath, upstream.CheckOptions{
			SourceDir: sourceDir,
			Ref:       ref,
		})
		if err != nil {
			fmt.Fprintf(stderr, "odd-agy upstream check: %v\n", err)
			return exitError
		}
		fmt.Fprintf(stdout, "Upstream check (%s) lock=%s\n", rep.Source, rep.LockPath)
		printUpstreamGroup(stdout, "prompts-and-skills", rep.PromptsAndSkills)
		printUpstreamGroup(stdout, "enforcement-code", rep.EnforcementCode)
		fmt.Fprintf(stdout, "summary: %d unchanged, %d changed, %d missing\n",
			rep.UnchangedCount, rep.ChangedCount, rep.MissingCount)
		if rep.ChangedCount > 0 || rep.MissingCount > 0 {
			return exitError
		}
		return exitOK

	case "sync":
		rep, err := upstream.Sync(context.Background(), lockPath, upstream.SyncOptions{
			SourceDir: sourceDir,
			Ref:       ref,
		})
		if err != nil {
			fmt.Fprintf(stderr, "odd-agy upstream sync: %v\n", err)
			return exitError
		}
		fmt.Fprintf(stdout, "upstream sync complete: %d updated, %d unchanged, validated=%t (%s)\n",
			rep.UpdatedCount, rep.UnchangedCount, rep.Validated, rep.LockPath)
		return exitOK

	default:
		fmt.Fprintf(stderr, "odd-agy upstream: unknown subcommand %q (expected \"check\" or \"sync\")\n", sub)
		return exitUsage
	}
}

func printUpstreamGroup(w io.Writer, label string, entries []upstream.EntryResult) {
	fmt.Fprintf(w, "[%s]\n", label)
	for _, e := range entries {
		detail := e.Path
		if len(e.ChangedFiles) > 0 || len(e.MissingFiles) > 0 {
			detail = fmt.Sprintf("%s (changed=%v missing=%v)", e.Path, e.ChangedFiles, e.MissingFiles)
		}
		fmt.Fprintf(w, "  %-10s %s:%s\n", "["+e.Status+"]", e.Repo, detail)
	}
}

func sanitizeProviderErr(p config.ProviderConfig, err error) string {
	if p.BaseURLEnv != "" {
		if val, ok := os.LookupEnv(p.BaseURLEnv); ok && strings.TrimSpace(val) != "" {
			return fmt.Sprintf("invalid %s (%s)", p.BaseURLEnv, llm.RedactURL(val))
		}
	}
	return fmt.Sprintf("invalid endpoint (%v)", err)
}

func printHelp(w io.Writer) {
	fmt.Fprintf(w, `odd-agy %s - Project-scoped ODD & RDD harness for Antigravity CLI/IDE

Usage:
  odd-agy [--cwd <dir>] [<command> [args]]

Commands:
  ui                                        Launch interactive Bubble Tea TUI (default when no command is given)
  init [--mode=local|standalone]            Install .agents/ rules, agents, skills, hooks, and MCP config
       [--delegation=full-delegation|organic]
       [--profile=agy-oauth] [--binary=...] [--allow-mcp] [--force]
  launch [--agent=<name>] [--no-agent] [-- <extra agy args...>]
                                            Launch Antigravity CLI with project-scoped .agents/odd/agy-session-settings.json
  permissions [status|allow-mcp|revoke-mcp] Manage optional global ~/.gemini/antigravity-cli/settings.json MCP rule
  sync [--binary=...] [--force]             Refresh managed .agents/ files against manifest
  doctor                                    Run workspace, CLI, provider, and collision diagnostics
  uninstall                                 Remove managed .agents/ files and hook/MCP entries
  mode [status|full-delegation|organic]     View or set delegation mode
  rdd [status|enable|disable]               View or toggle frozen-snapshot RDD
  profile [list|show|use <name>|providers|test <provider>]
                                            Manage model routing profiles and probe providers
  hook <SessionStart|PreToolUse|PostToolUse|PreInvocation|Stop>
                                            Dispatch an Antigravity lifecycle hook via stdin/stdout JSON
  mcp                                       Run the unified stdio MCP server (todo, RDD, external runner)
  upstream check [--lock=...] [--source-dir=...] [--ref=...]
                                            Check tracked Gentle Shell/AI upstream drift
  upstream sync [--ref=...] [--lock=...] [--source-dir=...]
                                            Sync upstream lock digests and validate embedded contracts
  version, --version                        Print version information
  help, --help                              Show this help message
`, version.Version)
}
