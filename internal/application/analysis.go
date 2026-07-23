package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"postra/internal/adapters/persistence"
	"postra/internal/domain"
	"postra/internal/platform/mask"
)

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
	u, err := url.Parse(a.currentAIConfig().BaseURL)
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
	if err := a.checkAIPolicy(ctx); err != nil {
		return nil, err
	}
	aiCfg := a.currentAIConfig()
	sum := sha256.Sum256([]byte(analysisType + "|" + pv + "|" + aiCfg.Model + "|" + userTask + "|" + untrusted))
	inputHash := hex.EncodeToString(sum[:])
	if cached, err := a.Store.FindCachedAnalysis(ctx, userID, analysisType, inputHash, aiCfg.Model); err == nil {
		return cached, nil // AI-008 cache
	}

	task := spec.task
	if userTask != "" {
		task += "\n\nUser instruction: " + userTask
	}
	// AI-011: mask PII/secrets before content leaves the box to an external
	// endpoint. Local endpoints skip masking unless forced by policy.
	content := truncateRunes(untrusted, maxAIBodyChars)
	if aiCfg.MaskExternalPII && !a.aiEndpointLocal(ctx) {
		masked, hits := mask.Mask(content)
		content = masked
		if len(hits) > 0 {
			a.audit(ctx, "ai_pii_masked", targetType+":"+targetID, "ok", fmt.Sprintf("%v", hits))
		}
	}
	res, err := a.AI.Generate(ctx, domain.GenerationRequest{
		System:    spec.system,
		User:      task,
		Untrusted: content,
		JSONMode:  true,
	})
	if err != nil {
		a.audit(ctx, "ai_analysis", targetType+":"+targetID, "error", analysisType+": "+err.Error())
		return nil, err
	}
	resultJSON, err := extractJSON(res.Text)
	if err != nil {
		return nil, fmt.Errorf("AI returned non-JSON output (AI-005 validation failed): %w", err)
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
	if strings.TrimSpace(question) == "" {
		return nil, userErrf("question is empty")
	}
	res, err := a.Search(ctx, domain.SearchQuery{Text: question, AccountID: accountID, Limit: 5})
	if err != nil {
		return nil, err
	}
	if len(res.Messages) == 0 {
		// keyword fallback: recent messages
		res, err = a.Search(ctx, domain.SearchQuery{AccountID: accountID, Limit: 5})
		if err != nil {
			return nil, err
		}
	}
	var sb strings.Builder
	for _, m := range res.Messages {
		body, _ := a.Store.GetBody(ctx, userIDFrom(ctx), m.ID)
		text := ""
		if body != nil {
			text = truncateRunes(body.TextBody, 2500)
		}
		fmt.Fprintf(&sb, "[%s] Subject: %s | From: %s | Date: %s\n%s\n\n",
			m.ID, m.Subject, m.From.Email, fmtUnix(m.Date), text)
	}
	return a.runAnalysis(ctx, "question_answer", "query", "adhoc", "Question: "+question, sb.String())
}

func fmtUnix(u int64) string {
	if u <= 0 {
		return "unknown"
	}
	return time.Unix(u, 0).UTC().Format(time.RFC3339)
}
