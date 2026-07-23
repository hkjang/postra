package application

import (
	"testing"

	"postra/internal/domain"
)

func msg(from, subject, account string, important bool) *domain.Message {
	return &domain.Message{
		From:        domain.Address{Email: from},
		Subject:     subject,
		AccountID:   account,
		IsImportant: important,
	}
}

func TestRuleMatchesAllMode(t *testing.T) {
	r := domain.MailRule{
		Match: "all",
		Conditions: []domain.RuleCondition{
			{Field: domain.RuleFieldFrom, Operator: domain.RuleOpContains, Value: "@boss.com"},
			{Field: domain.RuleFieldSubject, Operator: domain.RuleOpStartsWith, Value: "URGENT"},
		},
	}
	if !ruleMatches(r, msg("ceo@boss.com", "URGENT: sign this", "acc", false), nil) {
		t.Fatal("expected all-mode match")
	}
	if ruleMatches(r, msg("ceo@boss.com", "fyi", "acc", false), nil) {
		t.Fatal("all-mode should fail when one condition fails")
	}
}

func TestRuleMatchesAnyMode(t *testing.T) {
	r := domain.MailRule{
		Match: "any",
		Conditions: []domain.RuleCondition{
			{Field: domain.RuleFieldFrom, Operator: domain.RuleOpContains, Value: "spam.com"},
			{Field: domain.RuleFieldSubject, Operator: domain.RuleOpContains, Value: "sale"},
		},
	}
	if !ruleMatches(r, msg("x@ok.com", "Big SALE today", "acc", false), nil) {
		t.Fatal("expected any-mode match on subject")
	}
	if ruleMatches(r, msg("x@ok.com", "hello", "acc", false), nil) {
		t.Fatal("any-mode should fail when no condition matches")
	}
}

func TestRuleMatchesBoolAndRegex(t *testing.T) {
	rBool := domain.MailRule{Match: "all", Conditions: []domain.RuleCondition{
		{Field: domain.RuleFieldIsImportant, Operator: domain.RuleOpIsTrue},
	}}
	if !ruleMatches(rBool, msg("a@b.com", "x", "acc", true), nil) {
		t.Fatal("expected is_important match")
	}
	rRe := domain.MailRule{Match: "all", Conditions: []domain.RuleCondition{
		{Field: domain.RuleFieldSubject, Operator: domain.RuleOpRegex, Value: `\[TICKET-\d+\]`},
	}}
	if !ruleMatches(rRe, msg("a@b.com", "Re: [TICKET-42] issue", "acc", false), nil) {
		t.Fatal("expected regex match")
	}
}

func TestRuleEmptyConditionsNeverMatch(t *testing.T) {
	r := domain.MailRule{Match: "all"}
	if ruleMatches(r, msg("a@b.com", "x", "acc", false), nil) {
		t.Fatal("a rule with no conditions must not match everything")
	}
}
