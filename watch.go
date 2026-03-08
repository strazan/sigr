package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Styles
var (
	bold   = lipgloss.NewStyle().Bold(true)
	dim    = lipgloss.NewStyle().Faint(true)
	green  = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	red    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	yellow = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)

// Model

type model struct {
	prRef      string
	pr         *ghPR
	checks     []check
	startedAt  time.Time
	collapsed  bool
	collapseAt time.Time
	exitCode   int
	quitting   bool
	back       bool // true = return to list instead of exiting
	fix        bool // true = enter fix mode
	fromList   bool // true = launched from list view
	listState  *listState
	err        error
	interval   time.Duration
}

// Messages

type errMsg struct{ error }

type tickMsg time.Time

type prDataMsg struct {
	pr *ghPR
}

// Commands

func fetchPR(prRef string) tea.Cmd {
	return func() tea.Msg {
		args := []string{"pr", "view", "--json",
			"statusCheckRollup,number,title,headRefName,reviewDecision,reviewRequests,reviews,comments"}
		if prRef != "" {
			args = append(args, prRef)
		}
		out, err := exec.Command("gh", args...).Output()
		if err != nil {
			return errMsg{fmt.Errorf("gh: %s", cmdError(err))}
		}
		var pr ghPR
		if err := json.Unmarshal(out, &pr); err != nil {
			return errMsg{fmt.Errorf("failed to parse PR JSON: %w", err)}
		}
		return prDataMsg{pr: &pr}
	}
}

func tickCmd(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// Init

func (m model) Init() tea.Cmd {
	return tea.Batch(fetchPR(m.prRef), tickCmd(m.interval))
}

// Update

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			m.quitting = true
			m.exitCode = 130
			return m, tea.Quit
		case "esc":
			if m.fromList {
				m.back = true
				return m, tea.Quit
			}
		case "f":
			if m.hasFailedChecks() {
				m.fix = true
				return m, tea.Quit
			}
		}

	case tickMsg:
		return m, tea.Batch(fetchPR(m.prRef), tickCmd(m.interval))

	case errMsg:
		m.err = msg.error
		return m, nil

	case prDataMsg:
		m.err = nil
		m.pr = msg.pr
		m.checks = parseChecks(msg.pr.StatusCheckRollup)

		// Collapse logic: start timer when we first see any successful check
		hasSuccess := false
		allDone := true
		for _, c := range m.checks {
			if c.Status == "COMPLETED" && isSuccess(c.Conclusion) {
				hasSuccess = true
			}
			if c.Status != "COMPLETED" {
				allDone = false
			}
		}

		if hasSuccess {
			if m.collapseAt.IsZero() {
				m.collapseAt = time.Now()
			}
			if time.Since(m.collapseAt) >= 3*time.Second {
				m.collapsed = true
			}
		} else {
			m.collapseAt = time.Time{}
			m.collapsed = false
		}

		// Exit logic
		if allDone && m.pr.ReviewDecision == "APPROVED" {
			hasFailure := false
			for _, c := range m.checks {
				if !isSuccess(c.Conclusion) {
					hasFailure = true
					break
				}
			}
			if hasFailure {
				m.exitCode = 1
			} else {
				m.exitCode = 0
			}
			m.quitting = true
			return m, tea.Quit
		}
	}

	return m, nil
}

// View

func (m model) View() string {
	if m.quitting && m.pr == nil {
		return ""
	}

	var b strings.Builder

	if m.err != nil {
		b.WriteString(red.Render("Error: " + m.err.Error()))
		b.WriteString("\n")
		b.WriteString(dim.Render(fmt.Sprintf("retrying in %s...", m.interval)))
		b.WriteString("\n")
		return b.String()
	}

	if m.pr == nil {
		b.WriteString(dim.Render("fetching PR data..."))
		b.WriteString("\n")
		return b.String()
	}

	pr := m.pr

	// Header
	b.WriteString(bold.Render(fmt.Sprintf("#%d", pr.Number)))
	b.WriteString(" " + pr.Title + " ")
	b.WriteString(dim.Render("(" + pr.HeadRefName + ")"))
	b.WriteString("\n\n")

	// Checks
	if m.collapsed {
		successCount := 0
		for _, c := range m.checks {
			if c.Status == "COMPLETED" && isSuccess(c.Conclusion) {
				successCount++
			}
		}
		// Show non-successful checks individually
		for _, c := range m.checks {
			if c.Status == "COMPLETED" && isSuccess(c.Conclusion) {
				continue
			}
			dot := checkDot(c)
			b.WriteString(" " + dot + " " + c.Name + "\n")
		}
		// Collapsed summary for successful checks
		b.WriteString(" " + green.Render("●") + fmt.Sprintf(" %d/%d checks successful", successCount, len(m.checks)))
		b.WriteString("\n")
	} else {
		for _, c := range m.checks {
			dot := checkDot(c)
			b.WriteString(" " + dot + " " + c.Name + "\n")
		}
	}

	// Review status
	b.WriteString("\n")
	decision := resolveReviewDecision(pr.ReviewDecision, pr.Reviews)
	switch decision {
	case "APPROVED":
		b.WriteString(" " + green.Render("●") + " Approved\n")
	case "CHANGES_REQUESTED":
		b.WriteString(" " + red.Render("●") + " Changes requested\n")
	case "REVIEW_REQUIRED":
		b.WriteString(" " + yellow.Render("●") + " Review required\n")
	default:
		hasReviewers := len(pr.Reviews) > 0 || len(pr.ReviewRequests) > 0
		if !hasReviewers {
			b.WriteString(" " + red.Render("●") + " No reviewers assigned\n")
		} else {
			b.WriteString(" " + dim.Render("●") + " No reviews yet\n")
		}
	}

	// Individual reviewers (latest review per author)
	type latestReview struct {
		author      string
		state       string
		submittedAt string
	}
	reviewMap := make(map[string]latestReview)
	for _, r := range pr.Reviews {
		existing, ok := reviewMap[r.Author.Login]
		if !ok || r.SubmittedAt > existing.submittedAt {
			reviewMap[r.Author.Login] = latestReview{
				author:      r.Author.Login,
				state:       r.State,
				submittedAt: r.SubmittedAt,
			}
		}
	}
	var reviewers []latestReview
	for _, r := range reviewMap {
		reviewers = append(reviewers, r)
	}
	sort.Slice(reviewers, func(i, j int) bool {
		return reviewers[i].author < reviewers[j].author
	})
	reviewedAuthors := make(map[string]bool)
	for _, r := range reviewers {
		reviewedAuthors[r.author] = true
		var style lipgloss.Style
		switch r.state {
		case "APPROVED":
			style = green
		case "CHANGES_REQUESTED":
			style = red
		default:
			style = dim
		}
		b.WriteString("   " + style.Render("└ "+r.author) + "\n")
	}

	// Pending review requests
	for _, rr := range pr.ReviewRequests {
		name := rr.Login
		if name == "" {
			name = rr.Name
		}
		if name == "" {
			name = rr.Slug
		}
		if name == "" || reviewedAuthors[name] {
			continue
		}
		b.WriteString("   " + yellow.Render("└ "+name) + " " + dim.Render("(pending)") + "\n")
	}

	// New comments
	var newComments []ghComment
	for _, c := range pr.Comments {
		if c.CreatedAt > m.startedAt.Format(time.RFC3339) {
			newComments = append(newComments, c)
		}
	}
	if len(newComments) > 0 {
		sort.Slice(newComments, func(i, j int) bool {
			return newComments[i].CreatedAt < newComments[j].CreatedAt
		})
		b.WriteString("\n " + bold.Render("New comments:") + "\n")
		for _, c := range newComments {
			body := strings.SplitN(c.Body, "\n", 2)[0]
			if len(body) > 80 {
				body = body[:80] + "…"
			}
			b.WriteString("   " + dim.Render(c.Author.Login+":") + " " + body + "\n")
		}
	}

	// Footer
	hint := fmt.Sprintf("polling every %s · q to quit", m.interval)
	if m.fromList {
		hint = fmt.Sprintf("polling every %s · esc back · q to quit", m.interval)
	}
	if m.hasFailedChecks() {
		hint += " · f fix"
	}
	b.WriteString("\n" + dim.Render(hint) + "\n")

	return b.String()
}

func (m model) hasFailedChecks() bool {
	for _, c := range m.checks {
		if c.Status == "COMPLETED" && !isSuccess(c.Conclusion) {
			return true
		}
	}
	return false
}

func checkDot(c check) string {
	if c.Status == "COMPLETED" {
		if isSuccess(c.Conclusion) {
			return green.Render("●")
		}
		return red.Render("●")
	}
	return yellow.Render("●")
}
