package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type RunSummary struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type StepView struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Attempt int    `json:"attempt"`
}

type RunDetail struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	Status string     `json:"status"`
	Steps  []StepView `json:"steps"`
}

// Client is a thin REST client for magisterd's /v1 API.
type Client struct {
	base  string
	token string
	hc    *http.Client
}

func NewClient(base, token string) *Client {
	return &Client{base: base, token: token, hc: &http.Client{Timeout: 15 * time.Second}}
}

func (c *Client) newReq(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

// do executes req and, on a non-2xx, returns an error carrying the status.
func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(b))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) ListRuns(ctx context.Context) ([]RunSummary, error) {
	req, err := c.newReq(ctx, http.MethodGet, "/v1/runs", nil)
	if err != nil {
		return nil, err
	}
	var out []RunSummary
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) GetRun(ctx context.Context, id string) (RunDetail, error) {
	req, err := c.newReq(ctx, http.MethodGet, "/v1/runs/"+id, nil)
	if err != nil {
		return RunDetail{}, err
	}
	var out RunDetail
	if err := c.do(req, &out); err != nil {
		return RunDetail{}, err
	}
	return out, nil
}

func (c *Client) Approve(ctx context.Context, id, step string, approve bool, reason string) error {
	body, _ := json.Marshal(map[string]any{"approve": approve, "reason": reason})
	req, err := c.newReq(ctx, http.MethodPost, "/v1/runs/"+id+"/steps/"+step+"/approve", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, nil)
}

func (c *Client) Cancel(ctx context.Context, id string) error {
	req, err := c.newReq(ctx, http.MethodDelete, "/v1/runs/"+id, nil)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

func (c *Client) Retry(ctx context.Context, id string) error {
	req, err := c.newReq(ctx, http.MethodPost, "/v1/runs/"+id+"/retry", nil)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}
