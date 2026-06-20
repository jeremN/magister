package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"concentus/internal/core"
	"concentus/internal/flow"
	"concentus/internal/host"
	"concentus/internal/workspace"
)

// PROpts configures PR. Zero values mean: origin remote, magister/<runID> head, the
// unique terminal step (for the body summary), the repo's default base branch,
// generated title/body, not a draft.
type PROpts struct {
	Remote, As, Step, Base, Title, Body string
	Draft                               bool
}

// PRResult is returned by PR on success.
type PRResult struct {
	URL, Repo, Head, Base string
	Draft                 bool
}

// PRError carries an HTTP status so the API maps failures without string-matching.
type PRError struct {
	Status int
	Msg    string
}

func (e *PRError) Error() string { return e.Msg }

func prErr(status int, format string, a ...any) *PRError {
	return &PRError{Status: status, Msg: fmt.Sprintf(format, a...)}
}

// prCore does the PR work and reports whether an open PR already existed. On an
// already-existing PR it returns (PRResult{URL:…}, true, nil); on a newly created
// PR (PRResult{URL:…}, false, nil); on failure (PRResult{}, false, *PRError). It is
// the shared core of PR (strict: existing→409) and Ship (idempotent: existing→ok).
func (s *Supervisor) prCore(ctx context.Context, runID core.RunID, opts PROpts) (PRResult, bool, error) {
	rs, err := s.store.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, core.ErrRunNotFound) {
			return PRResult{}, false, prErr(http.StatusNotFound, "unknown run %q", runID)
		}
		return PRResult{}, false, prErr(http.StatusInternalServerError, "load run %q: %v", runID, err)
	}
	if rs.Repo == "" {
		return PRResult{}, false, prErr(http.StatusBadRequest, "run %q is not an external-repo run (no --repo)", runID)
	}
	if rs.Status != core.RunSucceeded {
		return PRResult{}, false, prErr(http.StatusConflict, "run %q is %s, not succeeded", runID, rs.Status)
	}
	head := opts.As
	if head == "" {
		head = "magister/" + string(runID)
	}
	if !safePRRef(head) {
		return PRResult{}, false, prErr(http.StatusBadRequest, "invalid head branch %q", head)
	}
	if opts.Base != "" && !safePRRef(opts.Base) {
		return PRResult{}, false, prErr(http.StatusBadRequest, "invalid base branch %q", opts.Base)
	}
	remoteURL, err := workspace.ResolveRemote(rs.Repo, opts.Remote)
	if err != nil {
		return PRResult{}, false, prErr(http.StatusBadRequest, "remote: %v", err)
	}
	_, owner, repo, err := host.ParseRemote(remoteURL)
	if err != nil {
		return PRResult{}, false, prErr(http.StatusBadRequest, "%v", err)
	}
	f, err := flow.ParseBytes([]byte(rs.FlowYAML))
	if err != nil {
		return PRResult{}, false, prErr(http.StatusInternalServerError, "parse stored flow: %v", err)
	}
	term, perr := pickResultStep(f, opts.Step)
	if perr != nil {
		return PRResult{}, false, prErr(perr.Status, "%s", perr.Msg)
	}
	title := opts.Title
	if title == "" {
		title = defaultPRTitle(rs)
	}
	body := opts.Body
	if body == "" {
		body = generatePRBody(rs, term)
	}

	runner := s.hostRunner()
	if url, exists, err := runner.ExistingOpenPR(ctx, owner, repo, head); err != nil {
		return PRResult{}, false, prErr(http.StatusBadGateway, "%v", err)
	} else if exists {
		return PRResult{URL: url, Repo: owner + "/" + repo, Head: head, Base: opts.Base, Draft: opts.Draft}, true, nil
	}

	url, err := runner.CreatePR(ctx, host.CreateOpts{
		Owner: owner, Repo: repo, Head: head, Base: opts.Base,
		Title: title, Body: body, Draft: opts.Draft,
	})
	if err != nil {
		if !runner.BranchExists(ctx, owner, repo, head) {
			return PRResult{}, false, prErr(http.StatusConflict, "branch %q not on remote; run `cm push %s` first", head, runID)
		}
		return PRResult{}, false, prErr(http.StatusBadGateway, "%v", err)
	}
	return PRResult{URL: url, Repo: owner + "/" + repo, Head: head, Base: opts.Base, Draft: opts.Draft}, false, nil
}

// PR opens a pull request on the host repo for a succeeded external-repo run. It is
// a post-run, store-driven operation (engine untouched, no scratch clone). An
// already-open PR for the head branch is a 409 carrying its URL. See the slice-3
// spec; Ship reuses prCore for the idempotent variant.
func (s *Supervisor) PR(ctx context.Context, runID core.RunID, opts PROpts) (PRResult, error) {
	res, existed, err := s.prCore(ctx, runID, opts)
	if err != nil {
		return PRResult{}, err
	}
	if existed {
		return PRResult{}, prErr(http.StatusConflict, "PR already exists for %s: %s", res.Head, res.URL)
	}
	return res, nil
}

// safePRRef guards a branch ref passed to gh (head/base): rejects empty, a leading
// "-", "..", a trailing "." or ".lock", and anything outside [A-Za-z0-9/._-] — so a
// user-supplied --as/--base cannot smuggle a flag or traverse the gh api path. Mirrors
// workspace.safeRef (Slice 2). The default head magister/<runID> and base main pass.
func safePRRef(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") || strings.Contains(s, "..") ||
		strings.HasSuffix(s, ".") || strings.HasSuffix(s, ".lock") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/' || r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

// defaultPRTitle builds the title when --title is not given: the flow name + a short id.
func defaultPRTitle(rs core.RunState) string {
	id := string(rs.ID)
	short := id
	if len(id) > 8 {
		short = id[len(id)-8:]
	}
	name := rs.Name
	if name == "" {
		name = "run"
	}
	return fmt.Sprintf("magister: %s (%s)", name, short)
}

// generatePRBody builds the body when --body is not given: a small run summary from
// store data (flow name, run id, the result step + its commit, the step list).
func generatePRBody(rs core.RunState, term *flow.Step) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## magister run `%s` — %s\n\n", rs.ID, rs.Name)
	b.WriteString("Delivered by concentus-magister.\n\n")
	if term != nil {
		if _, commit := stepBranch(rs, term.ID); commit != "" {
			fmt.Fprintf(&b, "**Result:** step `%s` (commit %s)\n\n", term.ID, shortSHA(commit))
		} else {
			fmt.Fprintf(&b, "**Result:** step `%s`\n\n", term.ID)
		}
	}
	b.WriteString("**Steps:**\n")
	for _, st := range rs.Steps {
		fmt.Fprintf(&b, "- `%s` %s\n", st.StepID, statusMark(st.Status))
	}
	return b.String()
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func statusMark(s core.StepStatus) string {
	if s == core.StepSucceeded {
		return "✓"
	}
	return string(s)
}
