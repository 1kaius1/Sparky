// SPDX-License-Identifier: AGPL-3.0-or-later

package events

import (
	"testing"
	"time"
)

func TestBroker_PublishDeliversToSubscriber(t *testing.T) {
	b := NewBroker()
	ch, cancel := b.Subscribe()
	defer cancel()

	b.Publish(Event{Type: "telemetry"})

	select {
	case evt := <-ch:
		if evt.Type != "telemetry" {
			t.Errorf("Type = %q, want %q", evt.Type, "telemetry")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for published event")
	}
}

func TestBroker_PublishFansOutToMultipleSubscribers(t *testing.T) {
	b := NewBroker()
	ch1, cancel1 := b.Subscribe()
	defer cancel1()
	ch2, cancel2 := b.Subscribe()
	defer cancel2()

	b.Publish(Event{Type: "instance_result"})

	for i, ch := range []<-chan Event{ch1, ch2} {
		select {
		case evt := <-ch:
			if evt.Type != "instance_result" {
				t.Errorf("subscriber %d: Type = %q, want %q", i, evt.Type, "instance_result")
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d: timed out waiting for published event", i)
		}
	}
}

func TestBroker_PublishWithNoSubscribersDoesNotBlock(t *testing.T) {
	b := NewBroker()
	done := make(chan struct{})
	go func() {
		b.Publish(Event{Type: "transfer_progress"})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked with no subscribers")
	}
}

func TestBroker_PublishDropsForFullSubscriberBuffer(t *testing.T) {
	b := NewBroker()
	ch, cancel := b.Subscribe()
	defer cancel()

	// Fill the subscriber's buffer, then publish one more - it must be
	// dropped for this subscriber, not block the caller.
	for i := 0; i < subscriberBuffer; i++ {
		b.Publish(Event{Type: "telemetry"})
	}

	done := make(chan struct{})
	go func() {
		b.Publish(Event{Type: "overflow"})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a full subscriber buffer")
	}

	if got := len(ch); got != subscriberBuffer {
		t.Errorf("buffered event count = %d, want %d", got, subscriberBuffer)
	}
}

func TestBroker_CancelStopsDeliveryAndClosesChannel(t *testing.T) {
	b := NewBroker()
	ch, cancel := b.Subscribe()
	cancel()

	if _, ok := <-ch; ok {
		t.Error("channel not closed after cancel")
	}

	// Publish after cancel must not panic (send on closed channel) or
	// deliver anything - the subscriber was removed from the map before
	// the channel was closed.
	b.Publish(Event{Type: "telemetry"})
}

func TestBroker_CancelIsSafeToCallMultipleTimes(t *testing.T) {
	b := NewBroker()
	_, cancel := b.Subscribe()
	cancel()
	cancel()
}
