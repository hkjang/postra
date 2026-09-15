package domain

import "encoding/json"

// Empty persisted values identify pre-scope credentials. Preserve their
// former personal workflows; role/gateway checks still apply independently.
func DecodeMCPKeyScopes(raw string) ([]string, bool, error) {
	if raw == "" {
		return []string{"mail.read", "mail.search", "mail.ai", "mail.draft", "mail.send", "mail.delete", "mail.work"}, true, nil
	}
	var scopes []string
	if err := json.Unmarshal([]byte(raw), &scopes); err != nil {
		return nil, false, err
	}
	if scopes == nil {
		scopes = []string{}
	}
	return scopes, false, nil
}
