package application

import (
	"context"
	"encoding/json"
	"testing"

	"postra/internal/domain"
)

// SQL adapters legitimately return nil when no rows match. Use the same
// contract for global admin lists that a fixture database may already seed.
type emptyAdminCollectionStore struct{ Storage }

func (emptyAdminCollectionStore) ListUsers(context.Context) ([]domain.User, error) {
	return nil, nil
}
func (emptyAdminCollectionStore) ListAllMCPKeys(context.Context) ([]domain.MCPKey, error) {
	return nil, nil
}
func (emptyAdminCollectionStore) ListIncidents(context.Context, domain.IncidentFilter) ([]domain.Incident, error) {
	return nil, nil
}

func TestEmptyAdminCollectionContracts(t *testing.T) {
	app := &App{Store: emptyAdminCollectionStore{}}
	admin := WithPrincipal(context.Background(), domain.Principal{UserID: "admin", Role: domain.RoleAdmin})
	user := WithPrincipal(context.Background(), domain.Principal{UserID: "user", Role: domain.RoleUser})
	checks := map[string]func(context.Context) (any, error){
		"users": func(ctx context.Context) (any, error) { return app.AdminListUsers(ctx) },
		"keys":  func(ctx context.Context) (any, error) { return app.AdminListMCPKeys(ctx) },
		"incidents": func(ctx context.Context) (any, error) {
			return app.AdminListIncidents(ctx, domain.IncidentFilter{})
		},
	}
	for name, list := range checks {
		t.Run(name, func(t *testing.T) {
			rows, err := list(admin)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(rows)
			if err != nil || string(raw) != "[]" {
				t.Fatalf("empty admin list = %s (%v), want []", raw, err)
			}
			if _, err := list(user); err == nil {
				t.Fatal("empty collection bypassed admin authorization")
			}
		})
	}
}
