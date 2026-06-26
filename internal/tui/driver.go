package tui

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	"concentus/internal/event"
)

// runLoop consumes messages, applies the reducer, executes returned commands via
// exec, and renders each new model via render. It returns when msgs is closed.
func runLoop(ctx context.Context, msgs <-chan any, exec func([]any), render func(model)) error {
	m := initialModel()
	render(m)
	for {
		select {
		case <-ctx.Done():
			return nil
		case ms, ok := <-msgs:
			if !ok {
				return nil
			}
			var cmds []any
			m, cmds = update(m, ms)
			render(m)
			if len(cmds) > 0 {
				exec(cmds)
			}
		}
	}
}

// Run is the cm tui entry point: raw terminal, input + poll + SSE sources wired
// into runLoop, ANSI screen rendering, guaranteed restore on exit.
func Run(base, token string) error {
	c := NewClient(base, token)
	fd := int(os.Stdin.Fd())

	restore, err := enterRaw(fd)
	if err != nil {
		return err
	}
	defer restore() //nolint:errcheck // best-effort terminal restore

	os.Stdout.WriteString("\x1b[?1049h") // alt screen
	defer os.Stdout.WriteString("\x1b[?1049l\x1b[?25h")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	msgs := make(chan any, 64)
	var focusCancel context.CancelFunc

	// exec turns reducer commands into goroutines that push result messages back.
	exec := func(cmds []any) {
		for _, cmd := range cmds {
			switch v := cmd.(type) {
			case cmdQuit:
				cancel()
			case cmdFocus:
				id := string(v)
				if focusCancel != nil {
					focusCancel()
				}
				var fctx context.Context
				fctx, focusCancel = context.WithCancel(ctx)
				go func() {
					if d, err := c.GetRun(fctx, id); err == nil {
						trySend(msgs, runSnapshot(d))
					}
				}()
				go streamLoop(fctx, c, id, msgs)
			case cmdRefresh:
				// Snapshot-only refresh on a lifecycle event — does NOT touch the
				// SSE stream (that would tear down the stream delivering events and
				// reconnect from seq 0). Uses the parent ctx; a stale snapshot for a
				// run that is no longer focused is dropped by the reducer's guard.
				id := string(v)
				go func() {
					if d, err := c.GetRun(ctx, id); err == nil {
						trySend(msgs, runSnapshot(d))
					}
				}()
			case cmdApprove:
				go func() { trySend(msgs, actionResult{c.Approve(ctx, v.ID, v.Step, v.OK, v.Reason)}) }()
			case cmdCancel:
				go func() { trySend(msgs, actionResult{c.Cancel(ctx, string(v))}) }()
			case cmdRetry:
				go func() { trySend(msgs, actionResult{c.Retry(ctx, string(v))}) }()
			}
		}
	}

	// Input reader.
	go readKeys(ctx, os.Stdin, msgs)
	// Poll loop.
	go func() {
		t := time.NewTicker(1500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if runs, err := c.ListRuns(ctx); err == nil {
					trySend(msgs, runsLoaded(runs))
				} else {
					trySend(msgs, connMsg(false))
				}
			}
		}
	}()
	// Resize -> just re-render on next message; also handle SIGWINCH to force a frame.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-winch:
				trySend(msgs, redrawMsg{}) // force a re-render without touching conn
			}
		}
	}()

	render := func(m model) {
		w, h, err := size(fd)
		if err != nil || w == 0 {
			w, h = 80, 24
		}
		os.Stdout.WriteString("\x1b[H\x1b[2J") // home + clear
		os.Stdout.WriteString(view(m, w, h))
	}

	// Kick an initial fetch.
	go func() {
		if runs, err := c.ListRuns(ctx); err == nil {
			trySend(msgs, runsLoaded(runs))
		}
	}()

	return runLoop(ctx, msgs, exec, render)
}

func trySend(ch chan any, m any) {
	select {
	case ch <- m:
	default:
	}
}

// streamLoop keeps the per-run SSE stream open, reconnecting until ctx ends.
// A non-2xx response (run gone / server refusing) is permanent — stop, don't
// hammer-reconnect. Transport errors and clean EOF remain retryable.
func streamLoop(ctx context.Context, c *Client, id string, msgs chan any) {
	var last int64
	for ctx.Err() == nil {
		err := c.StreamEvents(ctx, id, last, func(e event.Event) {
			if e.Seq > last {
				last = e.Seq
			}
			trySend(msgs, sseEvent(e))
		})
		var se *streamStatusError
		if errors.As(err, &se) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond): // backoff before reconnect
		}
	}
}

// readKeys reads single bytes and forwards them as keyMsg.
func readKeys(ctx context.Context, in *os.File, msgs chan any) {
	buf := make([]byte, 1)
	for ctx.Err() == nil {
		n, err := in.Read(buf)
		if err != nil {
			return
		}
		if n == 1 {
			trySend(msgs, keyMsg(rune(buf[0])))
		}
	}
}
