package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func NormalizeAnnotationState(state AnnotationState) AnnotationState {
	normalized := AnnotationState{
		Accounts: make(map[string]AccountAnnotation, len(state.Accounts)),
		Groups:   make(map[string]GroupAnnotation, len(state.Groups)),
	}
	for key, annotation := range state.Accounts {
		annotation.Tags = normalizeTags(annotation.Tags)
		normalized.Accounts[key] = annotation
	}
	for key, annotation := range state.Groups {
		annotation.Tags = normalizeTags(annotation.Tags)
		normalized.Groups[key] = annotation
	}
	return normalized
}

func ResolveAnnotationKey(account AccountState) string {
	if account.ChatGPTAccountID == "" && account.Instance != 0 {
		return "instance:" + strconv.FormatUint(uint64(account.Instance), 10)
	}
	if account.AuthID != "" {
		return "auth:" + account.AuthID
	}
	if account.ChatGPTAccountID != "" {
		return "chatgpt:" + account.ChatGPTAccountID
	}
	if account.Email != "" {
		return "email:" + account.Email
	}
	if account.AuthIndex != "" {
		return "index:" + account.AuthIndex
	}
	return ""
}

func LoadAnnotations(path string) (AnnotationState, error) {
	if path == "" {
		return NormalizeAnnotationState(AnnotationState{}), nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return NormalizeAnnotationState(AnnotationState{}), nil
	}
	if err != nil {
		return AnnotationState{}, err
	}
	var state AnnotationState
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &state); err != nil {
			return AnnotationState{}, err
		}
	}
	return NormalizeAnnotationState(state), nil
}

func SaveAnnotations(path string, state AnnotationState) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(NormalizeAnnotationState(state), "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0600); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

func ApplyAnnotations(accounts []AccountState, state AnnotationState) []AccountState {
	normalized := NormalizeAnnotationState(state)
	applied := make([]AccountState, len(accounts))
	for i, account := range accounts {
		applied[i] = cloneAccountState(account)
		if annotation, ok := normalized.Accounts[ResolveAnnotationKey(account)]; ok {
			applied[i].Annotation = cloneAccountAnnotation(annotation)
		}
	}
	return applied
}

func normalizeTags(tags []string) []string {
	seen := make(map[string]struct{}, len(tags))
	normalized := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		normalized = append(normalized, tag)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}
