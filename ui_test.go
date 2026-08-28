package main

import (
	"strings"
	"testing"
)

func TestManagementUIRequiresExplicitManagementKeyAuthentication(t *testing.T) {
	for _, required := range []string{
		`id="managementKey"`,
		`type="password"`,
		`id="connectBtn"`,
		`Authorization`,
		`Bearer `,
		`需要管理认证`,
	} {
		if !strings.Contains(coreStatusHTML, required) {
			t.Fatalf("management UI missing authentication marker %q", required)
		}
	}
}

func TestManagementUIDoesNotPersistManagementKeyInBrowserStorage(t *testing.T) {
	lower := strings.ToLower(coreStatusHTML)
	for _, forbidden := range []string{"localstorage", "sessionstorage", "document.cookie", "indexeddb"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("management UI must not persist secrets via %s", forbidden)
		}
	}
}

func TestManagementUIHasNoExternalScriptOrStylesheetDependencies(t *testing.T) {
	lower := strings.ToLower(coreStatusHTML)
	for _, forbidden := range []string{"<script src=", `<link rel="stylesheet"`, "https://cdn.", "http://cdn."} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("management UI has forbidden external dependency %q", forbidden)
		}
	}
}

func TestManagementUIProvidesImmediateInteractionFeedback(t *testing.T) {
	for _, required := range []string{
		`classList.add('busy')`,
		`successButton`,
		`toastStack`,
		`有未保存修改`,
		`刷新排队`,
		`syncBar`,
		`prefers-reduced-motion`,
	} {
		if !strings.Contains(coreStatusHTML, required) {
			t.Fatalf("management UI missing interaction feedback marker %q", required)
		}
	}
}

func TestManagementUIPreservesUnsavedDraftsDuringPolling(t *testing.T) {
	for _, required := range []string{
		`hasEditingDraft`,
		`data-dirty`,
		`settingsDirty`,
	} {
		if !strings.Contains(coreStatusHTML, required) {
			t.Fatalf("management UI missing draft-preservation marker %q", required)
		}
	}
}
