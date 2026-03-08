package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

var (
	version = "dev"
	commit  = ""
	date    = ""
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("sigr %s\n", version)
		if commit != "" {
			fmt.Printf("  commit: %s\n", commit)
			fmt.Printf("  built:  %s\n", date)
		}
		return
	}

	interval := 10 * time.Second
	if s := os.Getenv("SIGR_INTERVAL"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			interval = time.Duration(n) * time.Second
		}
	}

	if len(os.Args) > 1 && os.Args[1] == "list" {
		runList(interval, nil)
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "pr" {
		prRef := runCreatePR(os.Args[2:])
		runWatch(prRef, interval, nil)
		return
	}

	prRef := ""
	if len(os.Args) > 1 {
		prRef = os.Args[1]
	}

	runWatch(prRef, interval, nil)
}

func runWatch(prRef string, interval time.Duration, ls *listState) {
	m := model{
		prRef:     prRef,
		startedAt: time.Now().UTC(),
		interval:  interval,
		fromList:  ls != nil,
		listState: ls,
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fm := finalModel.(model)
	if fm.fix && fm.pr != nil {
		runFix(fm.prRef, fm.pr, interval, fm.fromList, fm.listState)
		return
	}
	if fm.comments && fm.pr != nil {
		runComments(fm.prRef, fm.pr, interval, fm.fromList, fm.listState)
		return
	}
	if fm.back {
		runList(interval, fm.listState)
		return
	}
	if fm.err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", fm.err)
		os.Exit(1)
	}
	os.Exit(fm.exitCode)
}

func runCreatePR(args []string) string {
	ghArgs := append([]string{"pr", "create"}, args...)
	cmd := exec.Command("gh", ghArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stderr // gh prints the PR URL to stdout, show it to user via stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "gh pr create failed: %s\n", cmdError(err))
		os.Exit(1)
	}

	// Get the PR number for the current branch
	out, err := exec.Command("gh", "pr", "view", "--json", "number").Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get PR number: %s\n", cmdError(err))
		os.Exit(1)
	}
	var pr struct{ Number int }
	if err := json.Unmarshal(out, &pr); err != nil {
		fmt.Fprintf(os.Stderr, "failed to parse PR: %v\n", err)
		os.Exit(1)
	}
	return strconv.Itoa(pr.Number)
}

func runFix(prRef string, pr *ghPR, interval time.Duration, fromList bool, ls *listState) {
	m := fixModel{
		prRef:    prRef,
		pr:       pr,
		fromList: fromList,
		ls:       ls,
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fm := finalModel.(fixModel)
	if fm.back {
		runWatch(prRef, interval, ls)
		return
	}
}

func runComments(prRef string, pr *ghPR, interval time.Duration, fromList bool, ls *listState) {
	m := commentModel{
		prRef:    prRef,
		pr:       pr,
		fromList: fromList,
		ls:       ls,
		comments: pr.Comments,
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fm := finalModel.(commentModel)
	if fm.back {
		runWatch(prRef, interval, ls)
		return
	}
}
