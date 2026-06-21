package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type CodexCredentials struct {
	AccessToken      string
	IDToken          string
	ChatGPTAccountID string
}

func ExtractCodexCredentials(raw json.RawMessage) (CodexCredentials, error) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return CodexCredentials{}, err
	}

	accessToken, ok := lookupStringDeep(doc, "access_token")
	if !ok || accessToken == "" {
		return CodexCredentials{}, errors.New("missing access_token")
	}
	idToken, _ := lookupStringDeep(doc, "id_token")

	accountID, err := extractChatGPTAccountID(idToken)
	if err != nil {
		return CodexCredentials{}, err
	}

	return CodexCredentials{
		AccessToken:      accessToken,
		IDToken:          idToken,
		ChatGPTAccountID: accountID,
	}, nil
}

func extractChatGPTAccountID(idToken string) (string, error) {
	claims, err := decodeJWTClaims(idToken)
	if err != nil {
		return "", fmt.Errorf("missing chatgpt_account_id: %w", err)
	}
	if v, ok := stringFromMap(claims, "https://api.openai.com/auth.chatgpt_account_id"); ok && v != "" {
		return v, nil
	}
	if authClaim, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if v, ok := stringFromMap(authClaim, "chatgpt_account_id"); ok && v != "" {
			return v, nil
		}
	}
	if v, ok := lookupStringDeep(claims, "https://api.openai.com/auth.chatgpt_account_id"); ok && v != "" {
		return v, nil
	}
	if v, ok := lookupStringDeep(claims, "chatgpt_account_id"); ok && v != "" {
		return v, nil
	}
	return "", errors.New("missing chatgpt_account_id")
}

func decodeJWTClaims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, errors.New("invalid id_token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

func lookupStringDeep(v any, key string) (string, bool) {
	switch typed := v.(type) {
	case map[string]any:
		if found, ok := stringFromMap(typed, key); ok {
			return found, true
		}
		for _, child := range typed {
			if found, ok := lookupStringDeep(child, key); ok {
				return found, true
			}
		}
	case []any:
		for _, child := range typed {
			if found, ok := lookupStringDeep(child, key); ok {
				return found, true
			}
		}
	}
	return "", false
}

func stringFromMap(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}
