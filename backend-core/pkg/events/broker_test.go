package events

import (
	"sync"
	"testing"
	"time"
)

func TestBroker_DeliversOnlyToTopicSubscribers(t *testing.T) {
	b := NewBroker()
	a, cancelA := b.Subscribe("t1")
	other, cancelOther := b.Subscribe("t2")
	defer cancelOther()

	b.Publish("t1", "hello")
	select {
	case v := <-a:
		if v != "hello" {
			t.Errorf("got %v", v)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive the event")
	}
	select {
	case v := <-other:
		t.Errorf("t2 must not receive t1 events, got %v", v)
	default:
	}

	cancelA()
	cancelA() // idempotent
	if _, ok := <-a; ok {
		t.Error("channel must be closed after cancel")
	}
	b.Publish("t1", "after-cancel") // must not panic
}

func TestBroker_SlowSubscriberNeverBlocksPublisher(t *testing.T) {
	b := NewBroker()
	_, cancel := b.Subscribe("t")
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := 0; i < bufferSize*3; i++ {
			b.Publish("t", i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher blocked on a slow subscriber")
	}
}

func TestBroker_ConcurrentUse(t *testing.T) {
	b := NewBroker()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, cancel := b.Subscribe("t")
			b.Publish("t", 1)
			<-ch
			cancel()
		}()
	}
	wg.Wait()
}
