package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
	"postra/internal/platform/mask"
)

// errAIOutputInvalid marks a response the model produced but that failed
// validation. Callers must distinguish it from a provider outage: the output
// is deterministic for a given input (and cached), so retrying the same
// message is futile, whereas an outage should simply be retried later.
var errAIOutputInvalid = errors.New("AI returned non-JSON output")

// maxAIBodyChars bounds untrusted content per request.
const maxAIBodyChars = 12000

type promptSpec struct {
	system string
	task   string
}

type versionedPrompt struct {
	version string
	spec    promptSpec
}

// promptRegistry versions each analysis prompt (AI-013). Versions are ordered
// oldest→newest; the newest is active by default. ai.prompt_versions can pin
// or roll back a type to an earlier version. The chosen version is recorded on
// every analysis and folded into the cache key.
var promptRegistry = map[string][]versionedPrompt{
	"summarize": {
		{"v1", promptSpec{
			system: "You are an email analysis assistant. Respond with JSON only.",
			task: `Summarize the email in the untrusted block. Respond in the email's main language.
JSON schema: {"summary": string, "requests": [string], "dates": [string], "confidence": number (0-1)}`,
		}},
		{"v2", promptSpec{
			system: "You are a precise email analysis assistant. Respond with JSON only and never invent facts absent from the email.",
			task: `Summarize the email in the untrusted block in its main language. Be concise and cite only what is present.
JSON schema: {"summary": string, "requests": [string], "dates": [string], "confidence": number (0-1)}`,
		}},
	},
}

// singleVersionPrompts hold prompts that currently have only a v1.
var singleVersionPrompts = map[string]promptSpec{
	"triage": {
		system: "You are a senior email operations analyst. Respond with JSON only, distinguish explicit facts from recommendations, and never invent deadlines.",
		task: `Triage the email for a busy professional.
JSON schema: {"sender_intent": string, "priority": "urgent"|"high"|"normal"|"low", "reply_required": boolean, "deadline": string|null, "business_risks": [string], "recommended_next_action": string, "confidence": number}
Urgent requires explicit time sensitivity or material operational/security impact. A deadline must be copied from the email or null.`,
	},
	"classify": {
		system: "You are an email classification assistant. Respond with JSON only.",
		task: `Classify the email in the untrusted block.
JSON schema: {"category": "work"|"advertisement"|"notification"|"personal"|"security"|"other", "importance": "high"|"normal"|"low", "reason": string, "confidence": number}`,
	},
	"action_items": {
		system: "You are an email task extraction assistant. Respond with JSON only.",
		task: `Extract action items from the email in the untrusted block.
JSON schema: {"items": [{"task": string, "assignee": string|null, "due": string|null, "evidence": string, "confidence": number}]}
Low-confidence dates/assignees must have confidence < 0.5 so the user reviews them.`,
	},
	"action_cards": {
		system: "You are an assistant that turns emails into actionable cards. Respond with JSON only and never invent facts absent from the email.",
		task: `Extract actionable cards from the email in the untrusted block. Each card is one concrete action a person could take.
JSON schema: {"cards": [{"type": "meeting"|"todo"|"approval"|"inquiry"|"other", "title": string, "detail": string, "due": string|null, "assignee": string|null, "confidence": number}]}
Copy any due date verbatim from the email or use null. Return an empty cards array when there is nothing actionable.`,
	},
	"entities": {
		system: "You are an email entity extraction assistant. Respond with JSON only.",
		task: `Extract entities from the email in the untrusted block.
JSON schema: {"people": [string], "companies": [string], "projects": [string], "amounts": [string], "contacts": [string]}`,
	},
	"phishing": {
		system: "You are an email security analyst. Respond with JSON only.",
		task: `Assess phishing risk of the email in the untrusted block (headers included).
JSON schema: {"risk_score": number (0-100), "indicators": [string], "recommendation": string}`,
	},
	"document_summary": {
		system: "You are a document analysis assistant. Respond with JSON only and never invent facts absent from the document.",
		task: `Summarize the document text in the untrusted block.
JSON schema: {"summary": string, "key_points": [string], "tables_or_figures": [string], "risks": [string], "confidence": number (0-1)}`,
	},
	"thread_summary": {
		system: "You are an email thread analysis assistant. Respond with JSON only.",
		task: `The untrusted block contains a conversation (multiple emails, oldest first).
JSON schema: {"progress": string, "decisions": [string], "open_items": [string], "next_action": string}`,
	},
	"question_answer": {
		system: "You are an email question-answering assistant. Respond with JSON only. Answer strictly from the provided emails; if the answer is not present, say so.",
		task: `Answer the user's question using only the emails in the untrusted block. Each email is prefixed with [message_id].
JSON schema: {"answer": string, "evidence_message_ids": [string], "confidence": number}`,
	},
	"draft_reply": {
		system: "You are an email drafting assistant. Respond with JSON only. You draft replies; you never send mail or take actions.",
		task: `Write a reply draft to the email in the untrusted block, following the user instruction given above the block.
JSON schema: {"subject": string, "body": string, "language": string}`,
	},
	"rewrite": {
		system: "You are an email rewriting assistant. Respond with JSON only.",
		task: `Rewrite the draft in the untrusted block according to the user instruction given above the block. Keep the factual content identical.
JSON schema: {"subject": string, "body": string}`,
	},
	"daily_digest": {
		system: "You write a daily mail briefing for a busy professional. Respond with JSON only. Summarize only what the emails actually say; never invent senders, deadlines, or requests. Write in the language most of the mail uses.",
		task: `The untrusted block contains the emails received in the period, each prefixed with [message_id].
JSON schema: {"headline": string, "needs_reply": [{"message_id": string, "who": string, "what": string}], "deadlines": [{"message_id": string, "what": string, "when": string}], "fyi": [string], "volume_note": string}
Put only genuinely action-requiring mail in needs_reply, and copy deadlines verbatim from the mail. Keep the headline to one sentence.`,
	},
	"calendar_events": {
		system: "You extract calendar events from email. Respond with JSON only. Never invent a date, time, or location that is not in the email; omit a field you cannot ground. Times must be copied from the email, not guessed.",
		task: `Extract the schedulable events described by the email in the untrusted block.
JSON schema: {"events": [{"title": string, "start": string, "end": string|null, "all_day": boolean, "location": string|null, "description": string|null, "confidence": number (0-1)}]}
start/end use RFC3339 with offset when a time is given (2026-03-04T15:00:00+09:00), or YYYY-MM-DD when the email states only a date (then all_day is true). Return an empty events array when the email schedules nothing.`,
	},
	"rule_from_text": {
		system: "You translate a person's plain-language mail-filing request into a Postra rule. Respond with JSON only. Use only the fields and enum values given; never invent new ones. When the request is too vague to express as a rule, return an empty conditions array.",
		task: `The untrusted block contains the user's request, in their own words. Produce one mail rule expressing it.
JSON schema: {"name": string, "match": "all"|"any", "stop_on_match": boolean, "conditions": [{"field": "from"|"to"|"subject"|"body"|"account"|"has_attachment"|"is_important", "operator": "contains"|"equals"|"starts_with"|"ends_with"|"regex"|"is_true"|"is_false", "value": string}], "actions": [{"type": "add_label"|"remove_label"|"archive"|"mark_important"|"snooze"|"delete", "value": string}]}
Rules: use "contains" unless the user clearly demands an exact or pattern match; has_attachment/is_important take operator is_true or is_false with an empty value; snooze value is seconds from now; label actions put the label in value; other actions use an empty value. Name the rule concisely in the user's language.`,
	},
	"smart_reply": {
		system: "You are an email reply assistant. Respond with JSON only. Write short, ready-to-send replies in the SAME language as the email. Never invent facts, commitments, dates, numbers, or names that are not in the email; when a detail is unknown keep the wording general. Write natural prose — no placeholders like [Name] and no signature.",
		task: `Draft 3 distinct, concise reply options (each 1–3 sentences) for the email in the untrusted block, covering the stances that fit the content — for example an affirmative/accepting reply, a reply that asks a clarifying question, and a brief holding or polite-decline reply.
JSON schema: {"suggestions": [string, string, string]}`,
	},
}

func init() {
	// Fold single-version prompts into the registry as "v1".
	for name, spec := range singleVersionPrompts {
		promptRegistry[name] = []versionedPrompt{{"v1", spec}}
	}
}

// activePrompt resolves the prompt version for an analysis type: the config
// override (ai.prompt_versions) if present and valid, else the newest version
// (AI-013). Returns the version string and spec.
func (a *App) activePrompt(analysisType string) (string, promptSpec, bool) {
	versions, ok := promptRegistry[analysisType]
	if !ok || len(versions) == 0 {
		return "", promptSpec{}, false
	}
	if pin, ok := a.currentAIConfig().PromptVersions[analysisType]; ok {
		for _, vp := range versions {
			if vp.version == pin {
				return vp.version, vp.spec, true
			}
		}
	}
	last := versions[len(versions)-1]
	return last.version, last.spec, true
}

// aiEndpointLocal reports whether the configured AI endpoint resolves only to
// loopback/private addresses (no exfiltration risk).
func (a *App) aiEndpointLocal(ctx context.Context) bool {
	return localAIEndpoint(ctx, a.currentAIConfig().BaseURL)
}

func (a *App) aiTaskEndpoint(task string) string {
	cfg := a.currentAIConfig()
	if task == "embedding" {
		if cfg.EmbedBaseURL != "" {
			return cfg.EmbedBaseURL
		}
		return cfg.BaseURL
	}
	return cfg.RouteForTask(task).BaseURL
}

func (a *App) checkTaskAIPolicy(ctx context.Context, task string) error {
	if a.currentAIConfig().AllowExternal || localAIEndpoint(ctx, a.aiTaskEndpoint(task)) {
		return nil
	}
	return userErrf("선택한 AI 작업은 외부 Endpoint를 사용합니다. 관리자 외부 AI 호출 정책을 확인하세요")
}

func (a *App) maskTaskInput(ctx context.Context, task, input string) string {
	if a.currentAIConfig().MaskExternalPII && !localAIEndpoint(ctx, a.aiTaskEndpoint(task)) {
		masked, _ := mask.Mask(input)
		return masked
	}
	return input
}

func localAIEndpoint(ctx context.Context, endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate()
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return false
	}
	for _, ip := range ips {
		if !ip.IsLoopback() && !ip.IsPrivate() {
			return false
		}
	}
	return len(ips) > 0
}

// checkAIPolicy blocks sending mail content to non-local AI endpoints unless
// explicitly allowed (AI-011/012, §13 data-exfiltration control).
func (a *App) checkAIPolicy(ctx context.Context) error {
	cfg := a.currentAIConfig()
	if cfg.AllowExternal || a.aiEndpointLocal(ctx) {
		return nil
	}
	u, _ := url.Parse(cfg.BaseURL)
	host := ""
	if u != nil {
		host = u.Hostname()
	}
	return userErrf("AI endpoint %s is external; set ai.allow_external=true to permit sending mail content outside", host)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n[... truncated ...]"
}

func (a *App) runAnalysis(ctx context.Context, analysisType, targetType, targetID, userTask, untrusted string) (*domain.Analysis, error) {
	userID := userIDFrom(ctx)
	pv, spec, ok := a.activePrompt(analysisType)
	if !ok {
		return nil, userErrf("unknown analysis type %q", analysisType)
	}
	aiCfg := a.currentAIConfig()
	taskKey := analysisType
	switch analysisType {
	case "draft_reply":
		taskKey = "compose"
	case "question_answer":
		taskKey = "qa"
	case "daily_digest":
		taskKey = "digest"
	case "triage":
		taskKey = "classify"
	}
	if analysisType == "rewrite" && targetType == "mail" && targetID == "render" {
		taskKey = "smart_format"
	}
	// Explicit legacy task routes keep their meaning during migration.
	if _, legacy := aiCfg.TaskModels[analysisType]; legacy {
		taskKey = analysisType
	}
	// Resolve the effective model for this task so the cache key and stored
	// record reflect the actually-used model (§AI 작업별 모델 라우팅).
	route := aiCfg.RouteForTask(taskKey)
	if !aiCfg.AllowExternal && !localAIEndpoint(ctx, route.BaseURL) {
		return nil, userErrf("선택한 AI 작업은 외부 Endpoint를 사용합니다. 관리자 외부 AI 호출 정책을 확인하세요")
	}
	routedModel := route.Model
	routeJSON, _ := json.Marshal(struct {
		Route       any
		Temperature float64
		Context     int
		Mask        bool
	}{route, aiCfg.Temperature, aiCfg.ContextLength, aiCfg.MaskExternalPII})
	sum := sha256.Sum256([]byte(analysisType + "|" + pv + "|" + string(routeJSON) + "|" + userTask + "|" + untrusted))
	inputHash := hex.EncodeToString(sum[:])
	if cached, err := a.Store.FindCachedAnalysis(ctx, userID, analysisType, inputHash, routedModel); err == nil {
		return cached, nil // AI-008 cache
	}

	task := spec.task
	if userTask != "" {
		task += "\n\nUser instruction: " + userTask
	}
	// AI-011: mask PII/secrets before content leaves the box to an external
	// endpoint. Local endpoints skip masking unless forced by policy.
	content := truncateRunes(untrusted, maxAIBodyChars)
	// Screen the mail content for attempts to hijack the model. Content stays
	// in the untrusted block regardless (AI-014); this records the attempt so
	// the user/admin can distrust anything derived from this message.
	a.guardUntrusted(ctx, analysisType, targetType, targetID, content)
	if aiCfg.MaskExternalPII && !localAIEndpoint(ctx, route.BaseURL) {
		masked, hits := mask.Mask(content)
		content = masked
		task, _ = mask.Mask(task)
		if len(hits) > 0 {
			a.audit(ctx, "ai_pii_masked", targetType+":"+targetID, "ok", fmt.Sprintf("%v", hits))
		}
	}
	res, err := a.AI.Generate(ctx, domain.GenerationRequest{
		System:    spec.system,
		User:      task,
		Untrusted: content,
		JSONMode:  true,
		Task:      taskKey,
	})
	if err != nil {
		a.audit(ctx, "ai_analysis", targetType+":"+targetID, "error", analysisType+": "+providerDiagnostic(err))
		return nil, err
	}
	resultJSON, err := extractJSON(res.Text)
	if err != nil {
		return nil, fmt.Errorf("%w (AI-005 validation failed): %v", errAIOutputInvalid, err)
	}
	an := &domain.Analysis{
		ID: persistence.NewID("ana"), UserID: userID,
		TargetType: targetType, TargetID: targetID, AnalysisType: analysisType,
		ResultJSON: resultJSON, Model: res.Model, PromptVersion: pv, InputHash: inputHash,
	}
	if err := a.Store.SaveAnalysis(ctx, an); err != nil {
		return nil, err
	}
	a.audit(ctx, "ai_analysis", targetType+":"+targetID, "ok", analysisType)
	return an, nil
}

// extractJSON validates the model output as a JSON object, tolerating
// markdown fences local models like to add.
func extractJSON(text string) (string, error) {
	t := strings.TrimSpace(text)
	if i := strings.Index(t, "{"); i >= 0 {
		if j := strings.LastIndex(t, "}"); j > i {
			t = t[i : j+1]
		}
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(t), &v); err != nil {
		return "", err
	}
	return t, nil
}

func (a *App) messageAsAIInput(ctx context.Context, messageID string, includeHeaders bool) (*domain.Message, string, error) {
	mv, err := a.GetMessage(ctx, messageID, true)
	if err != nil {
		return nil, "", err
	}
	var sb strings.Builder
	m := mv.Message
	fmt.Fprintf(&sb, "Subject: %s\nFrom: %s <%s>\nDate: %s\n",
		m.Subject, m.From.Name, m.From.Email, fmtUnix(m.Date))
	if includeHeaders {
		fmt.Fprintf(&sb, "Message-ID: %s\nAuthentication-Results: %s\n", m.MessageID, m.AuthResults)
	}
	sb.WriteString("\n")
	if mv.Body != nil {
		sb.WriteString(mv.Body.TextBody)
	}
	return &mv.Message, sb.String(), nil
}

func (a *App) AnalyzeMessage(ctx context.Context, messageID, analysisType string) (*domain.Analysis, error) {
	_, input, err := a.messageAsAIInput(ctx, messageID, analysisType == "phishing")
	if err != nil {
		return nil, err
	}
	return a.runAnalysis(ctx, analysisType, "message", messageID, "", input)
}

func (a *App) SummarizeThread(ctx context.Context, threadID string) (*domain.Analysis, error) {
	tv, err := a.GetThread(ctx, threadID, true)
	if err != nil {
		return nil, err
	}
	var sb strings.Builder
	for _, mv := range tv.Messages {
		fmt.Fprintf(&sb, "--- [%s] %s | %s | %s ---\n",
			mv.Message.ID, fmtUnix(mv.Message.Date), mv.Message.From.Email, mv.Message.Subject)
		if mv.Body != nil {
			sb.WriteString(truncateRunes(mv.Body.TextBody, 3000))
		}
		sb.WriteString("\n")
	}
	return a.runAnalysis(ctx, "thread_summary", "thread", threadID, "", sb.String())
}

// AnswerQuestion retrieves candidate mails within the user's own scope and
// asks the model to answer with per-message citations (AI-009).
func (a *App) AnswerQuestion(ctx context.Context, question, accountID string) (*domain.Analysis, error) {
	result, err := a.Ask(ctx, AskInput{Question: question, AccountID: accountID})
	if err != nil {
		return nil, err
	}
	return result.Analysis, nil
}

func fmtUnix(u int64) string {
	if u <= 0 {
		return "unknown"
	}
	return time.Unix(u, 0).UTC().Format(time.RFC3339)
}
