package main

import "time"

type AccountFamily string

const (
	AccountFamilyUnknown AccountFamily = "unknown"
	AccountFamilyWeekly  AccountFamily = "weekly"
	AccountFamilyMonthly AccountFamily = "monthly"
)

type WindowKind string

const (
	WindowFiveHour WindowKind = "five_hour"
	WindowWeekly   WindowKind = "weekly"
	WindowMonthly  WindowKind = "monthly"
)

type QuotaWindow struct {
	Kind               WindowKind `json:"kind"`
	UsedPercent        *float64   `json:"used_percent,omitempty"`
	LimitWindowSeconds *int64     `json:"limit_window_seconds,omitempty"`
	ResetAt            time.Time  `json:"reset_at"`
	Exhausted          bool       `json:"exhausted"`
}

type ParsedQuota struct {
	PlanType                   string        `json:"plan_type,omitempty"`
	Family                     AccountFamily `json:"family"`
	FiveHour                   *QuotaWindow  `json:"five_hour,omitempty"`
	LongWindow                 *QuotaWindow  `json:"long_window,omitempty"`
	CodeReviewWindows          []QuotaWindow `json:"code_review_windows,omitempty"`
	AdditionalWindows          []QuotaWindow `json:"additional_windows,omitempty"`
	ResetCreditsAvailableCount *int          `json:"reset_credits_available_count,omitempty"`
}

type AccountAnnotation struct {
	Alias   string   `json:"alias,omitempty" yaml:"alias,omitempty"`
	Notes   string   `json:"notes,omitempty" yaml:"notes,omitempty"`
	Tags    []string `json:"tags,omitempty" yaml:"tags,omitempty"`
	GroupID string   `json:"group_id,omitempty" yaml:"group_id,omitempty"`
}

type GroupAnnotation struct {
	Name  string   `json:"name,omitempty" yaml:"name,omitempty"`
	Notes string   `json:"notes,omitempty" yaml:"notes,omitempty"`
	Tags  []string `json:"tags,omitempty" yaml:"tags,omitempty"`
	Color string   `json:"color,omitempty" yaml:"color,omitempty"`
}

type AnnotationState struct {
	Accounts map[string]AccountAnnotation `json:"accounts,omitempty" yaml:"accounts,omitempty"`
	Groups   map[string]GroupAnnotation   `json:"groups,omitempty" yaml:"groups,omitempty"`
}

type AccountState struct {
	AuthID             string
	AuthIndex          string
	DisplayName        string
	Email              string
	Provider           string
	Priority           int
	ChatGPTAccountID   string
	Family             AccountFamily
	Quota              ParsedQuota
	LastRefreshAt      time.Time
	LastSuccessAt      time.Time
	LastError          string
	Stale              bool
	TemporaryExhausted bool
	TemporaryResetAt   time.Time
	Annotation         AccountAnnotation
}

type StateSnapshot struct {
	Config       Config
	Accounts     []AccountState
	Annotations  AnnotationState
	Logs         []LogEntry
	LastSelected string
	LastReason   string
	Now          time.Time
}

type LogEntry struct {
	Time    time.Time      `json:"time"`
	Level   string         `json:"level"`
	Event   string         `json:"event"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}
