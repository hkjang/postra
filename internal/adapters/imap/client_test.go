package imap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"testing"

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
