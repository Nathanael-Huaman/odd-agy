package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"

	"github.com/Nathanael-Huaman/odd-agy/internal/config"
	"github.com/Nathanael-Huaman/odd-agy/internal/installer"
	"github.com/Nathanael-Huaman/odd-agy/internal/llm"
	"github.com/Nathanael-Huaman/odd-agy/internal/version"
)

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	mutedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	focusStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	warnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	errStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
)

// View implements tea.Model.
func (m Model) View() string {
	var b strings.Builder

	installLabel := "not initialized"
	if m.installed {
		installLabel = "installed"
	}
	b.WriteString(titleStyle.Render(fmt.Sprintf("odd-agy v%s", version.Version)))
	b.WriteString("  ")
	b.WriteString(mutedStyle.Render(m.cwd))
	b.WriteByte('\n')
	b.WriteString(fmt.Sprintf("Status: %s | Delegation: %s | RDD: %s | Profile: %s\n\n",
		installLabel, m.cfg.DelegationMode, boolOnOff(m.cfg.RDDEnabled), m.profiles.ActiveProfile))

	switch m.screen {
	case screenOverview:
		m.renderOverview(&b)
	case screenProfiles:
		m.renderProfiles(&b)
	case screenRoles:
		m.renderRoles(&b)
	case screenModelPicker:
		m.renderModelPicker(&b)
	case screenEffortPicker:
		m.renderEffortPicker(&b)
	case screenProviders:
		m.renderProviders(&b)
	case screenActions:
		m.renderActions(&b)
	}

	if m.inputMode != inputNone {
		b.WriteByte('\n')
		if m.inputMode == inputProviderKey {
			keyEnv := m.profiles.Providers[m.inputTarget].APIKeyEnv
			masked := strings.Repeat("•", utf8.RuneCountInString(m.inputBuffer))
			b.WriteString(focusStyle.Render(fmt.Sprintf("Set API key (%s) [empty to clear]: %s_", keyEnv, masked)))
		} else {
			b.WriteString(focusStyle.Render(fmt.Sprintf("Edit %s (%s): %s_", m.inputLabel(), m.inputTarget, m.inputBuffer)))
		}
		b.WriteByte('\n')
		if m.inputMode == inputRoleModel {
			if prof, ok := m.profiles.Profiles[m.profiles.ActiveProfile]; ok {
				if route, ok := prof.Roles[m.inputTarget]; ok && route.Runtime == config.RuntimeAgyNative {
					b.WriteString(mutedStyle.Render("Antigravity models: inherit, flash, gemini-3.8-flash-high, gemini-3.8-flash-medium, gemini-3.8-flash-low, gemini-4-hydrogen-high, claude-sonnet-4-6, claude-opus-4-6-thinking"))
					b.WriteByte('\n')
				}
			}
		}
		b.WriteString(mutedStyle.Render("[enter] save  [esc] cancel"))
		b.WriteByte('\n')
	}

	if m.confirmUninstall {
		b.WriteByte('\n')
		b.WriteString(warnStyle.Render("Confirm uninstall managed .agents/ files? Press [y] to uninstall or [n/esc] to cancel."))
		b.WriteByte('\n')
	}

	if m.errMsg != "" {
		b.WriteByte('\n')
		b.WriteString(errStyle.Render("Error: " + m.errMsg))
		b.WriteByte('\n')
	} else if m.statusMsg != "" {
		b.WriteByte('\n')
		b.WriteString(okStyle.Render(m.statusMsg))
		b.WriteByte('\n')
	}

	return b.String()
}

func (m Model) inputLabel() string {
	switch m.inputMode {
	case inputRoleModel:
		return "role model"
	case inputProviderURL:
		return "provider baseUrl"
	case inputProviderKeyEnv:
		return "provider apiKeyEnv"
	case inputProviderModel:
		return "provider defaultModel"
	case inputProviderKey:
		return "provider API key"
	default:
		return "value"
	}
}

func (m Model) renderOverview(b *strings.Builder) {
	items := []string{
		fmt.Sprintf("Toggle Delegation Mode (%s)", m.cfg.DelegationMode),
		fmt.Sprintf("Toggle RDD (%s)", boolOnOff(m.cfg.RDDEnabled)),
		fmt.Sprintf("Active Profile (%s)", m.profiles.ActiveProfile),
		fmt.Sprintf("Roles Editor (%d roles)", len(config.AllRoles)),
		"Providers & Probe",
		"Project Actions (Init / Sync / Doctor / Uninstall)",
	}
	b.WriteString("Main Menu:\n")
	for i, item := range items {
		b.WriteString(renderCursorLine(i == m.cursor, item))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(mutedStyle.Render("[up/down or j/k] move  [enter/space] select  [r] reload  [q] quit"))
	b.WriteByte('\n')
}

func (m Model) renderProfiles(b *strings.Builder) {
	b.WriteString("Select Active Profile:\n")
	for i, name := range m.profileNames {
		marker := " "
		if name == m.profiles.ActiveProfile {
			marker = "*"
		}
		desc := m.profiles.Profiles[name].Description
		line := fmt.Sprintf("%s %s", marker, name)
		if desc != "" {
			line += " — " + desc
		}
		b.WriteString(renderCursorLine(i == m.cursor, line))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(mutedStyle.Render("[up/down] move  [enter/space] activate  [esc] back  [q] quit"))
	b.WriteByte('\n')
}

func (m Model) renderRoles(b *strings.Builder) {
	prof := m.profiles.Profiles[m.profiles.ActiveProfile]
	b.WriteString(fmt.Sprintf("Roles Editor (profile: %s):\n", m.profiles.ActiveProfile))
	for i, role := range config.AllRoles {
		route := prof.Roles[role]
		prov := route.Provider
		if prov == "" {
			prov = "-"
		}
		effort := route.Effort
		if effort == "" {
			effort = "default"
		}
		line := fmt.Sprintf("%-18s %-12s %-18s %-26s %s", role, route.Runtime, prov, route.Model, effort)
		b.WriteString(renderCursorLine(i == m.cursor, line))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(mutedStyle.Render("[t] toggle runtime  [p] cycle provider  [m] model  [e] effort  [esc] back  [q] quit"))
	b.WriteByte('\n')
}

func (m Model) renderModelPicker(b *strings.Builder) {
	b.WriteString(fmt.Sprintf("Select Model for %s (%s):\n", m.pickerRole, m.profiles.ActiveProfile))
	var catalog []AgyModelOption
	if m.pickerRole == config.RoleOrchestrator {
		catalog = m.agyModels
		if !m.agyModelsLoaded {
			catalog = fallbackAgyModels
		}
	}
	opts := m.buildModelPickerOptions(catalog)
	for i, opt := range opts {
		var line string
		if opt.isCustom {
			line = opt.label
		} else if opt.label != "" {
			line = fmt.Sprintf("%-26s %s", opt.id, opt.label)
		} else {
			line = opt.id
		}
		b.WriteString(renderCursorLine(i == m.cursor, line))
		b.WriteByte('\n')
	}
	if m.pickerRole != config.RoleOrchestrator {
		b.WriteByte('\n')
		b.WriteString(mutedStyle.Render("Antigravity CLI native subagents accept inherit, flash, or pro (switch runtime with [t] to external-mcp for exact per-subagent models)."))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(mutedStyle.Render("[up/down] move  [enter] select  [esc] cancel  [q] quit"))
	b.WriteByte('\n')
}

func (m Model) renderEffortPicker(b *strings.Builder) {
	b.WriteString(fmt.Sprintf("Select Effort for %s (%s):\n", m.pickerRole, m.profiles.ActiveProfile))
	for i, opt := range effortPickerOptions {
		b.WriteString(renderCursorLine(i == m.cursor, opt.label))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(mutedStyle.Render("[up/down] move  [enter] select  [esc] cancel  [q] quit"))
	b.WriteByte('\n')
}

func (m Model) renderProviders(b *strings.Builder) {
	b.WriteString("Providers & Live Probe:\n")
	for i, name := range m.providerNames {
		p := m.profiles.Providers[name]
		ep, err := config.ResolveProviderEndpoint(p, m.opts.LookupEnv)

		redactedURL := llm.RedactURL(p.BaseURL)
		keyStatus := "unset"
		if err != nil {
			keyStatus = "invalid-url"
		} else {
			redactedURL = llm.RedactURL(ep.BaseURL)
			_, src := config.LookupSecretWithEnv(m.opts.HomeDir, p.APIKeyEnv, m.opts.LookupEnv)
			switch src {
			case "env":
				keyStatus = "env"
			case "stored":
				keyStatus = "stored"
			default:
				switch ep.KeySource {
				case "loopback-optional", "loopback-placeholder":
					keyStatus = ep.KeySource
				default:
					keyStatus = "unset"
				}
			}
		}

		envLabel := p.APIKeyEnv
		if envLabel == "" {
			envLabel = "-"
		}
		line := fmt.Sprintf("%-18s url=%s keyEnv=%s(%s) model=%s", name, redactedURL, envLabel, keyStatus, p.DefaultModel)
		if res, ok := m.probeResults[name]; ok {
			if res.Reachable {
				line += fmt.Sprintf(" [reachable HTTP %d %dms]", res.StatusCode, res.LatencyMS)
			} else {
				line += fmt.Sprintf(" [unreachable: %s]", res.Error)
			}
		}
		b.WriteString(renderCursorLine(i == m.cursor, line))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(mutedStyle.Render("[p/enter] probe  [t] test  [s] set key  [u] edit baseUrl  [k] edit apiKeyEnv  [m] edit model  [esc] back  [q] quit"))
	b.WriteByte('\n')
}

func (m Model) renderActions(b *strings.Builder) {
	globalMCPAction := "Allow MCP globally (~/.gemini)"
	if m.globalMCPAllowed && m.globalMCPRegistered {
		globalMCPAction = "Revoke global MCP rule & registration"
	}
	actions := []string{
		"Init (.agents/)",
		"Sync",
		"Doctor",
		"Uninstall",
		globalMCPAction,
	}
	b.WriteString("Project Lifecycle Actions:\n")
	for i, act := range actions {
		b.WriteString(renderCursorLine(i == m.cursor, act))
		b.WriteByte('\n')
	}

	b.WriteByte('\n')
	permState := "disabled"
	if m.globalMCPAllowed {
		permState = "enabled"
	}
	regState := "not registered"
	if m.globalMCPRegistered {
		regState = fmt.Sprintf("registered (command=%s)", m.globalMCPCommand)
	}
	b.WriteString("Global ~/.gemini/ Status:\n")
	b.WriteString(fmt.Sprintf("  Permissions (%s): %s\n", installer.GlobalSettingsPath(m.opts.HomeDir), permState))
	b.WriteString(fmt.Sprintf("  MCP server  (%s): %s\n", installer.GlobalMCPConfigPath(m.opts.HomeDir), regState))

	if m.doctorReport != nil {
		b.WriteByte('\n')
		b.WriteString("Doctor Report:\n")
		for _, c := range m.doctorReport.Checks {
			b.WriteString(fmt.Sprintf("  %-6s %-18s %s\n", "["+c.Status+"]", c.Name, c.Detail))
		}
	}

	b.WriteByte('\n')
	b.WriteString(mutedStyle.Render("[up/down] move  [enter/space] run  [esc] back  [q] quit"))
	b.WriteByte('\n')
}

func renderCursorLine(selected bool, text string) string {
	if selected {
		return focusStyle.Render("> " + text)
	}
	return "  " + text
}
