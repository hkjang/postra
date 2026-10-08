package imap

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// endlessUntaggedServer answers every command after login by repeating one
// untagged line for ever and never sending the tagged completion. exec's
// collection loop has no reason of its own to stop there: each line is well
// inside the line-length bound, and readLine refreshes the command deadline on
// every one of them. The writes are throttled so the pre-bound behaviour
// (accumulate until the test binary's own timeout) is observable without
// exhausting the test machine.
func endlessUntaggedServer(t *testing.T, repeated string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop); ln.Close() })
	var batch strings.Builder
	for batch.Len() < 4096 {
		batch.WriteString(repeated)
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
	sess := dialMax(t, endlessUntaggedServer(t, "* 1 FETCH (UID 1 RFC822.SIZE 20)\r\n"), 50<<20)
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

// TestIMAPEndlessBlankLinesAbandonSession is the same attack with the cheapest
// line there is. readBoundedLine returns a line with its CRLF already stripped,
// so a bare "\r\n" measures zero bytes: an accounting that charges only
// len(line) never advances at all against a server that sends nothing else, and
// exec appends "" to untagged for ever while readLine keeps pushing the command
// deadline out. Charging every line the terminator and the header it costs in
// untagged is what makes the bound reachable here.
func TestIMAPEndlessBlankLinesAbandonSession(t *testing.T) {
	sess := dialMax(t, endlessUntaggedServer(t, "\r\n"), 50<<20)
	defer sess.Close()
	ctx := context.Background()

	started := time.Now()
	_, err := sess.UIDL(ctx)
	elapsed := time.Since(started)

	if !errors.Is(err, errUnframed) {
		t.Fatalf("enumerate over an endless run of blank lines = %v, want an error wrapping errUnframed", err)
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
// mailbox instead of on a runaway server. The margin is measured on what exec
// actually charges — the wire text plus responseLineOverhead per line — so
// raising the overhead cannot quietly eat the headroom.
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
	charged := int64(size) + int64(enumerateBatch)*responseLineOverhead
	t.Logf("a full %d-message enumeration is %d bytes of protocol text, charged %d (bound %d)",
		enumerateBatch, size, charged, int64(maxResponseBytes))
	if charged*8 > maxResponseBytes {
		t.Fatalf("a full %d-message enumeration is charged %d bytes, within 8x of the %d-byte bound; the bound leaves no margin for longer FETCH lines", enumerateBatch, charged, int64(maxResponseBytes))
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

// indexRetryServer respects each FETCH range and rejects a chosen batch with a
// tagged NO, leaving the connection framed for another enumeration attempt.
// A negative failures count rejects that batch on every attempt.
func indexRetryServer(t *testing.T, count, rejectStart, failures int) (string, func() []string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var requests []string
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = ln.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("index server did not stop after session close")
		}
	})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		br := bufio.NewReader(conn)
		_, _ = io.WriteString(conn, "* OK IMAP4rev1 ready\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			sp := strings.SplitN(strings.TrimSpace(line), " ", 3)
			if len(sp) < 2 {
				return
			}
			tag, cmd := sp[0], sp[1]
			switch cmd {
			case "SELECT":
				fmt.Fprintf(conn, "* %d EXISTS\r\n* OK [UIDVALIDITY 777] ok\r\n%s OK\r\n", count, tag)
			case "FETCH":
				if len(sp) != 3 {
					return
				}
				mu.Lock()
				requests = append(requests, sp[2])
				mu.Unlock()
				var start, end int
				if n, err := fmt.Sscanf(sp[2], "%d:%d (UID RFC822.SIZE)", &start, &end); err != nil || n != 2 || start < 1 || end < start || end > count {
					fmt.Fprintf(conn, "%s BAD invalid metadata range\r\n", tag)
					continue
				}
				if start == rejectStart && failures != 0 {
					if failures > 0 {
						failures--
					}
					fmt.Fprintf(conn, "%s NO metadata refused\r\n", tag)
					continue
				}
				for i := start; i <= end; i++ {
					fmt.Fprintf(conn, "* %d FETCH (UID %d RFC822.SIZE %d)\r\n", i, 100+i, 2048+i)
				}
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			default:
				fmt.Fprintf(conn, "%s OK\r\n", tag)
			}
		}
	}()
	return ln.Addr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(requests)
	}
}

func TestIMAPEnumerationFailureNeverCachesPartialResults(t *testing.T) {
	for _, tc := range []struct {
		name        string
		count       int
		rejectStart int
		wantAttempt []string
	}{
		{"first_batch", 1, 1, []string{"1:1 (UID RFC822.SIZE)"}},
		{"later_batch", 2001, 2001, []string{"1:2000 (UID RFC822.SIZE)", "2001:2001 (UID RFC822.SIZE)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, requests := indexRetryServer(t, tc.count, tc.rejectStart, -1)
			sess := dial(t, addr)
			defer sess.Close()
			var want []string
			for i, enumerate := range []func(context.Context) ([]domain.RemoteMessage, error){sess.UIDL, sess.List, sess.UIDL} {
				msgs, err := enumerate(context.Background())
				if err == nil || len(msgs) != 0 {
					t.Fatalf("attempt %d: messages=%d err=%v, want error and no partial results", i+1, len(msgs), err)
				}
				var inbound *domain.InboundError
				if !errors.As(err, &inbound) || inbound.Stage != domain.StageEnumerate || inbound.Class != "rejected" {
					t.Fatalf("attempt %d: error = %v, want enumerate rejection", i+1, err)
				}
				want = append(want, tc.wantAttempt...)
				if got := requests(); !slices.Equal(got, want) {
					t.Fatalf("FETCH requests = %v, want %v", got, want)
				}
			}
		})
	}
}

func TestIMAPEnumerationRetryCommitsCompleteCache(t *testing.T) {
	addr, requests := indexRetryServer(t, 2001, 2001, 1)
	sess := dial(t, addr)
	defer sess.Close()
	if msgs, err := sess.UIDL(context.Background()); err == nil || len(msgs) != 0 {
		t.Fatalf("first UIDL: messages=%d err=%v, want error and no partial results", len(msgs), err)
	}
	for i, enumerate := range []func(context.Context) ([]domain.RemoteMessage, error){sess.List, sess.UIDL, sess.List} {
		msgs, err := enumerate(context.Background())
		if err != nil || len(msgs) != 2001 {
			t.Fatalf("call %d after failure: messages=%d err=%v, want 2001 complete messages", i+1, len(msgs), err)
		}
		for j, msg := range msgs {
			n := j + 1
			want := domain.RemoteMessage{Number: n, UIDL: fmt.Sprintf("777.%d", 100+n), Size: int64(2048 + n)}
			if msg != want {
				t.Fatalf("message %d = %+v, want %+v", n, msg, want)
			}
		}
		want := []string{"1:2000 (UID RFC822.SIZE)", "2001:2001 (UID RFC822.SIZE)", "1:2000 (UID RFC822.SIZE)", "2001:2001 (UID RFC822.SIZE)"}
		if got := requests(); !slices.Equal(got, want) {
			t.Fatalf("FETCH requests = %v, want %v (successful cache must not fetch again)", got, want)
		}
	}
}

func TestIMAPEmptyEnumerationNeedsNoFetch(t *testing.T) {
	addr, requests := indexRetryServer(t, 0, 0, 0)
	sess := dial(t, addr)
	defer sess.Close()
	for _, enumerate := range []func(context.Context) ([]domain.RemoteMessage, error){sess.UIDL, sess.List, sess.UIDL} {
		if msgs, err := enumerate(context.Background()); err != nil || len(msgs) != 0 {
			t.Fatalf("empty mailbox: messages=%d err=%v", len(msgs), err)
		}
	}
	if got := requests(); len(got) != 0 {
		t.Fatalf("empty mailbox fetched metadata: %v", got)
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

// selfSigned builds a throwaway server certificate for the STARTTLS fixture.
// Ported from internal/adapters/smtp/client_test.go, which upgrades the same way.
func selfSigned(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
}

// starttlsServer answers STARTTLS and upgrades, then serves LOGIN, SELECT and
// FETCH over TLS from the same loop. inject is appended to the tagged STARTTLS
// completion in the same write, so the injected plaintext reaches the client
// together with the response it is already reading — the shape of a STARTTLS
// command-injection attempt. The returned channel is closed when the server's
// connection handler returns, which is how the tests observe the client hanging
// up.
func starttlsServer(t *testing.T, inject string) (string, <-chan struct{}) {
	t.Helper()
	tlsCfg := selfSigned(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(20 * time.Second))
		br := bufio.NewReader(conn)
		var w io.Writer = conn
		io.WriteString(w, "* OK IMAP4rev1 ready\r\n")
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
			case "STARTTLS":
				if _, err := fmt.Fprintf(w, "%s OK Begin TLS negotiation now\r\n%s", tag, inject); err != nil {
					return
				}
				tconn := tls.Server(conn, tlsCfg)
				if err := tconn.Handshake(); err != nil {
					return
				}
				br = bufio.NewReader(tconn)
				w = tconn
			case "LOGIN":
				fmt.Fprintf(w, "%s OK LOGIN completed\r\n", tag)
			case "SELECT":
				io.WriteString(w, "* 2 EXISTS\r\n")
				io.WriteString(w, "* OK [UIDVALIDITY 777] ok\r\n")
				fmt.Fprintf(w, "%s OK [READ-WRITE] SELECT completed\r\n", tag)
			case "FETCH":
				io.WriteString(w, "* 1 FETCH (UID 101 RFC822.SIZE 20)\r\n")
				io.WriteString(w, "* 2 FETCH (UID 102 RFC822.SIZE 40)\r\n")
				fmt.Fprintf(w, "%s OK FETCH completed\r\n", tag)
			case "LOGOUT":
				io.WriteString(w, "* BYE\r\n")
				fmt.Fprintf(w, "%s OK LOGOUT completed\r\n", tag)
				return
			default:
				fmt.Fprintf(w, "%s OK\r\n", tag)
			}
		}
	}()
	return ln.Addr().String(), done
}

// dialStartTLS drives the real Dialer through a STARTTLS upgrade. The fixture's
// certificate is self-signed, so verification is skipped here and only here —
// the production default stays false.
func dialStartTLS(t *testing.T, addr string) (domain.InboundSession, error) {
	t.Helper()
	host, portStr, _ := net.SplitHostPort(addr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	return Dialer{}.Dial(context.Background(), domain.InboundDialOptions{
		Host: host, Port: port, Security: domain.SecurityStartTLS, InsecureSkipVerify: true,
		Username: "me", Password: domain.NewSecretHandle([]byte("pw")),
	})
}

// TestIMAPStartTLSInjectedPlaintextRefusesUpgrade is the POP3 case on the other
// inbound adapter, and both must answer the same input the same way. TLS is
// client-speaks-first, so a server with anything left to say after its tagged
// STARTTLS completion is injecting plaintext. Without the check the upgrade
// replaces the reader and those bytes are dropped silently, so Dial succeeds and
// nothing records that the server broke the protocol.
func TestIMAPStartTLSInjectedPlaintextRefusesUpgrade(t *testing.T) {
	addr, done := starttlsServer(t, "* OK injected\r\n")

	sess, err := dialStartTLS(t, addr)
	if err == nil {
		sess.Close()
		t.Fatal("Dial accepted a server that injected plaintext before the STARTTLS handshake")
	}
	if !strings.Contains(err.Error(), "before TLS handshake") {
		t.Fatalf("Dial error = %v, want it to name the pre-handshake bytes", err)
	}

	// Same contract as the POP3 adapter: the refusal has to name its own stage,
	// or syncDiagnosticStage's dial fallback reports it as a failed TCP connect.
	var inbound *domain.InboundError
	if !errors.As(err, &inbound) {
		t.Fatalf("Dial error = %T (%v), want a *domain.InboundError naming the stage", err, err)
	}
	if inbound.Stage != domain.StageStartTLS {
		t.Fatalf("Stage = %q, want %q", inbound.Stage, domain.StageStartTLS)
	}
	if inbound.Command != "STARTTLS" {
		t.Fatalf("Command = %q, want the verb actually sent, %q", inbound.Command, "STARTTLS")
	}
	// No new class label: "other" is already in syncClassLabels.
	if want := domain.ClassifyInbound(inbound.Err); inbound.Class != want || want != "other" {
		t.Fatalf("Class = %q, want the ClassifyInbound value %q (expected %q)", inbound.Class, want, "other")
	}
	if inbound.Elapsed <= 0 {
		t.Fatalf("Elapsed = %v, want the time actually spent on the upgrade", inbound.Elapsed)
	}
	if want := 60 * time.Second; inbound.Timeout != want {
		t.Fatalf("Timeout = %v, want the command budget %v", inbound.Timeout, want)
	}
	// Diagnostics carry the byte count, never the bytes.
	if strings.Contains(err.Error(), "injected") {
		t.Fatalf("Dial error = %q, want the count only and no server text", err)
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("server connection still open after the refused upgrade; want the client to have closed it")
	}
}

// TestIMAPStartTLSUpgradeRunsCommandsOverTLS is the false-positive guard: a
// refusal on a well-behaved server would stop mail collection outright. A clean
// upgrade must still log in, select INBOX and enumerate over the encrypted
// connection.
func TestIMAPStartTLSUpgradeRunsCommandsOverTLS(t *testing.T) {
	addr, _ := starttlsServer(t, "")

	sess, err := dialStartTLS(t, addr)
	if err != nil {
		t.Fatalf("Dial over a clean STARTTLS upgrade: %v", err)
	}
	defer sess.Close()

	msgs, err := sess.UIDL(context.Background())
	if err != nil {
		t.Fatalf("enumerate after the upgrade: %v", err)
	}
	if len(msgs) != 2 || msgs[0].UIDL != "777.101" || msgs[1].UIDL != "777.102" {
		t.Fatalf("UIDs = %+v, want 777.101 and 777.102", msgs)
	}
	if _, ok := sess.(*session).conn.(*tls.Conn); !ok {
		t.Fatalf("session connection is %T after STARTTLS, want *tls.Conn", sess.(*session).conn)
	}
}

// implicitTLSServer listens with TLS already in force — the 993 shape, which is
// the inbound default a new account gets (normSecurity in
// internal/application/accounts.go). It reuses the self-signed certificate of
// the STARTTLS fixture, so a client that verifies the chain must refuse it.
// Connections are accepted in a loop because both halves of
// TestIMAPImplicitTLSVerifiesServerCertificate dial the same listener, and the
// certificate has to be the same one for the comparison to mean anything.
//
// An empty username skips LOGIN (client.go only sends it when
// opts.Username != ""), but selectInbox() runs unconditionally, so the loop
// still has to answer SELECT for the session to establish.
func implicitTLSServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tln := tls.NewListener(ln, selfSigned(t))
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := tln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(20 * time.Second))
				// Writing the greeting is what drives the server side of the
				// handshake, so a client that rejects the certificate makes
				// this fail and the handler simply returns.
				if _, err := io.WriteString(conn, "* OK IMAP4rev1 ready\r\n"); err != nil {
					return
				}
				br := bufio.NewReader(conn)
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
						io.WriteString(conn, "* 0 EXISTS\r\n")
						io.WriteString(conn, "* OK [UIDVALIDITY 911] ok\r\n")
						fmt.Fprintf(conn, "%s OK [READ-WRITE] SELECT completed\r\n", tag)
					case "LOGOUT":
						io.WriteString(conn, "* BYE\r\n")
						fmt.Fprintf(conn, "%s OK LOGOUT completed\r\n", tag)
						return
					default:
						fmt.Fprintf(conn, "%s OK\r\n", tag)
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// dialImplicitTLS drives the real Dialer at an implicit-TLS server. skipVerify
// is passed through verbatim so the two halves of the test differ in that one
// field and nothing else.
func dialImplicitTLS(t *testing.T, addr string, skipVerify bool) (domain.InboundSession, error) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return Dialer{}.Dial(context.Background(), domain.InboundDialOptions{
		Host: host, Port: port, Security: domain.SecurityTLS, InsecureSkipVerify: skipVerify,
	})
}

// TestIMAPImplicitTLSVerifiesServerCertificate is the POP3 case on the other
// inbound adapter, and both must answer the same input the same way. Implicit
// TLS is the security a new account defaults to, so this is the dial most
// installations make, and nothing here tested that its certificate is verified
// at all — every other TLS test in this file asks for SecurityStartTLS with
// InsecureSkipVerify: true. Without this, tlsCfg's InsecureSkipVerify
// (client.go:54) could be pinned to a constant true and no test in the package
// would notice. MinVersion is deliberately not pinned here: Go's own client
// default is already TLS 1.2, so dropping that line would not fail this test.
//
// It also pins the stage connectFailure reports: with implicit TLS the dialer
// runs TCP and the handshake in one call, so only the error type separates
// them, and mapped to tcp_connect the operator is told the connection failed
// rather than that the certificate was untrusted.
func TestIMAPImplicitTLSVerifiesServerCertificate(t *testing.T) {
	addr := implicitTLSServer(t)

	t.Run("rejected by default", func(t *testing.T) {
		// Security is the only field set besides the address: InsecureSkipVerify
		// is left at its zero value, which is the production default.
		sess, err := dialImplicitTLS(t, addr, false)
		if err == nil {
			sess.Close()
			t.Fatal("Dial accepted a self-signed certificate with the default options; want verification")
		}
		var inbound *domain.InboundError
		if !errors.As(err, &inbound) {
			t.Fatalf("Dial error = %T (%v), want a *domain.InboundError naming the stage", err, err)
		}
		if inbound.Stage != domain.StageTLS {
			t.Fatalf("Stage = %q, want %q — tcp_connect would tell the operator the connection failed", inbound.Stage, domain.StageTLS)
		}
		if inbound.Class != "tls_certificate" {
			t.Fatalf("Class = %q, want %q (the label syncClassLabels renders as the untrusted-certificate sentence)", inbound.Class, "tls_certificate")
		}
		if inbound.Elapsed <= 0 {
			t.Fatalf("Elapsed = %v, want the time actually spent dialing", inbound.Elapsed)
		}
		// The message is the adapter's own prefix plus Go's x509 text. The
		// server's own bytes must not be in it: diagnostics are persisted and
		// shown, and a mail server can echo anything in its greeting.
		if !strings.Contains(err.Error(), "imap connect:") {
			t.Fatalf("Dial error = %q, want the adapter's own prefix", err)
		}
		if !strings.Contains(err.Error(), "x509:") {
			t.Fatalf("Dial error = %q, want Go's certificate-verification text", err)
		}
		if strings.Contains(err.Error(), "IMAP4rev1 ready") {
			t.Fatalf("Dial error = %q, want no text the mail server sent", err)
		}
	})

	t.Run("accepted with the account opt-in", func(t *testing.T) {
		// Same listener, same certificate, one field different. Without this
		// half the sub-test above would also pass against a server that is
		// simply unreachable, and the default would not be pinned at all.
		sess, err := dialImplicitTLS(t, addr, true)
		if err != nil {
			t.Fatalf("Dial with InsecureSkipVerify: true = %v, want the session to establish", err)
		}
		defer sess.Close()
		if _, ok := sess.(*session).conn.(*tls.Conn); !ok {
			t.Fatalf("session connection is %T, want *tls.Conn on the implicit-TLS path", sess.(*session).conn)
		}
	})
}
