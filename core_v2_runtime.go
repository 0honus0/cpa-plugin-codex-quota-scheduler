package main

import (
	"sync"
	"time"
)

var (
	coreEngineMu sync.RWMutex
	globalCore   *CoreEngine
)

func setGlobalCore(engine *CoreEngine) {
	coreEngineMu.Lock()
	old := globalCore
	globalCore = engine
	coreEngineMu.Unlock()
	if old != nil && old != engine {
		old.Stop()
	}
}

func currentCore() *CoreEngine {
	coreEngineMu.RLock()
	defer coreEngineMu.RUnlock()
	return globalCore
}

func ensureCurrentCore() *CoreEngine {
	if engine := currentCore(); engine != nil {
		return engine
	}
	engine := NewCoreEngine(ABIHostClient{}, defaultCoreStatePath(), time.Now)
	setGlobalCore(engine)
	return engine
}

func stopGlobalCore() {
	coreEngineMu.Lock()
	engine := globalCore
	globalCore = nil
	coreEngineMu.Unlock()
	if engine != nil {
		engine.Stop()
	}
}
