package application

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
)

// ---------- CRUD ----------

func (a *App) ListRules(ctx context.Context) ([]domain.MailRule, error) {
	return a.Store.ListRules(ctx, userIDFrom(ctx))
}

func (a *App) GetRule(ctx context.Context, id string) (*domain.MailRule, error) {
	return a.Store.GetRule(ctx, userIDFrom(ctx), id)
}

func (a *App) CreateRule(ctx context.Context, r domain.MailRule) (*domain.MailRule, error) {
	r.ID = persistence.NewID("rule")
	r.UserID = userIDFrom(ctx)
	if err := normalizeAndValidateRule(&r); err != nil {
		return nil, err
	}
	if err := a.Store.CreateRule(ctx, &r); err != nil {
		return nil, err
	}
	a.audit(ctx, "rule_create", "rule:"+r.ID, "ok", r.Name)
	return &r, nil
}

func (a *App) UpdateRule(ctx context.Context, r domain.MailRule) (*domain.MailRule, error) {
	r.UserID = userIDFrom(ctx)
	if r.ID == "" {
		return nil, userErrf("rule id is required")
	}
	if err := normalizeAndValidateRule(&r); err != nil {
		return nil, err
	}
	if err := a.Store.UpdateRule(ctx, &r); err != nil {
		return nil, err
	}
	a.audit(ctx, "rule_update", "rule:"+r.ID, "ok", r.Name)
	return &r, nil
}

func (a *App) DeleteRule(ctx context.Context, id string) error {
	if err := a.Store.DeleteRule(ctx, userIDFrom(ctx), id); err != nil {
		return err
	}
	a.audit(ctx, "rule_delete", "rule:"+id, "ok", "")
	return nil
}

var validRuleFields = map[string]bool{
	domain.RuleFieldFrom: true, domain.RuleFieldTo: true, domain.RuleFieldSubject: true,
	domain.RuleFieldBody: true, domain.RuleFieldAccount: true,
	domain.RuleFieldHasAttachment: true, domain.RuleFieldIsImportant: true,
}
var validRuleOps = map[string]bool{
	domain.RuleOpContains: true, domain.RuleOpEquals: true, domain.RuleOpStartsWith: true,
	domain.RuleOpEndsWith: true, domain.RuleOpRegex: true, domain.RuleOpIsTrue: true, domain.RuleOpIsFalse: true,
}
var validRuleActions = map[string]bool{
	domain.RuleActionAddLabel: true, domain.RuleActionRemoveLabel: true, domain.RuleActionArchive: true,
	domain.RuleActionMarkImportant: true, domain.RuleActionSnooze: true, domain.RuleActionDelete: true,
}

func normalizeAndValidateRule(r *domain.MailRule) error {
	if strings.TrimSpace(r.Name) == "" {
		return userErrf("rule name is required")
	}
	if r.Match == "" {
		r.Match = "all"
	}
	if r.Match != "all" && r.Match != "any" {
		return userErrf("rule match must be 'all' or 'any'")
	}
	if r.Priority == 0 {
		r.Priority = 100
	}
	if len(r.Conditions) == 0 {
		return userErrf("a rule needs at least one condition")
	}
	if len(r.Actions) == 0 {
		return userErrf("a rule needs at least one action")
	}
	for _, c := range r.Conditions {
		if !validRuleFields[c.Field] {
			return userErrf("unknown rule condition field %q", c.Field)
		}
		if !validRuleOps[c.Operator] {
			return userErrf("unknown rule condition operator %q", c.Operator)
		}
		if c.Operator == domain.RuleOpRegex {
			if _, err := regexp.Compile(c.Value); err != nil {
				return userErrf("invalid regex %q: %v", c.Value, err)
			}
		}
	}
	for _, act := range r.Actions {
		if !validRuleActions[act.Type] {
			return userErrf("unknown rule action %q", act.Type)
		}
		if (act.Type == domain.RuleActionAddLabel || act.Type == domain.RuleActionRemoveLabel) && strings.TrimSpace(act.Value) == "" {
			return userErrf("%s action requires a label value", act.Type)
		}
		if act.Type == domain.RuleActionSnooze {
			if n, err := strconv.Atoi(strings.TrimSpace(act.Value)); err != nil || n <= 0 {
				return userErrf("snooze action requires a positive seconds value")
			}
		}
	}
	return nil
}

// ---------- evaluation ----------

// RuleApplication records the actions one matched rule applied.
type RuleApplication struct {
	RuleID   string   `json:"rule_id"`
	RuleName string   `json:"rule_name"`
	Actions  []string `json:"actions"`
}

// RuleRunResult is the outcome of evaluating a message against the rule set.
type RuleRunResult struct {
	MessageID string            `json:"message_id"`
	Matched   []RuleApplication `json:"matched"`
	Deleted   bool              `json:"deleted"`
}

// ApplyRulesToMessage evaluates the caller's rules against one stored message.
func (a *App) ApplyRulesToMessage(ctx context.Context, messageID string) (*RuleRunResult, error) {
	userID := userIDFrom(ctx)
	m, err := a.Store.GetMessage(ctx, userID, messageID)
	if err != nil {
		return nil, err
	}
	body, _ := a.Store.GetBody(ctx, userID, messageID)
	rules, err := a.Store.ListRules(ctx, userID)
	if err != nil {
		return nil, err
	}
	return a.evaluateRules(ctx, m, body, rules)
}

// evaluateRulesOnIngest runs rules right after a message is stored. Best-effort:
// a failure is logged and audited but never fails the sync.
func (a *App) evaluateRulesOnIngest(ctx context.Context, m *domain.Message, body *domain.MessageBody) {
	rules, err := a.Store.ListRules(ctx, m.UserID)
	if err != nil || len(rules) == 0 {
		return
	}
	res, err := a.evaluateRules(ctx, m, body, rules)
	if err != nil {
		slog.Debug("rule evaluation on ingest failed", "message", m.ID, "err", err)
		return
	}
	if len(res.Matched) > 0 {
		a.audit(ctx, "rules_applied", "message:"+m.ID, "ok", fmt.Sprintf("matched=%d deleted=%v", len(res.Matched), res.Deleted))
	}
}

func (a *App) evaluateRules(ctx context.Context, m *domain.Message, body *domain.MessageBody, rules []domain.MailRule) (*RuleRunResult, error) {
	result := &RuleRunResult{MessageID: m.ID}
	changed := false
	for i := range rules {
		r := rules[i]
		if !r.Enabled || !ruleMatches(r, m, body) {
			continue
		}
		app := RuleApplication{RuleID: r.ID, RuleName: r.Name}
		for _, act := range r.Actions {
			switch act.Type {
			case domain.RuleActionDelete:
				if err := a.LocalDelete(ctx, m.ID); err != nil {
					return result, err
				}
				app.Actions = append(app.Actions, "delete")
				result.Matched = append(result.Matched, app)
				result.Deleted = true
				return result, nil // message gone; stop
			case domain.RuleActionArchive:
				m.IsArchived, changed = true, true
				app.Actions = append(app.Actions, "archive")
			case domain.RuleActionMarkImportant:
				m.IsImportant, changed = true, true
				app.Actions = append(app.Actions, "mark_important")
			case domain.RuleActionAddLabel:
				m.Labels, changed = addLabel(m.Labels, act.Value), true
				app.Actions = append(app.Actions, "add_label:"+act.Value)
			case domain.RuleActionRemoveLabel:
				m.Labels, changed = removeLabel(m.Labels, act.Value), true
				app.Actions = append(app.Actions, "remove_label:"+act.Value)
			case domain.RuleActionSnooze:
				if sec, err := strconv.Atoi(strings.TrimSpace(act.Value)); err == nil && sec > 0 {
					m.SnoozedUntil, changed = time.Now().Add(time.Duration(sec)*time.Second).Unix(), true
					app.Actions = append(app.Actions, "snooze")
				}
			}
		}
		result.Matched = append(result.Matched, app)
		if r.StopOnMatch {
			break
		}
	}
	if changed {
		if err := a.Store.UpdateMessage(ctx, m); err != nil {
			return result, err
		}
	}
	return result, nil
}

// ruleMatches reports whether a message satisfies a rule's conditions under its
// match mode (all = AND, any = OR).
func ruleMatches(r domain.MailRule, m *domain.Message, body *domain.MessageBody) bool {
	if len(r.Conditions) == 0 {
		return false
	}
	for _, c := range r.Conditions {
		ok := matchCondition(c, m, body)
		if r.Match == "any" && ok {
			return true
		}
		if r.Match != "any" && !ok {
			return false
		}
	}
	return r.Match != "any" // all-mode reaching here means every cond matched
}

func matchCondition(c domain.RuleCondition, m *domain.Message, body *domain.MessageBody) bool {
	switch c.Field {
	case domain.RuleFieldHasAttachment:
		return matchBool(c.Operator, m.HasAttachments)
	case domain.RuleFieldIsImportant:
		return matchBool(c.Operator, m.IsImportant)
	}
	var hay string
	switch c.Field {
	case domain.RuleFieldFrom:
		hay = strings.TrimSpace(m.From.Email + " " + m.From.Name)
	case domain.RuleFieldTo:
		var parts []string
		for _, addr := range m.To {
			parts = append(parts, addr.Email)
		}
		hay = strings.Join(parts, " ")
	case domain.RuleFieldSubject:
		hay = m.Subject
	case domain.RuleFieldBody:
		if body != nil {
			hay = body.TextBody
		}
	case domain.RuleFieldAccount:
		hay = m.AccountID
	default:
		return false
	}
	return matchString(c.Operator, c.Value, hay)
}

func matchBool(op string, v bool) bool {
	switch op {
	case domain.RuleOpIsTrue:
		return v
	case domain.RuleOpIsFalse:
		return !v
	default:
		return false
	}
}

func matchString(op, value, hay string) bool {
	if op == domain.RuleOpRegex {
		re, err := regexp.Compile(value)
		return err == nil && re.MatchString(hay)
	}
	h, v := strings.ToLower(hay), strings.ToLower(value)
	switch op {
	case domain.RuleOpContains:
		return strings.Contains(h, v)
	case domain.RuleOpEquals:
		return h == v
	case domain.RuleOpStartsWith:
		return strings.HasPrefix(h, v)
	case domain.RuleOpEndsWith:
		return strings.HasSuffix(h, v)
	default:
		return false
	}
}

// DraftRuleFromText turns a plain-language filing request ("뉴스레터는 보관함으로")
// into a concrete rule. The rule is validated but deliberately NOT saved: the
// model proposes, the same validator that guards hand-written rules enforces
// the schema, and the user confirms before anything starts acting on their
// mail. Callers persist the result with CreateRule.
func (a *App) DraftRuleFromText(ctx context.Context, instruction string) (*domain.MailRule, error) {
	instruction = strings.TrimSpace(instruction)
	if instruction == "" {
		return nil, userErrf("규칙으로 만들 요청 문장이 비어 있습니다")
	}
	// The request goes in the untrusted block: it is user text, and keeping it
	// out of the instruction channel means a pasted mail snippet cannot rewrite
	// the prompt (AI-014).
	an, err := a.runAnalysis(ctx, "rule_from_text", "query", "adhoc", "", instruction)
	if err != nil {
		return nil, err
	}
	var r domain.MailRule
	if err := json.Unmarshal([]byte(an.ResultJSON), &r); err != nil {
		return nil, userErrf("AI가 규칙 형식을 반환하지 못했습니다: %v", err)
	}
	if len(r.Conditions) == 0 {
		return nil, userErrf("요청을 규칙 조건으로 해석하지 못했습니다. 대상(보낸사람/제목 등)과 동작을 더 구체적으로 적어주세요")
	}
	if len(r.Actions) == 0 {
		return nil, userErrf("요청에서 수행할 동작을 찾지 못했습니다 (예: 라벨 추가, 보관, 중요 표시)")
	}
	r.UserID = userIDFrom(ctx)
	r.Enabled = true
	if strings.TrimSpace(r.Name) == "" {
		r.Name = truncateRunes(instruction, 60)
	}
	// Enforce exactly the same invariants as a hand-authored rule.
	if err := normalizeAndValidateRule(&r); err != nil {
		return nil, userErrf("AI가 만든 규칙이 유효하지 않습니다: %v", err)
	}
	a.audit(ctx, "rule_draft_from_text", "rule:preview", "ok", truncateRunes(instruction, 120))
	return &r, nil
}
