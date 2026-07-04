// Package portpool provides a mutex-guarded pool of host ports.
package portpool

import (
	"errors"
	"fmt"
	"sync"
)

// ErrExhausted is returned by Alloc when every port in the range is in use.
var ErrExhausted = errors.New("port pool exhausted")

// Pool hands out unique ports from an inclusive [lo, hi] range.
type Pool struct {
	mu   sync.Mutex
	lo   int
	hi   int
	used map[int]bool
}

// New creates a pool over the inclusive range [lo, hi].
func New(lo, hi int) *Pool {
	return &Pool{lo: lo, hi: hi, used: make(map[int]bool)}
}

// Alloc returns the lowest free port, or ErrExhausted.
func (p *Pool) Alloc() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for port := p.lo; port <= p.hi; port++ {
		if !p.used[port] {
			p.used[port] = true
			return port, nil
		}
	}
	return 0, ErrExhausted
}

// Reserve marks a specific port as used; it is how startup reconciliation
// claims ports already published by live containers.
func (p *Pool) Reserve(port int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if port < p.lo || port > p.hi {
		return fmt.Errorf("port %d outside pool range %d-%d", port, p.lo, p.hi)
	}
	if p.used[port] {
		return fmt.Errorf("port %d already allocated", port)
	}
	p.used[port] = true
	return nil
}

// Free returns a port to the pool. Freeing an unallocated port is a no-op.
func (p *Pool) Free(port int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.used, port)
}

// InUse reports how many ports are currently allocated.
func (p *Pool) InUse() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.used)
}
