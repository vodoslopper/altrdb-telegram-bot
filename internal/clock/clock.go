// Package clock provides the time source used across the program.
//
// Everything that stamps a row, schedules a retry or measures a poll interval
// takes a Clock, so tests can drive hours of behaviour in microseconds.
package clock

import (
	"sync"
	"time"
)

// Clock reports the current time in UTC.
type Clock interface {
	Now() time.Time
}

// Real is the wall clock.
type Real struct{}

// Now returns the current UTC time.
func (Real) Now() time.Time { return time.Now().UTC() }

// New returns the production clock.
func New() Clock { return Real{} }

// Fake is a manually advanced clock, safe for concurrent use.
type Fake struct {
	mu sync.Mutex
	t  time.Time
}

// NewFake returns a clock frozen at t (normalised to UTC).
func NewFake(t time.Time) *Fake { return &Fake{t: t.UTC()} }

// Now returns the frozen time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

// Advance moves the clock forward by d and returns the new time.
func (f *Fake) Advance(d time.Duration) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
	return f.t
}

// Set replaces the current time.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = t.UTC()
}
