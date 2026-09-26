package registrywatch

import "sync"

// Hub is an in-process latency optimization, not the source of truth. Watch
// also polls the durable ledger so another node's commit cannot be missed.
type Hub struct {
	mu          sync.Mutex
	next        uint64
	subscribers map[string]map[uint64]*hubSubscription
}

type hubSubscription struct {
	hub      *Hub
	tenantID string
	id       uint64
	channel  chan struct{}
	once     sync.Once
}

func NewHub() *Hub { return &Hub{subscribers: map[string]map[uint64]*hubSubscription{}} }

func (hub *Hub) Subscribe(tenantID string) Subscription {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	hub.next++
	subscription := &hubSubscription{hub: hub, tenantID: tenantID, id: hub.next, channel: make(chan struct{}, 1)}
	if hub.subscribers[tenantID] == nil {
		hub.subscribers[tenantID] = map[uint64]*hubSubscription{}
	}
	hub.subscribers[tenantID][subscription.id] = subscription
	return subscription
}

func (hub *Hub) Notify(tenantID string, _ uint64) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for _, subscription := range hub.subscribers[tenantID] {
		select {
		case subscription.channel <- struct{}{}:
		default:
		}
	}
}

func (hub *Hub) Subscribers(tenantID string) int {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return len(hub.subscribers[tenantID])
}

func (subscription *hubSubscription) C() <-chan struct{} { return subscription.channel }

func (subscription *hubSubscription) Close() {
	subscription.once.Do(func() {
		subscription.hub.mu.Lock()
		defer subscription.hub.mu.Unlock()
		delete(subscription.hub.subscribers[subscription.tenantID], subscription.id)
		if len(subscription.hub.subscribers[subscription.tenantID]) == 0 {
			delete(subscription.hub.subscribers, subscription.tenantID)
		}
		close(subscription.channel)
	})
}
