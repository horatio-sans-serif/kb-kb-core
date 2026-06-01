package events

import (
	"testing"
	"time"
)

func TestBus_FanOut(t *testing.T) {
	bus := NewBus()
	a, cancelA := bus.Subscribe()
	defer cancelA()
	b, cancelB := bus.Subscribe()
	defer cancelB()

	bus.Publish(New("hello", nil))

	for i, ch := range []<-chan Event{a, b} {
		select {
		case e := <-ch:
			if e.Type != "hello" {
				t.Errorf("subscriber %d got type %q, want hello", i, e.Type)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d received nothing", i)
		}
	}
}

func TestBus_SlowSubscriberDropped(t *testing.T) {
	bus := NewBus()

	slow, cancelSlow := bus.Subscribe() // never drained
	defer cancelSlow()
	fast, cancelFast := bus.Subscribe()
	defer cancelFast()

	// fast subscriber is actively drained; signal once it has seen a few events.
	got := make(chan struct{})
	go func() {
		n := 0
		for range fast {
			if n++; n == 5 {
				close(got)
				return
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		for range 1000 {
			bus.Publish(New("x", nil)) // must not block on the full slow subscriber
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow (full) subscriber")
	}
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("fast subscriber received nothing; bus not live")
	}
	_ = slow
}

func TestBus_DoubleCancelIsIdempotent(t *testing.T) {
	bus := NewBus()
	_, cancel := bus.Subscribe()
	cancel()
	cancel() // must not panic (sync.Once)
}

func TestBus_PublishWithNoSubscribers(t *testing.T) {
	bus := NewBus()
	bus.Publish(New("x", nil)) // must not panic or block
}

func TestBus_UnsubscribeStopsDelivery(t *testing.T) {
	bus := NewBus()
	ch, cancel := bus.Subscribe()
	cancel() // channel is now closed; Publish must not panic sending to it

	bus.Publish(New("x", nil))

	if _, ok := <-ch; ok {
		t.Error("expected closed channel after unsubscribe")
	}
}
