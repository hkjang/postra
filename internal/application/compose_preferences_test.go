package application

import (
	"context"
	"strings"
	"testing"
)

func TestAIComposeEntryPointsUseAccountPreferencesAndForcedPolicy(t *testing.T) {
	app, _, _, provider := newTestApp(t)
	ctx := WithActor(context.Background(), "test")
	account := mustAccount(t, app)
	provider.response = `{"subject":"Reply","body":"A generated reply."}`
	if _, err := app.SavePersonalSettings(ctx, "", SettingsPatch{Values: map[string]string{
		"compose.tone": "friendly", "compose.reply_length": "long", "compose.language": "en",
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SavePersonalSettings(ctx, account.ID, SettingsPatch{Values: map[string]string{
		"compose.tone": "formal", "compose.writing_style": "Use a concise opening.",
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.AdminPatchSettings(settingsAdmin(), SettingsPatch{
		Values: map[string]string{"compose.tone": "professional"}, Locks: map[string]bool{"compose.tone": true},
	}); err != nil {
		t.Fatal(err)
	}
	draft, err := app.CreateDraft(ctx, CreateDraftInput{
		AccountID: account.ID, To: []string{"to@corp.local"}, Instructions: "Confirm receipt.", Tone: "friendly",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertPreferences := func() {
		t.Helper()
		for _, want := range []string{"Tone: professional", "Length: long", "Language: en", "Writing style: Use a concise opening."} {
			if !strings.Contains(provider.lastRequest.User, want) {
				t.Fatalf("missing %q in effective AI instruction: %s", want, provider.lastRequest.User)
			}
		}
		if strings.Contains(provider.lastRequest.User, "Tone: friendly") || strings.Contains(provider.lastRequest.User, "Tone: formal") {
			t.Fatal("explicit request or account preference bypassed forced tone")
		}
	}
	assertPreferences()
	if _, err := app.RewriteDraft(ctx, draft.Draft.ID, "Improve clarity."); err != nil {
		t.Fatal(err)
	}
	assertPreferences()
	if _, err := app.RenderMail(ctx, RenderMailInput{AccountID: account.ID, Body: "Another request.", SmartFormat: true, Tone: "friendly"}); err != nil {
		t.Fatal(err)
	}
	assertPreferences()
}
