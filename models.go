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
