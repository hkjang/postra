package imap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"postra/internal/domain"
	"postra/internal/platform/config"
)

const testMsg = "From: a@x\r\nSubject: hi\r\n\r\nbody{with}braces\r\n"

// fakeServer scripts a minimal IMAP4rev1 conversation. rejectLogin makes LOGIN
// return NO so the adapter's AuthError path can be exercised.
func fakeServer(t *testing.T, rejectLogin bool) (addr string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		io.WriteString(conn, "* OK IMAP4rev1 ready\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			sp := strings.SplitN(line, " ", 3)
			if len(sp) < 2 {
				continue
			}
			tag, cmd := sp[0], strings.ToUpper(sp[1])
			switch cmd {
			case "LOGIN":
				if rejectLogin {
					fmt.Fprintf(conn, "%s NO [AUTHENTICATIONFAILED] bad creds\r\n", tag)
				} else {
					fmt.Fprintf(conn, "%s OK LOGIN completed\r\n", tag)
				}
			case "SELECT":
				io.WriteString(conn, "* 2 EXISTS\r\n")
				io.WriteString(conn, "* OK [UIDVALIDITY 777] ok\r\n")
				fmt.Fprintf(conn, "%s OK [READ-WRITE] SELECT completed\r\n", tag)
			case "FETCH":
				if strings.Contains(line, "RFC822.SIZE") {
					io.WriteString(conn, "* 1 FETCH (UID 101 RFC822.SIZE 20)\r\n")
					io.WriteString(conn, "* 2 FETCH (UID 102 RFC822.SIZE 40)\r\n")
					fmt.Fprintf(conn, "%s OK FETCH completed\r\n", tag)
				} else { // BODY.PEEK[]
					fmt.Fprintf(conn, "* 1 FETCH (UID 101 BODY[] {%d}\r\n", len(testMsg))
					io.WriteString(conn, testMsg)
					io.WriteString(conn, ")\r\n")
					fmt.Fprintf(conn, "%s OK FETCH completed\r\n", tag)
				}
			case "STORE":
				fmt.Fprintf(conn, "%s OK STORE completed\r\n", tag)
			case "EXPUNGE":
				io.WriteString(conn, "* 1 EXPUNGE\r\n")
				fmt.Fprintf(conn, "%s OK EXPUNGE completed\r\n", tag)
			case "LOGOUT":
				io.WriteString(conn, "* BYE\r\n")
				fmt.Fprintf(conn, "%s OK LOGOUT completed\r\n", tag)
				return
			default:
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			}
		}
	}()
	return ln.Addr().String()
}

// oversizeServer announces a BODY literal far larger than any allowed message,
// exercising the client's OOM guard: the client must refuse before allocating.
func oversizeServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		io.WriteString(conn, "* OK IMAP4rev1 ready\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			sp := strings.SplitN(strings.TrimRight(line, "\r\n"), " ", 3)
			if len(sp) < 2 {
				continue
			}
			tag, cmd := sp[0], strings.ToUpper(sp[1])
			switch cmd {
			case "LOGIN":
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			case "SELECT":
				io.WriteString(conn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 1] ok\r\n")
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			case "FETCH":
				// Announce a 4 GiB literal but send nothing — the guard must
				// trip before io.ReadFull ever allocates or blocks.
				io.WriteString(conn, "* 1 FETCH (UID 1 BODY[] {4294967296}\r\n")
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			default:
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			}
		}
	}()
	return ln.Addr().String()
}

func TestIMAPRejectsOversizeLiteral(t *testing.T) {
	addr := oversizeServer(t)
	host, portStr, _ := net.SplitHostPort(addr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	sess, err := Dialer{}.Dial(context.Background(), domain.InboundDialOptions{
		Host: host, Port: port, Security: domain.SecurityNone,
		Username: "me", Password: domain.NewSecretHandle([]byte("pw")),
		MaxMessageBytes: 50 << 20,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer sess.Close()
	if _, err := sess.Retrieve(context.Background(), 1); err == nil {
		t.Fatal("expected Retrieve to reject an oversized literal, got nil error")
	}
}

// trapHeader ends in a literal announcement — the shape a client that skipped a
// refused body's bytes would misread as protocol text.
var trapHeader = "X-Trap: {40}\r\n" + strings.Repeat("y", 40) + "\r\n"

// writeTrappedBody streams a size-byte message body starting with trapHeader,
// without ever holding it in memory, so a body above the shipped 50 MiB limit
// can be served cheaply.
func writeTrappedBody(w io.Writer, size int64) {
	io.WriteString(w, trapHeader)
	chunk := strings.Repeat("z", 1<<16)
	for remaining := size - int64(len(trapHeader)); remaining > 0; {
		n := int64(len(chunk))
		if n > remaining {
			n = remaining
		}
		if _, err := io.WriteString(w, chunk[:n]); err != nil {
			return
		}
		remaining -= n
	}
}

// refusedLiteralServer serves two messages: sequence 1 is a body of bodySize
// bytes that really arrives on the wire but is larger than the client's limit,
// sequence 2 is an ordinary message. It models the server the ingest loop meets
// when RFC822.SIZE is missing, so the application-level oversize skip cannot
// fire and the adapter's guard is what refuses the body mid-response.
func refusedLiteralServer(t *testing.T, bodySize int64) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		io.WriteString(conn, "* OK IMAP4rev1 ready\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			sp := strings.SplitN(strings.TrimRight(line, "\r\n"), " ", 3)
			if len(sp) < 2 {
				continue
			}
			tag, cmd := sp[0], strings.ToUpper(sp[1])
			switch {
			case cmd == "SELECT":
				io.WriteString(conn, "* 2 EXISTS\r\n* OK [UIDVALIDITY 9] ok\r\n")
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			case cmd == "FETCH" && len(sp) == 3 && strings.HasPrefix(sp[2], "1 "):
				fmt.Fprintf(conn, "* 1 FETCH (UID 1 BODY[] {%d}\r\n", bodySize)
				writeTrappedBody(conn, bodySize)
				io.WriteString(conn, ")\r\n")
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			case cmd == "FETCH":
				fmt.Fprintf(conn, "* 2 FETCH (UID 2 BODY[] {%d}\r\n", len(testMsg))
				io.WriteString(conn, testMsg)
				io.WriteString(conn, ")\r\n")
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			default:
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			}
		}
	}()
	return ln.Addr().String()
}

// TestIMAPRefusedLiteralKeepsStreamFramed pins the behaviour the ingest loop
// depends on: refusing one oversized body must not leave its bytes in the
// socket, because sync.go counts the failure and keeps fetching the remaining
// messages over the same session.
func TestIMAPRefusedLiteralKeepsStreamFramed(t *testing.T) {
	sess := dialMax(t, refusedLiteralServer(t, 3<<20), 1<<20)
	defer sess.Close()
	ctx := context.Background()

	// The refusal itself must be reported — reading the response to its tagged
	// completion must not turn the oversized body into a silent success for
	// commands that do not depend on a literal.
	_, err := sess.Retrieve(ctx, 1)
	if err == nil || !strings.Contains(err.Error(), "exceeds max message size") {
		t.Fatalf("first fetch error = %v, want the oversized literal to be refused", err)
	}
	rc, err := sess.Retrieve(ctx, 2)
	if err != nil {
		t.Fatalf("fetch after a refused literal: %v", err)
	}
	got, _ := io.ReadAll(rc)
	if string(got) != testMsg {
		t.Fatalf("body after a refused literal = %.60q, want %q", got, testMsg)
	}
}

// TestIMAPRefusedLiteralResyncsAtDefaultLimit runs the same resynchronization at
// the shipped Sync.MaxMessageBytes instead of a small test-only limit, because a
// drain budget that does not scale with the configured limit would make the
// resync path dead code in every real deployment: the body would be abandoned
// rather than drained, and the session — plus every message behind it — lost.
func TestIMAPRefusedLiteralResyncsAtDefaultLimit(t *testing.T) {
	maxBytes := config.Default().Sync.MaxMessageBytes
	// Just past the refusal threshold (MaxMessageBytes + 1 MiB of framing
	// margin): the smallest body the guard refuses at the default setting.
	sess := dialMax(t, refusedLiteralServer(t, maxBytes+(2<<20)), maxBytes)
	defer sess.Close()
	ctx := context.Background()

	_, err := sess.Retrieve(ctx, 1)
	if err == nil || !strings.Contains(err.Error(), "exceeds max message size") {
		t.Fatalf("first fetch error = %v, want the oversized literal to be refused", err)
	}
	if errors.Is(err, errUnframed) {
		t.Fatalf("first fetch abandoned the session at the default limit: %v", err)
	}
	rc, err := sess.Retrieve(ctx, 2)
	if err != nil {
		t.Fatalf("fetch after a refused literal at the default limit: %v", err)
	}
	got, _ := io.ReadAll(rc)
	if string(got) != testMsg {
		t.Fatalf("body after a refused literal = %.60q, want %q", got, testMsg)
	}
}

// TestResyncBudgetExceedsRefusalThreshold pins the invariant the test above
// depends on: every literal the client refuses starts out drainable, at any
// configured limit, so abandoning the session stays the fallback for a length
// the server is not really sending.
func TestResyncBudgetExceedsRefusalThreshold(t *testing.T) {
	for _, maxBytes := range []int64{1 << 20, config.Default().Sync.MaxMessageBytes, 1 << 40} {
		s := &session{maxLiteral: maxBytes}
		budget, limit := s.resyncBudget(), s.refusalThreshold()
		if limit+1 > budget {
			t.Errorf("maxLiteral=%d: smallest refused literal %d exceeds the resync budget %d", maxBytes, limit+1, budget)
		}
	}
	// A limit large enough to overflow the multiplication must saturate, not
	// wrap into a budget that refuses to drain anything.
	s := &session{maxLiteral: math.MaxInt64 - 1}
	if budget := s.resyncBudget(); budget != math.MaxInt64 {
		t.Errorf("resync budget at an extreme limit = %d, want saturation at %d", budget, int64(math.MaxInt64))
	}
}

// TestIMAPUndrainableLiteralAbandonsSession pins the other half: a literal too
// large to discard must close the session instead of being drained (the
// 4 GiB announcement is never sent, so draining it would block until the
// command deadline) and every later command must fail with that same error.
func TestIMAPUndrainableLiteralAbandonsSession(t *testing.T) {
	sess := dialMax(t, oversizeServer(t), 50<<20)
	defer sess.Close()
	ctx := context.Background()

	_, err := sess.Retrieve(ctx, 1)
	if !errors.Is(err, errUnframed) {
		t.Fatalf("first fetch error = %v, want one wrapping errUnframed", err)
	}
	if _, err := sess.Retrieve(ctx, 1); !errors.Is(err, errUnframed) {
		t.Fatalf("second fetch error = %v, want the session to stay unusable", err)
	}
}

// declaredLiteralServer announces a BODY literal whose length is taken verbatim
// from declared (so a length outside int64 can be scripted), then sends body and
// closes. It models the untrusted server an account's own host may be: the
// announced length is whatever it says, and the bytes behind it are whatever it
// chooses to send.
func declaredLiteralServer(t *testing.T, declared, body string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		io.WriteString(conn, "* OK IMAP4rev1 ready\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			sp := strings.SplitN(strings.TrimRight(line, "\r\n"), " ", 3)
			if len(sp) < 2 {
				continue
			}
			tag, cmd := sp[0], strings.ToUpper(sp[1])
			switch cmd {
			case "SELECT":
				io.WriteString(conn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 3] ok\r\n")
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			case "FETCH":
				fmt.Fprintf(conn, "* 1 FETCH (UID 1 BODY[] {%s}\r\n", declared)
				io.WriteString(conn, body)
				return // hang up rather than send the rest of the announced bytes
			default:
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			}
		}
	}()
	return ln.Addr().String()
}

// TestIMAPOutOfRangeLiteralAbandonsSession pins the parse of the announced
// length. On an account with no size limit (MaxMessageBytes <= 0 turns the
// refusal threshold off) a length outside int64 must not be read as a usable
// number: the client cannot know how many bytes follow, so it abandons the
// session rather than allocating for it or trying to drain it.
func TestIMAPOutOfRangeLiteralAbandonsSession(t *testing.T) {
	sess := dialMax(t, declaredLiteralServer(t, "99999999999999999999", ""), 0)
	defer sess.Close()
	ctx := context.Background()

	_, err := sess.Retrieve(ctx, 1)
	if !errors.Is(err, errUnframed) {
		t.Fatalf("fetch of an out-of-range literal = %v, want an error wrapping errUnframed", err)
	}
	if _, err := sess.Retrieve(ctx, 1); !errors.Is(err, errUnframed) {
		t.Fatalf("second fetch error = %v, want the session to stay unusable", err)
	}
}

// TestIMAPLiteralBufferTracksDeliveredBytes pins that the body buffer follows
// the bytes that actually arrive, not the size the server announced. Without
// that, a server with no size limit configured amplifies a short reply into a
// buffer of whatever length it cares to declare.
func TestIMAPLiteralBufferTracksDeliveredBytes(t *testing.T) {
	const declared = 1 << 30 // 1 GiB announced...
	const sent = "only these bytes arrive"

	sess := dialMax(t, declaredLiteralServer(t, strconv.Itoa(declared), sent), 0)
	defer sess.Close()

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := sess.Retrieve(context.Background(), 1)
	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatal("fetch of a truncated literal succeeded, want an error")
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<20 {
		t.Fatalf("fetch allocated %d bytes for a %d-byte announcement that delivered %d bytes; want the buffer to track delivery", grew, declared, len(sent))
	}
}

// unterminatedLineServer answers FETCH with a response line it never terminates:
// after the opening of an untagged FETCH it keeps writing bytes with no LF until
// the client hangs up. It models the server an account owner may point at — one
// that grows the client's line buffer for as long as the client keeps reading.
// The writes are throttled so the pre-bound behaviour (buffer until the command
// deadline) is observable without exhausting the test machine's memory.
func unterminatedLineServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop); ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		io.WriteString(conn, "* OK IMAP4rev1 ready\r\n")
		chunk := strings.Repeat("z", 4096)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			sp := strings.SplitN(strings.TrimRight(line, "\r\n"), " ", 3)
			if len(sp) < 2 {
				continue
			}
			tag, cmd := sp[0], strings.ToUpper(sp[1])
			switch cmd {
			case "SELECT":
				io.WriteString(conn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 5] ok\r\n")
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			case "FETCH", "IDLE":
				if cmd == "IDLE" {
					io.WriteString(conn, "+ idling\r\n")
				} else {
					io.WriteString(conn, "* 1 FETCH (UID 1 ")
				}
				for {
					select {
					case <-stop:
						return
					default:
					}
					if _, err := io.WriteString(conn, chunk); err != nil {
						return
					}
					time.Sleep(time.Millisecond)
				}
			default:
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			}
		}
	}()
	return ln.Addr().String()
}

// TestIMAPUnterminatedLineAbandonsSession pins the line-length bound: a server
// that streams bytes and never sends LF must not be able to grow the client's
// line buffer until the deadline expires. Once the bound trips, the offset the
// server believes the response continues at is unknown, so the session is
// abandoned like any other unframed response.
func TestIMAPUnterminatedLineAbandonsSession(t *testing.T) {
	sess := dialMax(t, unterminatedLineServer(t), 50<<20)
	defer sess.Close()
	ctx := context.Background()

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := sess.Retrieve(ctx, 1)
	runtime.ReadMemStats(&after)

	if !errors.Is(err, errUnframed) {
		t.Fatalf("fetch of an unterminated line = %v, want an error wrapping errUnframed", err)
	}
	if !strings.Contains(err.Error(), "protocol line exceeds") {
		t.Fatalf("fetch error = %v, want it to name the line-length bound", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 16*maxLineBytes {
		t.Fatalf("fetch buffered %d bytes of an unterminated line; want the %d-byte bound to stop it", grew, maxLineBytes)
	}
	if _, err := sess.Retrieve(ctx, 1); !errors.Is(err, errUnframed) {
		t.Fatalf("second fetch error = %v, want the session to stay unusable", err)
	}
}

// TestIMAPUnterminatedIdleLineAbandonsSession pins the bound on the IDLE path,
// which is where it matters most: readLineNoReset deliberately leaves the
// deadline alone, so the window an unterminated line could buffer for there is
// the 28-minute re-idle window rather than the 60-second command timeout. It
// also walks the abandon-during-IDLE exit, where the deferred DONE is written to
// a socket abandon has already closed.
func TestIMAPUnterminatedIdleLineAbandonsSession(t *testing.T) {
	sess := dialMax(t, unterminatedLineServer(t), 0)
	defer sess.Close()

	idler, ok := sess.(domain.IdleCapable)
	if !ok {
		t.Fatalf("session %T is not IdleCapable", sess)
	}
	err := idler.Idle(context.Background())
	if !errors.Is(err, errUnframed) {
		t.Fatalf("idle over an unterminated line = %v, want an error wrapping errUnframed", err)
	}
	// A bound trip is not a re-idle timeout: the caller must not loop back into
	// IDLE on a stream it can no longer frame.
	if err := idler.Idle(context.Background()); !errors.Is(err, errUnframed) {
		t.Fatalf("second idle = %v, want the session to stay unusable", err)
	}
}

// longLineServer answers every FETCH with the one untagged line given, so a
// response sitting exactly at the line-length bound can be checked end to end.
func longLineServer(t *testing.T, untagged string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		io.WriteString(conn, "* OK IMAP4rev1 ready\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			sp := strings.SplitN(strings.TrimRight(line, "\r\n"), " ", 3)
			if len(sp) < 2 {
				continue
			}
			tag, cmd := sp[0], strings.ToUpper(sp[1])
			switch cmd {
			case "SELECT":
				io.WriteString(conn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 777] ok\r\n")
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			case "FETCH":
				io.WriteString(conn, untagged+"\r\n")
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			default:
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			}
		}
	}()
	return ln.Addr().String()
}

// TestIMAPLongLineAtBoundIsReturnedWhole pins the other side of the bound: a
// line that fills it exactly — bigger than bufio's 4 KiB read buffer, so it is
// reassembled from many fragments — must come back byte-for-byte, with the UID
// and size at its far end still parsed. A bound that dropped or truncated the
// tail would silently lose every message's identity.
func TestIMAPLongLineAtBoundIsReturnedWhole(t *testing.T) {
	const prefix = "* 1 FETCH (X-Pad "
	const suffix = " UID 101 RFC822.SIZE 20)"
	// -2 for the CRLF, which counts against the bound as delivered bytes.
	line := prefix + strings.Repeat("z", maxLineBytes-len(prefix)-len(suffix)-2) + suffix

	sess := dialMax(t, longLineServer(t, line), 0)
	defer sess.Close()

	msgs, err := sess.UIDL(context.Background())
	if err != nil {
		t.Fatalf("enumerate over a %d-byte line: %v", len(line), err)
	}
	if len(msgs) != 1 || msgs[0].UIDL != "777.101" || msgs[0].Size != 20 {
		t.Fatalf("enumerate = %+v, want one message 777.101 of 20 bytes", msgs)
	}

	lines, err := sess.(*session).exec("FETCH 1:1 (UID RFC822.SIZE)")
	if err != nil {
		t.Fatalf("exec over a %d-byte line: %v", len(line), err)
	}
	if len(lines) != 1 {
		t.Fatalf("untagged lines = %d, want 1", len(lines))
	}
	if len(lines[0]) != len(line) {
		t.Fatalf("line length = %d, want %d (the bound must not truncate)", len(lines[0]), len(line))
	}
	if lines[0] != line {
		t.Fatalf("line differs from what the server sent (suffix %.40q, want %.40q)",
			lines[0][len(lines[0])-40:], line[len(line)-40:])
	}
}

// endlessUntaggedServer answers every command after login with short untagged
// lines and never sends the tagged completion. exec's collection loop has no
// reason of its own to stop there: each line is well inside the line-length
// bound, and readLine refreshes the command deadline on every one of them. The
// writes are throttled so the pre-bound behaviour (accumulate until the test
// binary's own timeout) is observable without exhausting the test machine.
func endlessUntaggedServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop); ln.Close() })
	var batch strings.Builder
	for batch.Len() < 4096 {
		batch.WriteString("* 1 FETCH (UID 1 RFC822.SIZE 20)\r\n")
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		io.WriteString(conn, "* OK IMAP4rev1 ready\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			sp := strings.SplitN(strings.TrimRight(line, "\r\n"), " ", 3)
			if len(sp) < 2 {
				continue
			}
			tag, cmd := sp[0], strings.ToUpper(sp[1])
			switch cmd {
			case "LOGIN":
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			case "SELECT":
				io.WriteString(conn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 11] ok\r\n")
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			default:
				for {
					select {
					case <-stop:
						return
					default:
					}
					if _, err := io.WriteString(conn, batch.String()); err != nil {
						return
					}
					time.Sleep(time.Millisecond)
				}
			}
		}
	}()
	return ln.Addr().String()
}

// TestIMAPEndlessUntaggedResponseAbandonsSession pins the bound on how much one
// response may accumulate. maxLineBytes bounds a single line; nothing bounded
// how many lines a response may contain, so a server that keeps sending short
// untagged lines and never completes the tag grew `untagged` for as long as it
// cared to — and because readLine refreshes the command deadline per line, that
// is forever, not sixty seconds.
func TestIMAPEndlessUntaggedResponseAbandonsSession(t *testing.T) {
	sess := dialMax(t, endlessUntaggedServer(t), 50<<20)
	defer sess.Close()
	ctx := context.Background()

	started := time.Now()
	_, err := sess.UIDL(ctx)
	elapsed := time.Since(started)

	if !errors.Is(err, errUnframed) {
		t.Fatalf("enumerate over an endless response = %v, want an error wrapping errUnframed", err)
	}
	if !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("enumerate error = %v, want it to name the response-size bound", err)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("enumerate took %v to give up; want the bound to stop it in seconds", elapsed)
	}
	if _, err := sess.Retrieve(ctx, 1); !errors.Is(err, errUnframed) {
		t.Fatalf("second command = %v, want the session to stay unusable", err)
	}
}

// endlessLiteralServer answers FETCH with a response that never stops handing
// over literals: every continuation line ends in another {n} announcement, so
// exec's inner literal loop never breaks out. Each literal costs only about
// thirty bytes of protocol text, which is what makes a bound on response *bytes*
// blind to it.
func endlessLiteralServer(t *testing.T, literal string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop); ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		io.WriteString(conn, "* OK IMAP4rev1 ready\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			sp := strings.SplitN(strings.TrimRight(line, "\r\n"), " ", 3)
			if len(sp) < 2 {
				continue
			}
			tag, cmd := sp[0], strings.ToUpper(sp[1])
			switch cmd {
			case "SELECT":
				io.WriteString(conn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 12] ok\r\n")
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			case "FETCH":
				fmt.Fprintf(conn, "* 1 FETCH (UID 1 BODY[] {%d}\r\n", len(literal))
				for {
					select {
					case <-stop:
						return
					default:
					}
					if _, err := io.WriteString(conn, literal); err != nil {
						return
					}
					if _, err := fmt.Fprintf(conn, " X-Part: {%d}\r\n", len(literal)); err != nil {
						return
					}
					time.Sleep(time.Millisecond)
				}
			default:
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			}
		}
	}()
	return ln.Addr().String()
}

// TestIMAPEndlessLiteralsAbandonSession pins the second bound: a response that
// keeps announcing literals grows s.literals without ever coming near the
// response-size bound, because the announcement itself is a few dozen bytes of
// line text. Retrieve and Top read exactly one literal and ensureIndex reads
// none, so a response carrying dozens is already a server the client cannot use.
func TestIMAPEndlessLiteralsAbandonSession(t *testing.T) {
	sess := dialMax(t, endlessLiteralServer(t, strings.Repeat("z", 4096)), 0)
	defer sess.Close()
	ctx := context.Background()

	started := time.Now()
	_, err := sess.Retrieve(ctx, 1)
	elapsed := time.Since(started)

	if !errors.Is(err, errUnframed) {
		t.Fatalf("fetch of an endless literal run = %v, want an error wrapping errUnframed", err)
	}
	if !strings.Contains(err.Error(), "literals") {
		t.Fatalf("fetch error = %v, want it to name the literal-count bound", err)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("fetch took %v to give up; want the bound to stop it in seconds", elapsed)
	}
	if _, err := sess.Retrieve(ctx, 1); !errors.Is(err, errUnframed) {
		t.Fatalf("second fetch = %v, want the session to stay unusable", err)
	}
}

// batchServer models a mailbox of count messages: SELECT reports them all and
// every FETCH answers with one metadata line per message, the shape ensureIndex
// meets on a real server at the enumerateBatch size. It returns the exact size
// of that response so the test can pin the margin the bound leaves it.
func batchServer(t *testing.T, count int) (string, int) {
	t.Helper()
	var payload strings.Builder
	for i := 1; i <= count; i++ {
		fmt.Fprintf(&payload, "* %d FETCH (UID %d RFC822.SIZE %d)\r\n", i, 100+i, 2048+i)
	}
	body := payload.String()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		io.WriteString(conn, "* OK IMAP4rev1 ready\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			sp := strings.SplitN(strings.TrimRight(line, "\r\n"), " ", 3)
			if len(sp) < 2 {
				continue
			}
			tag, cmd := sp[0], strings.ToUpper(sp[1])
			switch cmd {
			case "SELECT":
				fmt.Fprintf(conn, "* %d EXISTS\r\n* OK [UIDVALIDITY 777] ok\r\n", count)
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			case "FETCH":
				io.WriteString(conn, body)
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			default:
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			}
		}
	}()
	return ln.Addr().String(), len(body)
}

// TestIMAPFullEnumerateBatchStaysUnderResponseBound is the regression guard on
// the bound's value: the largest response the adapter legitimately asks for is a
// full enumerateBatch of FETCH metadata, and it must pass with room to spare. A
// bound tuned low enough to break this would abandon the session on every large
// mailbox instead of on a runaway server.
func TestIMAPFullEnumerateBatchStaysUnderResponseBound(t *testing.T) {
	addr, size := batchServer(t, enumerateBatch)
	sess := dialMax(t, addr, 0)
	defer sess.Close()

	msgs, err := sess.UIDL(context.Background())
	if err != nil {
		t.Fatalf("enumerate a full %d-message batch (%d bytes of protocol text): %v", enumerateBatch, size, err)
	}
	if len(msgs) != enumerateBatch {
		t.Fatalf("enumerate returned %d messages, want %d", len(msgs), enumerateBatch)
	}
	if msgs[0].UIDL != "777.101" || msgs[enumerateBatch-1].Size != int64(2048+enumerateBatch) {
		t.Fatalf("enumerate edges = %+v / %+v, want 777.101 first and %d bytes last", msgs[0], msgs[enumerateBatch-1], 2048+enumerateBatch)
	}
	t.Logf("a full %d-message enumeration is %d bytes of protocol text (bound %d)", enumerateBatch, size, int64(maxResponseBytes))
	if int64(size)*8 > maxResponseBytes {
		t.Fatalf("a full %d-message enumeration is %d bytes, within 8x of the %d-byte bound; the bound leaves no margin for longer FETCH lines", enumerateBatch, size, int64(maxResponseBytes))
	}
}

// TestIMAPLargeBodyWithoutLimitIsNotCapped pins what the new bounds deliberately
// do not count: literal *bytes*. On an account with no configured size limit a
// single body larger than the response-size bound must still arrive whole —
// bounding literal bytes here would break exactly the accounts that turned the
// limit off on purpose.
func TestIMAPLargeBodyWithoutLimitIsNotCapped(t *testing.T) {
	const bodySize = 12 << 20 // past maxResponseBytes
	sess := dialMax(t, refusedLiteralServer(t, bodySize), 0)
	defer sess.Close()

	rc, err := sess.Retrieve(context.Background(), 1)
	if err != nil {
		t.Fatalf("fetch a %d-byte body on an account with no size limit: %v", bodySize, err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(got) != bodySize {
		t.Fatalf("body = %d bytes, want %d", len(got), bodySize)
	}
	if !strings.HasPrefix(string(got), trapHeader) {
		t.Fatalf("body prefix = %.30q, want the trapped header intact", got)
	}
}

func dial(t *testing.T, addr string) domain.InboundSession {
	t.Helper()
	return dialMax(t, addr, 0)
}

func dialMax(t *testing.T, addr string, maxBytes int64) domain.InboundSession {
	t.Helper()
	host, portStr, _ := net.SplitHostPort(addr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	sess, err := Dialer{}.Dial(context.Background(), domain.InboundDialOptions{
		Host: host, Port: port, Security: domain.SecurityNone,
		Username: "me", Password: domain.NewSecretHandle([]byte("pw")),
		MaxMessageBytes: maxBytes,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return sess
}

func TestIMAPEnumerateAndFetch(t *testing.T) {
	sess := dial(t, fakeServer(t, false))
	defer sess.Close()
	ctx := context.Background()

	msgs, err := sess.UIDL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("UIDL count = %d, want 2", len(msgs))
	}
	if msgs[0].UIDL != "777.101" || msgs[1].UIDL != "777.102" {
		t.Fatalf("UIDs = %q,%q, want 777.101,777.102", msgs[0].UIDL, msgs[1].UIDL)
	}
	list, err := sess.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Size != 20 || list[1].Size != 40 {
		t.Fatalf("sizes = %d,%d, want 20,40", list[0].Size, list[1].Size)
	}

	// Fetch a body whose content contains {braces} — the literal framing must
	// return it byte-exact, not confuse it with a protocol literal.
	rc, err := sess.Retrieve(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	if string(got) != testMsg {
		t.Fatalf("body = %q, want %q", got, testMsg)
	}

	if err := sess.Delete(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := sess.Quit(ctx); err != nil {
		t.Fatalf("quit: %v", err)
	}
}

func TestIMAPAuthError(t *testing.T) {
	host, portStr, _ := net.SplitHostPort(fakeServer(t, true))
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	_, err := Dialer{}.Dial(context.Background(), domain.InboundDialOptions{
		Host: host, Port: port, Security: domain.SecurityNone,
		Username: "me", Password: domain.NewSecretHandle([]byte("bad")),
	})
	var ae *domain.AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *domain.AuthError, got %T (%v)", err, err)
	}
}
