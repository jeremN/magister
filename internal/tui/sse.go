package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"concentus/internal/event"
)

// parseEvents reads the SSE framing (`id:`/`event:`/`data:` lines, blank line
// terminates a frame) and calls emit for each event whose data decodes. It
// returns nil at clean EOF.
func parseEvents(r io.Reader, emit func(event.Event)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "": // frame boundary
			if data != "" {
				var e event.Event
				if json.Unmarshal([]byte(data), &e) == nil {
					emit(e)
				}
				data = ""
			}
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		default:
			// id: / event: lines are redundant with the JSON payload; ignore.
		}
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		return err
	}
	return nil
}

// StreamEvents opens the per-run SSE stream and feeds parseEvents until the
// stream ends or ctx is cancelled. lastSeq>0 is sent as Last-Event-ID to resume.
func (c *Client) StreamEvents(ctx context.Context, id string, lastSeq int64, emit func(event.Event)) error {
	req, err := c.newReq(ctx, http.MethodGet, "/v1/runs/"+id+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastSeq > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(lastSeq, 10))
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return parseEvents(resp.Body, emit)
}
