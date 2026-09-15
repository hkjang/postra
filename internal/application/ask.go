package application

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	_ "time/tzdata" // IANA zones remain available in the offline scratch image.

	"postra/internal/domain"
)

// AskInput is the shared REST/MCP retrieval contract. Explicit filters always
// take precedence over the small deterministic natural-language date router.
// SearchText is optional: it is a precise keyword constraint, never discarded.
type AskInput struct {
	Question       string `json:"question" jsonschema:"question to answer from your own mail and work evidence"`
	AccountID      string `json:"account_id,omitempty" jsonschema:"optional owned account"`
	Mode           string `json:"mode,omitempty" jsonschema:"auto, keyword, semantic, hybrid, work, or actions"`
	SearchText     string `json:"search_text,omitempty" jsonschema:"optional precise keyword filter; no unrelated-mail fallback"`
	From           string `json:"from,omitempty" jsonschema:"optional sender filter"`
	Since          int64  `json:"since,omitempty" jsonschema:"inclusive sender-date lower bound, Unix seconds"`
	Until          int64  `json:"until,omitempty" jsonschema:"exclusive sender-date upper bound, Unix seconds"`
	TimeZone       string `json:"time_zone,omitempty" jsonschema:"IANA zone for relative dates; defaults to UTC"`
	IncompleteOnly bool   `json:"incomplete_only,omitempty" jsonschema:"exclude completed work/actions; does not prove a mail is unanswered"`
}

type AskSource struct {
	MessageID  string   `json:"message_id"`
	Subject    string   `json:"subject"`
	From       string   `json:"from"`
	Date       int64    `json:"date"`
	WorkStatus string   `json:"work_status,omitempty"`
	ActionIDs  []string `json:"action_ids,omitempty"`
}
type AskRetrieval struct {
	Mode     string      `json:"mode"`
	Since    int64       `json:"since,omitempty"`
	Until    int64       `json:"until,omitempty"`
	TimeZone string      `json:"time_zone"`
	Sources  []AskSource `json:"sources"`
	Warnings []string    `json:"warnings"`
	Bounded  bool        `json:"bounded"`
}
type AskResult struct {
	*domain.Analysis
	Retrieval AskRetrieval `json:"retrieval"`
}

func containsAny(text string, terms ...string) bool {
	for _, term := range terms {
		if strings.Contains(text, term) {
			return true
		}
	}
	return false
}

func askPlan(in AskInput, now time.Time) (AskInput, error) {
	in.Question = strings.TrimSpace(in.Question)
	if in.Question == "" || len([]rune(in.Question)) > 4000 {
		return in, userErrf("질문은 1~4,000자여야 합니다")
	}
	if len(in.SearchText) > 1000 || len(in.From) > 320 {
		return in, userErrf("검색 조건이 너무 깁니다")
	}
	if in.Since < 0 || in.Until < 0 || in.Since > 253402300799 || in.Until > 253402300799 || in.Until > 0 && in.Since >= in.Until {
		return in, userErrf("검색 기간이 올바르지 않습니다")
	}
	if in.TimeZone == "" {
		in.TimeZone = "UTC"
	}
	zone, err := time.LoadLocation(in.TimeZone)
	if err != nil {
		return in, userErrf("시간대가 올바르지 않습니다")
	}
	q := strings.ToLower(in.Question)
	if in.Mode == "" {
		in.Mode = "auto"
	}
	if in.Mode == "auto" {
		switch {
		case containsAny(q, "action", "액션", "해야 할 일", "할일"):
			in.Mode = "actions"
		case containsAny(q, "업무", "미완료", "진행 중", "work", "task"):
			in.Mode = "work"
		default:
			in.Mode = "hybrid"
		}
	}
	switch in.Mode {
	case "keyword", "semantic", "hybrid", "work", "actions":
	default:
		return in, userErrf("지원하지 않는 검색 방식입니다")
	}
	if containsAny(q, "미완료", "아직 끝나지", "아직 끝내지", "incomplete", "unfinished") {
		in.IncompleteOnly = true
	}
	if in.Since == 0 && in.Until == 0 {
		now = now.In(zone)
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, zone)
		var since, until time.Time
		switch {
		case containsAny(q, "어제", "yesterday"):
			since, until = start.AddDate(0, 0, -1), start
		case containsAny(q, "지난주", "지난 주", "last week"):
			monday := start.AddDate(0, 0, -(int(start.Weekday())+6)%7)
			since, until = monday.AddDate(0, 0, -7), monday
		case containsAny(q, "이번 달", "이번달", "this month"):
			since = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, zone)
			until = since.AddDate(0, 1, 0)
		case containsAny(q, "오늘", "today"):
			since, until = start, start.AddDate(0, 0, 1)
		case containsAny(q, "최근 7일", "지난 7일", "last 7 days"):
			since, until = start.AddDate(0, 0, -6), start.AddDate(0, 0, 1)
		}
		if !since.IsZero() {
			in.Since, in.Until = since.Unix(), until.Unix()
		}
	}
	return in, nil
}

func askMatches(m domain.Message, in AskInput) bool {
	return (in.AccountID == "" || m.AccountID == in.AccountID) &&
		(in.Since == 0 || m.Date >= in.Since) && (in.Until == 0 || m.Date < in.Until) &&
		(in.From == "" || strings.Contains(strings.ToLower(m.From.Email+" "+m.From.Name), strings.ToLower(in.From)))
}

// Ask never searches outside the principal's namespace, including when the
// principal is an administrator. Work state is evidence, not an inferred reply
// receipt. A bounded/recent fallback is explicitly disclosed to both clients
// and the model, and cannot silently relax an explicit keyword/date filter.
func (a *App) Ask(ctx context.Context, input AskInput) (*AskResult, error) {
	in, err := askPlan(input, time.Now())
	if err != nil {
		return nil, err
	}
	preferences, err := a.PersonalSettings(ctx, input.AccountID)
	if err != nil {
		return nil, err
	}
	for _, field := range preferences.Fields {
		if field.Key == "search.default_mode" && (field.Locked || (input.Mode == "" || input.Mode == "auto") && in.Mode == "hybrid") {
			in.Mode = field.Value
		}
	}
	if in.AccountID != "" {
		if _, err := a.GetAccount(ctx, in.AccountID); err != nil {
			return nil, err
		}
	}
	retrieval := AskRetrieval{Mode: in.Mode, Since: in.Since, Until: in.Until, TimeZone: in.TimeZone, Sources: []AskSource{}, Warnings: []string{}, Bounded: true}
	msgs := []domain.Message{}
	cardsByMessage := map[string][]domain.ActionCard{}
	cards, err := a.ListActionCards(ctx, "", 200)
	if err != nil {
		return nil, err
	}
	for _, card := range cards {
		cardsByMessage[card.MessageID] = append(cardsByMessage[card.MessageID], card)
	}
	seen := map[string]bool{}
	add := func(m domain.Message) {
		if askMatches(m, in) && !seen[m.ID] {
			seen[m.ID] = true
			msgs = append(msgs, m)
		}
	}
	const candidateLimit = 200
	switch in.Mode {
	case "actions":
		for _, card := range cards {
			if in.IncompleteOnly && (card.Status == domain.ActionCardDone || card.Status == domain.ActionCardRejected || card.Status == domain.ActionCardExported) {
				continue
			}
			m, err := a.Store.GetMessage(ctx, userIDFrom(ctx), card.MessageID)
			if err != nil {
				continue
			}
			if in.SearchText != "" && !strings.Contains(strings.ToLower(m.Subject+" "+card.Title+" "+card.Detail), strings.ToLower(in.SearchText)) {
				continue
			}
			add(*m)
		}
	case "work":
		rows, err := a.TeamInbox(ctx, "", "", candidateLimit)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			status, _ := domain.CanonicalCollabStatus(row.Collab.Status)
			if row.Message == nil || in.IncompleteOnly && status == domain.CollabDone {
				continue
			}
			if in.SearchText != "" && !strings.Contains(strings.ToLower(row.Message.Subject+" "+row.Collab.Assignee), strings.ToLower(in.SearchText)) {
				continue
			}
			add(*row.Message)
		}
	default:
		text := strings.TrimSpace(in.SearchText)
		if text == "" {
			text = in.Question
		}
		// A precise keyword filter uses keyword retrieval even when the selected
		// mode normally uses vectors; semantic similarity is not an exact filter.
		if in.SearchText == "" && (in.Mode == "hybrid" || in.Mode == "semantic") {
			var hits []MessageView
			if in.Mode == "semantic" {
				hits, err = a.SemanticSearch(ctx, text, in.AccountID, 50)
			} else {
				hits, err = a.HybridSearch(ctx, HybridSearchOptions{Query: text, AccountID: in.AccountID, Limit: 50})
			}
			if err != nil {
				retrieval.Warnings = append(retrieval.Warnings, "의미 검색을 사용할 수 없어 키워드 검색으로 전환했습니다.")
			}
			for _, hit := range hits {
				add(hit.Message)
			}
		}
		if len(msgs) == 0 {
			res, err := a.Search(ctx, domain.SearchQuery{AccountID: in.AccountID, Text: text, From: in.From, Since: in.Since, Until: in.Until, Limit: 20})
			if err != nil {
				return nil, err
			}
			for _, m := range res.Messages {
				add(m)
			}
		}
		if len(msgs) == 0 && in.SearchText == "" {
			res, err := a.Search(ctx, domain.SearchQuery{AccountID: in.AccountID, From: in.From, Since: in.Since, Until: in.Until, Limit: 20})
			if err != nil {
				return nil, err
			}
			for _, m := range res.Messages {
				add(m)
			}
			retrieval.Warnings = append(retrieval.Warnings, "질문과 일치하는 검색 결과가 없어 지정 기간의 최근 메일을 참고했습니다. 검색어를 지정하면 이 대체 검색을 하지 않습니다.")
		}
	}
	if in.Mode == "work" || in.Mode == "actions" {
		retrieval.Warnings = append(retrieval.Warnings, "최근 업무/Action 최대 200건에서 찾았습니다. 업무 미완료 상태만으로 실제 미회신 여부를 단정하지 않습니다.")
	}
	if len(msgs) > 6 {
		msgs = msgs[:6]
	}
	if len(msgs) == 0 {
		return nil, userErrf("지정한 범위에서 답변의 근거를 찾지 못했습니다. 기간이나 검색 조건을 확인하세요")
	}
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	bodies, err := a.Store.BodyTextBatch(ctx, userIDFrom(ctx), ids)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	var contextText strings.Builder
	for _, m := range msgs {
		source := AskSource{MessageID: m.ID, Subject: m.Subject, From: m.From.Email, Date: m.Date}
		evidence := map[string]any{"message_id": m.ID, "subject": truncateRunes(m.Subject, 250), "from": m.From.Email, "date": fmtUnix(m.Date), "text": truncateRunes(bodies[m.ID], 1000)}
		if collab, err := a.GetMessageCollab(ctx, m.ID); err == nil {
			status, _ := domain.CanonicalCollabStatus(collab.Collab.Status)
			if in.IncompleteOnly && status == domain.CollabDone {
				continue
			}
			source.WorkStatus = collab.Collab.Status
			evidence["work"] = collab.Collab
			if len(collab.Notes) > 0 {
				evidence["latest_note"] = truncateRunes(collab.Notes[len(collab.Notes)-1].Body, 180)
			}
		}
		if m.HasAttachments {
			evidence["attachment_text"] = a.AttachmentsTextForIndex(ctx, m.ID, 300)
		}
		linked := []map[string]string{}
		for _, card := range cardsByMessage[m.ID] {
			if len(linked) == 2 {
				break
			}
			source.ActionIDs = append(source.ActionIDs, card.ID)
			linked = append(linked, map[string]string{"id": card.ID, "title": truncateRunes(card.Title, 120), "status": card.Status, "due": truncateRunes(card.Due, 60)})
		}
		evidence["actions"] = linked
		raw, _ := json.Marshal(evidence)
		entry := "[" + m.ID + "] " + string(raw) + "\n"
		if len([]rune(contextText.String()))+len([]rune(entry)) > maxAIBodyChars {
			break
		}
		contextText.WriteString(entry)
		allowed[m.ID] = true
		retrieval.Sources = append(retrieval.Sources, source)
	}
	if len(allowed) == 0 {
		return nil, userErrf("지정한 업무 조건과 일치하는 근거를 찾지 못했습니다")
	}
	warnings, _ := json.Marshal(retrieval.Warnings)
	instruction := "Question: " + in.Question + "\nRetrieval is a bounded sample, not the entire mailbox. Never infer an unanswered mail solely from work status. State missing evidence. Retrieval warnings: " + string(warnings)
	an, err := a.runAnalysis(ctx, "question_answer", "query", "adhoc", instruction, contextText.String())
	if err != nil {
		return nil, err
	}
	a.applyCitationVerification(ctx, an, allowed)
	return &AskResult{Analysis: an, Retrieval: retrieval}, nil
}
