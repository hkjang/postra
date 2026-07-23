package domain

// MailRule is a user-defined automation: when an incoming (or selected)
// message matches the conditions, the actions are applied (§자동화 메일 규칙 엔진).
// Rules are evaluated in ascending Priority order.
type MailRule struct {
	ID          string          `json:"id"`
	UserID      string          `json:"user_id"`
	Name        string          `json:"name"`
	Enabled     bool            `json:"enabled"`
	Priority    int             `json:"priority"`
	Match       string          `json:"match"` // "all" (AND) | "any" (OR)
	Conditions  []RuleCondition `json:"conditions"`
	Actions     []RuleAction    `json:"actions"`
	StopOnMatch bool            `json:"stop_on_match"`
	CreatedAt   int64           `json:"created_at"`
	UpdatedAt   int64           `json:"updated_at"`
}

// RuleCondition tests one field of a message.
type RuleCondition struct {
	Field    string `json:"field"`    // from | to | subject | body | account | has_attachment | is_important
	Operator string `json:"operator"` // contains | equals | starts_with | ends_with | regex | is_true | is_false
	Value    string `json:"value"`
}

// RuleAction is applied when a rule matches.
type RuleAction struct {
	Type  string `json:"type"`  // add_label | remove_label | archive | mark_important | snooze | delete
	Value string `json:"value"` // label for label actions; seconds-from-now for snooze
}

// Rule field, operator, and action vocabularies.
const (
	RuleFieldFrom          = "from"
	RuleFieldTo            = "to"
	RuleFieldSubject       = "subject"
	RuleFieldBody          = "body"
	RuleFieldAccount       = "account"
	RuleFieldHasAttachment = "has_attachment"
	RuleFieldIsImportant   = "is_important"

	RuleOpContains   = "contains"
	RuleOpEquals     = "equals"
	RuleOpStartsWith = "starts_with"
	RuleOpEndsWith   = "ends_with"
	RuleOpRegex      = "regex"
	RuleOpIsTrue     = "is_true"
	RuleOpIsFalse    = "is_false"

	RuleActionAddLabel      = "add_label"
	RuleActionRemoveLabel   = "remove_label"
	RuleActionArchive       = "archive"
	RuleActionMarkImportant = "mark_important"
	RuleActionSnooze        = "snooze"
	RuleActionDelete        = "delete"
)
