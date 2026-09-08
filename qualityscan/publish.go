package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Publishing is idempotent by construction: every issue body carries an HTML
// comment marker naming its rule, and a publish run matches on that marker
// before it decides to create anything. Re-running the scan updates the issues
// it opened last time rather than opening a second set.
//
// Matching is done by reading the issue list and comparing bodies locally,
// not with `gh issue list --search`. GitHub's search index lags behind writes
// by seconds to minutes, so a search-based check would happily create a
// duplicate of an issue opened moments earlier.

// existingIssue is the subset of an issue the publisher needs.
type existingIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
}

// PublishOptions controls a publish run.
type PublishOptions struct {
	Repo    string // owner/name; empty means whatever `gh` infers from the cwd
	DryRun  bool
	Reopen  bool // update issues that were closed, instead of leaving them shut
	Limit   int  // how many existing issues to read when looking for markers
	Verbose bool
}

// Publish creates or updates one tracker issue per proposed issue and reports
// what it did. With DryRun set it reads the tracker but writes nothing.
func Publish(w io.Writer, issues []Issue, opts PublishOptions) error {
	if _, err := exec.LookPath("gh"); err != nil {
		return fmt.Errorf("the GitHub CLI (gh) is not on PATH; install it or use -format markdown")
	}
	if opts.Limit <= 0 {
		opts.Limit = 500
	}

	existing, err := listIssues(opts)
	if err != nil {
		return err
	}

	created, updated, skipped := 0, 0, 0
	for _, iss := range issues {
		prior, found := matchByMarker(existing, iss.Marker)

		switch {
		case found && prior.State == "CLOSED" && !opts.Reopen:
			// Someone closed it on purpose. Re-opening it on every scan would
			// make the tool argue with the people using it.
			fmt.Fprintf(w, "skip    #%d  %s (closed; pass -reopen to update anyway)\n", prior.Number, iss.Title)
			skipped++

		case found:
			fmt.Fprintf(w, "update  #%d  %s (%d findings)\n", prior.Number, iss.Title, iss.Findings)
			if !opts.DryRun {
				if err := ghEditIssue(prior.Number, iss, opts); err != nil {
					return err
				}
			}
			updated++

		default:
			fmt.Fprintf(w, "create  new  %s (%d findings)\n", iss.Title, iss.Findings)
			if !opts.DryRun {
				if err := ghCreateIssue(iss, opts); err != nil {
					return err
				}
			}
			created++
		}
	}

	verb := "published"
	if opts.DryRun {
		verb = "would publish"
	}
	fmt.Fprintf(w, "\n%s: %d created, %d updated, %d skipped\n", verb, created, updated, skipped)
	if opts.DryRun {
		fmt.Fprintln(w, "dry run: nothing was written to the tracker. Re-run with -publish to apply.")
	}
	return nil
}

func matchByMarker(existing []existingIssue, marker string) (existingIssue, bool) {
	for _, e := range existing {
		if strings.Contains(e.Body, marker) {
			return e, true
		}
	}
	return existingIssue{}, false
}

func listIssues(opts PublishOptions) ([]existingIssue, error) {
	args := []string{"issue", "list", "--state", "all",
		"--limit", fmt.Sprint(opts.Limit), "--json", "number,title,body,state"}
	out, err := gh(opts, args...)
	if err != nil {
		return nil, err
	}
	var issues []existingIssue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("parse gh issue list output: %w", err)
	}
	return issues, nil
}

func ghCreateIssue(iss Issue, opts PublishOptions) error {
	args := []string{"issue", "create", "--title", iss.Title, "--body", iss.Body}
	for _, l := range iss.Labels {
		args = append(args, "--label", l)
	}
	_, err := gh(opts, args...)
	return err
}

func ghEditIssue(number int, iss Issue, opts PublishOptions) error {
	// Labels are added, never removed: someone may have re-triaged the issue,
	// and a scan has no business overruling that.
	args := []string{"issue", "edit", fmt.Sprint(number), "--body", iss.Body}
	for _, l := range iss.Labels {
		args = append(args, "--add-label", l)
	}
	_, err := gh(opts, args...)
	return err
}

func gh(opts PublishOptions, args ...string) ([]byte, error) {
	if opts.Repo != "" {
		args = append(args, "--repo", opts.Repo)
	}
	cmd := exec.Command("gh", args...)
	cmd.Stderr = os.Stderr
	if opts.Verbose {
		fmt.Fprintf(os.Stderr, "+ gh %s\n", strings.Join(args, " "))
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh %s: %w", strings.Join(args[:2], " "), err)
	}
	return out, nil
}
