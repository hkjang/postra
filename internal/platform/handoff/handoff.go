// Package handoff holds the settings and wire shapes for handing a mail
// message to another in-house service without a person carrying the file.
//
// A message read in Postra becomes a document in muni, slides in ptium, a
// report in weekly — and until now every step was a download and an upload.
// The standard (aidev HANDOFF-STANDARD.md) makes the hand: the sending
// service issues a claim — a random token bound to one document and one
// person's right to read it, good for five minutes and one collection — and
// the receiving service is opened in the browser with it. Postra is a sending
// side only, and the one format it sends is markdown.
//
// Where a message may be sent is an administrator's allow list, stored with
// the other system settings. It is empty on a fresh installation, and while it
// is empty the message screen shows no "send to" button at all.
package handoff

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	// FormatMarkdown is the one format this service sends.
	FormatMarkdown = "markdown"

	// ContentType is what a collected markdown claim is served as.
	ContentType = "text/markdown; charset=utf-8"

	// ClaimTTL is how long a claim may be collected. The standard caps it at
	// five minutes.
	ClaimTTL = 5 * time.Minute

	// MaxTargets bounds the allow list; a menu longer than this is a mistake.
	MaxTargets = 20

	// claimBytes is the entropy in a claim: the standard asks for at least 128
	// bits, this is 256.
	claimBytes = 32
)

// Setting keys as stored in system settings.
const (
	// SettingTargets is a JSON array of Target — the allow list.
	SettingTargets = "handoff.targets"
	// SettingSourceOrigin is this service's public origin as the receiving
	// side will see it (scheme, host, port). Empty means "use the address the
	// request came in on".
	SettingSourceOrigin = "handoff.source_origin"
)

// SettingKeys lists every handoff setting so callers can register and default
// them in one place.
var SettingKeys = []string{SettingTargets, SettingSourceOrigin}

// Defaults are the values a fresh installation carries: no targets, no button.
var Defaults = map[string]string{SettingTargets: "", SettingSourceOrigin: ""}

// KnownFormats are the formats named in the standard's format table.
var KnownFormats = []string{"markdown", "docx", "csv", "xlsx", "txt", "pptx"}

// Target is one receiving service on the allow list.
type Target struct {
	Name    string   `json:"name"`
	Origin  string   `json:"origin"`
	Formats []string `json:"formats"`
}

// Accepts reports whether the target's receiving side reads format.
func (t Target) Accepts(format string) bool {
	for _, f := range t.Formats {
		if strings.EqualFold(f, format) {
			return true
		}
	}
	return false
}

// ParseTargets reads the stored allow list. An empty value is an empty list;
// anything else must be a JSON array whose entries have a display name, a
// bare origin, and known formats, with no origin listed twice.
func ParseTargets(raw string) ([]Target, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var targets []Target
	if err := json.Unmarshal([]byte(raw), &targets); err != nil {
		return nil, fmt.Errorf("JSON 배열이어야 합니다: %v", err)
	}
	if len(targets) > MaxTargets {
		return nil, fmt.Errorf("보낼 곳은 %d개까지입니다", MaxTargets)
	}
	seen := map[string]bool{}
	for i := range targets {
		t := &targets[i]
		t.Name = strings.TrimSpace(t.Name)
		origin, err := NormalizeOrigin(t.Origin)
		if err != nil {
			return nil, fmt.Errorf("%d번째 항목: %v", i+1, err)
		}
		t.Origin = origin
		if t.Name == "" {
			return nil, fmt.Errorf("%d번째 항목: 이름이 비어 있습니다", i+1)
		}
		if seen[origin] {
			return nil, fmt.Errorf("%d번째 항목: 같은 주소가 두 번 적혀 있습니다 (%s)", i+1, origin)
		}
		seen[origin] = true
		formats := make([]string, 0, len(t.Formats))
		for _, f := range t.Formats {
			f = strings.ToLower(strings.TrimSpace(f))
			if f == "" {
				continue
			}
			if !knownFormat(f) {
				return nil, fmt.Errorf("%d번째 항목: 모르는 형식 %q (가능: %s)", i+1, f, strings.Join(KnownFormats, ", "))
			}
			formats = append(formats, f)
		}
		if len(formats) == 0 {
			return nil, fmt.Errorf("%d번째 항목: 받는 형식이 비어 있습니다", i+1)
		}
		t.Formats = formats
	}
	return targets, nil
}

// Accepting filters the list to the services whose receiving side reads
// format — the ones a "send to" button may be made for.
func Accepting(targets []Target, format string) []Target {
	var out []Target
	for _, t := range targets {
		if t.Accepts(format) {
			out = append(out, t)
		}
	}
	return out
}

func knownFormat(f string) bool {
	for _, k := range KnownFormats {
		if k == f {
			return true
		}
	}
	return false
}

// NormalizeOrigin accepts a scheme and host (with optional port) and nothing
// else — no path, query, fragment or credentials — and returns it lower-cased
// without a trailing slash, the form the receiving side compares against.
func NormalizeOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("주소가 비어 있습니다")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("주소를 읽을 수 없습니다: %s", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("주소는 http(s)://호스트[:포트] 모양이어야 합니다: %s", raw)
	}
	if u.Host == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") || strings.ContainsAny(u.Host, " \t\r\n\"'<>") {
		return "", fmt.Errorf("주소는 스킴과 호스트(포트)까지만 적습니다: %s", raw)
	}
	return scheme + "://" + strings.ToLower(u.Host), nil
}

// NewClaim draws a fresh claim token and the digest under which it is stored.
// Only the digest ever touches the database: a read of the table yields
// nothing anybody could present.
func NewClaim() (token, digest string, err error) {
	b := make([]byte, claimBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, Digest(token), nil
}

// Digest is the stored form of a claim token.
func Digest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ValidClaimToken bounds what is looked up: a base64url string of the size
// NewClaim produces. Anything else is not a claim and is not worth a query.
func ValidClaimToken(token string) bool {
	if len(token) != base64.RawURLEncoding.EncodedLen(claimBytes) {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil
}

// URL is the address the receiving service is opened at — exactly the
// standard's shape.
func URL(target Target, source, claim string) string {
	q := url.Values{"source": {source}, "claim": {claim}}
	return target.Origin + "/handoff?" + q.Encode()
}

// ContentDisposition names the served file for a receiver that cares, in the
// RFC 6266 form that survives non-ASCII names.
func ContentDisposition(filename string) string {
	return "attachment; filename*=UTF-8''" + url.PathEscape(filename)
}

// Filename turns a message subject into a file name: the subject with the
// characters no file system accepts replaced, or a fallback when there is
// none, and the markdown extension.
func Filename(subject string) string {
	name := strings.TrimSpace(subject)
	if name == "" {
		name = "메일"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20, r == 0x7f:
			continue
		case strings.ContainsRune(`/\:*?"<>|`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	name = strings.TrimSpace(b.String())
	if name == "" || name == "." || name == ".." {
		name = "메일"
	}
	if len(name) > 120 {
		// Cut on a rune boundary so a multibyte subject stays valid UTF-8.
		cut := 120
		for cut > 0 && !isRuneStart(name[cut]) {
			cut--
		}
		name = name[:cut]
	}
	return name + ".md"
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
