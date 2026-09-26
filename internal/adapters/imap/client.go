// Package imap implements a minimal RFC 3501 client behind the
// domain.InboundDialer port (the same port POP3 uses). It covers exactly what
// ingest needs — connect, LOGIN/PREAUTH, SELECT INBOX, enumerate (UID +
// RFC822.SIZE), fetch a message body, and mark-delete + EXPUNGE — and supports
// implicit TLS (993), STARTTLS, and plaintext for offline networks. Policy
// gating for insecure modes stays in the application layer.
package imap

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
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

// errUnframed marks a session whose byte stream could no longer be put back in
// frame after a refused literal. Once it is set every command fails with it,
// which is the only safe answer: reading on would hand message bytes to the
// response parser.
var errUnframed = errors.New("imap session abandoned: response stream could not be resynchronized")

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
	broken      error    // set once the stream can no longer be trusted
}

// literalMargin is the slack allowed over MaxMessageBytes before a literal is
// refused, so a legitimate at-limit message still fetches with its envelope and
// header framing.
const literalMargin = 1 << 20

// resyncDrainFactor bounds how many bytes a refused literal may cost to
// discard, as a multiple of the size at which a literal is refused. Within that
// budget, dropping the body costs a moment and the ingest loop can carry on with
// the rest of the mailbox over the same session. Beyond it the announced length
// is no longer plausibly a message the server is really sending, and a bogus
// length announces bytes that never arrive and would only block until the
// command deadline, so the session is abandoned instead of drained. Deriving the
// budget from the refusal threshold keeps the drain path reachable at every
// configured MaxMessageBytes rather than only at small ones.
const resyncDrainFactor = 4

// drainChunk is how much of a refused literal is discarded between deadline
// refreshes, so a slow but live server is not cut off mid-drain.
const drainChunk = 1 << 20

// maxLineBytes caps how many bytes one protocol line may accumulate. Message
// bodies never travel on this path — they arrive as {n} literals and are read by
// readLiteral — so everything read here is protocol text (tagged completions,
// FETCH envelopes, capability and LIST lines), for which 1 MiB is generous.
// Without a cap a server that streams bytes and never sends LF grows the line
// buffer until the deadline expires, which on the IDLE path is the 28-minute
// re-idle window rather than the 60-second command timeout.
const maxLineBytes = 1 << 20

// maxResponseBytes caps how much protocol text one command's response may
// accumulate. maxLineBytes bounds a single line, but nothing bounds how many
// lines a response contains, and readLine refreshes the command deadline on
// every one of them — so a server that keeps sending short untagged lines and
// never sends the tagged completion grows exec's collected lines for as long as
// it cares to, with no deadline left to stop it. What is counted against the
// bound is each line's bytes plus responseLineOverhead, so the cap tracks what
// the response really costs rather than only its visible text. The largest
// response the adapter legitimately asks for is a full enumerateBatch of FETCH
// metadata, charged at 117,994 bytes for 2000 messages by
// TestIMAPFullEnumerateBatchStaysUnderResponseBound; 8 MiB leaves that seventy
// times over for longer UIDs, flags and envelopes while still bounding a
// runaway response.
const maxResponseBytes = 8 << 20

// responseLineOverhead is what every line of a response costs on top of the
// bytes readBoundedLine returns, and without it maxResponseBytes does not hold.
// A line arrives with its CRLF already stripped, so a bare "\r\n" measures zero:
// counting only len(line), a server that answers with nothing but blank lines
// never advances the total at all and exec appends "" for ever, which is the
// runaway maxResponseBytes exists to stop. A retained line also costs a 16-byte
// string header in untagged, so at one byte per line an 8 MiB total would sit on
// upwards of 130 MiB of live heap. Charging the two bytes the server really sent
// plus that header closes both: the worst case is now bounded at roughly
// maxResponseBytes of residency however short the lines are. Literal
// continuations are folded into the line they continue rather than retained
// separately, so charging them the same constant over-counts by at most
// maxResponseLiterals headers — a rounding error against the bound, and not
// worth a second constant.
const responseLineOverhead = 2 + 16

// maxResponseLiterals caps how many literals one command's response may deliver.
// maxResponseBytes cannot see this: announcing a literal costs about thirty
// bytes of line text, so a server could hand over gigabytes of s.literals while
// staying far inside the byte bound. The count is deliberately what is bounded
// and not the literal bytes — an account with no configured size limit
// (refusalThreshold() == 0) is entitled to a single body of any size. Retrieve
// and Top read one literal (firstLiteral) and ensureIndex reads none, so the
// legitimate count is 1.
const maxResponseLiterals = 64

func (s *session) deadline() { s.conn.SetDeadline(time.Now().Add(s.commandTO)) }

// readLine reads one CRLF-terminated protocol line (without the CRLF).
func (s *session) readLine() (string, error) {
	s.deadline()
	return s.readBoundedLine()
}

// readBoundedLine reads one LF-terminated line, without the trailing CRLF and
// without letting it grow past maxLineBytes. It is the only place the adapter
// reads raw protocol text: a server that never sends LF would otherwise keep the
// reader accumulating fragments for as long as the deadline allows. Once the
// bound trips, the offset the server believes the response continues at is
// unknown — the rest of that line is still on the wire — so the session is
// abandoned rather than resynchronized, exactly as for an undrainable literal.
//
// Deadline policy stays with the callers: readLine refreshes the command
// deadline, readLineNoReset deliberately does not (see its comment).
func (s *session) readBoundedLine() (string, error) {
	var acc []byte
	for {
		frag, err := s.r.ReadSlice('\n')
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return "", err
		}
		if int64(len(acc))+int64(len(frag)) > maxLineBytes {
			return "", s.abandon(fmt.Errorf("%w: protocol line exceeds %d bytes", errUnframed, maxLineBytes))
		}
		if err == nil {
			if len(acc) == 0 {
				// The whole line fit in one read: no need to copy it out of the
				// reader's buffer twice.
				return strings.TrimRight(string(frag), "\r\n"), nil
			}
			return strings.TrimRight(string(append(acc, frag...)), "\r\n"), nil
		}
		// frag points into the reader's buffer, which the next read reuses.
		acc = append(acc, frag...)
	}
}

// exec sends a tagged command and collects untagged responses until the
// matching tagged completion. Server literals ({n}) are read inline so the
// stream stays framed; the literal bytes are appended to the current line.
func (s *session) exec(format string, args ...any) ([]string, error) {
	if s.broken != nil {
		return nil, s.broken
	}
	s.tagN++
	tag := fmt.Sprintf("a%d", s.tagN)
	s.deadline()
	if _, err := fmt.Fprintf(s.conn, tag+" "+format+"\r\n", args...); err != nil {
		return nil, err
	}
	s.literals = nil
	var untagged []string
	// A refused literal is reported only once the response has been read to its
	// tagged completion, so the failure costs this one command and not the
	// framing of every command after it.
	var refused error
	// What this one response has cost so far. Unlike a refused literal, an
	// over-long response cannot be resynchronized — the server is still mid-reply
	// and owes a tagged completion it is not going to send — so tripping either
	// bound abandons the session rather than failing just this command.
	var respBytes int64
	var literals int
	for {
		line, err := s.readLine()
		if err != nil {
			return nil, err
		}
		if respBytes += int64(len(line)) + responseLineOverhead; respBytes > maxResponseBytes {
			return nil, s.abandon(fmt.Errorf("%w: response exceeds %d bytes of protocol text", errUnframed, int64(maxResponseBytes)))
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
			// The announced length comes from a server the account owner
			// chose, so it is untrusted input. A length that does not fit in
			// int64 leaves the number of bytes that follow unknown — there is
			// no offset to resynchronize at and nothing safe to drain — so the
			// session is abandoned rather than read on.
			n, err := strconv.ParseInt(m[1], 10, 64)
			if err != nil {
				return nil, s.abandon(fmt.Errorf("%w: unreadable literal length %q: %w", errUnframed, m[1], err))
			}
			// Counted on the announcement rather than on s.literals, so a run of
			// refused literals — which are drained, not kept — is bounded too:
			// each one costs a drain of up to the resync budget.
			if literals++; literals > maxResponseLiterals {
				return nil, s.abandon(fmt.Errorf("%w: response announces more than %d literals", errUnframed, maxResponseLiterals))
			}
			// Reject an oversized literal before allocating for it. Allow a
			// small margin over MaxMessageBytes for envelope/header framing so
			// legitimate at-limit messages still fetch.
			if limit := s.refusalThreshold(); limit > 0 && n > limit {
				if refused == nil {
					refused = fmt.Errorf("server literal %d bytes exceeds max message size %d", n, s.maxLiteral)
				}
				if err := s.discardLiteral(n); err != nil {
					return nil, err
				}
			} else {
				lit, err := s.readLiteral(n)
				if err != nil {
					return nil, err
				}
				s.literals = append(s.literals, lit)
			}
			cont, err := s.readLine()
			if err != nil {
				return nil, err
			}
			if respBytes += int64(len(cont)) + responseLineOverhead; respBytes > maxResponseBytes {
				return nil, s.abandon(fmt.Errorf("%w: response exceeds %d bytes of protocol text", errUnframed, int64(maxResponseBytes)))
			}
			line = strings.TrimSuffix(line, m[0]) + cont
		}
		if strings.HasPrefix(line, tag+" ") {
			status := strings.TrimPrefix(line, tag+" ")
			if strings.HasPrefix(status, "OK") {
				return untagged, refused
			}
			return untagged, fmt.Errorf("server: %s", status)
		}
		untagged = append(untagged, line)
	}
}

// refusalThreshold is the announced literal size above which a body is refused
// rather than buffered (0 when no limit is configured).
func (s *session) refusalThreshold() int64 {
	if s.maxLiteral <= 0 {
		return 0
	}
	if s.maxLiteral > math.MaxInt64-literalMargin {
		return math.MaxInt64
	}
	return s.maxLiteral + literalMargin
}

// resyncBudget is the most a refused literal may cost to drain before the
// session is abandoned instead.
func (s *session) resyncBudget() int64 {
	limit := s.refusalThreshold()
	if limit <= 0 || limit > math.MaxInt64/resyncDrainFactor {
		return math.MaxInt64
	}
	return limit * resyncDrainFactor
}

// readLiteral buffers a literal the client accepts. The buffer follows the
// bytes that actually arrive instead of the length the server announced:
// reserving the announced size would let a server amplify a short reply — or one
// it never sends at all — into an allocation of whatever length it cares to
// declare. Like discardLiteral it refreshes the deadline every chunk, so a slow
// but live server is not cut off part way through a body.
func (s *session) readLiteral(n int64) (string, error) {
	var b bytes.Buffer
	reserve := int64(drainChunk)
	if n < reserve {
		reserve = n
	}
	b.Grow(int(reserve))
	for remaining := n; remaining > 0; {
		chunk := remaining
		if chunk > drainChunk {
			chunk = drainChunk
		}
		s.deadline()
		got, err := io.CopyN(&b, s.r, chunk)
		if err != nil {
			return "", err
		}
		remaining -= got
	}
	return b.String(), nil
}

// discardLiteral drops the bytes of a literal the client refuses to buffer, so
// the next protocol line is read where the server believes the response
// continues. A length too large to drain — or a drain that fails part way —
// leaves the stream at an unknown offset, so the session is abandoned instead.
func (s *session) discardLiteral(n int64) error {
	if budget := s.resyncBudget(); n > budget {
		return s.abandon(fmt.Errorf("%w: %d-byte literal exceeds the %d-byte resync budget", errUnframed, n, budget))
	}
	for remaining := n; remaining > 0; {
		chunk := remaining
		if chunk > drainChunk {
			chunk = drainChunk
		}
		s.deadline()
		got, err := io.CopyN(io.Discard, s.r, chunk)
		if err != nil {
			return s.abandon(fmt.Errorf("%w: discarding a %d-byte literal: %w", errUnframed, n, err))
		}
		remaining -= got
	}
	return nil
}

// abandon closes the connection and records why every later command fails.
func (s *session) abandon(err error) error {
	s.broken = err
	s.conn.Close()
	return err
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
// would defeat the long IDLE window). The caller manages the deadline. The
// maxLineBytes bound matters most here: the deadline this path runs under is the
// 28-minute re-idle window, so an unbounded line would buffer for that long.
func (s *session) readLineNoReset() (string, error) {
	return s.readBoundedLine()
}

// Idle issues the RFC 2177 IDLE command and blocks until the server reports
// mailbox activity (EXISTS/RECENT/EXPUNGE), the ~28-minute re-idle window
// elapses (returns nil so the caller re-idles), or ctx is cancelled. It is
// self-contained: a single watcher goroutine pokes the read deadline on
// cancellation and is always joined, so no reader goroutine is leaked across
// successive Idle calls (§P1 IMAP IDLE).
func (s *session) Idle(ctx context.Context) error {
	if s.broken != nil {
		return s.broken
	}
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
