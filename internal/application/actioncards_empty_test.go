package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"postra/internal/domain"
)

func TestActionAndCalendarExtractionEmptyListsAndInvalidTypes(t *testing.T) {
	for _, kind := range []string{"cards", "events"} {
		for _, tc := range []struct {
			name, list string
			invalid    bool
		}{
			{"empty", `[]`, false}, {"null", `null`, false},
			{"empty_items", `[null,{}, {"title":"  "}]`, false},
			{"string", `"not-a-list"`, true}, {"object", `{}`, true},
			{"boolean", `false`, true}, {"scalar_item", `["not-an-object"]`, true},
			{"mixed_invalid", `[{"title":"must not be partially saved","start":"2026-09-20"},{"title":42}]`, true},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				app, _, _, ai := newTestApp(t)
				ctx := context.Background()
				account := mustAccount(t, app)
				message := recentMessage(t, app, ctx, account.ID, "empty-extraction", "No tasks", "A simple greeting")
				ai.response = `{"` + kind + `":` + tc.list + `}`
				var output any
				var err error
				if kind == "cards" {
					var cards []domain.ActionCard
					cards, err = app.ExtractActionCards(ctx, message.ID)
					output = map[string]any{"cards": cards, "count": len(cards)}
				} else {
					output, err = app.ExtractCalendarEvents(ctx, message.ID)
				}
				if tc.invalid {
					if err == nil {
						t.Fatal("invalid AI list type was accepted")
					}
					if rows, listErr := app.ListActionCards(ctx, "", 100); listErr != nil || len(rows) != 0 {
						t.Fatal("invalid AI schema persisted a partial action list")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(output)
				if err != nil || !strings.Contains(string(raw), `"`+kind+`":[]`) {
					t.Fatalf("empty extraction must be a JSON array: %s (%v)", raw, err)
				}
			})
		}
	}
}

func TestListNormalizationDoesNotHideStoreErrors(t *testing.T) {
	want := errors.New("fixture unavailable")
	rows, err := listResult[domain.ActionCard](nil, want)
	if rows != nil || !errors.Is(err, want) {
		t.Fatal("storage failure was disguised as a successful empty list")
	}
}

func TestEmptyHybridSearchCollection(t *testing.T) {
	app, _, _, _ := newTestApp(t)
	rows, err := app.HybridSearch(context.Background(), HybridSearchOptions{Query: "no-matching-messages", FTSWeight: 1})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rows)
	if string(raw) != "[]" {
		t.Fatalf("empty hybrid search must be a JSON array, got %s", raw)
	}
}
