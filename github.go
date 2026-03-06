package main

import (
	"encoding/json"
	"errors"
	"os/exec"
	"sort"
	"strings"
)

// gh pr view JSON shape

type ghPR struct {
	Number            int             `json:"number"`
	Title             string          `json:"title"`
	HeadRefName       string          `json:"headRefName"`
	ReviewDecision    string          `json:"reviewDecision"`
	StatusCheckRollup json.RawMessage `json:"statusCheckRollup"`
	Reviews           []ghReview      `json:"reviews"`
	ReviewRequests    []ghReviewReq   `json:"reviewRequests"`
	Comments          []ghComment     `json:"comments"`
}

type ghReview struct {
	State       string   `json:"state"`
	SubmittedAt string   `json:"submittedAt"`
	Author      ghAuthor `json:"author"`
}

type ghReviewReq struct {
	Login string `json:"login"`
	Name  string `json:"name"`
	Slug  string `json:"slug"`
}

type ghComment struct {
	Author    ghAuthor `json:"author"`
	Body      string   `json:"body"`
	CreatedAt string   `json:"createdAt"`
}

type ghAuthor struct {
	Login string `json:"login"`
}

// Parsed check

type check struct {
	Name       string
	Status     string // "COMPLETED" or "IN_PROGRESS"
	Conclusion string // "SUCCESS", "FAILURE", "NEUTRAL", "SKIPPED", etc.
}

func parseChecks(raw json.RawMessage) []check {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}

	var checks []check
	for _, item := range items {
		var m map[string]any
		if err := json.Unmarshal(item, &m); err != nil {
			continue
		}

		var c check
		if m["__typename"] == "StatusContext" {
			c.Name = str(m["context"])
			state := str(m["state"])
			if state == "PENDING" || state == "EXPECTED" {
				c.Status = "IN_PROGRESS"
			} else {
				c.Status = "COMPLETED"
			}
			c.Conclusion = state
		} else {
			c.Name = str(m["name"])
			c.Status = str(m["status"])
			c.Conclusion = str(m["conclusion"])
		}
		if c.Name == "" {
			c.Name = "-"
		}
		checks = append(checks, c)
	}

	sort.SliceStable(checks, func(i, j int) bool {
		ci, cj := checks[i], checks[j]
		iPending := ci.Status != "COMPLETED"
		jPending := cj.Status != "COMPLETED"
		if iPending != jPending {
			return iPending
		}
		return ci.Name < cj.Name
	})

	return checks
}

func str(v any) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

func isSuccess(conclusion string) bool {
	return conclusion == "SUCCESS" || conclusion == "NEUTRAL" || conclusion == "SKIPPED"
}

// latestReviews returns the most recent review per author.
func latestReviews(reviews []ghReview) map[string]ghReview {
	m := make(map[string]ghReview)
	for _, r := range reviews {
		if existing, ok := m[r.Author.Login]; !ok || r.SubmittedAt > existing.SubmittedAt {
			m[r.Author.Login] = r
		}
	}
	return m
}

// resolveReviewDecision computes an effective review decision from actual
// review data when the API returns an empty ReviewDecision (no branch
// protection review policy).
func resolveReviewDecision(decision string, reviews []ghReview) string {
	if decision != "" {
		return decision
	}
	latest := latestReviews(reviews)
	hasApproval := false
	for _, r := range latest {
		switch r.State {
		case "CHANGES_REQUESTED":
			return "CHANGES_REQUESTED"
		case "APPROVED":
			hasApproval = true
		}
	}
	if hasApproval {
		return "APPROVED"
	}
	return ""
}

func cmdError(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if msg := strings.TrimSpace(string(exitErr.Stderr)); msg != "" {
			return msg
		}
	}
	return err.Error()
}
