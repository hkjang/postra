// Package imap implements a minimal RFC 3501 client behind the
// domain.InboundDialer port (the same port POP3 uses). It covers exactly what
// ingest needs — connect, LOGIN/PREAUTH, SELECT INBOX, enumerate (UID +
// RFC822.SIZE), fetch a message body, and mark-delete + EXPUNGE — and supports
// implicit TLS (993), STARTTLS, and plaintext for offline networks. Policy
// gating for insecure modes stays in the application layer.
package imap

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"postra/internal/domain"
)

type Dialer struct{}

var (
	reFetch = regexp.MustCompile(`^\* (\d+) FETCH `)
	reUID   = regexp.MustCompile(`UID (\d+)`)
	reSize  = regexp.MustCompile(`RFC822\.SIZE (\d+)`)
	reExist = regexp.MustCompile(`^\* (\d+) EXISTS`)
	reValid = regexp.MustCompile(`UIDVALIDITY (\d+)`)
	reLit   = regexp.MustCompile(`\{(\d+)\}$`)
)

func (Dialer) Dial(ctx context.Context, opts domain.InboundDialOptions) (domain.InboundSession, error) {
	addr := net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port))
	d := &net.Dialer{Timeout: secondsOr(opts.ConnectTimeoutSec, 15)}
	tlsCfg := &tls.Config{
		ServerName: opts.Host,
		MinVersion: tls.VersionTLS12,
		// #nosec G402 -- offline/self-hosted IMAP servers may use self-signed
		// certs; skipping verification is an explicit per-account opt-in
		// (default false), required by offline-network mail support.
		InsecureSkipVerify: opts.InsecureSkipVerify,
	}

	var conn net.Conn
	var err error
	switch opts.Security {
	case domain.SecurityTLS:
		conn, err = (&tls.Dialer{NetDialer: d, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	case domain.SecurityStartTLS, domain.SecurityNone:
		conn, err = d.DialContext(ctx, "tcp", addr)
	default:
		return nil, fmt.Errorf("unknown security mode %q", opts.Security)
	}
	if err != nil {
		return nil, fmt.Errorf("imap connect %s: %w", addr, err)
	}

	s := &session{conn: conn, r: bufio.NewReader(conn), commandTO: secondsOr(opts.CommandTimeoutSec, 60), maxLiteral: opts.MaxMessageBytes}
	greeting, err := s.readLine()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("imap greeting: %w", err)
	}
	preAuth := strings.HasPrefix(greeting, "* PREAUTH")

	if opts.Security == domain.SecurityStartTLS {
		if _, err := s.exec("STARTTLS"); err != nil {
			conn.Close()
			return nil, fmt.Errorf("STARTTLS: %w", err)
		}
		tconn := tls.Client(conn, tlsCfg)
		if err := tconn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("STARTTLS handshake: %w", err)
		}
		s.conn = tconn
		s.r = bufio.NewReader(tconn)
	}

	// IMAP requires authentication before SELECT unless the server greeted
	// with PREAUTH. On offline maildrops "none" refers to transport, not auth.
	if !preAuth && opts.Username != "" {
		pass := ""
		if opts.Password != nil {
			pass = string(opts.Password.Reveal())
		}
		_, err := s.exec("LOGIN %s %s", quote(opts.Username), quote(pass))
		pass = ""
		_ = pass
		if err != nil {
			s.Close()
			return nil, &domain.AuthError{Err: fmt.Errorf("LOGIN rejected: %w", err)}
		}
	}

	if err := s.selectInbox(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

type session struct {
	conn        net.Conn
	r           *bufio.Reader
	commandTO   time.Duration
	maxLiteral  int64 // per-literal buffer cap (0 = unlimited); OOM guard
	tagN        int
	uidValidity string
	exists      int
	index       []domain.RemoteMessage
	indexed     bool
	deleted     bool
	literals    []string // literal payloads read during the last exec, in order
}

func (s *session) deadline() { s.conn.SetDeadline(time.Now().Add(s.commandTO)) }

// readLine reads one CRLF-terminated protocol line (without the CRLF).
func (s *session) readLine() (string, error) {
	s.deadline()
	line, err := s.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// exec sends a tagged command and collects untagged responses until the
// matching tagged completion. Server literals ({n}) are read inline so the
// stream stays framed; the literal bytes are appended to the current line.
func (s *session) exec(format string, args ...any) ([]string, error) {
	s.tagN++
	tag := fmt.Sprintf("a%d", s.tagN)
	s.deadline()
	if _, err := fmt.Fprintf(s.conn, tag+" "+format+"\r\n", args...); err != nil {
		return nil, err
	}
	s.literals = nil
	var untagged []string
	for {
		line, err := s.readLine()
		if err != nil {
			return nil, err
		}
		// A line ending in {n} announces an n-byte literal that follows on the
		// wire. Read it exactly, capture it, and continue with the rest of the
		// line so the response stays framed and message bytes are never
		// confused with protocol text.
		for {
			m := reLit.FindStringSubmatch(line)
			if m == nil {
				break
			}
			n, _ := strconv.Atoi(m[1])
			// Reject an oversized literal before allocating for it. Allow a
			// small margin over MaxMessageBytes for envelope/header framing so
			// legitimate at-limit messages still fetch.
			if s.maxLiteral > 0 && int64(n) > s.maxLiteral+(1<<20) {
				return nil, fmt.Errorf("server literal %d bytes exceeds max message size %d", n, s.maxLiteral)
			}
			buf := make([]byte, n)
			s.deadline()
			if _, err := io.ReadFull(s.r, buf); err != nil {
				return nil, err
			}
			s.literals = append(s.literals, string(buf))
			cont, err := s.readLine()
			if err != nil {
				return nil, err
			}
			line = strings.TrimSuffix(line, m[0]) + cont
		}
		if strings.HasPrefix(line, tag+" ") {
			status := strings.TrimPrefix(line, tag+" ")
			if strings.HasPrefix(status, "OK") {
				return untagged, nil
			}
			return untagged, fmt.Errorf("server: %s", status)
		}
		untagged = append(untagged, line)
	}
}

func (s *session) selectInbox() error {
	lines, err := s.exec("SELECT INBOX")
	if err != nil {
		return fmt.Errorf("SELECT INBOX: %w", err)
	}
	for _, l := range lines {
		if m := reExist.FindStringSubmatch(l); m != nil {
			s.exists, _ = strconv.Atoi(m[1])
		}
		if m := reValid.FindStringSubmatch(l); m != nil {
			s.uidValidity = m[1]
		}
	}
	return nil
}

// ensureIndex enumerates the mailbox once (UID + size per sequence number) and
// caches it. The stable per-message ID is "UIDVALIDITY.UID", which the sync
// layer stores as the dedup checkpoint.
// enumerateBatch bounds how many messages are enumerated per FETCH so the
// whole-mailbox metadata response is never buffered in memory at once — a
// single FETCH 1:N over a large mailbox was a primary OOM (pod restart) source.
const enumerateBatch = 2000

func (s *session) ensureIndex() error {
	if s.indexed {
		return nil
	}
	s.indexed = true
	if s.exists == 0 {
		return nil
	}
	for start := 1; start <= s.exists; start += enumerateBatch {
		end := start + enumerateBatch - 1
		if end > s.exists {
			end = s.exists
		}
		lines, err := s.exec("FETCH %d:%d (UID RFC822.SIZE)", start, end)
		if err != nil {
			return err
		}
		for _, l := range lines {
			fm := reFetch.FindStringSubmatch(l)
			if fm == nil {
				continue
			}
			seq, _ := strconv.Atoi(fm[1])
			rm := domain.RemoteMessage{Number: seq}
			if m := reUID.FindStringSubmatch(l); m != nil {
				rm.UIDL = s.uidValidity + "." + m[1]
			}
			if m := reSize.FindStringSubmatch(l); m != nil {
				rm.Size, _ = strconv.ParseInt(m[1], 10, 64)
			}
			s.index = append(s.index, rm)
		}
	}
	return nil
}

func (s *session) UIDL(ctx context.Context) ([]domain.RemoteMessage, error) {
	if err := s.ensureIndex(); err != nil {
		return nil, err
	}
	return s.index, nil
}

func (s *session) List(ctx context.Context) ([]domain.RemoteMessage, error) {
	if err := s.ensureIndex(); err != nil {
		return nil, err
	}
	return s.index, nil
}

func (s *session) Retrieve(ctx context.Context, number int) (io.ReadCloser, error) {
	if _, err := s.exec("FETCH %d (BODY.PEEK[])", number); err != nil {
		return nil, err
	}
	return s.firstLiteral()
}

func (s *session) Top(ctx context.Context, number, lines int) (io.ReadCloser, error) {
	if _, err := s.exec("FETCH %d (BODY.PEEK[HEADER])", number); err != nil {
		return nil, err
	}
	return s.firstLiteral()
}

// firstLiteral returns the message bytes captured by the preceding FETCH.
func (s *session) firstLiteral() (io.ReadCloser, error) {
	if len(s.literals) == 0 {
		return nil, fmt.Errorf("no message literal in FETCH response")
	}
	lit := s.literals[0]
	s.literals = nil
	return io.NopCloser(strings.NewReader(lit)), nil
}

func (s *session) Delete(ctx context.Context, number int) error {
	if _, err := s.exec(`STORE %d +FLAGS (\Deleted)`, number); err != nil {
		return err
	}
	s.deleted = true
	return nil
}

func (s *session) Quit(ctx context.Context) error {
	if s.deleted {
		_, _ = s.exec("EXPUNGE")
	}
	_, err := s.exec("LOGOUT")
	s.conn.Close()
	return err
}

func (s *session) Close() error { return s.conn.Close() }

// ListMailboxes returns the list of IMAP folder names available (§P1 IMAP 폴더 동기화).
func (s *session) ListMailboxes(ctx context.Context) ([]string, error) {
	lines, err := s.exec(`LIST "" "*"`)
	if err != nil {
		return nil, fmt.Errorf("LIST mailboxes: %w", err)
	}
	var mailboxes []string
	reList := regexp.MustCompile(`^\* LIST \(.*\) ".*" "?([^"]+)"?`)
	for _, l := range lines {
		if m := reList.FindStringSubmatch(l); m != nil {
			mailboxes = append(mailboxes, m[1])
		}
	}
	return mailboxes, nil
}

// SelectMailbox selects a specific folder/mailbox (e.g., "INBOX", "Sent", "Drafts").
func (s *session) SelectMailbox(name string) error {
	lines, err := s.exec("SELECT %s", quote(name))
	if err != nil {
		return fmt.Errorf("SELECT %s: %w", name, err)
	}
	s.exists = 0
	s.index = nil
	s.indexed = false
	for _, l := range lines {
		if m := reExist.FindStringSubmatch(l); m != nil {
			s.exists, _ = strconv.Atoi(m[1])
		}
		if m := reValid.FindStringSubmatch(l); m != nil {
			s.uidValidity = m[1]
		}
	}
	return nil
}

// FetchFlags fetches IMAP flags (e.g. \Seen, \Flagged, \Draft) for a sequence number.
func (s *session) FetchFlags(ctx context.Context, number int) ([]string, error) {
	lines, err := s.exec("FETCH %d (FLAGS)", number)
	if err != nil {
		return nil, err
	}
	reFlags := regexp.MustCompile(`FLAGS \(([^)]*)\)`)
	var flags []string
	for _, l := range lines {
		if m := reFlags.FindStringSubmatch(l); m != nil {
			for _, f := range strings.Fields(m[1]) {
				flags = append(flags, f)
			}
		}
	}
	return flags, nil
}

// readLineNoReset reads one CRLF-terminated line WITHOUT resetting the
// connection deadline (readLine resets it to commandTO on every call, which
// would defeat the long IDLE window). The caller manages the deadline.
func (s *session) readLineNoReset() (string, error) {
	line, err := s.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// Idle issues the RFC 2177 IDLE command and blocks until the server reports
// mailbox activity (EXISTS/RECENT/EXPUNGE), the ~28-minute re-idle window
// elapses (returns nil so the caller re-idles), or ctx is cancelled. It is
// self-contained: a single watcher goroutine pokes the read deadline on
// cancellation and is always joined, so no reader goroutine is leaked across
// successive Idle calls (§P1 IMAP IDLE).
func (s *session) Idle(ctx context.Context) error {
	s.tagN++
	tag := fmt.Sprintf("a%d", s.tagN)

	_ = s.conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprintf(s.conn, "%s IDLE\r\n", tag); err != nil {
		return err
	}
	line, err := s.readLineNoReset()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(line, "+") {
		return fmt.Errorf("IDLE rejected: %s", line)
	}

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = s.conn.SetDeadline(time.Now()) // unblock the pending read
		case <-stop:
		}
	}()

	// Terminate the IDLE cleanly and drain to the tagged completion so the
	// session is reusable for the next command / re-idle.
	defer func() {
		_ = s.conn.SetDeadline(time.Now().Add(10 * time.Second))
		_, _ = fmt.Fprintf(s.conn, "DONE\r\n")
		for {
			l, derr := s.readLineNoReset()
			if derr != nil || strings.HasPrefix(l, tag+" ") {
				break
			}
		}
	}()

	// Re-IDLE before servers' 30-minute limit; return nil on the timeout so the
	// caller starts a fresh IDLE.
	_ = s.conn.SetDeadline(time.Now().Add(28 * time.Minute))
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		l, err := s.readLineNoReset()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return nil // periodic re-idle
			}
			return err
		}
		if strings.Contains(l, "EXISTS") || strings.Contains(l, "RECENT") || strings.Contains(l, "EXPUNGE") {
			return nil
		}
	}
}

var _ domain.IdleCapable = (*session)(nil)

// quote wraps an IMAP astring in double quotes, escaping backslash and quote.
func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func secondsOr(v, def int) time.Duration {
	if v <= 0 {
		v = def
	}
	return time.Duration(v) * time.Second
}
