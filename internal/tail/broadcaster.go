package tail

import "sync"

type Broadcaster[T any] struct {
	mu   sync.Mutex
	subs map[chan T]struct{}
}

func New[T any]() *Broadcaster[T] {
	return &Broadcaster[T]{subs: make(map[chan T]struct{})}
}

func (b *Broadcaster[T]) Subscribe() (ch chan T, cancel func()) {
	ch = make(chan T, 16)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	cancel = func() {
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
	return ch, cancel
}

func (b *Broadcaster[T]) Publish(v T) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- v:
		default:
		}
	}
}
