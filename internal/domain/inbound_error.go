package domain

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
	"time"
)

// InboundError says where a POP3/IMAP session failed and why, in a closed
// vocabulary. It never carries the server's own text: a mail server can echo
// credentials or message data in an error, and diagnostics are persisted and
// shown. Err is kept for logs inside the process only.
type InboundError struct {
	Stage   string        // see the Stage constants
	Command string        // protocol verb (LOGIN, SELECT, FETCH, USER, RETR…) — never its arguments
	Class   string        // see ClassifyInbound
	Code    string        // server response code from a closed list (RFC 5530 / RFC 3206), else ""
	Elapsed time.Duration // how long the failing step ran
	Timeout time.Duration // the deadline that applied to that step
	Err     error
}

func (e *InboundError) Error() string {
	if e.Err == nil {
		return e.Stage
	}
	return e.Stage + ": " + e.Err.Error()
}
func (e *InboundError) Unwrap() error { return e.Err }

// Inbound session stages, in the order a sync meets them. The first three
// happen before any packet reaches the mail server.
const (
	StagePolicy     = "policy"
	StageSecret     = "secret"
	StageHostCheck  = "host_check"
	StageTCPConnect = "tcp_connect"
	StageTLS        = "tls_handshake"
	StageGreeting   = "greeting"
	StageStartTLS   = "starttls"
	StageLogin      = "login"
	StageSelect     = "select"
	StageEnumerate  = "enumerate"
	StageFetch      = "fetch"
	StageList       = "list"
	StageIdle       = "idle"
	StageLogout     = "logout"
)

// ClassifyInbound names the kind of failure from the error chain alone.
func ClassifyInbound(err error) string {
	var netErr net.Error
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var recordErr tls.RecordHeaderError
	var alert tls.AlertError
	var rejected *InboundRejected
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.As(err, &rejected):
		return "rejected"
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		return "dns_not_found"
	case errors.As(err, &dnsErr):
		return "dns_failed"
	case errors.As(err, &certErr), errors.As(err, &unknownAuthority), errors.As(err, &hostname), errors.As(err, &invalid):
		return "tls_certificate"
	case errors.As(err, &recordErr), errors.As(err, &alert):
		return "tls_handshake"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "reset"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "unreachable"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed):
		return "closed"
	}
	// Windows reports refused/reset connections with its own errno values,
	// which syscall does not map to the POSIX constants above.
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "connection refused") || strings.Contains(text, "actively refused"):
		return "refused"
	case strings.Contains(text, "connection reset") || strings.Contains(text, "forcibly closed"):
		return "reset"
	}
	return "other"
}

// InboundRejected is a server's negative reply (IMAP NO/BAD/BYE, POP3 -ERR).
// Only the response code survives, and only one from the closed list.
type InboundRejected struct {
	Code string
}

func (r *InboundRejected) Error() string {
	if r.Code == "" {
		return "server rejected the command"
	}
	return "server rejected the command [" + r.Code + "]"
}

// inboundCodes are the response codes a server may attach to a refusal
// (IMAP RFC 5530, POP3 RFC 2449/3206). They are fixed tokens, so safe to keep;
// the free text that follows them is not.
var inboundCodes = map[string]bool{
	"UNAVAILABLE": true, "AUTHENTICATIONFAILED": true, "AUTHORIZATIONFAILED": true, "EXPIRED": true,
	"PRIVACYREQUIRED": true, "CONTACTADMIN": true, "NOPERM": true, "INUSE": true, "EXPUNGEISSUED": true,
	"CORRUPTION": true, "SERVERBUG": true, "CLIENTBUG": true, "CANNOT": true, "LIMIT": true, "OVERQUOTA": true,
	"ALREADYEXISTS": true, "NONEXISTENT": true, "TRYCREATE": true, "ALERT": true, "THROTTLED": true,
	"IN-USE": true, "LOGIN-DELAY": true, "SYS/TEMP": true, "SYS/PERM": true, "AUTH": true,
}

// InboundResponseCode extracts a known response code from a server reply such
// as "NO [LIMIT] too many connections" or "-ERR [IN-USE] locked"; "" otherwise.
func InboundResponseCode(reply string) string {
	open := strings.IndexByte(reply, '[')
	if open < 0 {
		return ""
	}
	end := strings.IndexByte(reply[open:], ']')
	if end < 0 {
		return ""
	}
	fields := strings.Fields(reply[open+1 : open+end])
	if len(fields) == 0 {
		return ""
	}
	if token := strings.ToUpper(fields[0]); inboundCodes[token] {
		return token
	}
	return ""
}

// WrapInbound records where an inbound session broke. An error that already
// names its stage passes through unchanged, so the innermost step wins.
func WrapInbound(stage, command string, err error, elapsed, timeout time.Duration) error {
	var known *InboundError
	if err == nil || errors.As(err, &known) {
		return err
	}
	code := ""
	var rejected *InboundRejected
	if errors.As(err, &rejected) {
		code = rejected.Code
	}
	return &InboundError{Stage: stage, Command: command, Class: ClassifyInbound(err), Code: code, Elapsed: elapsed, Timeout: timeout, Err: err}
}

// TemporaryRefusal reports a server refusal that says nothing about the
// credentials: busy, throttled, locked, or over its connection limit. Treating
// one as a bad password would park the account in credential_error for good.
func TemporaryRefusal(code string) bool {
	switch code {
	case "UNAVAILABLE", "LIMIT", "INUSE", "SERVERBUG", "THROTTLED", "IN-USE", "LOGIN-DELAY", "SYS/TEMP":
		return true
	}
	return false
}
