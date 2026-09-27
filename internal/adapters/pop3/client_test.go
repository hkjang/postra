package pop3

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"postra/internal/domain"
)

// dial connects to a scripted server on 127.0.0.1 with the real Dialer. An
// empty username skips USER/PASS, which keeps the fixtures to the multi-line
// framing these tests are about.
func dial(t *testing.T, addr string) domain.POP3Session {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := Dialer{}.Dial(context.Background(), domain.POP3DialOptions{
		Host: host, Port: port, Security: domain.SecurityNone,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return sess
}

// maildropServer answers UIDL and LIST with scripted, properly "."-terminated
// multi-line responses. Anything else gets a bare +OK.
func maildropServer(t *testing.T, uidl, list string) string {
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
		io.WriteString(conn, "+OK POP3 ready\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			switch strings.ToUpper(fields[0]) {
			case "UIDL":
				io.WriteString(conn, "+OK\r\n"+uidl+".\r\n")
			case "LIST":
				io.WriteString(conn, "+OK\r\n"+list+".\r\n")
			case "QUIT":
				io.WriteString(conn, "+OK bye\r\n")
				return
			default:
				io.WriteString(conn, "+OK\r\n")
			}
		}
	}()
	return ln.Addr().String()
}

// endlessListServer accepts a command, answers +OK, and then repeats one line
// for as long as the client keeps reading — it never sends the "." that ends a
// multi-line response.
func endlessListServer(t *testing.T, repeated string) string {
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
	payload := batch.String()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		io.WriteString(conn, "+OK POP3 ready\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if len(strings.Fields(line)) == 0 {
				continue
			}
			io.WriteString(conn, "+OK\r\n")
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := io.WriteString(conn, payload); err != nil {
					return
				}
			}
		}
	}()
	return ln.Addr().String()
}

// TestPOP3EndlessListResponseDiscardsSession pins the bound readList needs.
// readList collects lines until the server sends "." and refreshes the command
// deadline on every line, so a server that dribbles short lines for ever is
// never stopped by the 60-second command timeout — the enumeration step hangs
// and the collected lines grow without limit.
func TestPOP3EndlessListResponseDiscardsSession(t *testing.T) {
	sess := dial(t, endlessListServer(t, "1 0123456789abcdef0123\r\n"))
	defer sess.Close()
	ctx := context.Background()

	started := time.Now()
	_, err := sess.UIDL(ctx)
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("UIDL over an unterminated multi-line response returned no error")
	}
	if !strings.Contains(err.Error(), "multi-line response exceeds") {
		t.Fatalf("UIDL error = %v, want it to name the multi-line response bound", err)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("UIDL took %v to give up; want the bound to stop it well inside the 60s command timeout", elapsed)
	}

	// The rest of the response is still on the wire, so the session cannot be
	// reused: both remaining commands must fail rather than read those bytes as
	// protocol.
	if _, err := sess.List(ctx); err == nil {
		t.Fatal("List on the discarded session succeeded; want an error")
	}
	if _, err := sess.Retrieve(ctx, 1); err == nil {
		t.Fatal("Retrieve on the discarded session succeeded; want an error")
	}
}

// TestPOP3EndlessBlankLinesDiscardSession is the same attack with the cheapest
// line there is. textproto.ReadLine strips the CRLF, so a bare "\r\n" comes back
// as zero bytes: an accounting that charges only len(line) never advances at all
// and readList appends "" for ever while the deadline keeps being pushed out.
// Charging every line the terminator it really cost plus the header it occupies
// is what makes the bound reachable here — the IMAP adapter took a review for
// exactly this bypass.
func TestPOP3EndlessBlankLinesDiscardSession(t *testing.T) {
	sess := dial(t, endlessListServer(t, "\r\n"))
	defer sess.Close()
	ctx := context.Background()

	started := time.Now()
	_, err := sess.UIDL(ctx)
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("UIDL over an endless run of blank lines returned no error")
	}
	if !strings.Contains(err.Error(), "multi-line response exceeds") {
		t.Fatalf("UIDL error = %v, want it to name the multi-line response bound", err)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("UIDL took %v to give up; want the bound to stop it well inside the 60s command timeout", elapsed)
	}
}

// TestPOP3FullMaildropStaysUnderListBound is the regression guard on the bound's
// size. A large maildrop is enumerated in one multi-line response, so the bound
// has to clear the biggest legitimate one by a wide margin. It measures what a
// full-length UIDL response actually costs against the bound and fails if the
// margin closes — and proves the normal path still parses byte-for-byte.
func TestPOP3FullMaildropStaysUnderListBound(t *testing.T) {
	const count = 5000
	const uidPrefix = "uuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuu" // 65 chars; + %05d = the RFC 1939 maximum of 70
	var uidl, list strings.Builder
	for i := 1; i <= count; i++ {
		fmt.Fprintf(&uidl, "%d %s%05d\r\n", i, uidPrefix, i)
		fmt.Fprintf(&list, "%d %d\r\n", i, 2048+i)
	}
	wire := uidl.String()

	sess := dial(t, maildropServer(t, wire, list.String()))
	defer sess.Close()
	ctx := context.Background()

	msgs, err := sess.UIDL(ctx)
	if err != nil {
		t.Fatalf("enumerate a %d-message maildrop (%d bytes of UIDL response): %v", count, len(wire), err)
	}
	if len(msgs) != count {
		t.Fatalf("UIDL returned %d messages, want %d", len(msgs), count)
	}
	for i, m := range msgs {
		wantN := i + 1
		wantUID := fmt.Sprintf("%s%05d", uidPrefix, wantN)
		if m.Number != wantN || m.UIDL != wantUID {
			t.Fatalf("UIDL entry %d = {Number:%d UIDL:%q}, want {Number:%d UIDL:%q}", i, m.Number, m.UIDL, wantN, wantUID)
		}
	}
	sizes, err := sess.List(ctx)
	if err != nil {
		t.Fatalf("list a %d-message maildrop: %v", count, err)
	}
	if len(sizes) != count {
		t.Fatalf("LIST returned %d messages, want %d", len(sizes), count)
	}
	for i, m := range sizes {
		if m.Number != i+1 || m.Size != int64(2048+i+1) {
			t.Fatalf("LIST entry %d = {Number:%d Size:%d}, want {Number:%d Size:%d}", i, m.Number, m.Size, i+1, 2048+i+1)
		}
	}

	// What that response cost the bound: the CRLF is stripped before the line is
	// charged and re-charged as part of listLineOverhead.
	charged := int64(len(wire)-2*count) + int64(count)*listLineOverhead
	perMessage := charged / count
	t.Logf("a %d-message UIDL response is %d bytes on the wire, charged %d against the %d-byte bound (%d per message, so the bound admits %d messages)",
		count, len(wire), charged, int64(maxListBytes), perMessage, int64(maxListBytes)/perMessage)
	if charged*8 > maxListBytes {
		t.Fatalf("a %d-message enumeration is charged %d bytes, within 8x of the %d-byte bound; the bound leaves no margin", count, charged, int64(maxListBytes))
	}
	// The real sizing constraint is maildrop scale, not this fixture: a bound
	// that cannot hold a few hundred thousand maximum-length UIDs would break
	// real maildrops rather than runaway servers.
	const wantMessages = 300000
	if perMessage*wantMessages > maxListBytes {
		t.Fatalf("at %d charged bytes per message the %d-byte bound admits only %d messages, fewer than the %d a large maildrop holds",
			perMessage, int64(maxListBytes), int64(maxListBytes)/perMessage, wantMessages)
	}
}
