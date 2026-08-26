package webui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// analysisField is one labelled piece of an AI result, prepared for display.
// Scalars use Text, collections use Items, and Badge marks a value that should
// stand out (a priority, a risk level) rather than read as ordinary prose.
type analysisField struct {
	Label string
	Text  string
	Items []string
	Badge string // "", "urgent", "high", "normal", "low"
}

// analysisSchema names the fields of each analysis type in reading order, with
// the labels a person expects. Results are JSON because the model is easier to
// keep honest that way — but JSON is not an answer, so the UI renders it.
var analysisSchema = map[string][]struct{ Key, Label string }{
	"triage": {
		{"sender_intent", "보낸이의 의도"},
		{"priority", "우선순위"},
		{"reply_required", "답장 필요"},
		{"deadline", "기한"},
		{"recommended_next_action", "권장 조치"},
		{"business_risks", "업무 위험"},
		{"confidence", "신뢰도"},
	},
	"summarize": {
		{"summary", "요약"},
		{"requests", "요청 사항"},
		{"dates", "언급된 일정"},
		{"confidence", "신뢰도"},
	},
	"classify": {
		{"category", "분류"},
		{"importance", "중요도"},
		{"reason", "판단 근거"},
		{"confidence", "신뢰도"},
	},
	"action_items": {
		{"items", "할 일"},
	},
	"entities": {
		{"people", "인물"},
		{"companies", "회사"},
		{"projects", "프로젝트"},
		{"amounts", "금액"},
		{"contacts", "연락처"},
	},
	"phishing": {
		{"risk_score", "위험 점수"},
		{"indicators", "의심 정황"},
		{"recommendation", "권고"},
	},
}

// analysisFields turns a raw result into display fields. Unknown analysis
// types and unexpected keys still render — a schema drift should degrade to
// "shown plainly", never to a blank page.
func analysisFields(kind, resultJSON string) []analysisField {
	var obj map[string]any
	if err := json.Unmarshal([]byte(resultJSON), &obj); err != nil || len(obj) == 0 {
		return nil
	}
	var out []analysisField
	seen := map[string]bool{}
	for _, f := range analysisSchema[kind] {
		if v, ok := obj[f.Key]; ok {
			seen[f.Key] = true
			if fld, ok := buildField(f.Label, f.Key, v); ok {
				out = append(out, fld)
			}
		}
	}
	// Anything the schema did not name is still the model's answer.
	var extra []string
	for k := range obj {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		if fld, ok := buildField(k, k, obj[k]); ok {
			out = append(out, fld)
		}
	}
	return out
}

// buildField renders one value, reporting false when there is nothing to show
// (an absent deadline, an empty risk list) so the view stays free of blanks.
func buildField(label, key string, v any) (analysisField, bool) {
	f := analysisField{Label: label}
	switch val := v.(type) {
	case nil:
		return f, false
	case bool:
		f.Text = "아니오"
		if val {
			f.Text = "예"
		}
	case string:
		if strings.TrimSpace(val) == "" {
			return f, false
		}
		f.Text = val
		if key == "priority" || key == "importance" {
			f.Badge = strings.ToLower(strings.TrimSpace(val))
		}
	case float64:
		f.Text = formatNumber(key, val)
		if key == "risk_score" {
			f.Badge = riskBadge(val)
		}
	case []any:
		for _, item := range val {
			if line := itemLine(item); line != "" {
				f.Items = append(f.Items, line)
			}
		}
		if len(f.Items) == 0 {
			return f, false
		}
	default:
		b, err := json.Marshal(val)
		if err != nil {
			return f, false
		}
		f.Text = string(b)
	}
	return f, true
}

// formatNumber shows a confidence as a percentage and leaves other numbers as
// written; a bare 0.82 tells a reader less than 82%.
func formatNumber(key string, v float64) string {
	if key == "confidence" && v >= 0 && v <= 1 {
		return strconv.Itoa(int(v*100+0.5)) + "%"
	}
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func riskBadge(score float64) string {
	switch {
	case score >= 70:
		return "urgent"
	case score >= 40:
		return "high"
	default:
		return "normal"
	}
}

// itemLine renders a list entry, flattening the object form used by action
// items (task / assignee / due) into one readable line.
func itemLine(item any) string {
	switch v := item.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return formatNumber("", v)
	case map[string]any:
		var head string
		for _, k := range []string{"task", "title", "name", "summary"} {
			if s, ok := v[k].(string); ok && strings.TrimSpace(s) != "" {
				head = strings.TrimSpace(s)
				break
			}
		}
		var meta []string
		for _, k := range []string{"assignee", "due", "evidence"} {
			if s, ok := v[k].(string); ok && strings.TrimSpace(s) != "" {
				meta = append(meta, strings.TrimSpace(s))
			}
		}
		if head == "" && len(meta) == 0 {
			b, _ := json.Marshal(v)
			return string(b)
		}
		if len(meta) == 0 {
			return head
		}
		return fmt.Sprintf("%s — %s", head, strings.Join(meta, " · "))
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}
