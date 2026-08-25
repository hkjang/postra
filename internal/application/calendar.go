package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CalendarEvent is one schedulable item found in a message.
type CalendarEvent struct {
	Title       string  `json:"title"`
	Start       string  `json:"start"`
	End         string  `json:"end,omitempty"`
	AllDay      bool    `json:"all_day"`
	Location    string  `json:"location,omitempty"`
	Description string  `json:"description,omitempty"`
	Confidence  float64 `json:"confidence,omitempty"`
}

// CalendarEvents is the extraction result for one message.
type CalendarEvents struct {
	MessageID string          `json:"message_id"`
	Events    []CalendarEvent `json:"events"`
}

// ExtractCalendarEvents finds meetings and deadlines in a message so they can
// be added to a calendar instead of retyped by hand. Extraction is grounded in
// the mail (the prompt forbids inventing times) and the result is offered as
// standard iCalendar, which every calendar app imports.
func (a *App) ExtractCalendarEvents(ctx context.Context, messageID string) (*CalendarEvents, error) {
	_, input, err := a.messageAsAIInput(ctx, messageID, false)
	if err != nil {
		return nil, err
	}
	an, err := a.runAnalysis(ctx, "calendar_events", "message", messageID, "", input)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Events []CalendarEvent `json:"events"`
	}
	if err := json.Unmarshal([]byte(an.ResultJSON), &parsed); err != nil {
		return nil, userErrf("AI가 일정 형식을 반환하지 못했습니다: %v", err)
	}
	out := &CalendarEvents{MessageID: messageID}
	for _, ev := range parsed.Events {
		ev.Title = strings.TrimSpace(ev.Title)
		ev.Start = strings.TrimSpace(ev.Start)
		// An event without a title or a parseable start is not schedulable;
		// dropping it beats emitting an ICS a calendar will reject.
		if ev.Title == "" || ev.Start == "" {
			continue
		}
		if _, _, err := parseEventTime(ev.Start); err != nil {
			continue
		}
		out.Events = append(out.Events, ev)
	}
	return out, nil
}

// ICS renders the extracted events as an iCalendar document (RFC 5545).
func (c *CalendarEvents) ICS() string {
	var sb strings.Builder
	sb.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Postra//Mail AI//EN\r\nCALSCALE:GREGORIAN\r\nMETHOD:PUBLISH\r\n")
	stamp := time.Now().UTC().Format("20060102T150405Z")
	for i, ev := range c.Events {
		start, allDay, err := parseEventTime(ev.Start)
		if err != nil {
			continue
		}
		if ev.AllDay {
			allDay = true
		}
		end, _, endErr := parseEventTime(ev.End)
		if endErr != nil || !end.After(start) {
			// Default duration keeps the event valid when the mail gives no end.
			if allDay {
				end = start.AddDate(0, 0, 1)
			} else {
				end = start.Add(time.Hour)
			}
		}
		sb.WriteString("BEGIN:VEVENT\r\n")
		fmt.Fprintf(&sb, "UID:%s@postra\r\n", eventUID(c.MessageID, ev, i))
		fmt.Fprintf(&sb, "DTSTAMP:%s\r\n", stamp)
		if allDay {
			fmt.Fprintf(&sb, "DTSTART;VALUE=DATE:%s\r\n", start.Format("20060102"))
			fmt.Fprintf(&sb, "DTEND;VALUE=DATE:%s\r\n", end.Format("20060102"))
		} else {
			fmt.Fprintf(&sb, "DTSTART:%s\r\n", start.UTC().Format("20060102T150405Z"))
			fmt.Fprintf(&sb, "DTEND:%s\r\n", end.UTC().Format("20060102T150405Z"))
		}
		sb.WriteString(icsLine("SUMMARY", ev.Title))
		if ev.Location != "" {
			sb.WriteString(icsLine("LOCATION", ev.Location))
		}
		if ev.Description != "" {
			sb.WriteString(icsLine("DESCRIPTION", ev.Description))
		}
		sb.WriteString("END:VEVENT\r\n")
	}
	sb.WriteString("END:VCALENDAR\r\n")
	return sb.String()
}

// parseEventTime accepts the two shapes the prompt allows: a full RFC3339
// timestamp, or a bare date (which means an all-day event).
func parseEventTime(v string) (t time.Time, allDay bool, err error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false, fmt.Errorf("empty time")
	}
	if t, err = time.Parse(time.RFC3339, v); err == nil {
		return t, false, nil
	}
	if t, err = time.Parse("2006-01-02", v); err == nil {
		return t, true, nil
	}
	// Some models emit a local timestamp without an offset.
	if t, err = time.Parse("2006-01-02T15:04:05", v); err == nil {
		return t, false, nil
	}
	return time.Time{}, false, fmt.Errorf("unrecognized time %q", v)
}

// icsLine escapes a value per RFC 5545 and folds it to 75 octets per line.
// Continuation lines begin with a space that itself counts toward the limit,
// so everything after the first fold is cut one octet shorter.
func icsLine(name, value string) string {
	r := strings.NewReplacer("\\", "\\\\", ";", "\\;", ",", "\\,", "\r\n", "\\n", "\n", "\\n", "\r", "\\n")
	line := name + ":" + r.Replace(value)
	var sb strings.Builder
	limit := 75
	for len(line) > limit {
		cut := limit
		// Never split a multi-byte rune across the fold.
		for cut > 0 && !utf8ValidBoundary(line, cut) {
			cut--
		}
		if cut == 0 { // a single rune wider than the limit; emit it whole
			cut = limit
		}
		sb.WriteString(line[:cut] + "\r\n ")
		line = line[cut:]
		limit = 74
	}
	sb.WriteString(line + "\r\n")
	return sb.String()
}

// eventUID derives a stable identifier for an extracted event. A random UID
// would make every download a new event, so re-importing the same message
// would duplicate entries in the calendar instead of updating them.
func eventUID(messageID string, ev CalendarEvent, index int) string {
	sum := sha256.Sum256([]byte(messageID + "|" + ev.Title + "|" + ev.Start + "|" + strconv.Itoa(index)))
	return hex.EncodeToString(sum[:16])
}

func utf8ValidBoundary(s string, i int) bool {
	if i <= 0 || i >= len(s) {
		return true
	}
	return s[i]&0xC0 != 0x80
}

var _ = context.Background
