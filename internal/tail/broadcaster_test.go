package tail

import (
	"sync"
	"testing"
	"time"
)

func TestSubscribe_ReceivesPublishedValue(t *testing.T) {
	b := New[string]()
	ch, cancel := b.Subscribe()
	defer cancel()

	b.Publish("hello")

	select {
	case got := <-ch:
		if got != "hello" {
			t.Errorf("got %q, want %q", got, "hello")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for published value")
	}
}

func TestPublish_FansOutToAllSubscribers(t *testing.T) {
	b := New[int]()

	const subscribers = 5
	var chans []chan int
	var cancels []func()
	for i := 0; i < subscribers; i++ {
		ch, cancel := b.Subscribe()
		chans = append(chans, ch)
		cancels = append(cancels, cancel)
	}
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()

	b.Publish(42)

	for i, ch := range chans {
		select {
		case got := <-ch:
			if got != 42 {
				t.Errorf("subscriber %d got %d, want 42", i, got)
			}
		case <-time.After(time.Second):
			t.Errorf("subscriber %d: timed out waiting for published value", i)
		}
	}
}

func TestPublish_NoSubscribers_DoesNotBlockOrPanic(t *testing.T) {
	b := New[string]()

	done := make(chan struct{})
	go func() {
		b.Publish("nobody listening")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish with no subscribers should return immediately, it hung instead")
	}
}

func TestCancel_StopsDeliveryAndClosesChannel(t *testing.T) {
	b := New[string]()
	ch, cancel := b.Subscribe()

	cancel()

	select {
	case got, ok := <-ch:
		if ok {
			t.Errorf("expected channel to be closed (ok=false), got value %q with ok=true", got)
		}
	case <-time.After(time.Second):
		t.Fatal("reading from a cancelled subscription's channel should not block")
	}

	b.Publish("should not panic")
}

func TestCancel_IsIdempotent(t *testing.T) {
	b := New[string]()
	_, cancel := b.Subscribe()

	cancel()
	cancel()
}

func TestCancelledSubscriber_DoesNotReceiveFuturePublishes(t *testing.T) {
	b := New[string]()
	ch1, cancel1 := b.Subscribe()
	ch2, cancel2 := b.Subscribe()
	defer cancel2()

	cancel1()
	b.Publish("after cancel")

	select {
	case _, ok := <-ch1:
		if ok {
			t.Error("cancelled subscriber should not receive values published after cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("closed channel read should not block")
	}

	select {
	case got := <-ch2:
		if got != "after cancel" {
			t.Errorf("got %q, want %q", got, "after cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("active subscriber should still receive published values")
	}
}

// TestPublish_FullSubscriberDoesNotBlockOthers is the core design
// guarantee: a slow/stalled subscriber (buffer full, nobody reading) must
// get its message dropped rather than blocking Publish or other
// subscribers - live tail is best-effort, ingestion must never wait on it.
func TestPublish_FullSubscriberDoesNotBlockOthers(t *testing.T) {
	b := New[int]()

	slowCh, slowCancel := b.Subscribe()
	defer slowCancel()
	fastCh, fastCancel := b.Subscribe()
	defer fastCancel()

	const bufferCap = 16
	for i := 0; i < bufferCap; i++ {
		b.Publish(i)
	}

	done := make(chan struct{})
	go func() {
		b.Publish(999)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a full subscriber buffer - it must drop instead")
	}

	go func() {
		for range fastCh {
		}
	}()

	received := 0
	timeout := time.After(2 * time.Second)
drain:
	for {
		select {
		case <-slowCh:
			received++
			if received >= bufferCap {
				break drain
			}
		case <-timeout:
			break drain
		}
	}
	if received != bufferCap {
		t.Errorf("slow subscriber received %d buffered messages, want %d (the 17th publish should have been dropped, not blocked)", received, bufferCap)
	}
}

// TestConcurrentSubscribePublishCancel exercises every operation at once
// under -race to catch any data races in the subs map / channel lifecycle.
func TestConcurrentSubscribePublishCancel(t *testing.T) {
	b := New[int]()

	var wg sync.WaitGroup

	for p := 0; p < 5; p++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				b.Publish(id*1000 + i)
			}
		}(p)
	}

	for s := 0; s < 5; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < 20; r++ {
				ch, cancel := b.Subscribe()
				timeout := time.After(5 * time.Millisecond)
			drainLoop:
				for {
					select {
					case _, ok := <-ch:
						if !ok {
							break drainLoop
						}
					case <-timeout:
						break drainLoop
					}
				}
				cancel()
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent subscribe/publish/cancel test timed out - possible deadlock")
	}
}
