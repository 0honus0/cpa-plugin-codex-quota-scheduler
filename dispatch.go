package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var (
	currentConfig   atomic.Value
	globalState     = NewPluginState(DefaultConfig())
	refresherMu     sync.Mutex
	globalRefresher *QuotaRefresher
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(request); err != nil {
			return nil, err
		}
		startGlobalRefresher()
		return okEnvelope(PluginRegistration())
	case pluginabi.MethodSchedulerPick:
		return handleSchedulerPick(request)
	case pluginabi.MethodUsageHandle:
		return handleUsageHandle(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(RegisterManagement())
	case pluginabi.MethodManagementHandle:
		return handleManagementHandle(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	cfg, err := DecodeConfig(req.ConfigYAML)
	if err != nil {
		return err
	}
	disk, loadedDisk, err := loadUserDataWithMigration(semanticStatePaths(defaultStatePath()), OSFileHooks(), nil)
	if err == nil && loadedDisk {
		cfg = disk.Config
	} else {
		disk = PluginDiskState{Config: cfg}
	}
	currentConfig.Store(cfg)
	globalState.ReplaceConfig(cfg)
	globalState.SetAnnotations(AnnotationState{Accounts: disk.Accounts, Groups: disk.Groups})
	return nil
}

func handleSchedulerPick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
	}
	now := time.Now()
	_, admissionVersion := globalState.CPAAdmissionVersioned()
	if admission, ok := HighestPriorityCodexAdmission(req); ok {
		admissionVersion = globalState.ReplaceCPAAdmission(admission)
		globalState.RecordLog("info", "scheduler.cpa_admission_updated", "CPA priority admission updated", map[string]any{
			"cpa_priority":   admission.Priority,
			"admitted_count": len(admission.AuthIDs),
			"excluded_count": codexCandidateCount(req) - len(admission.AuthIDs),
		}, now)
	}
	if requestIncludesCodex(req) {
		globalState.RecordCodexActivity(now)
		refreshGlobalRefresherDueSoon(req, admissionVersion, now)
	}
	decision := schedulerPickSnapshot(req, globalState.Snapshot(now), now)
	if decision.AuthID != "" {
		globalState.RecordSelection(decision.AuthID, decision.Reason)
	}
	logSchedulerDecision(globalState, req, decision, now)
	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:          decision.AuthID,
		DelegateBuiltin: decision.DelegateBuiltin,
		Handled:         decision.Handled,
	})
}

func schedulerPickSnapshot(req pluginapi.SchedulerPickRequest, snapshot StateSnapshot, now time.Time) PickDecision {
	return PickCodexAccount(req, snapshot, now)
}

func logSchedulerDecision(store *PluginState, req pluginapi.SchedulerPickRequest, decision PickDecision, now time.Time) {
	if store == nil {
		return
	}
	level := "info"
	event := "scheduler.unhandled"
	message := "请求未由插件接管"
	fields := map[string]any{
		"model":         req.Model,
		"provider":      req.Provider,
		"reason":        decision.Reason,
		"ordered_count": len(decision.Ordered),
	}
	if decision.AuthID != "" {
		event = "scheduler.selected"
		message = "请求已由插件接管"
		fields["auth_id"] = decision.AuthID
		if selected, ok := findScheduledAccount(decision.Ordered, decision.AuthID); ok {
			fields["selected_queue_status"] = string(selected.QueueStatus)
			fields["selected_sort_time"] = selected.SortTime.Format(time.RFC3339)
			fields["selected_cpa_priority"] = selected.CPAPriority
			fields["selected_scheduler_priority"] = selected.SchedulerPriority
		}
	} else if decision.DelegateBuiltin != "" {
		event = "scheduler.fallback"
		message = "插件触发内置调度 fallback"
		fields["fallback"] = decision.DelegateBuiltin
		fields["unavailable_summary"] = unavailableSummary(decision.Ordered)
	} else if decision.Handled {
		event = "scheduler.handled"
		message = "插件已处理但未选择账号"
	}
	store.RecordLog(level, event, message, fields, now)
}

func findScheduledAccount(accounts []ScheduledAccount, authID string) (ScheduledAccount, bool) {
	for _, account := range accounts {
		if account.AuthID == authID {
			return account, true
		}
	}
	return ScheduledAccount{}, false
}

func unavailableSummary(accounts []ScheduledAccount) string {
	if len(accounts) == 0 {
		return "no ordered candidates"
	}
	parts := make([]string, 0, len(accounts))
	for _, account := range accounts {
		reason := account.UnavailableReason
		if reason == "" && account.Available {
			reason = "available"
		}
		if reason == "" {
			reason = string(account.QueueStatus)
		}
		parts = append(parts, fmt.Sprintf("%s:%s:%s", account.AuthID, account.QueueStatus, reason))
	}
	return strings.Join(parts, "; ")
}

func handleUsageHandle(raw []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, err
		}
	}
	HandleUsageFeedback(globalState, record, time.Now())
	return okEnvelope(map[string]any{})
}

func handleManagementHandle(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
	}
	return okEnvelope(HandleManagementRequest(globalState, req, time.Now()))
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func startGlobalRefresher() {
	refresherMu.Lock()
	refresher := globalRefresher
	refresherMu.Unlock()
	if refresher == nil {
		return
	}
	refresher.Start()
	if globalState.Config().RefreshOnStartup {
		refresher.RefreshSoon()
	}
}

func refreshGlobalRefresherSoon() {
	refresherMu.Lock()
	refresher := globalRefresher
	refresherMu.Unlock()
	if refresher != nil {
		globalState.RecordLog("info", "quota.refresh_requested", "已请求后台刷新额度", nil, time.Now())
		refresher.RefreshSoon()
	}
}

func refreshGlobalRefresherOneSoon(authID string) {
	refresherMu.Lock()
	refresher := globalRefresher
	refresherMu.Unlock()
	if refresher != nil {
		globalState.RecordLog("info", "quota.refresh_one_requested", "已请求后台刷新单个账号额度", map[string]any{"auth_id": authID}, time.Now())
		refresher.RefreshOneSoon(authID)
	}
}

func refreshGlobalRefresherDueSoon(req pluginapi.SchedulerPickRequest, admissionVersion uint64, now time.Time) {
	refresherMu.Lock()
	refresher := globalRefresher
	refresherMu.Unlock()
	if refresher != nil {
		refresher.OnSchedulerPick(req, admissionVersion, now)
	}
}
