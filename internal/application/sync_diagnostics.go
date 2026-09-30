package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"postra/internal/domain"
)

// A failed sync is the most serious thing this service reports, so it must say
// exactly where it stopped: which step of the session, how long that step ran
// against which deadline, what the server answered, how many connections this
// node already held, and whether the Sent-folder pass was involved. "Response
// timed out" alone cannot be checked by an operator — it does not even say
// whether Postra ever reached the server.
//
// Everything here is Postra's own vocabulary or a number. A mail server's text
// can echo a password or message content and diagnostics are persisted and
// shown, so no server string is ever kept: only a response code from the
// closed RFC list travels (domain.InboundResponseCode).

// Job error one-liners. Each is a fixed literal so the read boundary
// (jobDiagnostic) can recognise it; the step-by-step detail lives in the
// job's diagnostic, which is re-rendered from its fields on every read.
const (
	syncStoppedTimeout     = "메일 서버가 제한 시간 안에 응답하지 않았습니다. 작업 상세에서 멈춘 단계와 소요 시간을 확인하세요."
	syncStoppedRefused     = "메일 서버가 연결을 거부했습니다. 호스트·포트와 방화벽 정책을 확인하세요."
	syncStoppedReset       = "메일 서버가 연결을 끊었습니다. 동시 접속 수 제한과 서버 로그를 확인하세요."
	syncStoppedUnreachable = "메일 서버까지 네트워크 경로가 없습니다. DNS·라우팅·방화벽을 확인하세요."
	syncStoppedDNS         = "메일 서버 주소를 확인할 수 없습니다. 호스트 이름과 내부 DNS를 확인하세요."
	syncStoppedTLS         = "TLS 연결을 완료하지 못했습니다. 인증서·신뢰 저장소와 보안 모드를 확인하세요."
	syncStoppedRejected    = "메일 서버가 명령을 거부했습니다. 작업 상세의 단계와 서버 응답 코드를 확인하세요."
	syncStoppedConfig      = "동기화를 시작하기 전 설정 확인 단계에서 중단되었습니다. 작업 상세의 단계를 확인하세요."
)

// syncFailureMessage picks the one-liner for a diagnostic. It is chosen from
// the failure class, so a connection that was refused no longer reads as a
// timeout.
func syncFailureMessage(d *domain.SyncDiagnostic) string {
	if d == nil {
		return providerSyncFailed
	}
	switch d.Class {
	case "timeout":
		return syncStoppedTimeout
	case "refused":
		return syncStoppedRefused
	case "reset", "closed":
		return syncStoppedReset
	case "unreachable":
		return syncStoppedUnreachable
	case "dns_not_found", "dns_failed":
		return syncStoppedDNS
	case "tls_certificate", "tls_handshake":
		return syncStoppedTLS
	case "rejected":
		return syncStoppedRejected
	case "config":
		return syncStoppedConfig
	}
	return providerSyncFailed
}

// syncFailure picks the message for a failed step: the class-specific one when
// the error named the step it stopped at, otherwise the caller's fallback.
func syncFailure(d *domain.SyncDiagnostic, err error, fallback string) string {
	var inbound *domain.InboundError
	if err != nil && errors.As(err, &inbound) {
		return syncFailureMessage(d)
	}
	return fallback
}

// A sync incident carries its diagnostic as JSON, not as prose: the read
// boundary then re-validates every field and renders the summary itself, so a
// row holding a provider error chain or a stack trace (what this detail used to
// be) still shows nothing, while a real diagnostic reaches the operator.
func diagnosticDetail(d *domain.SyncDiagnostic) string {
	if d == nil {
		return ""
	}
	payload, err := json.Marshal(d)
	if err != nil {
		return ""
	}
	return string(payload)
}

// syncIncidentDetail renders a stored sync incident detail. Anything that is
// not one of Postra's own diagnostics reads as nothing at all.
func syncIncidentDetail(raw string) string {
	if raw == "" || !strings.HasPrefix(strings.TrimSpace(raw), "{") || len(raw) > 8000 {
		return ""
	}
	var d domain.SyncDiagnostic
	if json.Unmarshal([]byte(raw), &d) != nil {
		return ""
	}
	safe := safeSyncDiagnostic(&d)
	if safe == nil {
		return ""
	}
	return safe.Summary
}

// ---------- closed vocabularies ----------

// classConfig marks a failure that never reached the server: policy, secrets,
// or host validation.
const classConfig = "config"

var syncStageLabels = map[string]string{
	domain.StagePolicy:     "관리자 정책 확인",
	domain.StageSecret:     "메일 비밀값 사용",
	domain.StageHostCheck:  "서버 주소 확인",
	domain.StageTCPConnect: "서버 연결(TCP)",
	domain.StageTLS:        "TLS 핸드셰이크",
	domain.StageGreeting:   "서버 환영 메시지 수신",
	domain.StageStartTLS:   "STARTTLS 전환",
	domain.StageLogin:      "로그인",
	domain.StageSelect:     "메일함 선택",
	domain.StageEnumerate:  "메일 목록 조회",
	domain.StageFetch:      "메일 본문 수신",
	domain.StageList:       "메일함 목록 조회",
	domain.StageIdle:       "실시간 대기(IDLE)",
	domain.StageLogout:     "세션 종료",
}

var syncClassLabels = map[string]string{
	"timeout":         "제한 시간 안에 응답이 없었습니다",
	"refused":         "서버가 연결을 거부했습니다",
	"reset":           "서버가 연결을 끊었습니다",
	"closed":          "응답이 끝나기 전에 연결이 닫혔습니다",
	"unreachable":     "서버까지 네트워크 경로가 없었습니다",
	"dns_not_found":   "호스트 이름을 찾지 못했습니다",
	"dns_failed":      "이름 조회에 실패했습니다",
	"tls_certificate": "서버 인증서를 신뢰할 수 없었습니다",
	"tls_handshake":   "TLS 핸드셰이크가 실패했습니다",
	"rejected":        "서버가 명령을 거부했습니다",
	"cancelled":       "작업이 취소되었습니다",
	classConfig:       "서버에 연결하기 전에 중단되었습니다",
	"other":           "알 수 없는 오류로 중단되었습니다",
}

// sentOutcomes are the ways the Sent-folder pass can end. It never fails the
// sync: received mail is synced either way.
var sentOutcomeLabels = map[string]string{
	"synced":    "보낸 메일함도 확인했습니다",
	"throttled": "보낸 메일함은 실시간 동기화에서 생략했습니다(주기적 동기화에서 확인)",
	"none":      "서버에 보낸 메일함이 없어 건너뛰었습니다",
	"failed":    "보낸 메일함 확인에 실패했습니다(받은 메일은 정상 동기화)",
}

// ---------- building a diagnostic ----------

// syncDiagnosticStage records where a sync stopped. An error that named its own
// stage (every inbound adapter does) wins over the caller's guess, so the
// innermost failing step is what the operator sees.
func syncDiagnosticStage(d *domain.SyncDiagnostic, fallbackStage string, err error) {
	if d == nil || err == nil {
		return
	}
	var inbound *domain.InboundError
	if errors.As(err, &inbound) {
		d.Stage, d.Command, d.Class, d.Code = inbound.Stage, inbound.Command, inbound.Class, inbound.Code
		d.ElapsedMS, d.TimeoutMS = durationMS(inbound.Elapsed), durationMS(inbound.Timeout)
		return
	}
	d.Stage, d.Command, d.Code = fallbackStage, "", ""
	d.Class = domain.ClassifyInbound(err)
	if isConfigStage(fallbackStage) {
		d.Class = classConfig
	}
	// A refusal keeps its response code even when the adapter wrapped it in
	// something else (a rejected password arrives as an AuthError): the code
	// is what separates a wrong password from a locked or throttled mailbox.
	var rejected *domain.InboundRejected
	if errors.As(err, &rejected) {
		d.Code = rejected.Code
	}
}

func isConfigStage(stage string) bool {
	switch stage {
	case domain.StagePolicy, domain.StageSecret, domain.StageHostCheck:
		return true
	}
	return false
}

func durationMS(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return d.Milliseconds()
}

// ---------- rendering ----------

// syncDiagnosticSummary writes the explanation shown to people, from the
// diagnostic's own fields only. It is recomputed on every read, so a row
// written by another version can never smuggle text through it.
func syncDiagnosticSummary(d *domain.SyncDiagnostic) string {
	if d == nil {
		return ""
	}
	parts := []string{}
	if target := syncTarget(d); target != "" {
		parts = append(parts, target)
	}
	if step := syncStep(d); step != "" {
		parts = append(parts, step)
	}
	if timing := syncTiming(d); timing != "" {
		parts = append(parts, timing)
	}
	if d.Attempts > 1 {
		attempts := fmt.Sprintf("연결 시도 %d회", d.Attempts)
		if class := syncClassLabels[d.Recovered]; class != "" {
			attempts += "(이전 시도: " + class + " — 재연결 후 진행)"
		}
		parts = append(parts, attempts)
	}
	if d.SlotWaitMS >= 1000 {
		parts = append(parts, fmt.Sprintf("동시 실행 제한으로 %s 대기 후 시작", seconds(d.SlotWaitMS)))
	}
	if sessions := syncSessions(d); sessions != "" {
		parts = append(parts, sessions)
	}
	if hint := syncHint(d); hint != "" {
		parts = append(parts, hint)
	}
	if sent := sentOutcomeLabels[d.Sent]; sent != "" {
		if d.Sent == "failed" {
			sent += syncSentDetail(d)
		}
		parts = append(parts, sent)
	}
	return strings.Join(parts, " · ")
}

func syncTarget(d *domain.SyncDiagnostic) string {
	if d.Host == "" {
		return ""
	}
	target := strings.ToUpper(d.Protocol) + " " + d.Host
	if d.Port > 0 {
		target += fmt.Sprintf(":%d", d.Port)
	}
	if d.Security != "" {
		target += " (" + d.Security + ")"
	}
	return strings.TrimSpace(target)
}

func syncStep(d *domain.SyncDiagnostic) string {
	stage := syncStageLabels[d.Stage]
	class := syncClassLabels[d.Class]
	switch {
	case stage == "" && class == "":
		return ""
	case stage == "":
		return class
	case class == "":
		return stage + " 단계에서 중단되었습니다"
	}
	step := stage + " 단계: " + class
	if d.Command != "" {
		step = stage + " 단계(" + d.Command + "): " + class
	}
	if d.Code != "" {
		step += " [" + d.Code + "]"
	}
	return step
}

// syncTiming distinguishes a step that used up its own deadline from one that
// stopped early — the difference between a slow server and a connection that
// was cut, which the single word "timeout" hides.
func syncTiming(d *domain.SyncDiagnostic) string {
	switch {
	case d.ElapsedMS == 0 && d.TimeoutMS == 0:
		return ""
	case d.TimeoutMS == 0:
		return seconds(d.ElapsedMS) + " 후 중단"
	case d.ElapsedMS == 0:
		return "제한 " + seconds(d.TimeoutMS)
	case d.Class == "timeout" && d.ElapsedMS*10 < d.TimeoutMS*9:
		// The step's own deadline had not arrived: the budget came from
		// outside (a cancelled job, a shorter connect timeout).
		return fmt.Sprintf("제한 %s 중 %s에 중단(이 단계의 제한 시간에는 이르지 않았습니다)", seconds(d.TimeoutMS), seconds(d.ElapsedMS))
	default:
		return fmt.Sprintf("제한 %s 중 %s 경과", seconds(d.TimeoutMS), seconds(d.ElapsedMS))
	}
}

func syncSessions(d *domain.SyncDiagnostic) string {
	if d.HostSessions <= 0 {
		return ""
	}
	out := fmt.Sprintf("이 서버에 Postra가 유지 중인 연결 %d개", d.HostSessions)
	if d.AccountSessions > 0 {
		out += fmt.Sprintf("(이 계정 %d개)", d.AccountSessions)
	}
	return out
}

// syncHint adds the one thing an operator should check next, when the class and
// the connection census together point at it.
func syncHint(d *domain.SyncDiagnostic) string {
	connecting := d.Stage == domain.StageTCPConnect || d.Stage == domain.StageGreeting || d.Stage == domain.StageLogin
	switch {
	case d.Class == "rejected" && domain.TemporaryRefusal(d.Code):
		return "서버가 일시적인 사유로 거부했습니다. 비밀번호 문제가 아니며 계정 상태는 바꾸지 않았습니다"
	case connecting && d.AccountSessions > 1:
		return "이 계정의 동시 접속 허용 수를 확인하세요. 실시간 대기(IDLE) 연결과 동기화 연결을 함께 사용합니다"
	case d.Stage == domain.StageGreeting && (d.Class == "timeout" || d.Class == "closed" || d.Class == "reset"):
		return "TCP 연결은 되었으나 서버가 세션을 시작하지 않았습니다. 동시 접속 제한·차단 목록·프록시를 확인하세요"
	case d.Stage == domain.StageTLS && d.Class == "timeout":
		return "보안 모드(TLS/STARTTLS)와 포트 조합을 확인하세요"
	case d.Stage == domain.StageFetch && d.Class == "timeout":
		return "큰 첨부를 받는 중 멈췄을 수 있습니다. 메일 최대 크기와 명령 제한 시간을 확인하세요"
	}
	return ""
}

func syncSentDetail(d *domain.SyncDiagnostic) string {
	step := syncStep(&domain.SyncDiagnostic{Stage: d.SentStage, Class: d.SentClass})
	if step == "" {
		return ""
	}
	return " — " + step
}

func seconds(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1f초", float64(ms)/1000)
}

// ---------- read boundary ----------

// safeSyncDiagnostic keeps only known tokens and numbers, then rewrites the
// summary from them. Rows written by an older build, another node, or a
// future field pass through this same gate, so nothing a mail server wrote can
// reach a screen even if it somehow reached the column.
func safeSyncDiagnostic(d *domain.SyncDiagnostic) *domain.SyncDiagnostic {
	if d == nil {
		return nil
	}
	out := &domain.SyncDiagnostic{
		Stage: allowed(d.Stage, syncStageLabels), Class: allowed(d.Class, syncClassLabels),
		Command: allowedCommand(d.Command), Code: allowedCode(d.Code),
		ElapsedMS: positive(d.ElapsedMS), TimeoutMS: positive(d.TimeoutMS), Attempts: int(positive(int64(d.Attempts))),
		Recovered: allowed(d.Recovered, syncClassLabels),
		Protocol:  allowedToken(d.Protocol, "pop3", "imap"),
		Host:      allowedHost(d.Host), Port: int(positive(int64(d.Port))),
		Security:     allowedToken(d.Security, string(domain.SecurityTLS), string(domain.SecurityStartTLS), string(domain.SecurityNone)),
		HostSessions: positive(d.HostSessions), AccountSessions: positive(d.AccountSessions),
		SlotWaitMS: positive(d.SlotWaitMS),
		Sent:       allowed(d.Sent, sentOutcomeLabels),
		SentStage:  allowed(d.SentStage, syncStageLabels), SentClass: allowed(d.SentClass, syncClassLabels),
	}
	// Numbers alone say nothing; without a known step, outcome or target there
	// is no diagnostic to show.
	if out.Stage == "" && out.Class == "" && out.Sent == "" && out.Recovered == "" && out.Host == "" {
		return nil
	}
	out.Summary = syncDiagnosticSummary(out)
	if out.Summary == "" {
		return nil
	}
	return out
}

func allowed[T any](value string, vocabulary map[string]T) string {
	if _, ok := vocabulary[value]; ok {
		return value
	}
	return ""
}

func allowedToken(value string, tokens ...string) string {
	for _, token := range tokens {
		if value == token {
			return value
		}
	}
	return ""
}

// allowedCommand keeps a protocol verb: upper-case letters and one space, at
// most 20 characters ("UID FETCH"). A server never chooses this value, but the
// boundary does not rely on that.
func allowedCommand(value string) string {
	if value == "" || len(value) > 20 {
		return ""
	}
	for _, r := range value {
		if !(r >= 'A' && r <= 'Z' || r == ' ') {
			return ""
		}
	}
	return value
}

func allowedCode(value string) string {
	if domain.InboundResponseCode("["+value+"]") == value {
		return value
	}
	return ""
}

// allowedHost keeps the operator-configured mail host, which already appears
// in the account settings. Anything that is not a plain host name or address is
// dropped rather than shown.
func allowedHost(value string) string {
	if value == "" || len(value) > 253 {
		return ""
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == ':' || r == '_') {
			return ""
		}
	}
	return value
}

func positive[T int | int64](v T) T {
	if v < 0 {
		return 0
	}
	return v
}

// ---------- inbound session census ----------

// inboundCensus counts the inbound connections this node holds, per host and
// per account. A server that limits concurrent connections answers the next
// one with a timeout, a silent close, or a bare "BYE" — indistinguishable from
// an outage unless the diagnostic can say Postra was already holding
// connections of its own.
type inboundCensus struct {
	hosts    sync.Map // host -> *atomic.Int64
	accounts sync.Map // accountID -> *atomic.Int64
}

func censusCount(m *sync.Map, key string) *atomic.Int64 {
	if v, ok := m.Load(key); ok {
		return v.(*atomic.Int64)
	}
	v, _ := m.LoadOrStore(key, &atomic.Int64{})
	return v.(*atomic.Int64)
}

// hold records one inbound session for the account and returns its release.
func (c *inboundCensus) hold(acc *domain.MailAccount) func() {
	host, accounts := censusCount(&c.hosts, acc.POP3Host), censusCount(&c.accounts, acc.ID)
	host.Add(1)
	accounts.Add(1)
	return func() {
		host.Add(-1)
		accounts.Add(-1)
	}
}

// count reports the sessions held right now, this one included.
func (c *inboundCensus) count(acc *domain.MailAccount) (hosts, accounts int64) {
	return censusCount(&c.hosts, acc.POP3Host).Load(), censusCount(&c.accounts, acc.ID).Load()
}

// ---------- connecting ----------

// syncDialAttempts is how many times one sync opens a session before giving
// up. A mail server that is briefly busy, throttling, or closing sessions at a
// connection limit is the common cause of repeated sync failures, and a single
// attempt turns it into a failed sync every time.
const syncDialAttempts = 3

// syncDialBackoff waits between attempts. It is short: the sync already holds
// its concurrency slot, and the scheduler will come round again.
var syncDialBackoff = []time.Duration{2 * time.Second, 5 * time.Second}

// retryableInbound reports a failure that another attempt may get past. A
// rejected password, a policy refusal, a missing host and an untrusted
// certificate are not retried: they fail the same way every time.
func retryableInbound(err error) bool {
	var auth *domain.AuthError
	if errors.As(err, &auth) {
		return false
	}
	var rejected *domain.InboundRejected
	if errors.As(err, &rejected) {
		return domain.TemporaryRefusal(rejected.Code)
	}
	// The adapter's own classification wins: it saw the failure, while
	// re-classifying a wrapped error only reaches the outermost message.
	class, code := domain.ClassifyInbound(err), ""
	var inbound *domain.InboundError
	if errors.As(err, &inbound) {
		code = inbound.Code
		if inbound.Class != "" {
			class = inbound.Class
		}
	}
	if class == "rejected" {
		// Busy, throttled, locked or over a connection limit: the next attempt
		// may well get in. Any other refusal fails the same way every time.
		return domain.TemporaryRefusal(code)
	}
	switch class {
	case "timeout", "refused", "reset", "closed", "other":
		return true
	}
	return false
}

// dialInboundForSync opens the session a sync runs on, retrying a failure that
// may be transient, and records every attempt in the diagnostic — including on
// success, so a sync that only worked on its second try is visible before the
// day it stops working at all.
func (a *App) dialInboundForSync(ctx context.Context, acc *domain.MailAccount, d *domain.SyncDiagnostic) (domain.InboundSession, error) {
	var err error
	for attempt := 1; attempt <= syncDialAttempts; attempt++ {
		var sess domain.InboundSession
		d.Attempts = attempt
		sess, err = a.dialInbound(ctx, acc, domain.PurposePOP3Auth)
		if err == nil {
			if attempt > 1 {
				// The session is open, so the earlier failure is history, not
				// the outcome: it moves to Recovered, where the summary reads
				// "connected on attempt 2; the first was cut by the server".
				d.Recovered = d.Class
				d.Stage, d.Command, d.Class, d.Code = "", "", "", ""
				d.ElapsedMS, d.TimeoutMS = 0, 0
			}
			return sess, nil
		}
		syncDiagnosticStage(d, domain.StageTCPConnect, err)
		d.HostSessions, d.AccountSessions = a.inbound.count(acc)
		if attempt == syncDialAttempts || !retryableInbound(err) || ctx.Err() != nil {
			return nil, err
		}
		wait := syncDialBackoff[min(attempt, len(syncDialBackoff))-1]
		slog.Debug("sync: retrying the inbound connection", "account", acc.ID, "attempt", attempt, "class", d.Class, "stage", d.Stage, "wait", wait)
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(wait):
		}
	}
	return nil, err
}
