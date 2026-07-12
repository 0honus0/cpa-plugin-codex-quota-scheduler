package main

import (
	"errors"
	"sync"
)

const FenceBlockSize uint64 = 1 << 20

var ErrFenceOverflow = errors.New("fence sequence overflow")
var ErrFenceUnsafe = errors.New("fence ceiling is not trustworthy")

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
		if a.state.FenceUnsafe {
			return 0, ErrFenceUnsafe
		}
		a.next = a.state.ReservedCeiling + 1
		a.ceiling = a.state.ReservedCeiling
		a.initialized = true
	}
	if a.next > a.ceiling {
		if a.state.ReservedCeiling > ^uint64(0)-FenceBlockSize {
			return 0, ErrFenceOverflow
		}
		newCeiling := a.state.ReservedCeiling + FenceBlockSize
		//kpoint:K_FENCE_CEILING_WRITE
		if a.crash != nil {
			if err := a.crash.Hit("K_FENCE_CEILING_WRITE"); err != nil {
				return 0, err
			}
		}
		var err error
		if updater, ok := a.store.(interface {
			Update(func(*PersistentState) error) error
		}); ok {
			err = updater.Update(func(s *PersistentState) error {
				if s.FenceUnsafe {
					return ErrFenceUnsafe
				}
				if s.ReservedCeiling > ^uint64(0)-FenceBlockSize {
					return ErrFenceOverflow
				}
				newCeiling = s.ReservedCeiling + FenceBlockSize
				s.ReservedCeiling = newCeiling
				return nil
			})
		} else {
			a.state.ReservedCeiling = newCeiling
			err = a.store.WriteThrough(clonePersistentState(a.state))
		}
		if err != nil {
			return 0, err
		}
		a.state.ReservedCeiling = newCeiling
		if a.crash != nil {
			//kpoint:K_FENCE_AFTER_CEILING
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
