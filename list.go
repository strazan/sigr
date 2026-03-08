package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type listPR struct {
	Number            int             `json:"number"`
	Title             string          `json:"title"`
	HeadRefName       string          `json:"headRefName"`
	ReviewDecision    string          `json:"reviewDecision"`
	StatusCheckRollup json.RawMessage `json:"statusCheckRollup"`
	Reviews           []ghReview      `json:"reviews"`
	ReviewRequests    []ghReviewReq   `json:"reviewRequests"`
	IsDraft           bool            `json:"isDraft"`
}

type listDataMsg struct {
	prs []listPR
}

type listState struct {
	prs    []listPR
	cursor int
}

type listModel struct {
	prs        []listPR
	cursor     int
	chosen     int // PR number selected by enter, 0 = none
	err        error
	quitting   bool
	interval   time.Duration
	merging    bool
	mergePRNum int
	mergeErr   error
}

func fetchPRList() tea.Cmd {
	return func() tea.Msg {
		out, err := exec.Command("gh", "pr", "list", "--author", "@me", "--json",
			"number,title,headRefName,reviewDecision,statusCheckRollup,reviews,reviewRequests,isDraft").Output()
		if err != nil {
			return errMsg{fmt.Errorf("gh: %s", cmdError(err))}
		}
		var prs []listPR
		if err := json.Unmarshal(out, &prs); err != nil {
			return errMsg{fmt.Errorf("failed to parse PRs: %w", err)}
		}
		return listDataMsg{prs: prs}
	}
}

func (m listModel) Init() tea.Cmd {
	return tea.Batch(fetchPRList(), tickCmd(m.interval))
}

func (m listModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if m.mergeErr != nil {
			m.merging = false
			m.mergeErr = nil
			return m, nil
		}
		if m.merging {
			return m, nil
		}
		switch msg.String() {
		case "ctrl+c", "q":
			m.quitting = true
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.prs != nil && m.cursor < len(m.prs)-1 {
				m.cursor++
			}
		case "enter":
			if m.prs != nil && m.cursor < len(m.prs) {
				m.chosen = m.prs[m.cursor].Number
				return m, tea.Quit
			}
		case "m":
			if m.prs != nil && m.cursor < len(m.prs) {
				m.merging = true
				m.mergePRNum = m.prs[m.cursor].Number
				return m, mergePRCmd(m.prs[m.cursor].Number, false)
			}
		case "M":
			if m.prs != nil && m.cursor < len(m.prs) {
				m.merging = true
				m.mergePRNum = m.prs[m.cursor].Number
				return m, mergePRCmd(m.prs[m.cursor].Number, true)
			}
		}

	case mergeResultMsg:
		m.merging = false
		if msg.err != nil {
			m.mergeErr = msg.err
			m.merging = true // keep showing error state
		} else {
			return m, fetchPRList()
		}

	case tickMsg:
		return m, tea.Batch(fetchPRList(), tickCmd(m.interval))

	case errMsg:
		m.err = msg.error
		return m, nil

	case listDataMsg:
		m.err = nil
		m.prs = msg.prs
		if m.cursor >= len(m.prs) {
			m.cursor = max(0, len(m.prs)-1)
		}
	}

	return m, nil
}

func reviewSummary(decision string, reviews []ghReview, requests []ghReviewReq) string {
	decision = resolveReviewDecision(decision, reviews)

	byAuthor := latestReviews(reviews)

	var approvers []string
	var changesBy []string
	for login, r := range byAuthor {
		switch r.State {
		case "APPROVED":
			approvers = append(approvers, login)
		case "CHANGES_REQUESTED":
			changesBy = append(changesBy, login)
		}
	}
	sort.Strings(approvers)
	sort.Strings(changesBy)

	// Collect pending review requests (not yet reviewed)
	var pending []string
	for _, rr := range requests {
		name := rr.Login
		if name == "" {
			name = rr.Name
		}
		if name == "" {
			name = rr.Slug
		}
		if _, reviewed := byAuthor[name]; name != "" && !reviewed {
			pending = append(pending, name)
		}
	}

	var parts []string

	switch decision {
	case "APPROVED":
		dot := green.Render("●")
		if len(approvers) > 0 {
			parts = append(parts, dot+" Approved by "+strings.Join(approvers, ", "))
		} else {
			parts = append(parts, dot+" Approved")
		}
	case "CHANGES_REQUESTED":
		dot := red.Render("●")
		if len(changesBy) > 0 {
			parts = append(parts, dot+" Changes requested by "+strings.Join(changesBy, ", "))
		} else {
			parts = append(parts, dot+" Changes requested")
		}
	case "REVIEW_REQUIRED":
		if len(approvers) > 0 {
			parts = append(parts, green.Render("●")+" Approved by "+strings.Join(approvers, ", "))
		}
		if len(pending) > 0 {
			parts = append(parts, yellow.Render("●")+" Waiting on "+strings.Join(pending, ", "))
		}
		if len(parts) == 0 {
			parts = append(parts, yellow.Render("●")+" Review required")
		}
		return strings.Join(parts, "  ")
	default:
		return dim.Render("●") + " No reviewers"
	}

	if len(pending) > 0 {
		parts = append(parts, yellow.Render("●")+" Waiting on "+strings.Join(pending, ", "))
	}
	return strings.Join(parts, "  ")
}

func (m listModel) View() string {
	if m.quitting || m.chosen != 0 {
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

	if m.prs == nil {
		b.WriteString(dim.Render("fetching PRs..."))
		b.WriteString("\n")
		return b.String()
	}

	if m.merging {
		if m.mergeErr != nil {
			b.WriteString(red.Render(fmt.Sprintf("Merge #%d failed: %s", m.mergePRNum, m.mergeErr.Error())) + "\n")
			b.WriteString(dim.Render("press any key to continue") + "\n")
		} else {
			b.WriteString(dim.Render(fmt.Sprintf("merging #%d...", m.mergePRNum)) + "\n")
		}
		return b.String()
	}

	if len(m.prs) == 0 {
		b.WriteString(dim.Render("no open PRs"))
		b.WriteString("\n")
		return b.String()
	}

	for i, pr := range m.prs {
		checks := parseChecks(pr.StatusCheckRollup)
		total := len(checks)
		passed := 0
		failed := 0
		pending := 0
		for _, c := range checks {
			if c.Status == "COMPLETED" {
				if isSuccess(c.Conclusion) {
					passed++
				} else {
					failed++
				}
			} else {
				pending++
			}
		}

		var checksDot string
		var checksSummary string
		if failed > 0 {
			checksDot = red.Render("●")
			checksSummary = fmt.Sprintf("%d/%d failed", failed, total)
		} else if pending > 0 {
			checksDot = yellow.Render("●")
			checksSummary = fmt.Sprintf("%d/%d pending", pending, total)
		} else if total > 0 {
			checksDot = green.Render("●")
			checksSummary = fmt.Sprintf("%d/%d passed", passed, total)
		} else {
			checksDot = dim.Render("●")
			checksSummary = "no checks"
		}

		draft := ""
		if pr.IsDraft {
			draft = dim.Render(" [draft]")
		}

		review := reviewSummary(pr.ReviewDecision, pr.Reviews, pr.ReviewRequests)

		prefix := "  "
		if i == m.cursor {
			prefix = "▸ "
		}

		// Line 1: title
		title := bold.Render(fmt.Sprintf("#%d", pr.Number)) + "  " + pr.Title + draft
		if i == m.cursor {
			title = bold.Render(fmt.Sprintf("#%d  %s", pr.Number, pr.Title)) + draft
		}
		fmt.Fprintf(&b, "%s%s\n", prefix, title)

		// Line 2: checks + review
		fmt.Fprintf(&b, "  %s %s  %s\n", checksDot, checksSummary, review)

		// Line 3: branch
		fmt.Fprintf(&b, "  %s\n", dim.Render(pr.HeadRefName))

		if i < len(m.prs)-1 {
			b.WriteString("\n")
		}
	}

	b.WriteString("\n" + dim.Render(fmt.Sprintf("↑/↓ navigate · enter select · m merge · M auto-merge · q quit · polling every %s", m.interval)) + "\n")

	return b.String()
}

func runList(interval time.Duration, prev *listState) {
	m := listModel{interval: interval}
	if prev != nil {
		m.prs = prev.prs
		m.cursor = prev.cursor
	}
	p := tea.NewProgram(m, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fm := finalModel.(listModel)
	if fm.err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", fm.err)
		os.Exit(1)
	}
	if fm.chosen != 0 {
		st := &listState{prs: fm.prs, cursor: fm.cursor}
		runWatch(fmt.Sprintf("%d", fm.chosen), interval, st)
	}
}
