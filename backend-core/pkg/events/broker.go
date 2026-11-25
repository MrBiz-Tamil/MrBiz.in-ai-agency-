// Package events is a small in-process publish/subscribe broker used for GraphQL subscriptions.
//
// It is per-process: with several backend instances, fan-out would need Redis pub/sub
// behind the same interface.
package events

import "sync"

const bufferSize = 64

type Broker struct {
	mu   sync.RWMutex
	subs map[string]map[chan any]struct{}
}

func NewBroker() *Broker { return &Broker{subs: map[string]map[chan any]struct{}{}} }

// Subscribe returns a channel of events for topic and a cancel function that must be
// called to release it. The channel is closed by cancel.
func (b *Broker) Subscribe(topic string) (<-chan any, func()) {
	ch := make(chan any, bufferSize)
	b.mu.Lock()
	if b.subs[topic] == nil {
		b.subs[topic] = map[chan any]struct{}{}
	}
	b.subs[topic][ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs[topic], ch)
			if len(b.subs[topic]) == 0 {
				delete(b.subs, topic)
			}
			b.mu.Unlock()
			close(ch)
		})
	}
}

// Publish delivers v to every subscriber of topic without ever blocking the publisher:
// a subscriber whose buffer is full misses the event.
func (b *Broker) Publish(topic string, v any) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs[topic] {
		select {
		case ch <- v:
		default:
		}
	}
}

// Topic names.
func TicketTopic(workspaceID string) string   { return "ticket:" + workspaceID }
func ApprovalTopic(workspaceID string) string { return "approval:" + workspaceID }
func BudgetTopic(workspaceID string) string   { return "budget:" + workspaceID }
func AuditTopic(workspaceID, ticketID string) string {
	return "audit:" + workspaceID + ":" + ticketID
}
