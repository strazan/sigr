package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Agent mode phases
type agentPhase int

const (
	agentFetchIssues agentPhase = iota
	agentClaim
	agentWorktree
	agentImplement
	agentDiff
	agentCommit
	agentCreatePR
	agentDone
	agentError
)

type agentModel struct {
	phase         agentPhase
	label         string
	all           bool
	issue         *ghIssue
	branch        string
	worktreeDir   string
	diff          string
	prNumber      int
	quitting      bool
	errMsg        string
	skippedIssues map[int]bool
	log           []healLogEntry
}

// Messages

type agentIssuesMsg struct{ issues []ghIssue }
type agentClaimedMsg struct{}
type agentWorktreeReadyMsg struct{ dir string }
type agentImplementedMsg struct{}
type agentDiffMsg struct{ diff string }
type agentCommittedMsg struct{}
type agentPRCreatedMsg struct{ number int }
type agentErrMsg struct{ err string }

// Commands

func agentFetchIssuesCmd(label string, all bool) tea.Cmd {
	return func() tea.Msg {
		args := []string{"issue", "list", "--state", "open",
			"--json", "number,title,body,labels,assignees", "--limit", "20"}
		if !all {
			args = append(args, "--label", label)
		}
		out, err := exec.Command("gh", args...).Output()
		if err != nil {
			return agentErrMsg{err: fmt.Sprintf("gh issue list: %s", cmdError(err))}
		}
		var issues []ghIssue
		if err := json.Unmarshal(out, &issues); err != nil {
			return agentErrMsg{err: fmt.Sprintf("parse issues: %v", err)}
		}
		// Filter to unassigned issues
		var unassigned []ghIssue
		for _, issue := range issues {
			if len(issue.Assignees) == 0 {
				unassigned = append(unassigned, issue)
			}
		}
		return agentIssuesMsg{issues: unassigned}
	}
}

func agentClaimIssue(number int) tea.Cmd {
	return func() tea.Msg {
		n := fmt.Sprintf("%d", number)
		if out, err := exec.Command("gh", "issue", "edit", n,
			"--add-assignee", "@me").CombinedOutput(); err != nil {
			return agentErrMsg{err: fmt.Sprintf("assign issue: %s", strings.TrimSpace(string(out)))}
		}
		if out, err := exec.Command("gh", "issue", "comment", n,
			"-b", "Picking up this issue. Will open a PR shortly.").CombinedOutput(); err != nil {
			return agentErrMsg{err: fmt.Sprintf("comment on issue: %s", strings.TrimSpace(string(out)))}
		}
		return agentClaimedMsg{}
	}
}

func agentSetupWorktree(branch string) tea.Cmd {
	return func() tea.Msg {
		exec.Command("git", "fetch", "origin").Run()

		// Determine default branch
		defaultBranch := "main"
		if out, err := exec.Command("gh", "repo", "view", "--json",
			"defaultBranchRef", "-q", ".defaultBranchRef.name").Output(); err == nil {
			if b := strings.TrimSpace(string(out)); b != "" {
				defaultBranch = b
			}
		}

		// Clean up leftover branch from previous attempt
		exec.Command("git", "branch", "-D", branch).Run()

		// Determine worktree directory
		dirName := strings.ReplaceAll(branch, "/", "-")
		var dir string
		if root := detectSigtree(); root != "" {
			dir = filepath.Join(root, dirName)
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return agentErrMsg{err: fmt.Sprintf("home dir: %v", err)}
			}
			dir = filepath.Join(home, ".sigr", "worktrees", repoName(), dirName)
		}

		os.MkdirAll(filepath.Dir(dir), 0755)
		if _, err := os.Stat(dir); err == nil {
			exec.Command("git", "worktree", "remove", "--force", dir).Run()
		}

		out, err := exec.Command("git", "worktree", "add", "-b", branch,
			dir, "origin/"+defaultBranch).CombinedOutput()
		if err != nil {
			return agentErrMsg{err: fmt.Sprintf("git worktree add: %s", strings.TrimSpace(string(out)))}
		}
		return agentWorktreeReadyMsg{dir: dir}
	}
}

func agentImplementCmd(issue *ghIssue, dir string) tea.Cmd {
	return func() tea.Msg {
		prompt := fmt.Sprintf(
			"Implement the following GitHub issue. Edit the files directly.\n\nIssue #%d: %s\n\n%s",
			issue.Number, issue.Title, issue.Body)
		cmd := exec.Command("claude", "-p", prompt)
		cmd.Dir = dir
		if _, err := cmd.Output(); err != nil {
			return agentErrMsg{err: fmt.Sprintf("claude implement failed: %s", cmdError(err))}
		}
		return agentImplementedMsg{}
	}
}

func agentFetchDiffCmd(dir string) tea.Cmd {
	return func() tea.Msg {
		cmd := exec.Command("git", "diff", "--stat")
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			return agentErrMsg{err: fmt.Sprintf("git diff failed: %s", cmdError(err))}
		}
		diff := strings.TrimSpace(string(out))

		cmd = exec.Command("git", "ls-files", "--others", "--exclude-standard")
		cmd.Dir = dir
		untrackedOut, _ := cmd.Output()
		untracked := strings.TrimSpace(string(untrackedOut))

		if diff == "" && untracked == "" {
			return agentErrMsg{err: "claude made no changes"}
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
		return agentDiffMsg{diff: result}
	}
}

func agentCommitAndPushCmd(branch, message, dir string) tea.Cmd {
	return func() tea.Msg {
		cmd := exec.Command("git", "add", "-A")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return agentErrMsg{err: fmt.Sprintf("git add: %s", firstLine(out, err))}
		}

		cmd = exec.Command("git", "commit", "--no-verify", "-m", message)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return agentErrMsg{err: fmt.Sprintf("git commit: %s", firstLine(out, err))}
		}

		cmd = exec.Command("git", "push", "-u", "origin", branch)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return agentErrMsg{err: fmt.Sprintf("git push: %s", firstLine(out, err))}
		}

		return agentCommittedMsg{}
	}
}

func agentCreatePRCmd(issue *ghIssue, branch, dir string) tea.Cmd {
	return func() tea.Msg {
		title := issue.Title
		if len(title) > 72 {
			title = title[:72]
		}
		body := fmt.Sprintf("Closes #%d\n\nImplemented by sigr agent.", issue.Number)

		cmd := exec.Command("gh", "pr", "create",
			"--title", title,
			"--body", body,
			"--head", branch)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return agentErrMsg{err: fmt.Sprintf("gh pr create: %s", strings.TrimSpace(string(out)))}
		}

		// Get PR number
		cmd = exec.Command("gh", "pr", "view", branch, "--json", "number")
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			return agentErrMsg{err: fmt.Sprintf("get PR number: %s", cmdError(err))}
		}
		var pr struct {
			Number int `json:"number"`
		}
		if err := json.Unmarshal(out, &pr); err != nil {
			return agentErrMsg{err: fmt.Sprintf("parse PR: %v", err)}
		}
		return agentPRCreatedMsg{number: pr.Number}
	}
}

func agentUnclaimIssue(number int) {
	exec.Command("gh", "issue", "edit", fmt.Sprintf("%d", number),
		"--remove-assignee", "@me").Run()
}

func agentCleanupWorktree(dir, branch string) {
	if dir != "" {
		exec.Command("git", "worktree", "remove", "--force", dir).Run()
	}
	if branch != "" {
		exec.Command("git", "branch", "-D", branch).Run()
	}
}

// slugify generates a URL-safe slug from an issue title.
var nonAlphanumDash = regexp.MustCompile(`[^a-z0-9-]+`)

func slugify(title string) string {
	s := strings.ToLower(title)
	s = strings.ReplaceAll(s, " ", "-")
	s = nonAlphanumDash.ReplaceAllString(s, "")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = s[:40]
		s = strings.TrimRight(s, "-")
	}
	if s == "" {
		s = "issue"
	}
	return s
}

// Init

func (m agentModel) Init() tea.Cmd {
	return agentFetchIssuesCmd(m.label, m.all)
}

// Update

func (m agentModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			if m.phase == agentImplement || m.phase == agentCommit || m.phase == agentCreatePR {
				return m, nil
			}
			m.quitting = true
			if m.issue != nil && m.phase != agentDone {
				agentUnclaimIssue(m.issue.Number)
			}
			agentCleanupWorktree(m.worktreeDir, m.branch)
			return m, tea.Quit
		}

	case agentIssuesMsg:
		// Filter out skipped issues
		var available []ghIssue
		for _, issue := range msg.issues {
			if !m.skippedIssues[issue.Number] {
				available = append(available, issue)
			}
		}
		if len(available) == 0 {
			m.log = append(m.log, healLogEntry{time: time.Now(), message: "No more issues to work on"})
			m.quitting = true
			return m, tea.Quit
		}
		m.issue = &available[0]
		m.log = append(m.log, healLogEntry{time: time.Now(),
			message: fmt.Sprintf("Found issue #%d: %s", m.issue.Number, m.issue.Title)})
		m.phase = agentClaim
		return m, agentClaimIssue(m.issue.Number)

	case agentClaimedMsg:
		m.log = append(m.log, healLogEntry{time: time.Now(),
			message: fmt.Sprintf("Claimed issue #%d", m.issue.Number)})
		m.branch = fmt.Sprintf("agent/%d-%s", m.issue.Number, slugify(m.issue.Title))
		m.phase = agentWorktree
		return m, agentSetupWorktree(m.branch)

	case agentWorktreeReadyMsg:
		m.worktreeDir = msg.dir
		m.log = append(m.log, healLogEntry{time: time.Now(), message: "Worktree ready at " + msg.dir})
		m.phase = agentImplement
		m.log = append(m.log, healLogEntry{time: time.Now(), message: "Claude is implementing..."})
		return m, agentImplementCmd(m.issue, m.worktreeDir)

	case agentImplementedMsg:
		m.log = append(m.log, healLogEntry{time: time.Now(), message: "Implementation complete"})
		m.phase = agentDiff
		return m, agentFetchDiffCmd(m.worktreeDir)

	case agentDiffMsg:
		m.diff = msg.diff
		m.log = append(m.log, healLogEntry{time: time.Now(), message: "Changes detected"})
		m.phase = agentCommit
		commitMsg := fmt.Sprintf("feat: implement #%d %s", m.issue.Number, m.issue.Title)
		if len(commitMsg) > 72 {
			commitMsg = commitMsg[:72]
		}
		return m, agentCommitAndPushCmd(m.branch, commitMsg, m.worktreeDir)

	case agentCommittedMsg:
		m.log = append(m.log, healLogEntry{time: time.Now(), message: "Pushed to " + m.branch})
		m.phase = agentCreatePR
		return m, agentCreatePRCmd(m.issue, m.branch, m.worktreeDir)

	case agentPRCreatedMsg:
		m.prNumber = msg.number
		m.log = append(m.log, healLogEntry{time: time.Now(),
			message: fmt.Sprintf("Created PR #%d", msg.number)})
		m.phase = agentDone
		return m, tea.Quit

	case agentErrMsg:
		m.log = append(m.log, healLogEntry{time: time.Now(), message: "Error: " + msg.err})

		// Unclaim and cleanup on failure
		if m.issue != nil {
			agentUnclaimIssue(m.issue.Number)
			m.log = append(m.log, healLogEntry{time: time.Now(),
				message: fmt.Sprintf("Unclaimed issue #%d, trying next...", m.issue.Number)})
			m.skippedIssues[m.issue.Number] = true
		}
		agentCleanupWorktree(m.worktreeDir, m.branch)
		m.worktreeDir = ""

		// Reset and try next issue
		m.issue = nil
		m.branch = ""
		m.diff = ""
		m.phase = agentFetchIssues
		return m, agentFetchIssuesCmd(m.label, m.all)
	}

	return m, nil
}

// View

func (m agentModel) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	b.WriteString(yellow.Render("AGENT MODE"))
	if m.issue != nil {
		b.WriteString("  " + bold.Render(fmt.Sprintf("#%d", m.issue.Number)) + " " + m.issue.Title)
	}
	b.WriteString("\n\n")

	// Status
	switch m.phase {
	case agentFetchIssues:
		b.WriteString(dim.Render(" ◌ Fetching issues..."))
	case agentClaim:
		b.WriteString(dim.Render(" ◌ Claiming issue..."))
	case agentWorktree:
		b.WriteString(dim.Render(" ◌ Setting up worktree..."))
	case agentImplement:
		b.WriteString(dim.Render(" ◌ Claude is implementing..."))
	case agentDiff:
		b.WriteString(dim.Render(" ◌ Checking changes..."))
	case agentCommit:
		b.WriteString(dim.Render(" ◌ Committing and pushing..."))
	case agentCreatePR:
		b.WriteString(dim.Render(" ◌ Creating pull request..."))
	case agentDone:
		b.WriteString(green.Render(" ● Done") + dim.Render(fmt.Sprintf(" — PR #%d created", m.prNumber)))
	case agentError:
		b.WriteString(red.Render(" ● Error: " + m.errMsg))
	}
	b.WriteString("\n")

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
	b.WriteString("\n" + dim.Render(" q quit") + "\n")

	return b.String()
}
