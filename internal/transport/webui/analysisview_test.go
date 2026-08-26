package webui

import (
	"strings"
	"testing"
)

func fieldByLabel(fs []analysisField, label string) *analysisField {
	for i := range fs {
		if fs[i].Label == label {
			return &fs[i]
		}
	}
	return nil
}

// Analyses are JSON so the model stays checkable, but JSON is not an answer:
// the six AI tools on the message page all ended in a raw dump.
func TestAnalysisFieldsTriage(t *testing.T) {
	fs := analysisFields("triage", `{"sender_intent":"승인 요청","priority":"urgent",
	 "reply_required":true,"deadline":null,"business_risks":["기한 초과 시 계약 지연"],
	 "recommended_next_action":"오늘 중 회신","confidence":0.82}`)

	if f := fieldByLabel(fs, "보낸이의 의도"); f == nil || f.Text != "승인 요청" {
		t.Fatalf("intent not rendered: %+v", f)
	}
	// Priority is a judgement the reader should see at a glance.
	if f := fieldByLabel(fs, "우선순위"); f == nil || f.Badge != "urgent" {
		t.Fatalf("priority not badged: %+v", f)
	}
	if f := fieldByLabel(fs, "답장 필요"); f == nil || f.Text != "예" {
		t.Fatalf("boolean not rendered in Korean: %+v", f)
	}
	// A bare 0.82 says less to a reader than 82%.
	if f := fieldByLabel(fs, "신뢰도"); f == nil || f.Text != "82%" {
		t.Fatalf("confidence not shown as a percentage: %+v", f)
	}
	if f := fieldByLabel(fs, "업무 위험"); f == nil || len(f.Items) != 1 {
		t.Fatalf("list not rendered: %+v", f)
	}
	// An absent deadline is not a field with an empty value.
	if f := fieldByLabel(fs, "기한"); f != nil {
		t.Fatalf("null field should be omitted, got %+v", f)
	}
	// Schema order is reading order, not map order.
	if fs[0].Label != "보낸이의 의도" {
		t.Fatalf("fields are not in schema order: %s first", fs[0].Label)
	}
}

func TestAnalysisFieldsListsAndObjects(t *testing.T) {
	// Action items arrive as objects and must read as lines, not JSON.
	fs := analysisFields("action_items", `{"items":[
	  {"task":"계약서 검토","assignee":"김철수","due":"2026-09-01","confidence":0.9},
	  {"task":"회신"}]}`)
	f := fieldByLabel(fs, "할 일")
	if f == nil || len(f.Items) != 2 {
		t.Fatalf("action items not rendered: %+v", f)
	}
	if !strings.Contains(f.Items[0], "계약서 검토") || !strings.Contains(f.Items[0], "김철수") {
		t.Fatalf("object item flattened badly: %q", f.Items[0])
	}
	if f.Items[1] != "회신" {
		t.Fatalf("an item with no metadata should be just its task: %q", f.Items[1])
	}

	// Empty collections are omitted rather than shown as empty headings.
	fs = analysisFields("entities", `{"people":["박영희"],"companies":[],"amounts":null}`)
	if f := fieldByLabel(fs, "인물"); f == nil || len(f.Items) != 1 {
		t.Fatalf("people not rendered: %+v", f)
	}
	for _, gone := range []string{"회사", "금액"} {
		if f := fieldByLabel(fs, gone); f != nil {
			t.Errorf("empty field %q should be omitted", gone)
		}
	}

	// Risk score drives a badge so a dangerous mail is not a plain number.
	fs = analysisFields("phishing", `{"risk_score":85,"indicators":["발신 도메인 불일치"],"recommendation":"열지 마세요"}`)
	if f := fieldByLabel(fs, "위험 점수"); f == nil || f.Text != "85" || f.Badge != "urgent" {
		t.Fatalf("risk score not highlighted: %+v", f)
	}
}

// Schema drift must degrade to "shown plainly", never to a blank page.
func TestAnalysisFieldsToleratesUnknownShapes(t *testing.T) {
	fs := analysisFields("summarize", `{"summary":"요약본","unexpected_key":"새 값"}`)
	if f := fieldByLabel(fs, "summary"); f != nil {
		t.Fatal("a known key should use its label, not its raw name")
	}
	if f := fieldByLabel(fs, "요약"); f == nil || f.Text != "요약본" {
		t.Fatalf("known field missing: %+v", f)
	}
	if f := fieldByLabel(fs, "unexpected_key"); f == nil || f.Text != "새 값" {
		t.Fatalf("unknown key dropped instead of shown: %+v", f)
	}

	// An unknown analysis type still shows everything it received.
	if fs = analysisFields("brand_new_type", `{"a":"1","b":"2"}`); len(fs) != 2 {
		t.Fatalf("unknown type rendered %d fields, want 2", len(fs))
	}
	// Unparseable output yields nothing, so the page falls back to the raw JSON.
	if fs = analysisFields("triage", "not json"); fs != nil {
		t.Fatalf("invalid JSON should produce no fields, got %+v", fs)
	}
}
