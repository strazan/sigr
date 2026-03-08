package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Heal mode phases
type healPhase int

const (
	healSetup healPhase = iota
	healWatching
	healFetchingRuns
	healFetchingLogs
	healAnalyzing
	healFixingCI
	healFixingComment
	healDiffCheck
	healCommitting
	healError
	healTeardown
)

type healLogEntry struct {
	time    time.Time
	message string
}

type healModel struct {
	phase       healPhase
	prRef       string
	pr          *ghPR
	checks      []check
	interval    time.Duration
	fromList    bool
	listState   *listState
	worktreeDir string
	worktreeOwn bool // true if we created it (cleanup on exit)
	startedAt   time.Time
	quitting    bool
	back        bool

	// Track addressed issues
	addressedRuns     map[int]string  // runID → headSha
	addressedComments map[string]bool // createdAt → true

	// Current work
	currentRunID         int
	currentRunSha        string
	currentLogs          string
	currentCommentAuthor string
	currentAction        string

	// Log
	log []healLogEntry
}

// Messages

type healTickMsg time.Time

type healWorktreeReadyMsg struct {
	dir     string
	created bool
}

type healWorktreeRemovedMsg struct{}
type healPRMsg struct{ pr *ghPR }
type healRunsMsg struct{ runs []failedRun }
type healLogsMsg struct{ logs string }
type healAnalysisMsg struct{ analysis string }
type healFixedMsg struct{}
type healDiffMsg struct{ diff string }
type healCommittedMsg struct{}
type healErrMsg struct{ err string }

// Worktree helpers

// findExistingWorktree checks git worktree list for a worktree on the given
// branch, skipping the current working tree.
func findExistingWorktree(branch string) string {
	out, err := exec.Command("git", "worktree", "list", "--porcelain").Output()
	if err != nil {
		return ""
	}

	currentOut, _ := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	current := strings.TrimSpace(string(currentOut))

	targetRef := "branch refs/heads/" + branch
	var path string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "worktree ") {
			path = strings.TrimPrefix(line, "worktree ")
		}
		if line == targetRef && path != current {
			return path
		}
	}
	return ""
}

// detectSigtree returns the sigtree root directory if the current repo is
// managed by sigtree (bare repo layout with .bare/ sibling), or "".
func detectSigtree() string {
	toplevel, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	top := strings.TrimSpace(string(toplevel))
	parent := filepath.Dir(top)
	bareDir := filepath.Join(parent, ".bare")
	if info, err := os.Stat(bareDir); err == nil && info.IsDir() {
		return parent
	}
	return ""
}

// repoName extracts the repository name from the origin remote URL.
func repoName() string {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "unknown"
	}
	url := strings.TrimSpace(string(out))
	url = strings.TrimSuffix(url, ".git")
	if i := strings.LastIndex(url, "/"); i >= 0 {
		return url[i+1:]
	}
	return url
}

// worktreeClean returns true if the worktree has no uncommitted changes.
func worktreeClean(dir string) bool {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == ""
}

// Commands

func healTickCmd(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg {
		return healTickMsg(t)
	})
}

func healSetupWorktree(branch string) tea.Cmd {
	return func() tea.Msg {
		// 1. Reuse existing worktree for this branch (sigtree or manual)
		if existing := findExistingWorktree(branch); existing != "" {
			if worktreeClean(existing) {
				return healWorktreeReadyMsg{dir: existing, created: false}
			}
			// Dirty worktree — fall through and create our own
		}

		exec.Command("git", "fetch", "origin", branch).Run()

		// 2. Sigtree layout: create as sibling directory
		if root := detectSigtree(); root != "" {
			dirName := strings.ReplaceAll(branch, "/", "-")
			dir := filepath.Join(root, dirName)
			os.MkdirAll(filepath.Dir(dir), 0755)

			if _, err := os.Stat(dir); err == nil {
				exec.Command("git", "worktree", "remove", "--force", dir).Run()
			}
			out, err := exec.Command("git", "worktree", "add", "-f", dir, branch).CombinedOutput()
			if err != nil {
				return healErrMsg{err: fmt.Sprintf("git worktree add: %s", strings.TrimSpace(string(out)))}
			}
			return healWorktreeReadyMsg{dir: dir, created: true}
		}

		// 3. Fallback: ~/.sigr/worktrees/<repo>/<branch>
		home, err := os.UserHomeDir()
		if err != nil {
			return healErrMsg{err: fmt.Sprintf("home dir: %v", err)}
		}
		dirName := strings.ReplaceAll(branch, "/", "-")
		dir := filepath.Join(home, ".sigr", "worktrees", repoName(), dirName)
		os.MkdirAll(filepath.Dir(dir), 0755)

		if _, err := os.Stat(dir); err == nil {
			exec.Command("git", "worktree", "remove", "--force", dir).Run()
		}
		out, err := exec.Command("git", "worktree", "add", "-f", dir, branch).CombinedOutput()
		if err != nil {
			return healErrMsg{err: fmt.Sprintf("git worktree add: %s", strings.TrimSpace(string(out)))}
		}
		return healWorktreeReadyMsg{dir: dir, created: true}
	}
}

func healTeardownWorktree(dir string, created bool) tea.Cmd {
	return func() tea.Msg {
		// Always reset uncommitted state
		cmd := exec.Command("git", "checkout", ".")
		cmd.Dir = dir
		cmd.Run()

		cmd = exec.Command("git", "clean", "-fd")
		cmd.Dir = dir
		cmd.Run()

		// Only remove worktrees we created
		if created {
			exec.Command("git", "worktree", "remove", "--force", dir).Run()
		}
		return healWorktreeRemovedMsg{}
	}
}

func healFetchPR(prRef string) tea.Cmd {
	return func() tea.Msg {
		args := []string{"pr", "view", "--json",
			"statusCheckRollup,number,title,headRefName,state,reviewDecision,reviewRequests,reviews,comments"}
		if prRef != "" {
			args = append(args, prRef)
		}
		out, err := exec.Command("gh", args...).Output()
		if err != nil {
			return healErrMsg{err: fmt.Sprintf("gh: %s", cmdError(err))}
		}
		var pr ghPR
		if err := json.Unmarshal(out, &pr); err != nil {
			return healErrMsg{err: fmt.Sprintf("parse PR: %v", err)}
		}
		return healPRMsg{pr: &pr}
	}
}

func healFetchRuns(branch string) tea.Cmd {
	return func() tea.Msg {
		msg := fetchFailedRuns(branch)()
		switch msg := msg.(type) {
		case fixRunsMsg:
			return healRunsMsg{runs: msg.runs}
		case fixErrMsg:
			return healErrMsg{err: msg.err}
		default:
			return msg
		}
	}
}

func healFetchLogs(runID int) tea.Cmd {
	return func() tea.Msg {
		msg := fetchFailedLogs(runID)()
		switch msg := msg.(type) {
		case fixLogsMsg:
			return healLogsMsg{logs: msg.logs}
		case fixErrMsg:
			return healErrMsg{err: msg.err}
		default:
			return msg
		}
	}
}

func healAnalyze(logs string) tea.Cmd {
	return func() tea.Msg {
		msg := analyzeWithClaude(logs)()
		switch msg := msg.(type) {
		case fixAnalysisMsg:
			return healAnalysisMsg{analysis: msg.analysis}
		case fixErrMsg:
			return healErrMsg{err: msg.err}
		default:
			return msg
		}
	}
}

func healFixCI(analysis, logs, dir string) tea.Cmd {
	return func() tea.Msg {
		prompt := fmt.Sprintf("Fix the CI failures described below. Edit the files directly.\n\nAnalysis:\n%s\n\nLogs:\n%s", analysis, logs)
		cmd := exec.Command("claude", "-p", prompt)
		cmd.Dir = dir
		if _, err := cmd.Output(); err != nil {
			return healErrMsg{err: fmt.Sprintf("claude fix failed: %s", cmdError(err))}
		}
		return healFixedMsg{}
	}
}

func healFixComment(comment, dir string) tea.Cmd {
	return func() tea.Msg {
		prompt := "Fix the issue described in this PR comment. Edit the files directly.\n\nComment:\n" + comment
		cmd := exec.Command("claude", "-p", prompt)
		cmd.Dir = dir
		if _, err := cmd.Output(); err != nil {
			return healErrMsg{err: fmt.Sprintf("claude fix failed: %s", cmdError(err))}
		}
		return healFixedMsg{}
	}
}

func healFetchDiff(dir string) tea.Cmd {
	return func() tea.Msg {
		cmd := exec.Command("git", "diff", "--stat")
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			return healErrMsg{err: fmt.Sprintf("git diff failed: %s", cmdError(err))}
		}
		diff := strings.TrimSpace(string(out))

		cmd = exec.Command("git", "ls-files", "--others", "--exclude-standard")
		cmd.Dir = dir
		untrackedOut, _ := cmd.Output()
		untracked := strings.TrimSpace(string(untrackedOut))

		if diff == "" && untracked == "" {
			return healErrMsg{err: "claude made no changes"}
		}

		result := diff
		if untracked != "" {
			if result != "" {
				result += "\n"
			}
			for _, f := range strings.Split(untracked, "\n") {
				if f != "" {
					result += fmt.Sprintf(" %s (new file)\n", f)
				}
			}
		}
		return healDiffMsg{diff: result}
	}
}

func healCommitAndPush(branch, message, dir string) tea.Cmd {
	return func() tea.Msg {
		cmd := exec.Command("git", "add", "-A")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return healErrMsg{err: fmt.Sprintf("git add: %s", firstLine(out, err))}
		}

		cmd = exec.Command("git", "commit", "--no-verify", "-m", message)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return healErrMsg{err: fmt.Sprintf("git commit: %s", firstLine(out, err))}
		}

		cmd = exec.Command("git", "push")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return healErrMsg{err: fmt.Sprintf("git push: %s", firstLine(out, err))}
		}

		return healCommittedMsg{}
	}
}

func healResetWorktree(dir string) {
	cmd := exec.Command("git", "checkout", ".")
	cmd.Dir = dir
	cmd.Run()

	cmd = exec.Command("git", "clean", "-fd")
	cmd.Dir = dir
	cmd.Run()
}

// Init

func (m healModel) Init() tea.Cmd {
	return healSetupWorktree(m.pr.HeadRefName)
}

// Update

func (m healModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			if m.phase == healFixingCI || m.phase == healFixingComment || m.phase == healCommitting {
				return m, nil
			}
			m.quitting = true
			if m.worktreeDir != "" {
				m.phase = healTeardown
				return m, healTeardownWorktree(m.worktreeDir, m.worktreeOwn)
			}
			return m, tea.Quit
		case "esc":
			if m.phase == healFixingCI || m.phase == healFixingComment || m.phase == healCommitting {
				return m, nil
			}
			m.quitting = true
			m.back = true
			if m.worktreeDir != "" {
				m.phase = healTeardown
				return m, healTeardownWorktree(m.worktreeDir, m.worktreeOwn)
			}
			return m, tea.Quit
		}

	case healWorktreeReadyMsg:
		m.worktreeDir = msg.dir
		m.worktreeOwn = msg.created
		m.phase = healWatching
		if msg.created {
			m.log = append(m.log, healLogEntry{time: time.Now(), message: "Created worktree at " + msg.dir})
		} else {
			m.log = append(m.log, healLogEntry{time: time.Now(), message: "Using existing worktree at " + msg.dir})
		}
		return m, tea.Batch(healFetchPR(m.prRef), healTickCmd(m.interval))

	case healWorktreeRemovedMsg:
		return m, tea.Quit

	case healTickMsg:
		if m.phase == healWatching {
			return m, tea.Batch(healFetchPR(m.prRef), healTickCmd(m.interval))
		}
		return m, healTickCmd(m.interval)

	case healPRMsg:
		m.pr = msg.pr
		m.checks = parseChecks(msg.pr.StatusCheckRollup)

		if m.phase != healWatching {
			return m, nil
		}

		// 1. Failed checks → fetch runs
		hasFailure := false
		for _, c := range m.checks {
			if c.Status == "COMPLETED" && !isSuccess(c.Conclusion) {
				hasFailure = true
				break
			}
		}

		if hasFailure {
			m.phase = healFetchingRuns
			m.currentAction = "Fetching failed runs"
			m.log = append(m.log, healLogEntry{time: time.Now(), message: "Failed checks detected"})
			return m, healFetchRuns(m.pr.HeadRefName)
		}

		// 2. New unaddressed comments
		for _, c := range m.pr.Comments {
			if c.CreatedAt > m.startedAt.Format(time.RFC3339) && !m.addressedComments[c.CreatedAt] {
				m.phase = healFixingComment
				m.currentCommentAuthor = c.Author.Login
				m.currentAction = fmt.Sprintf("Fixing comment from @%s", c.Author.Login)
				m.addressedComments[c.CreatedAt] = true
				m.log = append(m.log, healLogEntry{time: time.Now(), message: fmt.Sprintf("Addressing comment from @%s", c.Author.Login)})
				return m, healFixComment(c.Body, m.worktreeDir)
			}
		}

		return m, nil

	case healRunsMsg:
		for _, run := range msg.runs {
			if sha, ok := m.addressedRuns[run.ID]; !ok || sha != run.HeadSha {
				m.currentRunID = run.ID
				m.currentRunSha = run.HeadSha
				m.phase = healFetchingLogs
				m.currentAction = "Fetching logs for " + run.Name
				m.log = append(m.log, healLogEntry{time: time.Now(), message: "Fetching logs for " + run.Name})
				return m, healFetchLogs(run.ID)
			}
		}
		// All runs already addressed
		m.phase = healWatching
		m.currentAction = ""
		return m, nil

	case healLogsMsg:
		m.currentLogs = msg.logs
		m.phase = healAnalyzing
		m.currentAction = "Analyzing failures"
		m.log = append(m.log, healLogEntry{time: time.Now(), message: "Analyzing failures..."})
		return m, healAnalyze(m.currentLogs)

	case healAnalysisMsg:
		m.phase = healFixingCI
		m.currentAction = "Fixing CI failures"
		m.log = append(m.log, healLogEntry{time: time.Now(), message: "Claude is fixing..."})
		return m, healFixCI(msg.analysis, m.currentLogs, m.worktreeDir)

	case healFixedMsg:
		m.phase = healDiffCheck
		m.currentAction = "Checking changes"
		return m, healFetchDiff(m.worktreeDir)

	case healDiffMsg:
		m.phase = healCommitting
		commitMsg := "fix(ci): autofix failures"
		if m.currentCommentAuthor != "" {
			commitMsg = fmt.Sprintf("fix: address comment from @%s", m.currentCommentAuthor)
		}
		m.currentAction = "Committing and pushing"
		m.log = append(m.log, healLogEntry{time: time.Now(), message: "Committing: " + commitMsg})
		return m, healCommitAndPush(m.pr.HeadRefName, commitMsg, m.worktreeDir)

	case healCommittedMsg:
		if m.currentRunID != 0 {
			m.addressedRuns[m.currentRunID] = m.currentRunSha
		}
		m.log = append(m.log, healLogEntry{time: time.Now(), message: "Pushed successfully"})
		m.currentRunID = 0
		m.currentRunSha = ""
		m.currentLogs = ""
		m.currentCommentAuthor = ""
		m.currentAction = ""
		m.phase = healWatching
		return m, nil

	case healErrMsg:
		m.log = append(m.log, healLogEntry{time: time.Now(), message: "Error: " + msg.err})

		// If no worktree yet, can't recover
		if m.worktreeDir == "" {
			m.phase = healError
			return m, nil
		}

		// Mark run as addressed if we already attempted fix (avoid retry loops)
		if m.currentRunID != 0 && (m.phase == healFixingCI || m.phase == healDiffCheck || m.phase == healCommitting) {
			m.addressedRuns[m.currentRunID] = m.currentRunSha
		}

		healResetWorktree(m.worktreeDir)
		m.currentRunID = 0
		m.currentRunSha = ""
		m.currentLogs = ""
		m.currentCommentAuthor = ""
		m.currentAction = ""
		m.phase = healWatching
		return m, nil
	}

	return m, nil
}

// View

func (m healModel) View() string {
	if m.quitting && m.phase != healTeardown {
		return ""
	}

	var b strings.Builder

	if m.pr != nil {
		b.WriteString(bold.Render(fmt.Sprintf("#%d", m.pr.Number)))
		b.WriteString(" " + m.pr.Title + " ")
		b.WriteString(dim.Render("(" + m.pr.HeadRefName + ")"))
	}
	b.WriteString("  " + yellow.Render("HEAL MODE"))
	b.WriteString("\n\n")

	// Status
	switch m.phase {
	case healSetup:
		b.WriteString(dim.Render(" ◌ Setting up worktree..."))
	case healWatching:
		b.WriteString(green.Render(" ● Watching") + dim.Render(fmt.Sprintf(" (polling every %s)", m.interval)))
	case healFetchingRuns:
		b.WriteString(dim.Render(" ◌ Fetching failed runs..."))
	case healFetchingLogs:
		b.WriteString(dim.Render(" ◌ Fetching failure logs..."))
	case healAnalyzing:
		b.WriteString(dim.Render(" ◌ Analyzing failures..."))
	case healFixingCI:
		b.WriteString(dim.Render(" ◌ Claude is fixing CI failures..."))
	case healFixingComment:
		b.WriteString(dim.Render(" ◌ Claude is addressing comment..."))
	case healDiffCheck:
		b.WriteString(dim.Render(" ◌ Checking changes..."))
	case healCommitting:
		b.WriteString(dim.Render(" ◌ Committing and pushing..."))
	case healTeardown:
		b.WriteString(dim.Render(" ◌ Cleaning up..."))
	case healError:
		b.WriteString(red.Render(" ● Error"))
	}
	b.WriteString("\n")

	// Addressed counts
	ciCount := len(m.addressedRuns)
	commentCount := len(m.addressedComments)
	if ciCount > 0 || commentCount > 0 {
		b.WriteString("\n")
		if ciCount > 0 {
			b.WriteString(fmt.Sprintf(" CI fixes: %d\n", ciCount))
		}
		if commentCount > 0 {
			b.WriteString(fmt.Sprintf(" Comments addressed: %d\n", commentCount))
		}
	}

	// Log
	if len(m.log) > 0 {
		b.WriteString("\n " + bold.Render("Log:") + "\n")
		start := 0
		if len(m.log) > 15 {
			start = len(m.log) - 15
		}
		for _, entry := range m.log[start:] {
			ts := entry.time.Format("15:04:05")
			b.WriteString(fmt.Sprintf("   %s %s\n", dim.Render(ts), entry.message))
		}
	}

	// Footer
	b.WriteString("\n" + dim.Render(" esc stop heal mode") + "\n")

	return b.String()
}
