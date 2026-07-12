package main

import (
	"errors"
	"testing"
)

type fenceStore struct {
	state  PersistentState
	writes []uint64
	fail   bool
}

func (s *fenceStore) WriteThrough(st PersistentState) error {
	if s.fail {
		return errors.New("persist failed")
	}
	s.state = st
	s.writes = append(s.writes, st.ReservedCeiling)
	return nil
}
func TestFencePersistsCeilingBeforeIssuing(t *testing.T) {
	s := &fenceStore{state: NewPersistentState()}
	a := NewFenceAllocator(s, s.state, nil)
	v, err := a.Next()
	if err != nil || v != 1 || len(s.writes) != 1 || s.writes[0] != FenceBlockSize {
		t.Fatalf("v=%d writes=%v err=%v", v, s.writes, err)
	}
}
func TestFenceBlockExhaustionAndRestartMonotonicity(t *testing.T) {
	s := &fenceStore{state: NewPersistentState()}
	a := NewFenceAllocator(s, s.state, nil)
	for i := uint64(0); i < FenceBlockSize; i++ {
		if _, err := a.Next(); err != nil {
			t.Fatal(err)
		}
	}
	v, _ := a.Next()
	if v != FenceBlockSize+1 || s.writes[len(s.writes)-1] != 2*FenceBlockSize {
		t.Fatalf("v=%d writes=%v", v, s.writes)
	}
	restart := NewFenceAllocator(s, s.state, nil)
	v, _ = restart.Next()
	if v != 2*FenceBlockSize+1 {
		t.Fatalf("restart v=%d", v)
	}
}
func TestFenceDoesNotIssueWhenCeilingPersistenceFails(t *testing.T) {
	s := &fenceStore{state: NewPersistentState(), fail: true}
	a := NewFenceAllocator(s, s.state, nil)
	if _, err := a.Next(); err == nil {
		t.Fatal("issued with failed persistence")
	}
}
func TestFenceCrashAfterCeilingPersistenceCreatesSafeGap(t *testing.T) {
	s := &fenceStore{state: NewPersistentState()}
	a := NewFenceAllocator(s, s.state, crashController("K_FENCE_AFTER_CEILING"))
	if _, err := a.Next(); err == nil {
		t.Fatal("crash not injected")
	}
	restart := NewFenceAllocator(s, s.state, nil)
	v, err := restart.Next()
	if err != nil || v != FenceBlockSize+1 {
		t.Fatalf("v=%d err=%v", v, err)
	}
}
