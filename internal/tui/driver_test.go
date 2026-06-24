package tui

import (
	"context"
	"testing"
	"time"
)

// runLoop must process messages, apply update, render via render(), and exit on cmdQuit.
func TestRunLoopRendersAndQuits(t *testing.T) {
	msgs := make(chan any, 8)
	var lastFrame string
	rendered := make(chan struct{}, 16)
	exec := func(cmds []any) {
		for _, c := range cmds {
			if _, ok := c.(cmdQuit); ok {
				close(msgs)
			}
		}
	}
	render := func(m model) {
		lastFrame = view(m, 80, 24)
		select {
		case rendered <- struct{}{}:
		default:
		}
	}
	go func() {
		msgs <- runsLoaded{{ID: "a1", Name: "feature", Status: "running"}}
		<-rendered
		msgs <- keyMsg('q')
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runLoop(ctx, msgs, exec, render); err != nil {
		t.Fatal(err)
	}
	if lastFrame == "" {
		t.Fatal("expected at least one rendered frame")
	}
}
