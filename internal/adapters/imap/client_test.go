package imap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"postra/internal/domain"
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

func dial(t *testing.T, addr string) domain.InboundSession {
	t.Helper()
	host, portStr, _ := net.SplitHostPort(addr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	sess, err := Dialer{}.Dial(context.Background(), domain.InboundDialOptions{
		Host: host, Port: port, Security: domain.SecurityNone,
		Username: "me", Password: domain.NewSecretHandle([]byte("pw")),
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
