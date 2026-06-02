package event

import "sync"

// Bus is an in-process publish/subscribe hub for live observers. Publish never
// blocks: each subscriber has a buffered channel, and an event is dropped for a
// subscriber that has fallen behind. The durable record lives in the store, so a
// dropped live frame is recoverable via replay.
type Bus struct {
	mu   sync.RWMutex
	subs map[int]chan Event
	next int
}

func NewBus() *Bus {
	return &Bus{subs: make(map[int]chan Event)}
}

// Subscribe returns a receive channel and an unsubscribe func. buffer sets the
// per-subscriber backlog before events start being dropped.
func (b *Bus) Subscribe(buffer int) (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan Event, buffer)
	b.subs[id] = ch
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
	}
}

// Publish fans an event out to every current subscriber, dropping it for any
// whose buffer is full.
func (b *Bus) Publish(e Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default: // subscriber behind; drop (store holds the durable copy)
		}
	}
}
