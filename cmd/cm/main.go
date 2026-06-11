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
		fmt.Fprintln(out, "usage: cm <run|ls|get|watch|approve|reject|cancel> ...")
		return 2
	}
	c := &client{base: base, http: &http.Client{Timeout: 0}}
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
	default:
		fmt.Fprintf(out, "unknown command %q\n", args[0])
		return 2
	}
}

type client struct {
	base string
	http *http.Client
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
	resp, err := c.http.Get(c.base + "/v1/runs/" + id + "/events")
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

func printErr(resp *http.Response, out io.Writer) int {
	b, _ := io.ReadAll(resp.Body)
	fmt.Fprintf(out, "error (%d): %s\n", resp.StatusCode, bytes.TrimSpace(b))
	return 1
}
