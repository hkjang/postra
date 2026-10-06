package pop3

import (
	"bufio"
	"bytes"
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
	"math/big"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
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

// selfSigned builds a throwaway server certificate for the STLS fixtures. Ported
// from internal/adapters/smtp/client_test.go, which upgrades the same way.
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

// stlsServer answers STLS and upgrades, then serves UIDL and LIST over TLS from
// the same loop. inject is appended to the STLS "+OK" in the same write, so the
// injected plaintext reaches the client together with the response it is
// already reading — the shape of a STARTTLS command-injection attempt. The
// returned channel is closed when the server's connection handler returns,
// which is how the tests observe the client hanging up.
func stlsServer(t *testing.T, inject, uidl, list string) (string, <-chan struct{}) {
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
		io.WriteString(w, "+OK POP3 ready\r\n")
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
			case "STLS":
				if _, err := io.WriteString(w, "+OK begin TLS negotiation\r\n"+inject); err != nil {
					return
				}
				tconn := tls.Server(conn, tlsCfg)
				if err := tconn.Handshake(); err != nil {
					return
				}
				br = bufio.NewReader(tconn)
				w = tconn
			case "UIDL":
				io.WriteString(w, "+OK\r\n"+uidl+".\r\n")
			case "LIST":
				io.WriteString(w, "+OK\r\n"+list+".\r\n")
			case "QUIT":
				io.WriteString(w, "+OK bye\r\n")
				return
			default:
				io.WriteString(w, "+OK\r\n")
			}
		}
	}()
	return ln.Addr().String(), done
}

// dialStartTLS drives the real Dialer through an STLS upgrade. The fixture's
// certificate is self-signed, so verification is skipped here and only here —
// the production default stays false.
func dialStartTLS(t *testing.T, addr string) (domain.POP3Session, error) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return Dialer{}.Dial(context.Background(), domain.POP3DialOptions{
		Host: host, Port: port, Security: domain.SecurityStartTLS, InsecureSkipVerify: true,
	})
}

// TestPOP3STLSInjectedPlaintextRefusesUpgrade pins the pre-handshake check. TLS
// is client-speaks-first, so a server that has anything left to say after its
// STLS "+OK" is injecting plaintext into the session. Without the check the
// upgrade replaces the reader and the injected bytes are dropped silently, so
// Dial succeeds and nothing records that the server broke the protocol.
func TestPOP3STLSInjectedPlaintextRefusesUpgrade(t *testing.T) {
	addr, done := stlsServer(t, "+OK injected\r\n", "1 abc\r\n", "1 10\r\n")

	sess, err := dialStartTLS(t, addr)
	if err == nil {
		sess.Close()
		t.Fatal("Dial accepted a server that injected plaintext before the STLS handshake")
	}
	if !strings.Contains(err.Error(), "before TLS handshake") {
		t.Fatalf("Dial error = %v, want it to name the pre-handshake bytes", err)
	}

	// The refusal has to reach the operator as the step it happened at. As a
	// plain error it misses syncDiagnosticStage's errors.As and falls through to
	// the dial fallback, so the adapter's most security-relevant refusal is
	// reported as a failed TCP connection that took no time at all.
	var inbound *domain.InboundError
	if !errors.As(err, &inbound) {
		t.Fatalf("Dial error = %T (%v), want a *domain.InboundError naming the stage", err, err)
	}
	if inbound.Stage != domain.StageStartTLS {
		t.Fatalf("Stage = %q, want %q", inbound.Stage, domain.StageStartTLS)
	}
	if inbound.Command != "STLS" {
		t.Fatalf("Command = %q, want the verb actually sent, %q", inbound.Command, "STLS")
	}
	// No new class label: "other" is already in syncClassLabels, so the
	// diagnostic's allowed() keeps it instead of dropping the refusal.
	if want := domain.ClassifyInbound(inbound.Err); inbound.Class != want || want != "other" {
		t.Fatalf("Class = %q, want the ClassifyInbound value %q (expected %q)", inbound.Class, want, "other")
	}
	if inbound.Elapsed <= 0 {
		t.Fatalf("Elapsed = %v, want the time actually spent on the upgrade", inbound.Elapsed)
	}
	if want := 60 * time.Second; inbound.Timeout != want {
		t.Fatalf("Timeout = %v, want the command budget %v", inbound.Timeout, want)
	}
	// Diagnostics carry the byte count, never the bytes: the injected text is
	// exactly what must not reach an operator-facing string.
	if strings.Contains(err.Error(), "injected") {
		t.Fatalf("Dial error = %q, want the count only and no server text", err)
	}

	// The connection must be gone, not left dangling in plaintext.
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("server connection still open after the refused upgrade; want the client to have closed it")
	}
}

// TestPOP3STLSUpgradeRunsCommandsOverTLS is the false-positive guard: a refusal
// on a well-behaved server would stop mail collection outright. A clean upgrade
// must still authenticate nothing, negotiate TLS, and answer UIDL and LIST on
// the encrypted connection.
func TestPOP3STLSUpgradeRunsCommandsOverTLS(t *testing.T) {
	addr, _ := stlsServer(t, "", "1 uid-one\r\n2 uid-two\r\n", "1 2048\r\n2 4096\r\n")

	sess, err := dialStartTLS(t, addr)
	if err != nil {
		t.Fatalf("Dial over a clean STLS upgrade: %v", err)
	}
	defer sess.Close()
	ctx := context.Background()

	msgs, err := sess.UIDL(ctx)
	if err != nil {
		t.Fatalf("UIDL after the upgrade: %v", err)
	}
	if len(msgs) != 2 || msgs[0].UIDL != "uid-one" || msgs[1].UIDL != "uid-two" {
		t.Fatalf("UIDL = %+v, want uid-one and uid-two", msgs)
	}
	sizes, err := sess.List(ctx)
	if err != nil {
		t.Fatalf("LIST after the upgrade: %v", err)
	}
	if len(sizes) != 2 || sizes[0].Size != 2048 || sizes[1].Size != 4096 {
		t.Fatalf("LIST = %+v, want sizes 2048 and 4096", sizes)
	}
	if _, ok := sess.(*session).conn.(*tls.Conn); !ok {
		t.Fatalf("session connection is %T after STLS, want *tls.Conn", sess.(*session).conn)
	}
}

// implicitTLSServer listens with TLS already in force — the 995 shape, which is
// what a new account gets by default (normSecurity(in.POP3Security,
// domain.SecurityTLS) in internal/application/accounts.go). It reuses the
// self-signed certificate of the STLS fixtures, so a client that verifies the
// chain must refuse it. Connections are accepted in a loop because both halves
// of TestPOP3ImplicitTLSVerifiesServerCertificate dial the same listener, and
// the certificate has to be the same one for the comparison to mean anything.
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
				if _, err := io.WriteString(conn, "+OK POP3 ready\r\n"); err != nil {
					return
				}
				br := bufio.NewReader(conn)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					fields := strings.Fields(line)
					if len(fields) == 0 {
						continue
					}
					if strings.ToUpper(fields[0]) == "QUIT" {
						io.WriteString(conn, "+OK bye\r\n")
						return
					}
					io.WriteString(conn, "+OK\r\n")
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// dialImplicitTLS drives the real Dialer at an implicit-TLS server. skipVerify
// is passed through verbatim so the two halves of the test differ in that one
// field and nothing else.
func dialImplicitTLS(t *testing.T, addr string, skipVerify bool) (domain.POP3Session, error) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return Dialer{}.Dial(context.Background(), domain.POP3DialOptions{
		Host: host, Port: port, Security: domain.SecurityTLS, InsecureSkipVerify: skipVerify,
	})
}

// TestPOP3ImplicitTLSVerifiesServerCertificate pins the default on the path a
// new account actually takes: POP3 security defaults to implicit TLS, so this
// is the dial most installations make, and nothing here tested that its
// certificate is verified at all — every other TLS test in this file asks for
// SecurityStartTLS with InsecureSkipVerify: true. Without this, tlsCfg's
// InsecureSkipVerify (client.go:69) could be pinned to a constant true and no
// test in the package would notice. The outbound adapter already carries the
// same guard (smtp/client_test.go, "self-signed cert is rejected without
// InsecureSkipVerify"). tlsCfg's MinVersion is deliberately not pinned here:
// Go's own client default is already TLS 1.2, so dropping that line would not
// fail this test and claiming otherwise would be false.
//
// It also pins the stage connectFailure reports. With implicit TLS the dialer
// runs TCP and the handshake in one call, so only the error type separates
// them; mapped to tcp_connect instead of tls_handshake the operator is told the
// connection failed rather than that the certificate was untrusted, which is
// the sentence sync_diagnostics.go has for tls_certificate.
func TestPOP3ImplicitTLSVerifiesServerCertificate(t *testing.T) {
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
		if !strings.Contains(err.Error(), "pop3 connect:") {
			t.Fatalf("Dial error = %q, want the adapter's own prefix", err)
		}
		if !strings.Contains(err.Error(), "x509:") {
			t.Fatalf("Dial error = %q, want Go's certificate-verification text", err)
		}
		if strings.Contains(err.Error(), "POP3 ready") {
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

// Each exchange is an exact command and its wire response. The final read is
// reported separately: only an actual EOF with no bytes proves a peer hangup.
type pop3Exchange struct {
	command, response string
}

type pop3ScriptResult struct {
	commands []string
	err      error
}

func pop3ScriptServer(t *testing.T, greeting string, exchanges []pop3Exchange, wantEOF bool) (string, <-chan pop3ScriptResult) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan pop3ScriptResult, 1)
	finished := make(chan struct{})
	var mu sync.Mutex
	var accepted net.Conn
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		if accepted != nil {
			accepted.Close()
		}
		mu.Unlock()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("script server did not stop")
		}
	})
	go func() {
		defer close(finished)
		var result pop3ScriptResult
		defer func() { results <- result }()
		conn, err := ln.Accept()
		if err != nil {
			result.err = err
			return
		}
		defer conn.Close()
		mu.Lock()
		accepted = conn
		mu.Unlock()
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			result.err = err
			return
		}
		if _, err := io.WriteString(conn, greeting); err != nil {
			result.err = err
			return
		}
		br := bufio.NewReader(conn)
		for _, exchange := range exchanges {
			line, err := br.ReadString('\n')
			result.commands = append(result.commands, line)
			if err != nil {
				result.err = fmt.Errorf("read command: %w", err)
				return
			}
			if line != exchange.command {
				result.err = fmt.Errorf("command = %q, want %q", line, exchange.command)
				return
			}
			if _, err := io.WriteString(conn, exchange.response); err != nil {
				result.err = err
				return
			}
		}
		if wantEOF {
			line, err := br.ReadString('\n')
			if line != "" || err != io.EOF {
				result.err = fmt.Errorf("after rejection read = (%q, %v), want empty data and EOF", line, err)
			}
		}
	}()
	return ln.Addr().String(), results
}

func checkPOP3Script(t *testing.T, results <-chan pop3ScriptResult, exchanges []pop3Exchange) {
	t.Helper()
	select {
	case result := <-results:
		if result.err != nil {
			t.Fatalf("script server: %v (commands: %q)", result.err, result.commands)
		}
		var want []string
		for _, exchange := range exchanges {
			want = append(want, exchange.command)
		}
		if !reflect.DeepEqual(result.commands, want) {
			t.Fatalf("commands = %q, want %q", result.commands, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for script server result")
	}
}

func TestPOP3DialAuthenticationContracts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		greeting  string
		exchanges []pop3Exchange
		refused   bool
		authError bool
	}{
		{
			name: "USER rejection", greeting: "+OK ready\r\n", refused: true, authError: true,
			exchanges: []pop3Exchange{{"USER test-user\r\n", "-ERR unknown user\r\n"}},
		},
		{
			name: "PASS rejection", greeting: "+OK ready\r\n", refused: true, authError: true,
			exchanges: []pop3Exchange{
				{"USER test-user\r\n", "+OK user accepted\r\n"},
				{"PASS test-password\r\n", "-ERR password rejected\r\n"},
			},
		},
		{name: "greeting rejection", greeting: "-ERR unavailable\r\n", refused: true},
		{
			name: "successful login", greeting: "+OK ready\r\n",
			exchanges: []pop3Exchange{
				{"USER test-user\r\n", "+OK user accepted\r\n"},
				{"PASS test-password\r\n", "+OK authenticated\r\n"},
				{"LIST\r\n", "+OK\r\n1 123\r\n2 456\r\n.\r\n"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, results := pop3ScriptServer(t, tc.greeting, tc.exchanges, tc.refused)
			host, portStr, err := net.SplitHostPort(addr)
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(portStr)
			if err != nil {
				t.Fatal(err)
			}
			password := domain.NewSecretHandle([]byte("test-password"))
			t.Cleanup(password.Zero)
			sess, err := (Dialer{}).Dial(context.Background(), domain.POP3DialOptions{
				Host: host, Port: port, Security: domain.SecurityNone,
				Username: "test-user", Password: password,
				ConnectTimeoutSec: 5, CommandTimeoutSec: 5,
			})
			if sess != nil {
				t.Cleanup(func() { sess.Close() })
			}
			if tc.refused {
				if sess != nil || err == nil {
					t.Fatalf("Dial = (%v, %v), want nil session and error", sess, err)
				}
				var authErr *domain.AuthError
				if got := errors.As(err, &authErr); got != tc.authError {
					t.Fatalf("AuthError classification = %v, want %v (error: %v)", got, tc.authError, err)
				}
				// No client cleanup runs until after this observes the actual EOF.
				checkPOP3Script(t, results, tc.exchanges)
				return
			}
			if err != nil || sess == nil {
				t.Fatalf("Dial = (%v, %v), want authenticated session", sess, err)
			}
			msgs, err := sess.List(context.Background())
			want := []domain.RemoteMessage{{Number: 1, Size: 123}, {Number: 2, Size: 456}}
			if err != nil || !reflect.DeepEqual(msgs, want) {
				t.Fatalf("LIST = (%+v, %v), want %+v", msgs, err, want)
			}
			checkPOP3Script(t, results, tc.exchanges)
		})
	}
}

func TestPOP3BodyDotUnstuffingPreservesFraming(t *testing.T) {
	const wire = "Subject: dot-stuffed message\r\nX-Test: framing\r\n\r\nordinary body\r\n..foo\r\n..\r\n...bar\r\n.\r\n"
	const wantBody = "Subject: dot-stuffed message\r\nX-Test: framing\r\n\r\nordinary body\r\n.foo\r\n.\r\n..bar\r\n"
	for _, tc := range []struct {
		name    string
		command string
		read    func(domain.POP3Session) (io.ReadCloser, error)
	}{
		{
			name: "RETR", command: "RETR 7\r\n",
			read: func(sess domain.POP3Session) (io.ReadCloser, error) {
				return sess.Retrieve(context.Background(), 7)
			},
		},
		{
			name: "TOP", command: "TOP 7 4\r\n",
			read: func(sess domain.POP3Session) (io.ReadCloser, error) {
				return sess.Top(context.Background(), 7, 4)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exchanges := []pop3Exchange{
				{tc.command, "+OK message follows\r\n" + wire},
				{"LIST\r\n", "+OK\r\n7 321\r\n9 654\r\n.\r\n"},
			}
			addr, results := pop3ScriptServer(t, "+OK ready\r\n", exchanges, false)
			sess := dial(t, addr)
			t.Cleanup(func() { sess.Close() })
			body, err := tc.read(sess)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { body.Close() })
			got, err := io.ReadAll(body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, []byte(wantBody)) {
				t.Fatalf("body = %q, want %q", got, wantBody)
			}
			msgs, err := sess.List(context.Background())
			want := []domain.RemoteMessage{{Number: 7, Size: 321}, {Number: 9, Size: 654}}
			if err != nil || !reflect.DeepEqual(msgs, want) {
				t.Fatalf("LIST after body = (%+v, %v), want %+v", msgs, err, want)
			}
			checkPOP3Script(t, results, exchanges)
		})
	}
}

// stalledBodyServer accepts a command, acknowledges it with +OK, and then
// sends neither body nor the terminating ".", so retrBody waits out the whole
// command budget. The accepted command is reported back for the assertion.
func stalledBodyServer(t *testing.T) (string, <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		io.WriteString(conn, "+OK ready\r\n")
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		got <- strings.TrimSpace(line)
		// Acknowledge, then stall: the body never arrives.
		io.WriteString(conn, "+OK message follows\r\n")
		<-t.Context().Done()
	}()
	return ln.Addr().String(), got
}

// dialShortCommandTO uses the real Dialer with a 1s command budget so a
// stalled body fails inside the test timeout instead of the 60s default.
func dialShortCommandTO(t *testing.T, addr string) domain.POP3Session {
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
		CommandTimeoutSec: 1,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

// TestPOP3StalledBodyReportsFetchTiming pins the two facts the fetch-stage
// diagnostic is built on: the elapsed time actually spent waiting, and the
// verb that was actually sent. syncTiming treats ElapsedMS == 0 as "no
// measurement" and prints only the limit, which hides whether the server used
// up its own budget (slow server) or the wait was cut short from outside.
func TestPOP3StalledBodyReportsFetchTiming(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wire    string
		command string
		read    func(domain.POP3Session) (io.ReadCloser, error)
	}{
		{
			name: "RETR", wire: "RETR 7", command: "RETR",
			read: func(sess domain.POP3Session) (io.ReadCloser, error) {
				return sess.Retrieve(context.Background(), 7)
			},
		},
		{
			name: "TOP", wire: "TOP 7 4", command: "TOP",
			read: func(sess domain.POP3Session) (io.ReadCloser, error) {
				return sess.Top(context.Background(), 7, 4)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, sent := stalledBodyServer(t)
			sess := dialShortCommandTO(t, addr)

			start := time.Now()
			body, err := tc.read(sess)
			total := time.Since(start)
			if err == nil {
				body.Close()
				t.Fatalf("%s returned a body from a server that never sent one", tc.name)
			}
			if got := <-sent; got != tc.wire {
				t.Fatalf("server received %q, want %q", got, tc.wire)
			}

			var ie *domain.InboundError
			if !errors.As(err, &ie) {
				t.Fatalf("error %v is not a *domain.InboundError", err)
			}
			if ie.Stage != domain.StageFetch {
				t.Errorf("Stage = %q, want %q", ie.Stage, domain.StageFetch)
			}
			if ie.Command != tc.command {
				t.Errorf("Command = %q, want %q", ie.Command, tc.command)
			}
			if ie.Timeout != time.Second {
				t.Errorf("Timeout = %v, want 1s", ie.Timeout)
			}
			// The wait consumed its own 1s budget, so the recorded elapsed
			// must reflect it and cannot exceed the call's real duration.
			if ie.Elapsed < 900*time.Millisecond {
				t.Errorf("Elapsed = %v, want the time actually spent waiting (>= 900ms of the 1s budget)", ie.Elapsed)
			}
			if ie.Elapsed > total {
				t.Errorf("Elapsed = %v, longer than the whole call (%v)", ie.Elapsed, total)
			}
		})
	}
}
