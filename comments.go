package main

import (
	"fmt"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// Comment mode phases
type commentPhase int

const (
	commentBrowse commentPhase = iota
	commentView
	commentInput
	commentFixing
	commentDiffReview
	commentCommitting
	commentError
)

// Comment model

type commentModel struct {
	phase    commentPhase
	pr       *ghPR
	prRef    string
	fromList bool
	ls       *listState

	comments []ghComment
	cursor   int
	selected *ghComment

	input  string
	diff   string
	errMsg string
	back   bool
}

// Messages

type commentFixedMsg struct{}
type commentDiffMsg struct{ diff string }
type commentCommittedMsg struct{}
type commentResetMsg struct{}
type commentErrMsg struct{ err string }

// Commands

func fixFromComment(comment, instructions string) tea.Cmd {
	return func() tea.Msg {
		prompt := ""
		if instructions != "" {
			prompt = "Additional instructions from user: " + instructions + "\n\n"
		}
		prompt += "Fix the issue described in this PR comment. Edit the files directly.\n\nComment:\n" + comment
		_, err := exec.Command("claude", "-p", prompt).Output()
		if err != nil {
			return commentErrMsg{err: fmt.Sprintf("claude fix failed: %s", cmdError(err))}
		}
		return commentFixedMsg{}
	}
}

func commentFetchDiff() tea.Cmd {
	return func() tea.Msg {
		msg := fetchDiff()()
		switch msg := msg.(type) {
		case fixDiffMsg:
			return commentDiffMsg{diff: msg.diff}
		case fixErrMsg:
			return commentErrMsg{err: msg.err}
		default:
			return msg
		}
	}
}

func commentCommitAndPush(branch string) tea.Cmd {
	return func() tea.Msg {
		msg := commitAndPush(branch)()
		switch msg.(type) {
		case fixCommittedMsg:
			return commentCommittedMsg{}
		case fixErrMsg:
			return commentErrMsg{err: msg.(fixErrMsg).err}
		default:
			return msg
		}
	}
}

func commentGitReset() tea.Cmd {
	return func() tea.Msg {
		gitReset()()
		return commentResetMsg{}
	}
}

// Init

func (m commentModel) Init() tea.Cmd {
	return nil
}

// Update

func (m commentModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			if m.phase == commentFixing || m.phase == commentCommitting {
				return m, nil
			}
			m.back = true
			return m, tea.Quit
		case "esc":
			switch m.phase {
			case commentBrowse:
				m.back = true
				return m, tea.Quit
			case commentView:
				m.phase = commentBrowse
				m.selected = nil
				return m, nil
			case commentInput:
				m.input = ""
				m.phase = commentView
				return m, nil
			case commentDiffReview:
				return m, commentGitReset()
			case commentError:
				if m.selected != nil {
					m.phase = commentView
					m.errMsg = ""
					return m, nil
				}
				m.back = true
				return m, tea.Quit
			}
		case "up", "k":
			if m.phase == commentBrowse && m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.phase == commentBrowse && m.cursor < len(m.comments)-1 {
				m.cursor++
			}
		case "enter":
			if m.phase == commentBrowse {
				m.selected = &m.comments[m.cursor]
				m.phase = commentView
				return m, nil
			}
			if m.phase == commentInput {
				m.phase = commentFixing
				return m, fixFromComment(m.selected.Body, m.input)
			}
		case "backspace", "ctrl+h":
			if m.phase == commentInput && len(m.input) > 0 {
				runes := []rune(m.input)
				m.input = string(runes[:len(runes)-1])
				return m, nil
			}
		case "f":
			if m.phase == commentView {
				m.phase = commentFixing
				return m, fixFromComment(m.selected.Body, "")
			}
		case "i":
			if m.phase == commentView {
				m.phase = commentInput
				m.input = ""
				return m, nil
			}
		case "y":
			if m.phase == commentDiffReview {
				m.phase = commentCommitting
				return m, commentCommitAndPush(m.pr.HeadRefName)
			}
		case "n":
			if m.phase == commentDiffReview {
				return m, commentGitReset()
			}
		default:
			if m.phase == commentInput {
				if msg.Type == tea.KeyRunes {
					m.input += string(msg.Runes)
					return m, nil
				}
			}
		}

	case commentFixedMsg:
		return m, commentFetchDiff()

	case commentDiffMsg:
		m.diff = msg.diff
		m.phase = commentDiffReview
		return m, nil

	case commentCommittedMsg:
		m.back = true
		return m, tea.Quit

	case commentResetMsg:
		if m.selected != nil {
			m.phase = commentView
			m.diff = ""
			return m, nil
		}
		m.back = true
		return m, tea.Quit

	case commentErrMsg:
		m.errMsg = msg.err
		m.phase = commentError
		return m, nil
	}

	return m, nil
}

// View

func (m commentModel) View() string {
	var b strings.Builder

	b.WriteString(bold.Render(fmt.Sprintf("#%d", m.pr.Number)))
	b.WriteString(" " + m.pr.Title + " ")
	b.WriteString(dim.Render("(" + m.pr.HeadRefName + ")"))
	b.WriteString("\n\n")

	switch m.phase {
	case commentBrowse:
		b.WriteString(" " + bold.Render("Comments:") + "\n")
		for i, c := range m.comments {
			cursor := "  "
			if i == m.cursor {
				cursor = " ▸"
			}
			body := strings.SplitN(c.Body, "\n", 2)[0]
			if len(body) > 60 {
				body = body[:60] + "…"
			}
			b.WriteString(cursor + " " + dim.Render(c.Author.Login+":") + " " + body + "\n")
		}
		b.WriteString("\n" + dim.Render(" ↑/↓ select · enter view · esc back") + "\n")

	case commentView:
		b.WriteString(" " + dim.Render(m.selected.Author.Login+":") + "\n")
		for _, line := range strings.Split(m.selected.Body, "\n") {
			b.WriteString("   " + line + "\n")
		}
		b.WriteString("\n" + dim.Render(" f fix · i instructions · esc back") + "\n")

	case commentInput:
		b.WriteString(" " + dim.Render(m.selected.Author.Login+":") + "\n")
		for _, line := range strings.Split(m.selected.Body, "\n") {
			b.WriteString("   " + line + "\n")
		}
		b.WriteString("\n " + bold.Render("Instructions: ") + m.input + "_\n")
		b.WriteString("\n" + dim.Render(" enter submit · esc cancel") + "\n")

	case commentFixing:
		b.WriteString(dim.Render(" ◌ Claude is fixing the code..."))
		b.WriteString("\n")

	case commentDiffReview:
		b.WriteString(" " + bold.Render("Changes:") + "\n")
		for _, line := range strings.Split(m.diff, "\n") {
			b.WriteString("   " + line + "\n")
		}
		b.WriteString("\n" + dim.Render(" y commit & push · n discard · esc discard") + "\n")

	case commentCommitting:
		b.WriteString(dim.Render(" ◌ Committing and pushing..."))
		b.WriteString("\n")

	case commentError:
		b.WriteString(" " + red.Render("Error: "+m.errMsg) + "\n")
		b.WriteString("\n" + dim.Render(" esc back") + "\n")
	}

	return b.String()
}
