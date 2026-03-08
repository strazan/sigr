package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Worker phases

type workerPhase int

const (
	workerClaim workerPhase = iota
	workerWorktree
	workerImplement
	workerDiff
	workerCommit
	workerCreatePR
	workerHealing
	workerHealFetchRuns
	workerHealFetchLogs
	workerHealAnalyzing
	workerHealFixingCI
	workerHealFixingComment
	workerHealDiffCheck
	workerHealCommitting
)

type issueWorker struct {
	issue     *ghIssue
	phase     workerPhase
	branch    string
	worktree  string
	prNumber  int
	pr        *ghPR
	checks    []check
	startedAt time.Time
	commitMsg string

	// Heal tracking
	addressedRuns     map[int]string
	addressedComments map[string]bool
	currentRunID      int
	currentRunSha     string
	currentLogs       string
	currentComment    string
}

func (w *issueWorker) statusText() string {
	switch w.phase {
	case workerClaim:
		return "Claiming issue..."
	case workerWorktree:
		return "Setting up worktree..."
	case workerImplement:
		return "Claude is implementing..."
	case workerDiff:
		return "Checking changes..."
	case workerCommit:
		return "Committing and pushing..."
	case workerCreatePR:
		return "Creating pull request..."
	case workerHealing:
		if w.prNumber > 0 {
			passing, total := w.checkCounts()
			return fmt.Sprintf("Healing PR #%d (%d/%d checks passing)", w.prNumber, passing, total)
		}
		return "Healing..."
	case workerHealFetchRuns:
		return "Fetching failed runs..."
	case workerHealFetchLogs:
		return "Fetching failure logs..."
	case workerHealAnalyzing:
		return "Analyzing failures..."
	case workerHealFixingCI:
		return "Claude is fixing CI..."
	case workerHealFixingComment:
		return "Addressing comment..."
	case workerHealDiffCheck:
		return "Checking changes..."
	case workerHealCommitting:
		return "Committing fix..."
	}
	return ""
}

func (w *issueWorker) checkCounts() (passing, total int) {
	for _, c := range w.checks {
		total++
		if c.Status == "COMPLETED" && isSuccess(c.Conclusion) {
			passing++
		}
	}
	return
}

func (w *issueWorker) isActive() bool {
	switch w.phase {
	case workerImplement, workerCommit, workerCreatePR,
		workerHealFixingCI, workerHealFixingComment, workerHealCommitting:
		return true
	}
	return false
}

// Agent model

type agentModel struct {
	label     string
	all       bool
	acceptAll bool
	interval  time.Duration
	quitting  bool
	workers   map[int]*issueWorker // keyed by issue number
	pending   []ghIssue            // issues awaiting user acceptance
	cursor    int                  // cursor position in pending list
	skipped   map[int]bool         // issues the user has skipped
	log       []healLogEntry
}

// Message wrapper for routing to workers

type workerMsg struct {
	issueNumber int
	msg         tea.Msg
}

func tagCmd(issueNumber int, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		return workerMsg{issueNumber: issueNumber, msg: cmd()}
	}
}

// Worker-specific messages

type agentIssuesMsg struct{ issues []ghIssue }
type agentClaimedMsg struct{}
type agentWorktreeReadyMsg struct{ dir string }
type agentImplementedMsg struct{}
type agentDiffMsg struct{ diff string }
type agentCommittedMsg struct{ commitMsg string }
type agentPRCreatedMsg struct{ number int }
type agentHealPRMsg struct{ pr *ghPR }
type agentErrMsg struct{ err string }
type agentTickMsg time.Time

// Commands

func agentTickCmd(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg {
		return agentTickMsg(t)
	})
}

func agentFetchIssuesCmd(label string, all bool) tea.Cmd {
	return func() tea.Msg {
		args := []string{"issue", "list", "--state", "open",
			"--json", "number,title,body,labels,assignees", "--limit", "20"}
		if !all {
			args = append(args, "--label", label)
		}
		out, err := exec.Command("gh", args...).Output()
		if err != nil {
			return agentIssuesMsg{} // silently return empty on error
		}
		var issues []ghIssue
		if err := json.Unmarshal(out, &issues); err != nil {
			return agentIssuesMsg{}
		}
		var unassigned []ghIssue
		for _, issue := range issues {
			if len(issue.Assignees) == 0 {
				unassigned = append(unassigned, issue)
			}
		}
		return agentIssuesMsg{issues: unassigned}
	}
}

func agentClaimIssue(issueNum int) tea.Cmd {
	return tagCmd(issueNum, func() tea.Msg {
		n := fmt.Sprintf("%d", issueNum)
		if out, err := exec.Command("gh", "issue", "edit", n,
			"--add-assignee", "@me").CombinedOutput(); err != nil {
			return agentErrMsg{err: fmt.Sprintf("assign issue: %s", strings.TrimSpace(string(out)))}
		}
		if out, err := exec.Command("gh", "issue", "comment", n,
			"-b", "Picking up this issue. Will open a PR shortly.").CombinedOutput(); err != nil {
			return agentErrMsg{err: fmt.Sprintf("comment on issue: %s", strings.TrimSpace(string(out)))}
		}
		return agentClaimedMsg{}
	})
}

func agentSetupWorktree(issueNum int, branch string) tea.Cmd {
	return tagCmd(issueNum, func() tea.Msg {
		exec.Command("git", "fetch", "origin").Run()

		defaultBranch := "main"
		if out, err := exec.Command("gh", "repo", "view", "--json",
			"defaultBranchRef", "-q", ".defaultBranchRef.name").Output(); err == nil {
			if b := strings.TrimSpace(string(out)); b != "" {
				defaultBranch = b
			}
		}

		exec.Command("git", "branch", "-D", branch).Run()

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
	})
}

func agentImplement(issueNum int, issue *ghIssue, dir string) tea.Cmd {
	return tagCmd(issueNum, func() tea.Msg {
		prompt := fmt.Sprintf(
			"Implement the following GitHub issue. Edit the files directly.\n\nIssue #%d: %s\n\n%s",
			issue.Number, issue.Title, issue.Body)
		cmd := exec.Command("claude", "-p", prompt)
		cmd.Dir = dir
		if _, err := cmd.Output(); err != nil {
			return agentErrMsg{err: fmt.Sprintf("claude implement failed: %s", cmdError(err))}
		}
		return agentImplementedMsg{}
	})
}

func agentFetchDiff(issueNum int, dir string) tea.Cmd {
	return tagCmd(issueNum, func() tea.Msg {
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
	})
}

func agentCommitAndPush(issueNum int, issue *ghIssue, branch, dir string) tea.Cmd {
	return tagCmd(issueNum, func() tea.Msg {
		cmd := exec.Command("git", "add", "-A")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return agentErrMsg{err: fmt.Sprintf("git add: %s", firstLine(out, err))}
		}

		logCmd := exec.Command("git", "log", "--oneline", "-5")
		logCmd.Dir = dir
		logOut, _ := logCmd.Output()

		diffCmd := exec.Command("git", "diff", "--staged", "--stat")
		diffCmd.Dir = dir
		diffOut, _ := diffCmd.Output()

		prompt := fmt.Sprintf(
			"Generate a single-line git commit message for this change. "+
				"Match the style and convention of the recent commits below. "+
				"Output ONLY the commit message, nothing else.\n\n"+
				"Recent commits:\n%s\n"+
				"Changes:\n%s\n"+
				"This implements issue #%d: %s",
			string(logOut), string(diffOut), issue.Number, issue.Title)
		claudeOut, err := exec.Command("claude", "-p", prompt).Output()
		commitMsg := strings.TrimSpace(string(claudeOut))
		if err != nil || commitMsg == "" {
			commitMsg = fmt.Sprintf("feat: implement #%d", issue.Number)
		}
		if i := strings.Index(commitMsg, "\n"); i > 0 {
			commitMsg = commitMsg[:i]
		}

		cmd = exec.Command("git", "commit", "--no-verify", "-m", commitMsg)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return agentErrMsg{err: fmt.Sprintf("git commit: %s", firstLine(out, err))}
		}

		cmd = exec.Command("git", "push", "-u", "origin", branch)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return agentErrMsg{err: fmt.Sprintf("git push: %s", firstLine(out, err))}
		}

		return agentCommittedMsg{commitMsg: commitMsg}
	})
}

func agentCreatePR(issueNum int, issue *ghIssue, commitMsg, branch, dir string) tea.Cmd {
	return tagCmd(issueNum, func() tea.Msg {
		title := commitMsg
		if title == "" {
			title = issue.Title
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
	})
}

func agentFetchPR(issueNum int, prRef string) tea.Cmd {
	return tagCmd(issueNum, func() tea.Msg {
		out, err := exec.Command("gh", "pr", "view", prRef, "--json",
			"statusCheckRollup,number,title,headRefName,state,reviewDecision,reviewRequests,reviews,comments").Output()
		if err != nil {
			return agentErrMsg{err: fmt.Sprintf("gh pr view: %s", cmdError(err))}
		}
		var pr ghPR
		if err := json.Unmarshal(out, &pr); err != nil {
			return agentErrMsg{err: fmt.Sprintf("parse PR: %v", err)}
		}
		return agentHealPRMsg{pr: &pr}
	})
}

func agentHealFetchRuns(issueNum int, branch string) tea.Cmd {
	return tagCmd(issueNum, func() tea.Msg {
		msg := fetchFailedRuns(branch)()
		switch msg := msg.(type) {
		case fixRunsMsg:
			return healRunsMsg{runs: msg.runs}
		case fixErrMsg:
			return agentErrMsg{err: msg.err}
		default:
			return msg
		}
	})
}

func agentHealFetchLogs(issueNum, runID int) tea.Cmd {
	return tagCmd(issueNum, func() tea.Msg {
		msg := fetchFailedLogs(runID)()
		switch msg := msg.(type) {
		case fixLogsMsg:
			return healLogsMsg{logs: msg.logs}
		case fixErrMsg:
			return agentErrMsg{err: msg.err}
		default:
			return msg
		}
	})
}

func agentHealAnalyze(issueNum int, logs string) tea.Cmd {
	return tagCmd(issueNum, func() tea.Msg {
		msg := analyzeWithClaude(logs)()
		switch msg := msg.(type) {
		case fixAnalysisMsg:
			return healAnalysisMsg{analysis: msg.analysis}
		case fixErrMsg:
			return agentErrMsg{err: msg.err}
		default:
			return msg
		}
	})
}

func agentHealFixCI(issueNum int, analysis, logs, dir string) tea.Cmd {
	return tagCmd(issueNum, func() tea.Msg {
		prompt := fmt.Sprintf("Fix the CI failures described below. Edit the files directly.\n\nAnalysis:\n%s\n\nLogs:\n%s", analysis, logs)
		cmd := exec.Command("claude", "-p", prompt)
		cmd.Dir = dir
		if _, err := cmd.Output(); err != nil {
			return agentErrMsg{err: fmt.Sprintf("claude fix failed: %s", cmdError(err))}
		}
		return healFixedMsg{}
	})
}

func agentHealFixComment(issueNum int, comment, dir string) tea.Cmd {
	return tagCmd(issueNum, func() tea.Msg {
		prompt := "Fix the issue described in this PR comment. Edit the files directly.\n\nComment:\n" + comment
		cmd := exec.Command("claude", "-p", prompt)
		cmd.Dir = dir
		if _, err := cmd.Output(); err != nil {
			return agentErrMsg{err: fmt.Sprintf("claude fix failed: %s", cmdError(err))}
		}
		return healFixedMsg{}
	})
}

func agentHealFetchDiff(issueNum int, dir string) tea.Cmd {
	return tagCmd(issueNum, func() tea.Msg {
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
		return healDiffMsg{diff: result}
	})
}

func agentHealCommitAndPush(issueNum int, branch, message, dir string) tea.Cmd {
	return tagCmd(issueNum, func() tea.Msg {
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

		cmd = exec.Command("git", "push")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return agentErrMsg{err: fmt.Sprintf("git push: %s", firstLine(out, err))}
		}

		return healCommittedMsg{}
	})
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
	return tea.Batch(agentFetchIssuesCmd(m.label, m.all), agentTickCmd(m.interval))
}

// Update

func (m agentModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			for _, w := range m.workers {
				if w.isActive() {
					return m, nil
				}
			}
			m.quitting = true
			for num, w := range m.workers {
				agentUnclaimIssue(num)
				agentCleanupWorktree(w.worktree, w.branch)
			}
			return m, tea.Quit
		case "up", "k":
			if len(m.pending) > 0 && m.cursor > 0 {
				m.cursor--
			}
			return m, nil
		case "down", "j":
			if len(m.pending) > 0 && m.cursor < len(m.pending)-1 {
				m.cursor++
			}
			return m, nil
		case "enter", "y":
			return m, m.acceptCurrent()
		case "n":
			m.skipCurrent()
			return m, nil
		}

	case agentTickMsg:
		var cmds []tea.Cmd
		cmds = append(cmds, agentFetchIssuesCmd(m.label, m.all))
		cmds = append(cmds, agentTickCmd(m.interval))

		// Poll PR status for all healing workers
		for num, w := range m.workers {
			if w.phase == workerHealing && w.prNumber > 0 {
				cmds = append(cmds, agentFetchPR(num, fmt.Sprintf("%d", w.prNumber)))
			}
		}
		return m, tea.Batch(cmds...)

	case agentIssuesMsg:
		var cmds []tea.Cmd
		for _, issue := range msg.issues {
			if _, exists := m.workers[issue.Number]; exists {
				continue
			}
			if m.isPending(issue.Number) || m.skipped[issue.Number] {
				continue
			}
			if m.acceptAll {
				issueCopy := issue
				m.startWorker(&issueCopy)
				cmds = append(cmds, agentClaimIssue(issue.Number))
			} else {
				m.pending = append(m.pending, issue)
				m.addLog(fmt.Sprintf("#%d Found issue: %s (awaiting acceptance)", issue.Number, issue.Title))
			}
		}
		return m, tea.Batch(cmds...)

	case workerMsg:
		w, ok := m.workers[msg.issueNumber]
		if !ok {
			return m, nil
		}
		return m.updateWorker(msg.issueNumber, w, msg.msg)
	}

	return m, nil
}

func (m agentModel) updateWorker(num int, w *issueWorker, msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	// --- Implementation phases ---

	case agentClaimedMsg:
		m.addLog(fmt.Sprintf("#%d Claimed issue", num))
		w.branch = fmt.Sprintf("agent/%d-%s", num, slugify(w.issue.Title))
		w.phase = workerWorktree
		return m, agentSetupWorktree(num, w.branch)

	case agentWorktreeReadyMsg:
		w.worktree = msg.dir
		m.addLog(fmt.Sprintf("#%d Worktree ready", num))
		w.phase = workerImplement
		m.addLog(fmt.Sprintf("#%d Claude is implementing...", num))
		return m, agentImplement(num, w.issue, w.worktree)

	case agentImplementedMsg:
		m.addLog(fmt.Sprintf("#%d Implementation complete", num))
		w.phase = workerDiff
		return m, agentFetchDiff(num, w.worktree)

	case agentDiffMsg:
		m.addLog(fmt.Sprintf("#%d Changes detected", num))
		w.phase = workerCommit
		return m, agentCommitAndPush(num, w.issue, w.branch, w.worktree)

	case agentCommittedMsg:
		w.commitMsg = msg.commitMsg
		m.addLog(fmt.Sprintf("#%d Pushed: %s", num, msg.commitMsg))
		w.phase = workerCreatePR
		return m, agentCreatePR(num, w.issue, w.commitMsg, w.branch, w.worktree)

	case agentPRCreatedMsg:
		w.prNumber = msg.number
		m.addLog(fmt.Sprintf("#%d Created PR #%d — entering heal mode", num, msg.number))
		w.phase = workerHealing
		return m, agentFetchPR(num, fmt.Sprintf("%d", w.prNumber))

	// --- Heal phases ---

	case agentHealPRMsg:
		w.pr = msg.pr
		w.checks = parseChecks(msg.pr.StatusCheckRollup)

		if msg.pr.State == "MERGED" {
			m.addLog(fmt.Sprintf("#%d PR #%d merged!", num, w.prNumber))
			agentCleanupWorktree(w.worktree, w.branch)
			delete(m.workers, num)
			return m, nil
		}

		if w.phase != workerHealing {
			return m, nil
		}

		hasFailure := false
		for _, c := range w.checks {
			if c.Status == "COMPLETED" && !isSuccess(c.Conclusion) {
				hasFailure = true
				break
			}
		}

		if hasFailure {
			w.phase = workerHealFetchRuns
			m.addLog(fmt.Sprintf("#%d Failed checks detected", num))
			return m, agentHealFetchRuns(num, w.pr.HeadRefName)
		}

		for _, c := range w.pr.Comments {
			if c.CreatedAt > w.startedAt.Format(time.RFC3339) && !w.addressedComments[c.CreatedAt] {
				w.phase = workerHealFixingComment
				w.currentComment = c.Author.Login
				w.addressedComments[c.CreatedAt] = true
				m.addLog(fmt.Sprintf("#%d Addressing comment from @%s", num, c.Author.Login))
				return m, agentHealFixComment(num, c.Body, w.worktree)
			}
		}

		return m, nil

	case healRunsMsg:
		for _, run := range msg.runs {
			if sha, ok := w.addressedRuns[run.ID]; !ok || sha != run.HeadSha {
				w.currentRunID = run.ID
				w.currentRunSha = run.HeadSha
				w.phase = workerHealFetchLogs
				m.addLog(fmt.Sprintf("#%d Fetching logs for %s", num, run.Name))
				return m, agentHealFetchLogs(num, run.ID)
			}
		}
		w.phase = workerHealing
		return m, nil

	case healLogsMsg:
		w.currentLogs = msg.logs
		w.phase = workerHealAnalyzing
		m.addLog(fmt.Sprintf("#%d Analyzing failures...", num))
		return m, agentHealAnalyze(num, w.currentLogs)

	case healAnalysisMsg:
		w.phase = workerHealFixingCI
		m.addLog(fmt.Sprintf("#%d Claude is fixing CI...", num))
		return m, agentHealFixCI(num, msg.analysis, w.currentLogs, w.worktree)

	case healFixedMsg:
		w.phase = workerHealDiffCheck
		return m, agentHealFetchDiff(num, w.worktree)

	case healDiffMsg:
		w.phase = workerHealCommitting
		commitMsg := "fix(ci): autofix failures"
		if w.currentComment != "" {
			commitMsg = fmt.Sprintf("fix: address comment from @%s", w.currentComment)
		}
		m.addLog(fmt.Sprintf("#%d Committing: %s", num, commitMsg))
		return m, agentHealCommitAndPush(num, w.pr.HeadRefName, commitMsg, w.worktree)

	case healCommittedMsg:
		if w.currentRunID != 0 {
			w.addressedRuns[w.currentRunID] = w.currentRunSha
		}
		m.addLog(fmt.Sprintf("#%d Pushed fix successfully", num))
		w.currentRunID = 0
		w.currentRunSha = ""
		w.currentLogs = ""
		w.currentComment = ""
		w.phase = workerHealing
		return m, nil

	// --- Errors ---

	case agentErrMsg:
		m.addLog(fmt.Sprintf("#%d Error: %s", num, msg.err))

		// If in heal mode, reset and keep watching
		if w.phase >= workerHealing {
			if w.currentRunID != 0 && (w.phase == workerHealFixingCI || w.phase == workerHealDiffCheck || w.phase == workerHealCommitting) {
				w.addressedRuns[w.currentRunID] = w.currentRunSha
			}
			if w.worktree != "" {
				healResetWorktree(w.worktree)
			}
			w.currentRunID = 0
			w.currentRunSha = ""
			w.currentLogs = ""
			w.currentComment = ""
			w.phase = workerHealing
			return m, nil
		}

		// Implementation error: unclaim, cleanup, remove worker
		agentUnclaimIssue(num)
		agentCleanupWorktree(w.worktree, w.branch)
		m.addLog(fmt.Sprintf("#%d Unclaimed and cleaned up", num))
		delete(m.workers, num)
		return m, nil
	}

	return m, nil
}

func (m *agentModel) isPending(number int) bool {
	for _, p := range m.pending {
		if p.Number == number {
			return true
		}
	}
	return false
}

func (m *agentModel) startWorker(issue *ghIssue) {
	w := &issueWorker{
		issue:             issue,
		phase:             workerClaim,
		startedAt:         time.Now().UTC(),
		addressedRuns:     make(map[int]string),
		addressedComments: make(map[string]bool),
	}
	m.workers[issue.Number] = w
	m.addLog(fmt.Sprintf("#%d Accepted issue: %s", issue.Number, issue.Title))
}

func (m *agentModel) acceptCurrent() tea.Cmd {
	if len(m.pending) == 0 {
		return nil
	}
	issue := m.pending[m.cursor]
	m.pending = append(m.pending[:m.cursor], m.pending[m.cursor+1:]...)
	issueCopy := issue
	m.startWorker(&issueCopy)
	if m.cursor >= len(m.pending) && m.cursor > 0 {
		m.cursor--
	}
	return agentClaimIssue(issue.Number)
}

func (m *agentModel) skipCurrent() {
	if len(m.pending) == 0 {
		return
	}
	issue := m.pending[m.cursor]
	m.pending = append(m.pending[:m.cursor], m.pending[m.cursor+1:]...)
	m.skipped[issue.Number] = true
	m.addLog(fmt.Sprintf("#%d Skipped issue: %s", issue.Number, issue.Title))
	if m.cursor >= len(m.pending) && m.cursor > 0 {
		m.cursor--
	}
}

func (m *agentModel) addLog(message string) {
	m.log = append(m.log, healLogEntry{time: time.Now(), message: message})
}

// View

func (m agentModel) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	b.WriteString(yellow.Render("AGENT MODE"))
	b.WriteString(dim.Render(fmt.Sprintf("  (polling every %s)", m.interval)))
	b.WriteString("\n\n")

	// Pending issues
	if len(m.pending) > 0 {
		b.WriteString(bold.Render(" Pending issues:") + "\n")
		for i, issue := range m.pending {
			cursor := "  "
			if i == m.cursor {
				cursor = "> "
			}
			line := fmt.Sprintf("%s%s %s", cursor, bold.Render(fmt.Sprintf("#%d", issue.Number)), issue.Title)
			if i == m.cursor {
				b.WriteString(yellow.Render(line) + "\n")
			} else {
				b.WriteString(dim.Render(line) + "\n")
			}
		}
		b.WriteString("\n")
	}

	// Active workers
	if len(m.workers) == 0 && len(m.pending) == 0 {
		b.WriteString(dim.Render(" No active issues") + "\n")
	} else if len(m.workers) > 0 {
		nums := make([]int, 0, len(m.workers))
		for n := range m.workers {
			nums = append(nums, n)
		}
		sort.Ints(nums)

		for _, num := range nums {
			w := m.workers[num]
			b.WriteString(fmt.Sprintf(" %s %s\n", bold.Render(fmt.Sprintf("#%d", num)), w.issue.Title))
			status := w.statusText()
			if w.phase == workerHealing {
				b.WriteString(fmt.Sprintf("    %s\n", green.Render("● "+status)))
			} else {
				b.WriteString(fmt.Sprintf("    %s\n", dim.Render("◌ "+status)))
			}
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

	if len(m.pending) > 0 {
		b.WriteString("\n" + dim.Render(" enter/y accept · n skip · q quit") + "\n")
	} else {
		b.WriteString("\n" + dim.Render(" q quit") + "\n")
	}

	return b.String()
}
