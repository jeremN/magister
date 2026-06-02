package event

import (
	"testing"
	"time"
)

func TestBusDeliversToSubscriber(t *testing.T) {
	b := NewBus()
	ch, unsub := b.Subscribe(4)
	defer unsub()

	b.Publish(Event{RunID: "r1", Kind: RunStarted})

	select {
	case got := <-ch:
		if got.RunID != "r1" || got.Kind != RunStarted {
			t.Fatalf("got %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestBusDropsWhenSubscriberFull(t *testing.T) {
	b := NewBus()
	_, unsub := b.Subscribe(1) // buffer of 1, never drained
	defer unsub()

	// Must not block even though the subscriber never reads.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			b.Publish(Event{RunID: "r1", Kind: StepDone})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a full subscriber")
	}
}

func TestUnsubscribeClosesChannel(t *testing.T) {
	b := NewBus()
	ch, unsub := b.Subscribe(1)
	unsub()
	if _, open := <-ch; open {
		t.Fatal("channel should be closed after unsubscribe")
	}
}
