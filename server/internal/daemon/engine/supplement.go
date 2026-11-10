package engine

import (
	"context"
	"errors"
	"sync"
)

type supplementInput struct {
	id, text string
	ctx      context.Context
	done     chan error
}
type supplements struct {
	mu            sync.Mutex
	ready, closed bool
	pending       *supplementInput
}

func (q *supplements) accepts() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.ready && !q.closed && q.pending == nil
}
func (q *supplements) offer(ctx context.Context, id, text string) error {
	if id == "" || len(id) > 200 || text == "" || len(text) > 64<<10 {
		return errors.New("invalid additional input")
	}
	input := &supplementInput{id: id, text: text, ctx: ctx, done: make(chan error, 1)}
	q.mu.Lock()
	if !q.ready || q.closed || q.pending != nil {
		q.mu.Unlock()
		return errors.New("native turn cannot accept input now")
	}
	q.pending = input
	q.mu.Unlock()
	select {
	case err := <-input.done:
		return err
	case <-ctx.Done():
		q.mu.Lock()
		if q.pending == input {
			q.pending = nil
			q.mu.Unlock()
			return ctx.Err()
		}
		q.mu.Unlock()
		// A safe step already took ownership. Report its durable outcome,
		// including when cancellation races a successful checkpoint commit.
		return <-input.done
	}
}
func (q *supplements) enable() { q.mu.Lock(); q.ready = true; q.mu.Unlock() }
func (q *supplements) take(final bool) *supplementInput {
	q.mu.Lock()
	defer q.mu.Unlock()
	input := q.pending
	q.pending = nil
	if input == nil && final {
		q.closed = true
	}
	return input
}
func (q *supplements) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	if q.pending != nil {
		q.pending.done <- errors.New("native turn ended before durable input acceptance")
		q.pending = nil
	}
}
