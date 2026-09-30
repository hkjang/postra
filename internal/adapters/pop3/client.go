// Package pop3 implements a minimal RFC 1939 client behind the
// domain.POP3Dialer port. Supports implicit TLS (995), STLS upgrade, and —
// for offline / air-gapped networks — plaintext with optional
// authentication. Policy gating for insecure modes happens in the
// application layer, not here.
package pop3

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"postra/internal/domain"
)

// maxListBytes caps how much of one multi-line response (LIST, UIDL) readList
// may accumulate. Nothing else bounds it: readList collects lines until the
// server sends the terminating ".", and it refreshes the command deadline on
// every line it reads, so a server that keeps dribbling short lines and never
// terminates the response holds the collecting worker for ever with no deadline
// left to stop it. That is worse here than on the IMAP side, where the same
// shape was closed in v0.23.7: readList runs in the enumeration step (the sync
// loop's UIDL, then LIST), so one such maildrop stalls a whole account's sync
// before the first message is touched.
//
// The bound has to clear the biggest legitimate maildrop, which arrives in a
// single response: a UIDL line is a message number, a space and a unique-id of
// at most 70 characters (RFC 1939 §7), charged at 92 bytes per message by
// TestPOP3FullMaildropStaysUnderListBound. 32 MiB therefore leaves room for
// roughly 364,000 messages in one maildrop while still bounding a runaway
// response — 8 MiB, the IMAP figure, would refuse a maildrop of 91,000.
const maxListBytes = 32 << 20

// listLineOverhead is what every line of a multi-line response costs on top of
// the bytes textproto.ReadLine returns, and without it maxListBytes does not
// hold. A line arrives with its CRLF already stripped, so a bare "\r\n" measures
// zero: charging only len(line), a server that answers with nothing but blank
// lines never advances the total at all and readList appends "" for ever, which
// is the runaway the bound exists to stop. A retained line also costs a 16-byte
// string header in lines, so at one byte per line a 32 MiB total would sit on
// far more live heap than the bound suggests. Charging the two bytes the server
// really sent plus that header closes both. The IMAP adapter carries the same
// constant as responseLineOverhead, added for exactly this bypass.
const listLineOverhead = 2 + 16

type Dialer struct{}

func (Dialer) Dial(ctx context.Context, opts domain.POP3DialOptions) (domain.POP3Session, error) {
	addr := net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port))
	connectTO := secondsOr(opts.ConnectTimeoutSec, 15)
	d := &net.Dialer{Timeout: connectTO}

	var conn net.Conn
	var err error
	tlsCfg := &tls.Config{
		ServerName: opts.Host,
		MinVersion: tls.VersionTLS12,
		// #nosec G402 -- offline/self-hosted POP3 servers may use self-signed
		// certs; skipping verification is an explicit per-account opt-in
		// (default false), required by offline-network mail support.
		InsecureSkipVerify: opts.InsecureSkipVerify,
	}
	dialStart := time.Now()
	switch opts.Security {
	case domain.SecurityTLS:
		conn, err = (&tls.Dialer{NetDialer: d, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	case domain.SecurityStartTLS, domain.SecurityNone:
		conn, err = d.DialContext(ctx, "tcp", addr)
	default:
		return nil, fmt.Errorf("unknown security mode %q", opts.Security)
	}
	if err != nil {
		return nil, connectFailure(err, time.Since(dialStart), connectTO)
	}

	s := &session{
		conn:      conn,
		text:      textproto.NewConn(conn),
		commandTO: secondsOr(opts.CommandTimeoutSec, 60),
	}
	greetStart := time.Now()
	if _, err := s.readResponse(); err != nil {
		conn.Close()
		return nil, domain.WrapInbound(domain.StageGreeting, "", fmt.Errorf("pop3 greeting: %w", err), time.Since(greetStart), s.commandTO)
	}

	if opts.Security == domain.SecurityStartTLS {
		if _, err := s.cmd("STLS"); err != nil {
			conn.Close()
			return nil, fmt.Errorf("STLS: %w", err)
		}
		// TLS is client-speaks-first: after the "+OK" that grants the upgrade a
		// conforming server sends nothing until it has seen the ClientHello. So
		// anything already sitting in the reader is plaintext the server pushed
		// ahead of the handshake — a STARTTLS command injection (CVE-2011-0411
		// and the 2021 "NO STARTTLS" survey), whose payload the upgrade below
		// would otherwise discard along with the old reader, leaving the session
		// to continue as if nothing had happened. Refusing is the safe verdict,
		// and it cannot cost a well-behaved server anything.
		//
		// Best-effort by construction: only bytes that reached this buffer are
		// visible, so an injection still in flight in the kernel or on the wire
		// goes unseen. It narrows the window rather than closing it.
		if n := s.text.R.Buffered(); n > 0 {
			conn.Close()
			return nil, fmt.Errorf("STLS: server sent %d bytes before TLS handshake", n)
		}
		tconn := tls.Client(conn, tlsCfg)
		tlsStart := time.Now()
		if err := tconn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, &domain.InboundError{Stage: domain.StageTLS, Command: "STLS", Class: domain.ClassifyInbound(err), Elapsed: time.Since(tlsStart), Timeout: s.commandTO, Err: fmt.Errorf("STLS handshake: %w", err)}
		}
		s.conn = tconn
		s.text = textproto.NewConn(tconn)
	}

	// Authentication is optional: some maildrops on isolated networks accept
	// sessions without USER/PASS. Only the server's refusal of the
	// credentials is an auth failure; a timeout, a lost connection or a
	// refusal for load ([IN-USE], [LOGIN-DELAY], [SYS/TEMP]) says nothing
	// about the password, and calling it one parks the account in
	// credential_error for good.
	if opts.Username != "" {
		if _, err := s.cmd("USER %s", opts.Username); err != nil {
			s.Close()
			return nil, loginFailure("USER", err)
		}
		pass := ""
		if opts.Password != nil {
			pass = string(opts.Password.Reveal())
		}
		_, err := s.cmd("PASS %s", pass)
		pass = ""
		_ = pass
		if err != nil {
			s.Close()
			return nil, loginFailure("PASS", err)
		}
	}
	return s, nil
}

func loginFailure(verb string, err error) error {
	var rejected *domain.InboundRejected
	if errors.As(err, &rejected) && !domain.TemporaryRefusal(rejected.Code) {
		return &AuthError{Err: fmt.Errorf("%s rejected: %w", verb, err)}
	}
	return fmt.Errorf("%s: %w", verb, err)
}

// connectFailure names the step a failed dial stopped at. With implicit TLS
// the dialer runs TCP and the handshake as one call; a TLS error type tells
// the two apart.
func connectFailure(err error, elapsed, timeout time.Duration) error {
	stage := domain.StageTCPConnect
	switch domain.ClassifyInbound(err) {
	case "tls_certificate", "tls_handshake":
		stage = domain.StageTLS
	}
	return &domain.InboundError{Stage: stage, Class: domain.ClassifyInbound(err), Elapsed: elapsed, Timeout: timeout, Err: fmt.Errorf("pop3 connect: %w", err)}
}

// commandStage places a POP3 command in the session's life for diagnostics.
// Only the verb is kept: PASS carries the password.
func commandStage(format string) (stage, verb string) {
	fields := strings.Fields(format)
	if len(fields) > 0 {
		verb = strings.ToUpper(fields[0])
	}
	switch verb {
	case "USER", "PASS", "APOP", "AUTH":
		return domain.StageLogin, verb
	case "STLS":
		return domain.StageStartTLS, verb
	case "STAT", "LIST", "UIDL":
		return domain.StageEnumerate, verb
	case "QUIT":
		return domain.StageLogout, verb
	default:
		return domain.StageFetch, verb
	}
}

// AuthError aliases the shared domain type so the sync layer can recognize
// credential failures from any inbound adapter (POP-011).
type AuthError = domain.AuthError

type session struct {
	conn      net.Conn
	text      *textproto.Conn
	commandTO time.Duration
}

func (s *session) deadline() { s.conn.SetDeadline(time.Now().Add(s.commandTO)) }

func (s *session) readResponse() (string, error) {
	s.deadline()
	line, err := s.text.ReadLine()
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(line, "+OK") {
		// Only a known response code leaves the adapter, never the text.
		return "", fmt.Errorf("%w: server: %s", &domain.InboundRejected{Code: domain.InboundResponseCode(line)}, line)
	}
	return line, nil
}

func (s *session) cmd(format string, args ...any) (string, error) {
	start := time.Now()
	stage, verb := commandStage(format)
	s.deadline()
	if err := s.text.PrintfLine(format, args...); err != nil {
		return "", domain.WrapInbound(stage, verb, err, time.Since(start), s.commandTO)
	}
	line, err := s.readResponse()
	if err != nil {
		return "", domain.WrapInbound(stage, verb, err, time.Since(start), s.commandTO)
	}
	return line, nil
}

// readList collects a multi-line response up to its "." terminator, bounded by
// maxListBytes so an unterminated response cannot hold the session for ever.
func (s *session) readList() ([]string, error) {
	var lines []string
	var charged int64
	for {
		s.deadline()
		line, err := s.text.ReadLine()
		if err != nil {
			return nil, err
		}
		if line == "." {
			return lines, nil
		}
		charged += int64(len(line)) + listLineOverhead
		if charged > maxListBytes {
			// The rest of the response is still on the wire and its length is
			// unknown, so the offset the next command would read at is
			// unknowable: discard the session rather than let a later command
			// mistake those bytes for its own reply.
			s.conn.Close()
			return nil, fmt.Errorf("pop3 multi-line response exceeds %d bytes", int64(maxListBytes))
		}
		lines = append(lines, strings.TrimPrefix(line, "."))
	}
}

func (s *session) List(ctx context.Context) ([]domain.RemoteMessage, error) {
	if _, err := s.cmd("LIST"); err != nil {
		return nil, err
	}
	listStart := time.Now()
	lines, err := s.readList()
	if err != nil {
		return nil, domain.WrapInbound(domain.StageEnumerate, "LIST", err, time.Since(listStart), s.commandTO)
	}
	var out []domain.RemoteMessage
	for _, l := range lines {
		var n int
		var size int64
		if _, err := fmt.Sscanf(l, "%d %d", &n, &size); err == nil {
			out = append(out, domain.RemoteMessage{Number: n, Size: size})
		}
	}
	return out, nil
}

func (s *session) UIDL(ctx context.Context) ([]domain.RemoteMessage, error) {
	if _, err := s.cmd("UIDL"); err != nil {
		return nil, err // caller falls back to LIST + content hash (POP-005)
	}
	listStart := time.Now()
	lines, err := s.readList()
	if err != nil {
		return nil, domain.WrapInbound(domain.StageEnumerate, "UIDL", err, time.Since(listStart), s.commandTO)
	}
	var out []domain.RemoteMessage
	for _, l := range lines {
		parts := strings.SplitN(strings.TrimSpace(l), " ", 2)
		if len(parts) != 2 {
			continue
		}
		n, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		out = append(out, domain.RemoteMessage{Number: n, UIDL: strings.TrimSpace(parts[1])})
	}
	return out, nil
}

// retrBody reads a multi-line response body with dot-unstuffing.
func (s *session) retrBody() (io.ReadCloser, error) {
	var buf bytes.Buffer
	for {
		s.deadline()
		line, err := s.text.ReadLineBytes()
		if err != nil {
			return nil, err
		}
		if len(line) == 1 && line[0] == '.' {
			break
		}
		if len(line) > 1 && line[0] == '.' {
			line = line[1:]
		}
		buf.Write(line)
		buf.WriteString("\r\n")
	}
	return io.NopCloser(&buf), nil
}

func (s *session) Retrieve(ctx context.Context, number int) (io.ReadCloser, error) {
	if _, err := s.cmd("RETR %d", number); err != nil {
		return nil, err
	}
	body, err := s.retrBody()
	if err != nil {
		return nil, domain.WrapInbound(domain.StageFetch, "RETR", err, 0, s.commandTO)
	}
	return body, nil
}

func (s *session) Top(ctx context.Context, number, lines int) (io.ReadCloser, error) {
	if _, err := s.cmd("TOP %d %d", number, lines); err != nil {
		return nil, err
	}
	body, err := s.retrBody()
	if err != nil {
		return nil, domain.WrapInbound(domain.StageFetch, "RETR", err, 0, s.commandTO)
	}
	return body, nil
}

func (s *session) Delete(ctx context.Context, number int) error {
	_, err := s.cmd("DELE %d", number)
	return err
}

func (s *session) Quit(ctx context.Context) error {
	_, err := s.cmd("QUIT")
	s.conn.Close()
	return err
}

func (s *session) Close() error { return s.conn.Close() }

func secondsOr(v, def int) time.Duration {
	if v <= 0 {
		v = def
	}
	return time.Duration(v) * time.Second
}
