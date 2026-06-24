// Command cm is the thin CLI client for magisterd: pure HTTP calls, no
// orchestration logic. Every subcommand is scriptable (--json, exit codes).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"concentus/internal/tui"
)

// approve retry window for the transient 409 a resumed run briefly returns before
// its gate re-registers. Package vars so tests can shrink the interval.
var (
	approveRetryFor   = 10 * time.Second
	approveRetryEvery = 100 * time.Millisecond
)

func main() {
	base := os.Getenv("MAGISTER_ADDR")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	os.Exit(dispatch(os.Args[1:], base, os.Stdout))
}

func dispatch(args []string, base string, out io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(out, "usage: cm <run|ls|get|watch|approve|reject|cancel|retry|push|pr|ship|gc|rm|loglevel|tui> ...")
		return 2
	}
	c := &client{
		base:      base,
		http:      &http.Client{Timeout: 30 * time.Second},
		watchHTTP: &http.Client{Timeout: 0},
	}
	switch args[0] {
	case "run":
		return c.run(args[1:], out)
	case "ls":
		return c.get("/v1/runs", out)
	case "get":
		if len(args) < 2 {
			fmt.Fprintln(out, "usage: cm get <run>")
			return 2
		}
		return c.get("/v1/runs/"+args[1], out)
	case "watch":
		if len(args) < 2 {
			fmt.Fprintln(out, "usage: cm watch <run>")
			return 2
		}
		return c.watch(args[1], out)
	case "approve":
		return c.approve(args[1:], true, out)
	case "reject":
		return c.approve(args[1:], false, out)
	case "cancel":
		if len(args) < 2 {
			fmt.Fprintln(out, "usage: cm cancel <run>")
			return 2
		}
		return c.delete("/v1/runs/"+args[1], out)
	case "retry":
		return c.retry(args[1:], out)
	case "gc":
		return c.gc(args[1:], out)
	case "rm":
		if len(args) < 2 {
			fmt.Fprintln(out, "usage: cm rm <run>")
			return 2
		}
		return c.rm(args[1], out)
	case "push":
		return c.push(args[1:], out)
	case "pr":
		return c.pr(args[1:], out)
	case "ship":
		return c.ship(args[1:], out)
	case "loglevel":
		return c.loglevel(args[1:], out)
	case "tui":
		if err := tui.Run(base, os.Getenv("MAGISTER_BEARER_TOKEN")); err != nil {
			fmt.Fprintln(os.Stderr, "tui:", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(out, "unknown command %q\n", args[0])
		return 2
	}
}

type client struct {
	base      string
	http      *http.Client // 30s timeout; used by all commands except watch
	watchHTTP *http.Client // no timeout; used for SSE streams
}

func (c *client) run(args []string, out io.Writer) int {
	watch := false
	var path, repo, base string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--watch":
			watch = true
		case "--repo":
			i++
			if i >= len(args) {
				fmt.Fprintln(out, "usage: --repo requires a value")
				return 2
			}
			repo = args[i]
		case "--base":
			i++
			if i >= len(args) {
				fmt.Fprintln(out, "usage: --base requires a value")
				return 2
			}
			base = args[i]
		default:
			path = args[i]
		}
	}
	if path == "" {
		fmt.Fprintln(out, "usage: cm run <flow.yaml> [--repo <path>] [--base <ref>] [--watch]")
		return 2
	}
	body, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(out, "read flow:", err)
		return 1
	}
	endpoint := c.base + "/v1/runs"
	if repo != "" {
		q := url.Values{}
		q.Set("repo", repo)
		if base != "" {
			q.Set("base", base)
		}
		endpoint += "?" + q.Encode()
	}
	resp, err := c.http.Post(endpoint, "application/x-yaml", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(out, "submit:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return printErr(resp, out)
	}
	var rr struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&rr)
	fmt.Fprintln(out, rr.ID)
	if watch {
		return c.watch(rr.ID, out)
	}
	return 0
}

func (c *client) watch(id string, out io.Writer) int {
	resp, err := c.watchHTTP.Get(c.base + "/v1/runs/" + id + "/events")
	if err != nil {
		fmt.Fprintln(out, "watch:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return printErr(resp, out)
	}
	_, _ = io.Copy(out, resp.Body) // stream SSE frames verbatim until run.done closes it
	return 0
}

func (c *client) approve(args []string, approve bool, out io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(out, "usage: cm approve|reject <run> <step> [reason]")
		return 2
	}
	reason := ""
	if len(args) >= 3 {
		reason = args[2]
	}
	body, _ := json.Marshal(map[string]any{"approve": approve, "reason": reason})
	url := c.base + "/v1/runs/" + args[0] + "/steps/" + args[1] + "/approve"

	deadline := time.Now().Add(approveRetryFor)
	for {
		resp, err := c.http.Post(url, "application/json", bytes.NewReader(body))
		if err != nil {
			fmt.Fprintln(out, "approve:", err)
			return 1
		}
		if resp.StatusCode == http.StatusConflict && time.Now().Before(deadline) {
			resp.Body.Close()
			time.Sleep(approveRetryEvery)
			continue
		}
		if resp.StatusCode == http.StatusConflict {
			// retried until the deadline; the gate never registered.
			resp.Body.Close()
			fmt.Fprintf(out, "approve: gate not ready after %s\n", approveRetryFor)
			return 1
		}
		if resp.StatusCode != http.StatusOK {
			code := printErr(resp, out)
			resp.Body.Close()
			return code
		}
		resp.Body.Close()
		fmt.Fprintln(out, "ok")
		return 0
	}
}

func (c *client) get(path string, out io.Writer) int {
	resp, err := c.http.Get(c.base + path)
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return printErr(resp, out)
	}
	_, _ = io.Copy(out, resp.Body)
	fmt.Fprintln(out)
	return 0
}

func (c *client) delete(path string, out io.Writer) int {
	req, _ := http.NewRequest(http.MethodDelete, c.base+path, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return printErr(resp, out)
	}
	fmt.Fprintln(out, "canceled")
	return 0
}

func (c *client) retry(args []string, out io.Writer) int {
	watch := false
	var run string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--watch":
			watch = true
		default:
			run = args[i]
		}
	}
	if run == "" {
		fmt.Fprintln(out, "usage: cm retry <run> [--watch]")
		return 2
	}
	resp, err := c.http.Post(c.base+"/v1/runs/"+run+"/retry", "application/json", nil)
	if err != nil {
		fmt.Fprintln(out, "retry:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return printErr(resp, out)
	}
	var rr struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&rr)
	fmt.Fprintln(out, "resuming", rr.ID)
	if watch {
		return c.watch(rr.ID, out)
	}
	return 0
}

func (c *client) gc(args []string, out io.Writer) int {
	path := "/v1/gc"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--older-than":
			if i+1 >= len(args) {
				fmt.Fprintln(out, "usage: --older-than requires a value")
				return 2
			}
			i++
			path = "/v1/gc?older_than=" + url.QueryEscape(args[i])
		default:
			fmt.Fprintln(out, "usage: cm gc [--older-than <dur>]")
			return 2
		}
	}
	resp, err := c.http.Post(c.base+path, "application/json", nil)
	if err != nil {
		fmt.Fprintln(out, "gc:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return printErr(resp, out)
	}
	var body struct {
		Reclaimed int `json:"reclaimed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		fmt.Fprintln(out, "gc: decode:", err)
		return 1
	}
	fmt.Fprintf(out, "reclaimed %d\n", body.Reclaimed)
	return 0
}

func (c *client) rm(run string, out io.Writer) int {
	req, _ := http.NewRequest(http.MethodDelete, c.base+"/v1/runs/"+run+"/scratch", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		fmt.Fprintln(out, "rm:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return printErr(resp, out)
	}
	var body struct {
		Removed bool `json:"removed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		fmt.Fprintln(out, "rm: decode:", err)
		return 1
	}
	if body.Removed {
		fmt.Fprintln(out, "removed")
	} else {
		fmt.Fprintln(out, "already gone")
	}
	return 0
}

func (c *client) push(args []string, out io.Writer) int {
	var run, remote, as, step string
	force := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--force":
			force = true
		case "--remote":
			i++
			if i >= len(args) {
				fmt.Fprintln(out, "usage: --remote requires a value")
				return 2
			}
			remote = args[i]
		case "--as":
			i++
			if i >= len(args) {
				fmt.Fprintln(out, "usage: --as requires a value")
				return 2
			}
			as = args[i]
		case "--step":
			i++
			if i >= len(args) {
				fmt.Fprintln(out, "usage: --step requires a value")
				return 2
			}
			step = args[i]
		default:
			run = args[i]
		}
	}
	if run == "" {
		fmt.Fprintln(out, "usage: cm push <run> [--remote <url-or-name>] [--as <branch>] [--step <id>] [--force]")
		return 2
	}
	q := url.Values{}
	if remote != "" {
		q.Set("remote", remote)
	}
	if as != "" {
		q.Set("as", as)
	}
	if step != "" {
		q.Set("step", step)
	}
	if force {
		q.Set("force", "true")
	}
	endpoint := c.base + "/v1/runs/" + run + "/push"
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	resp, err := c.http.Post(endpoint, "application/json", nil)
	if err != nil {
		fmt.Fprintln(out, "push:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return printErr(resp, out)
	}
	var pr struct {
		Remote       string `json:"remote"`
		Branch       string `json:"branch"`
		SourceBranch string `json:"source_branch"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		fmt.Fprintln(out, "push: decode response:", err)
		return 1
	}
	fmt.Fprintf(out, "pushed %s → %s on %s\n", pr.SourceBranch, pr.Branch, pr.Remote)
	return 0
}

func (c *client) pr(args []string, out io.Writer) int {
	var run string
	body := map[string]any{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--draft":
			body["draft"] = true
		case "--head-repo":
			i++
			if i >= len(args) {
				fmt.Fprintln(out, "usage: --head-repo requires a value")
				return 2
			}
			body["head_repo"] = args[i]
		case "--remote", "--as", "--step", "--base", "--title", "--body":
			flag := args[i]
			i++
			if i >= len(args) {
				fmt.Fprintf(out, "usage: %s requires a value\n", flag)
				return 2
			}
			body[flag[2:]] = args[i] // strip "--"
		default:
			run = args[i]
		}
	}
	if run == "" {
		fmt.Fprintln(out, "usage: cm pr <run> [--remote <url-or-name>] [--head-repo <url-or-name>] [--as <branch>] [--step <id>] [--base <branch>] [--title <t>] [--body <b>] [--draft]")
		return 2
	}
	payload, _ := json.Marshal(body)
	resp, err := c.http.Post(c.base+"/v1/runs/"+run+"/pr", "application/json", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintln(out, "pr:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return printErr(resp, out)
	}
	var pr struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		fmt.Fprintln(out, "pr: decode response:", err)
		return 1
	}
	fmt.Fprintln(out, "opened", pr.URL)
	return 0
}

func (c *client) ship(args []string, out io.Writer) int {
	var run string
	body := map[string]any{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--draft":
			body["draft"] = true
		case "--force":
			body["force"] = true
		case "--head-repo":
			i++
			if i >= len(args) {
				fmt.Fprintln(out, "usage: --head-repo requires a value")
				return 2
			}
			body["head_repo"] = args[i]
		case "--remote", "--as", "--step", "--base", "--title", "--body":
			flag := args[i]
			i++
			if i >= len(args) {
				fmt.Fprintf(out, "usage: %s requires a value\n", flag)
				return 2
			}
			body[flag[2:]] = args[i] // strip "--"
		default:
			run = args[i]
		}
	}
	if run == "" {
		fmt.Fprintln(out, "usage: cm ship <run> [--remote <url-or-name>] [--head-repo <url-or-name>] [--as <branch>] [--step <id>] [--base <branch>] [--title <t>] [--body <b>] [--draft] [--force]")
		return 2
	}
	payload, _ := json.Marshal(body)
	resp, err := c.http.Post(c.base+"/v1/runs/"+run+"/ship", "application/json", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintln(out, "ship:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return printErr(resp, out)
	}
	var sr struct {
		Pushed struct {
			Remote       string `json:"remote"`
			Branch       string `json:"branch"`
			SourceBranch string `json:"source_branch"`
		} `json:"pushed"`
		PR struct {
			URL string `json:"url"`
		} `json:"pr"`
		PRExisted bool `json:"pr_existed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		fmt.Fprintln(out, "ship: decode response:", err)
		return 1
	}
	fmt.Fprintf(out, "pushed %s → %s on %s\n", sr.Pushed.SourceBranch, sr.Pushed.Branch, sr.Pushed.Remote)
	verb := "opened"
	if sr.PRExisted {
		verb = "exists"
	}
	fmt.Fprintln(out, verb, sr.PR.URL)
	return 0
}

// logLevelBody is the JSON body cm sends to POST /v1/loglevel.
type logLevelBody struct {
	Level string `json:"level"`
}

// loglevel reports (no arg) or sets (one arg) the daemon's live log threshold.
func (c *client) loglevel(args []string, out io.Writer) int {
	if len(args) == 0 {
		return c.get("/v1/loglevel", out)
	}
	body, err := json.Marshal(logLevelBody{Level: args[0]})
	if err != nil {
		fmt.Fprintln(out, "loglevel:", err)
		return 1
	}
	resp, err := c.http.Post(c.base+"/v1/loglevel", "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(out, "loglevel:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return printErr(resp, out)
	}
	_, _ = io.Copy(out, resp.Body)
	fmt.Fprintln(out)
	return 0
}
func printErr(resp *http.Response, out io.Writer) int {
	b, _ := io.ReadAll(resp.Body)
	fmt.Fprintf(out, "error (%d): %s\n", resp.StatusCode, bytes.TrimSpace(b))
	return 1
}
