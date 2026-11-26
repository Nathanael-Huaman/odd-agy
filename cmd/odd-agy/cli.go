package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/Nathanael-Huaman/odd-agy/internal/config"
	"github.com/Nathanael-Huaman/odd-agy/internal/version"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

var stubSubcommands = map[string]bool{
	"init":      true,
	"sync":      true,
	"doctor":    true,
	"uninstall": true,
	"hook":      true,
	"mcp":       true,
	"ui":        true,
	"upstream":  true,
}

func Run(args []string, stdout, stderr io.Writer, cwd string) int {
	pos, targetCwd, err := extractCwdFlag(args, cwd)
	if err != nil {
		fmt.Fprintf(stderr, "odd-agy: %v\n", err)
		return exitUsage
	}

	if len(pos) == 0 {
		printHelp(stdout)
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
	case "mode":
		return runMode(rest, stdout, stderr, targetCwd)
	case "rdd":
		return runRDD(rest, stdout, stderr, targetCwd)
	case "profile":
		return runProfile(rest, stdout, stderr, targetCwd)
	default:
		if stubSubcommands[cmd] {
			fmt.Fprintf(stderr, "odd-agy %s: not implemented yet\n", cmd)
			return exitUsage
		}
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
		if err := config.SaveProfiles(cwd, cfg); err != nil {
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
		for _, name := range names {
			p := cfg.Providers[name]
			ep, err := config.ResolveProviderEndpoint(p, os.LookupEnv)
			if err != nil {
				fmt.Fprintf(stdout, "%-18s invalid (%v)\n", name, err)
				continue
			}
			fmt.Fprintf(stdout, "%-18s url=%s compat=%s keyEnv=%s keyStatus=%s defaultModel=%s\n",
				name, ep.BaseURL, ep.Compatibility, p.APIKeyEnv, ep.KeySource, p.DefaultModel)
		}
		return exitOK

	default:
		fmt.Fprintf(stderr, "odd-agy profile: unknown subcommand %q (expected \"list\", \"show\", \"use\", or \"providers\")\n", sub)
		return exitUsage
	}
}

func printHelp(w io.Writer) {
	fmt.Fprintf(w, `odd-agy %s - Project-scoped ODD & RDD harness for Antigravity CLI/IDE

Usage:
  odd-agy [--cwd <dir>] <command> [args]

Commands:
  mode [status|full-delegation|organic]   View or set delegation mode
  rdd [status|enable|disable]             View or toggle frozen-snapshot RDD
  profile [list|show|use <name>|providers] Manage model routing profiles
  version, --version                      Print version information
  help, --help                            Show this help message

Planned commands (implemented in later slices):
  init, sync, doctor, uninstall, hook, mcp, ui, upstream
`, version.Version)
}
