package main

import (
	"errors"
	"sync"
)

const FenceBlockSize uint64 = 1 << 20

var ErrFenceOverflow = errors.New("fence sequence overflow")

type FenceAllocator struct {
	mu            sync.Mutex
	store         StateWriter
	state         PersistentState
	next, ceiling uint64
	crash         CrashHitter
	initialized   bool
}

func NewFenceAllocator(store StateWriter, state PersistentState, crash CrashHitter) *FenceAllocator {
	return &FenceAllocator{store: store, state: state, crash: crash}
}
func (a *FenceAllocator) Next() (uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.initialized {
		a.next = a.state.ReservedCeiling + 1
		a.ceiling = a.state.ReservedCeiling
		a.initialized = true
	}
	if a.next > a.ceiling {
		if a.state.ReservedCeiling > ^uint64(0)-FenceBlockSize {
			return 0, ErrFenceOverflow
		}
		a.state.ReservedCeiling += FenceBlockSize
		//kpoint:K_FENCE_CEILING_WRITE
		if err := a.store.WriteThrough(clonePersistentState(a.state)); err != nil {
			return 0, err
		}
		if a.crash != nil {
			if err := a.crash.Hit("K_FENCE_AFTER_CEILING"); err != nil {
				return 0, err
			}
		}
		a.ceiling = a.state.ReservedCeiling
	}
	value := a.next
	a.next++
	return value, nil
}
