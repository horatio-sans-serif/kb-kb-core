package events

import "sync"

// defaultBuffer is the per-subscriber channel buffer. A subscriber that falls
// this far behind starts dropping events rather than blocking publishers.
const defaultBuffer = 64

// Bus is an in-process fan-out bus. Publish never blocks: a subscriber whose
// buffer is full drops the event. Safe for concurrent use.
type Bus struct {
	mu     sync.Mutex
	subs   map[int]chan Event
	nextID int
	buffer int
}

// NewBus returns a ready bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[int]chan Event), buffer: defaultBuffer}
}

// Subscribe returns a receive channel and an unsubscribe func. The func is
// idempotent and closes the channel; call it (e.g. via defer) when done.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	id := b.nextID
	b.nextID++
	ch := make(chan Event, b.buffer)
	b.subs[id] = ch

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if c, ok := b.subs[id]; ok {
				delete(b.subs, id)
				close(c)
			}
		})
	}
	return ch, cancel
}

// Publish delivers e to every current subscriber without blocking. A subscriber
// whose buffer is full silently drops this event.
func (b *Bus) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default: // full: drop for this subscriber, never block the publisher
		}
	}
}
