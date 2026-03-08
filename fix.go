package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Fix mode phases
type fixPhase int

const (
	fixSelectRun fixPhase = iota
	fixFetchLogs
	fixAnalyzing
	fixSummary
	fixInput
	fixFixing
	fixDiffReview
	fixCommitting
	fixDone
	fixError
)

type failedRun struct {
	ID        int       `json:"databaseId"`
	Name      string    `json:"name"`
	HeadSha   string    `json:"headSha"`
	CreatedAt time.Time `json:"createdAt"`
}

// Fix model

type fixModel struct {
	phase    fixPhase
	pr       *ghPR
	prRef    string
	interval interface{} // unused, kept for bridge
	fromList bool
	ls       *listState

	runs   []failedRun
	cursor int

	logs     string
	analysis string
	diff     string
	errMsg   string
	input    string

	back bool
}

// Messages

type fixRunsMsg struct{ runs []failedRun }
type fixLogsMsg struct{ logs string }
type fixAnalysisMsg struct{ analysis string }
type fixFixedMsg struct{}
type fixDiffMsg struct{ diff string }
type fixCommittedMsg struct{}
type fixResetMsg struct{}
type fixErrMsg struct{ err string }

// Commands

func fetchFailedRuns(branch string) tea.Cmd {
	return func() tea.Msg {
		out, err := exec.Command("gh", "run", "list",
			"--branch", branch,
			"--status", "failure",
			"--limit", "5",
			"--json", "databaseId,name,headSha,createdAt",
		).Output()
		if err != nil {
			return fixErrMsg{err: fmt.Sprintf("failed to list runs: %s", cmdError(err))}
		}

		var runs []failedRun
		if err := json.Unmarshal(out, &runs); err != nil {
			return fixErrMsg{err: fmt.Sprintf("failed to parse runs: %v", err)}
		}
		if len(runs) == 0 {
			return fixErrMsg{err: "no failed runs found"}
		}

		// Keep only the latest run per workflow name (list is newest-first)
		seen := map[string]bool{}
		var deduped []failedRun
		for _, r := range runs {
			if !seen[r.Name] {
				seen[r.Name] = true
				deduped = append(deduped, r)
			}
		}

		return fixRunsMsg{runs: deduped}
	}
}

func fetchFailedLogs(runID int) tea.Cmd {
	return func() tea.Msg {
		out, err := exec.Command("gh", "run", "view",
			fmt.Sprintf("%d", runID),
			"--log-failed",
		).Output()
		if err != nil {
			return fixErrMsg{err: fmt.Sprintf("failed to fetch logs: %s", cmdError(err))}
		}

		logs := string(out)
		if strings.TrimSpace(logs) == "" {
			return fixErrMsg{err: "no failure logs found"}
		}

		// Tail-preserving truncation to 80KB
		const maxBytes = 80 * 1024
		if len(logs) > maxBytes {
			logs = logs[len(logs)-maxBytes:]
		}

		return fixLogsMsg{logs: logs}
	}
}

func analyzeWithClaude(logs string) tea.Cmd {
	return func() tea.Msg {
		prompt := "Summarize these CI failure logs concisely. Focus on the root cause and what needs to be fixed:\n\n" + logs
		out, err := exec.Command("claude", "-p", prompt).Output()
		if err != nil {
			return fixErrMsg{err: fmt.Sprintf("claude analyze failed: %s", cmdError(err))}
		}
		return fixAnalysisMsg{analysis: strings.TrimSpace(string(out))}
	}
}

func fixWithClaude(analysis, logs, instructions string) tea.Cmd {
	return func() tea.Msg {
		prompt := ""
		if instructions != "" {
			prompt = "Additional instructions from user: " + instructions + "\n\n"
		}
		prompt += fmt.Sprintf("Fix the CI failures described below. Edit the files directly.\n\nAnalysis:\n%s\n\nLogs:\n%s", analysis, logs)
		_, err := exec.Command("claude", "-p", prompt).Output()
		if err != nil {
			return fixErrMsg{err: fmt.Sprintf("claude fix failed: %s", cmdError(err))}
		}
		return fixFixedMsg{}
	}
}

func fetchDiff() tea.Cmd {
	return func() tea.Msg {
		out, err := exec.Command("git", "diff", "--stat").Output()
		if err != nil {
			return fixErrMsg{err: fmt.Sprintf("git diff failed: %s", cmdError(err))}
		}
		diff := strings.TrimSpace(string(out))

		// Also check for untracked files
		untrackedOut, _ := exec.Command("git", "ls-files", "--others", "--exclude-standard").Output()
		untracked := strings.TrimSpace(string(untrackedOut))

		if diff == "" && untracked == "" {
			return fixErrMsg{err: "claude made no changes"}
		}

		if untracked != "" {
			if diff != "" {
				diff += "\n"
			}
			for _, f := range strings.Split(untracked, "\n") {
				if f != "" {
					diff += fmt.Sprintf(" %s (new file)\n", f)
				}
			}
		}

		return fixDiffMsg{diff: diff}
	}
}

func commitAndPush(branch string) tea.Cmd {
	return func() tea.Msg {
		if out, err := exec.Command("git", "add", "-A").CombinedOutput(); err != nil {
			return fixErrMsg{err: fmt.Sprintf("git add: %s", firstLine(out, err))}
		}
		if out, err := exec.Command("git", "commit", "--no-verify", "-m", "fix(ci): address failures").CombinedOutput(); err != nil {
			return fixErrMsg{err: fmt.Sprintf("git commit: %s", firstLine(out, err))}
		}
		if out, err := exec.Command("git", "push").CombinedOutput(); err != nil {
			return fixErrMsg{err: fmt.Sprintf("git push: %s", firstLine(out, err))}
		}
		return fixCommittedMsg{}
	}
}

func gitReset() tea.Cmd {
	return func() tea.Msg {
		exec.Command("git", "checkout", ".").Run()
		exec.Command("git", "clean", "-fd").Run()
		return fixResetMsg{}
	}
}

// Init

func (m fixModel) Init() tea.Cmd {
	return fetchFailedRuns(m.pr.HeadRefName)
}

// Update

func (m fixModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			if m.phase == fixFixing || m.phase == fixCommitting {
				return m, nil // don't interrupt ongoing operations
			}
			return m, gitReset()
		case "esc":
			switch m.phase {
			case fixSelectRun:
				m.back = true
				return m, tea.Quit
			case fixSummary:
				m.back = true
				return m, tea.Quit
			case fixInput:
				m.input = ""
				m.phase = fixSummary
				return m, nil
			case fixDiffReview:
				return m, gitReset()
			case fixError:
				m.back = true
				return m, tea.Quit
			}
		case "up", "k":
			if m.phase == fixSelectRun && m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.phase == fixSelectRun && m.cursor < len(m.runs)-1 {
				m.cursor++
			}
		case "enter":
			if m.phase == fixSelectRun {
				m.phase = fixFetchLogs
				return m, fetchFailedLogs(m.runs[m.cursor].ID)
			}
			if m.phase == fixInput {
				m.phase = fixFixing
				return m, fixWithClaude(m.analysis, m.logs, m.input)
			}
		case "backspace", "ctrl+h":
			if m.phase == fixInput && len(m.input) > 0 {
				runes := []rune(m.input)
				m.input = string(runes[:len(runes)-1])
				return m, nil
			}
		case "f":
			if m.phase == fixSummary {
				m.phase = fixFixing
				return m, fixWithClaude(m.analysis, m.logs, "")
			}
		case "i":
			if m.phase == fixSummary {
				m.phase = fixInput
				m.input = ""
				return m, nil
			}
		case "y":
			if m.phase == fixDiffReview {
				m.phase = fixCommitting
				return m, commitAndPush(m.pr.HeadRefName)
			}
		case "n":
			if m.phase == fixDiffReview {
				return m, gitReset()
			}
		case "r":
			if m.phase == fixError {
				m.phase = fixFetchLogs
				m.errMsg = ""
				return m, fetchFailedRuns(m.pr.HeadRefName)
			}
		default:
			if m.phase == fixInput {
				if msg.Type == tea.KeyRunes {
					m.input += string(msg.Runes)
					return m, nil
				}
			}
		}

	case fixRunsMsg:
		m.runs = msg.runs
		if len(msg.runs) == 1 {
			m.phase = fixFetchLogs
			return m, fetchFailedLogs(msg.runs[0].ID)
		}
		m.phase = fixSelectRun
		return m, nil

	case fixLogsMsg:
		m.logs = msg.logs
		m.phase = fixAnalyzing
		return m, analyzeWithClaude(m.logs)

	case fixAnalysisMsg:
		m.analysis = msg.analysis
		m.phase = fixSummary
		return m, nil

	case fixFixedMsg:
		return m, fetchDiff()

	case fixDiffMsg:
		m.diff = msg.diff
		m.phase = fixDiffReview
		return m, nil

	case fixCommittedMsg:
		m.phase = fixDone
		m.back = true
		return m, tea.Quit

	case fixResetMsg:
		m.back = true
		return m, tea.Quit

	case fixErrMsg:
		m.errMsg = msg.err
		m.phase = fixError
		return m, nil
	}

	return m, nil
}

// View

func (m fixModel) View() string {
	var b strings.Builder

	b.WriteString(bold.Render(fmt.Sprintf("#%d", m.pr.Number)))
	b.WriteString(" " + m.pr.Title + " ")
	b.WriteString(dim.Render("(" + m.pr.HeadRefName + ")"))
	b.WriteString("\n\n")

	switch m.phase {
	case fixSelectRun:
		b.WriteString(" " + bold.Render("Failed runs:") + "\n")
		for i, run := range m.runs {
			cursor := "  "
			if i == m.cursor {
				cursor = " ▸"
			}
			sha := run.HeadSha
			if len(sha) > 7 {
				sha = sha[:7]
			}
			line := run.Name + " " + dim.Render(sha+" · "+relativeTime(run.CreatedAt))
			b.WriteString(cursor + " " + line + "\n")
		}
		b.WriteString("\n" + dim.Render(" ↑/↓ select · enter fix · esc back") + "\n")

	case fixFetchLogs:
		b.WriteString(dim.Render(" ◌ Fetching failure logs..."))
		b.WriteString("\n")

	case fixAnalyzing:
		b.WriteString(dim.Render(" ◌ Analyzing failures with Claude..."))
		b.WriteString("\n")

	case fixSummary:
		b.WriteString(" " + bold.Render("Analysis:") + "\n")
		for _, line := range strings.Split(m.analysis, "\n") {
			b.WriteString("   " + line + "\n")
		}
		b.WriteString("\n" + dim.Render(" f fix · i instructions · esc back") + "\n")

	case fixInput:
		b.WriteString(" " + bold.Render("Analysis:") + "\n")
		for _, line := range strings.Split(m.analysis, "\n") {
			b.WriteString("   " + line + "\n")
		}
		b.WriteString("\n " + bold.Render("Instructions: ") + m.input + "_\n")
		b.WriteString("\n" + dim.Render(" enter submit · esc cancel") + "\n")

	case fixFixing:
		b.WriteString(dim.Render(" ◌ Claude is fixing the code..."))
		b.WriteString("\n")

	case fixDiffReview:
		b.WriteString(" " + bold.Render("Changes:") + "\n")
		for _, line := range strings.Split(m.diff, "\n") {
			b.WriteString("   " + line + "\n")
		}
		b.WriteString("\n" + dim.Render(" y commit & push · n discard · esc discard") + "\n")

	case fixCommitting:
		b.WriteString(dim.Render(" ◌ Committing and pushing..."))
		b.WriteString("\n")

	case fixError:
		b.WriteString(" " + red.Render("Error: "+m.errMsg) + "\n")
		b.WriteString("\n" + dim.Render(" r retry · esc back") + "\n")
	}

	return b.String()
}

func firstLine(out []byte, err error) string {
	s := strings.TrimSpace(string(out))
	if s != "" {
		if i := strings.Index(s, "\n"); i > 0 {
			s = s[:i]
		}
		return s
	}
	return err.Error()
}

func relativeTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(math.Round(d.Minutes()))
		if m == 1 {
			return "1m ago"
		}
		return fmt.Sprintf("%dm ago", m)
	case d < 24*time.Hour:
		h := int(math.Round(d.Hours()))
		if h == 1 {
			return "1h ago"
		}
		return fmt.Sprintf("%dh ago", h)
	default:
		days := int(math.Round(d.Hours() / 24))
		if days == 1 {
			return "1d ago"
		}
		return fmt.Sprintf("%dd ago", days)
	}
}
