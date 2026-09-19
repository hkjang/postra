package smtp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"math/big"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"postra/internal/domain"
)

// relayOpts scripts one loopback SMTP relay. Every scenario below drives the
// real Client over a real TCP socket against this script; nothing is faked on
// the client side.
type relayOpts struct {
	ehlo          []string // extension lines advertised after "250-localhost"
	authReply     string   // final reply to AUTH (default "235 2.7.0 ok")
	mailReply     string   // reply to MAIL FROM (default "250 ok")
	rcptReply     string   // reply to RCPT TO (default "250 ok")
	dropAfterData bool     // close without the final DATA reply
	starttls      bool     // honour STARTTLS with a self-signed cert
	implicitTLS   bool     // listen with tls.Listen (SMTPS / 465 style)
}

// recorded collects every command line the relay read, tagged with whether
// it arrived over TLS, so tests can assert on presence, absence and order.
type recorded struct {
	mu       sync.Mutex
	lines    []string
	tls      []bool
	finished chan struct{}
}

func (r *recorded) add(line string, overTLS bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	r.tls = append(r.tls, overTLS)
}

func (r *recorded) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

// find returns the first recorded line with the given prefix, and whether it
// was received on the TLS leg of the conversation.
func (r *recorded) find(prefix string) (line string, overTLS, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, l := range r.lines {
		if strings.HasPrefix(l, prefix) {
			return l, r.tls[i], true
		}
	}
	return "", false, false
}

func (r *recorded) has(prefix string) bool { _, _, ok := r.find(prefix); return ok }

// waitConn blocks until the relay has finished serving one connection, so
// assertions never race the server goroutine.
func (r *recorded) waitConn(t *testing.T) {
	t.Helper()
	select {
	case <-r.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("relay connection did not finish")
	}
}

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

func fakeRelay(t *testing.T, o relayOpts) (port int, got *recorded) {
	t.Helper()
	if o.authReply == "" {
		o.authReply = "235 2.7.0 ok"
	}
	if o.mailReply == "" {
		o.mailReply = "250 ok"
	}
	if o.rcptReply == "" {
		o.rcptReply = "250 ok"
	}
	var tlsCfg *tls.Config
	if o.starttls || o.implicitTLS {
		tlsCfg = selfSigned(t)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if o.implicitTLS {
		ln = tls.NewListener(ln, tlsCfg)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got = &recorded{finished: make(chan struct{}, 8)}

	serve := func(conn net.Conn) {
		defer conn.Close()
		defer func() { got.finished <- struct{}{} }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		overTLS := o.implicitTLS
		wire := textproto.NewConn(conn)
		if wire.PrintfLine("220 localhost Postra test relay") != nil {
			return
		}
		for {
			line, err := wire.ReadLine()
			if err != nil {
				return
			}
			got.add(line, overTLS)
			command, _, _ := strings.Cut(line, " ")
			switch strings.ToUpper(command) {
			case "EHLO":
				ext := append([]string{"250-localhost"}, o.ehlo...)
				if o.starttls && !overTLS {
					ext = append(ext, "STARTTLS")
				}
				resp := ext[0]
				for _, e := range ext[1:] {
					resp += "\r\n250-" + e
				}
				_ = wire.PrintfLine("%s\r\n250 8BITMIME", resp)
			case "STARTTLS":
				if !o.starttls || overTLS {
					_ = wire.PrintfLine("454 TLS not available")
					continue
				}
				if wire.PrintfLine("220 go ahead") != nil {
					return
				}
				tc := tls.Server(conn, tlsCfg)
				if err := tc.Handshake(); err != nil {
					return
				}
				wire = textproto.NewConn(tc)
				overTLS = true
			case "AUTH":
				if strings.HasPrefix(strings.ToUpper(line), "AUTH LOGIN") {
					// net/smtp decodes 334 challenges before handing them to Auth.Next.
					for _, prompt := range []string{"Username:", "Password:"} {
						_ = wire.PrintfLine("334 %s", base64.StdEncoding.EncodeToString([]byte(prompt)))
						answer, err := wire.ReadLine()
						if err != nil {
							return
						}
						got.add(answer, overTLS)
					}
				}
				_ = wire.PrintfLine("%s", o.authReply)
			case "*":
				_ = wire.PrintfLine("501 auth aborted")
			case "MAIL":
				_ = wire.PrintfLine("%s", o.mailReply)
			case "RCPT":
				_ = wire.PrintfLine("%s", o.rcptReply)
			case "DATA":
				_ = wire.PrintfLine("354 send content")
				if _, err := wire.ReadDotBytes(); err != nil {
					return
				}
				if o.dropAfterData {
					return
				}
				_ = wire.PrintfLine("250 queued")
			case "QUIT":
				_ = wire.PrintfLine("221 bye")
				return
			default:
				_ = wire.PrintfLine("500 unsupported command")
			}
		}
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(conn)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, got
}

func sendOpts(port int, method, user, pass string) domain.SMTPSendOptions {
	opts := domain.SMTPSendOptions{Host: "127.0.0.1", Port: port, Security: domain.SecurityNone, AuthMethod: method, Username: user, ConnectTimeoutSec: 3}
	if pass != "" {
		opts.Password = domain.NewSecretHandle([]byte(pass))
	}
	return opts
}

var testEnvelope = domain.Envelope{From: "sender@corp.local", To: []string{"rcpt@corp.local"}}

func doSend(t *testing.T, opts domain.SMTPSendOptions) (domain.SendReceipt, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return Client{}.Send(ctx, opts, testEnvelope, strings.NewReader("Subject: t\r\n\r\nhi\r\n"))
}

func asSendError(t *testing.T, err error) *SendError {
	t.Helper()
	var se *SendError
	if !errors.As(err, &se) {
		t.Fatalf("expected *SendError, got %T: %v", err, err)
	}
	return se
}

func TestSendAuth(t *testing.T) {
	t.Run("auto picks PLAIN when advertised", func(t *testing.T) {
		port, got := fakeRelay(t, relayOpts{ehlo: []string{"AUTH PLAIN LOGIN"}})
		receipt, err := doSend(t, sendOpts(port, "auto", "user", "pass"))
		if err != nil {
			t.Fatal(err)
		}
		if receipt.ServerResponse != "250 accepted" || receipt.Uncertain {
			t.Fatalf("receipt: %+v", receipt)
		}
		got.waitConn(t)
		want := "AUTH PLAIN " + base64.StdEncoding.EncodeToString([]byte("\x00user\x00pass"))
		if line, _, ok := got.find("AUTH "); !ok || line != want {
			t.Fatalf("AUTH line = %q, want %q (all: %v)", line, want, got.all())
		}
	})

	t.Run("falls back to LOGIN challenge exchange", func(t *testing.T) {
		port, got := fakeRelay(t, relayOpts{ehlo: []string{"AUTH LOGIN"}})
		receipt, err := doSend(t, sendOpts(port, "auto", "user", "pass"))
		if err != nil {
			t.Fatal(err)
		}
		if receipt.ServerResponse != "250 accepted" {
			t.Fatalf("receipt: %+v", receipt)
		}
		got.waitConn(t)
		lines := got.all()
		i := -1
		for n, l := range lines {
			if l == "AUTH LOGIN" {
				i = n
				break
			}
		}
		if i < 0 || len(lines) < i+3 {
			t.Fatalf("no AUTH LOGIN exchange in %v", lines)
		}
		u := base64.StdEncoding.EncodeToString([]byte("user"))
		p := base64.StdEncoding.EncodeToString([]byte("pass"))
		if lines[i+1] != u || lines[i+2] != p {
			t.Fatalf("LOGIN answers = %q %q, want %q %q", lines[i+1], lines[i+2], u, p)
		}
	})

	t.Run("rejected credentials are a permanent AuthError", func(t *testing.T) {
		port, got := fakeRelay(t, relayOpts{ehlo: []string{"AUTH PLAIN"}, authReply: "535 5.7.8 bad credentials"})
		_, err := doSend(t, sendOpts(port, "auto", "user", "wrong"))
		var ae *AuthError
		if !errors.As(err, &ae) {
			t.Fatalf("expected AuthError, got %T: %v", err, err)
		}
		if se := asSendError(t, err); se.Temporary() {
			t.Fatalf("auth failure must be permanent: %v", err)
		}
		if !strings.Contains(err.Error(), "535") {
			t.Fatalf("server code lost: %v", err)
		}
		got.waitConn(t)
		if got.has("MAIL ") {
			t.Fatalf("MAIL sent after failed AUTH: %v", got.all())
		}
	})

	t.Run("auto proceeds unauthenticated when AUTH is not advertised", func(t *testing.T) {
		port, got := fakeRelay(t, relayOpts{})
		receipt, err := doSend(t, sendOpts(port, "auto", "user", "pass"))
		if err != nil {
			t.Fatal(err)
		}
		if receipt.ServerResponse != "250 accepted" {
			t.Fatalf("receipt: %+v", receipt)
		}
		got.waitConn(t)
		if got.has("AUTH") {
			t.Fatalf("AUTH sent to a relay that offers none: %v", got.all())
		}
		if !got.has("MAIL FROM:<sender@corp.local>") {
			t.Fatalf("MAIL missing: %v", got.all())
		}
	})

	t.Run("explicit method fails when AUTH is not advertised", func(t *testing.T) {
		port, got := fakeRelay(t, relayOpts{})
		_, err := doSend(t, sendOpts(port, "login", "user", "pass"))
		if err == nil || !strings.Contains(err.Error(), "does not offer AUTH") {
			t.Fatalf("err = %v", err)
		}
		if se := asSendError(t, err); !se.Temporary() {
			t.Fatalf("connect-phase error must be temporary: %v", err)
		}
		got.waitConn(t)
		if got.has("AUTH") || got.has("MAIL ") {
			t.Fatalf("unexpected commands: %v", got.all())
		}
	})

	t.Run("none skips AUTH even when advertised", func(t *testing.T) {
		port, got := fakeRelay(t, relayOpts{ehlo: []string{"AUTH PLAIN"}})
		if _, err := doSend(t, sendOpts(port, "none", "user", "pass")); err != nil {
			t.Fatal(err)
		}
		got.waitConn(t)
		if got.has("AUTH") {
			t.Fatalf("AUTH sent with auth=none: %v", got.all())
		}
	})
}

func TestSendClassifiesReplies(t *testing.T) {
	cases := []struct {
		name     string
		opts     relayOpts
		wantTemp bool
		wantMsg  string
	}{
		{"RCPT 4xx is temporary", relayOpts{rcptReply: "450 4.2.0 mailbox busy"}, true, "RCPT TO rcpt@corp.local"},
		{"RCPT 5xx is permanent", relayOpts{rcptReply: "550 5.1.1 no such user"}, false, "RCPT TO rcpt@corp.local"},
		{"MAIL 5xx is permanent", relayOpts{mailReply: "553 5.1.8 sender rejected"}, false, "MAIL FROM"},
		{"MAIL 4xx is temporary", relayOpts{mailReply: "421 4.3.2 shutting down"}, true, "MAIL FROM"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			port, got := fakeRelay(t, tc.opts)
			_, err := doSend(t, sendOpts(port, "none", "", ""))
			se := asSendError(t, err)
			if se.Temporary() != tc.wantTemp {
				t.Fatalf("Temporary() = %v, want %v: %v", se.Temporary(), tc.wantTemp, err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("err %q lacks %q", err, tc.wantMsg)
			}
			got.waitConn(t)
			if got.has("DATA") {
				t.Fatalf("DATA sent after rejection: %v", got.all())
			}
		})
	}

	t.Run("lost final DATA reply is uncertain not failed", func(t *testing.T) {
		port, got := fakeRelay(t, relayOpts{dropAfterData: true})
		receipt, err := doSend(t, sendOpts(port, "none", "", ""))
		if err != nil {
			t.Fatal(err)
		}
		if !receipt.Uncertain {
			t.Fatalf("receipt: %+v", receipt)
		}
		got.waitConn(t)
		if !got.has("DATA") {
			t.Fatalf("DATA missing: %v", got.all())
		}
	})
}

func TestSendSTARTTLS(t *testing.T) {
	t.Run("required but not advertised fails before MAIL", func(t *testing.T) {
		port, got := fakeRelay(t, relayOpts{ehlo: []string{"AUTH PLAIN"}})
		opts := sendOpts(port, "auto", "user", "pass")
		opts.Security = domain.SecurityStartTLS
		_, err := doSend(t, opts)
		if err == nil || !strings.Contains(err.Error(), "does not offer STARTTLS") {
			t.Fatalf("err = %v", err)
		}
		if se := asSendError(t, err); !se.Temporary() {
			t.Fatalf("should be temporary: %v", err)
		}
		got.waitConn(t)
		if got.has("STARTTLS") || got.has("AUTH") || got.has("MAIL ") {
			t.Fatalf("unexpected commands: %v", got.all())
		}
	})

	t.Run("upgrades and sends AUTH and MAIL over TLS", func(t *testing.T) {
		port, got := fakeRelay(t, relayOpts{ehlo: []string{"AUTH PLAIN"}, starttls: true})
		opts := sendOpts(port, "auto", "user", "pass")
		opts.Security = domain.SecurityStartTLS
		opts.InsecureSkipVerify = true
		receipt, err := doSend(t, opts)
		if err != nil {
			t.Fatal(err)
		}
		if receipt.ServerResponse != "250 accepted" {
			t.Fatalf("receipt: %+v", receipt)
		}
		got.waitConn(t)
		if _, overTLS, ok := got.find("STARTTLS"); !ok || overTLS {
			t.Fatalf("STARTTLS must arrive once on the plaintext leg: %v", got.all())
		}
		for _, prefix := range []string{"AUTH ", "MAIL ", "RCPT ", "DATA"} {
			if _, overTLS, ok := got.find(prefix); !ok || !overTLS {
				t.Fatalf("%s must arrive over TLS: %v", prefix, got.all())
			}
		}
	})

	t.Run("self-signed cert is rejected without InsecureSkipVerify", func(t *testing.T) {
		port, got := fakeRelay(t, relayOpts{starttls: true})
		opts := sendOpts(port, "none", "", "")
		opts.Security = domain.SecurityStartTLS
		_, err := doSend(t, opts)
		if err == nil || !strings.Contains(err.Error(), "STARTTLS:") {
			t.Fatalf("err = %v", err)
		}
		if se := asSendError(t, err); !se.Temporary() {
			t.Fatalf("should be temporary: %v", err)
		}
		got.waitConn(t)
		if got.has("MAIL ") {
			t.Fatalf("MAIL sent after failed handshake: %v", got.all())
		}
	})
}

func TestSendImplicitTLS(t *testing.T) {
	port, got := fakeRelay(t, relayOpts{ehlo: []string{"AUTH PLAIN"}, implicitTLS: true})
	opts := sendOpts(port, "auto", "user", "pass")
	opts.Security = domain.SecurityTLS
	opts.InsecureSkipVerify = true
	receipt, err := doSend(t, opts)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ServerResponse != "250 accepted" {
		t.Fatalf("receipt: %+v", receipt)
	}
	got.waitConn(t)
	if got.has("STARTTLS") {
		t.Fatalf("STARTTLS sent on an implicit-TLS session: %v", got.all())
	}
	for _, prefix := range []string{"EHLO", "AUTH ", "MAIL "} {
		if _, overTLS, ok := got.find(prefix); !ok || !overTLS {
			t.Fatalf("%s must arrive over TLS: %v", prefix, got.all())
		}
	}
}

func TestSendZeroesPassword(t *testing.T) {
	port, _ := fakeRelay(t, relayOpts{ehlo: []string{"AUTH PLAIN"}})
	opts := sendOpts(port, "auto", "user", "pass")
	if _, err := doSend(t, opts); err != nil {
		t.Fatal(err)
	}
	if len(opts.Password.Reveal()) != 0 {
		t.Fatal("password not zeroed after Send")
	}

	port, _ = fakeRelay(t, relayOpts{ehlo: []string{"AUTH PLAIN"}, authReply: "535 no"})
	opts = sendOpts(port, "auto", "user", "pass")
	if _, err := doSend(t, opts); err == nil {
		t.Fatal("expected auth failure")
	}
	if len(opts.Password.Reveal()) != 0 {
		t.Fatal("password not zeroed after failed Send")
	}
}

func TestTestConnection(t *testing.T) {
	t.Run("reports both steps ok", func(t *testing.T) {
		port, got := fakeRelay(t, relayOpts{ehlo: []string{"AUTH PLAIN"}})
		diag, err := Client{}.TestConnection(context.Background(), sendOpts(port, "auto", "user", "pass"))
		if err != nil {
			t.Fatal(err)
		}
		if !diag.OK || diag.Target != "smtp" {
			t.Fatalf("diag: %+v", diag)
		}
		if len(diag.Steps) != 2 || diag.Steps[0].Step != "dns" || !diag.Steps[0].OK || diag.Steps[1].Step != "smtp_ehlo_auth" || !diag.Steps[1].OK {
			t.Fatalf("steps: %+v", diag.Steps)
		}
		got.waitConn(t)
		if !got.has("AUTH PLAIN ") || !got.has("QUIT") || got.has("MAIL ") {
			t.Fatalf("commands: %v", got.all())
		}
	})

	t.Run("auth failure is returned as a step not an error", func(t *testing.T) {
		port, _ := fakeRelay(t, relayOpts{ehlo: []string{"AUTH PLAIN"}, authReply: "535 5.7.8 bad credentials"})
		diag, err := Client{}.TestConnection(context.Background(), sendOpts(port, "auto", "user", "wrong"))
		if err != nil {
			t.Fatal(err)
		}
		if diag.OK {
			t.Fatalf("diag should not be ok: %+v", diag)
		}
		if len(diag.Steps) != 2 || diag.Steps[1].Step != "smtp_ehlo_auth" || diag.Steps[1].OK || !strings.Contains(diag.Steps[1].Detail, "smtp auth") {
			t.Fatalf("steps: %+v", diag.Steps)
		}
	})

	t.Run("connection refused stops at the smtp step", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
		diag, err := Client{}.TestConnection(context.Background(), sendOpts(port, "none", "", ""))
		if err != nil {
			t.Fatal(err)
		}
		if diag.OK || len(diag.Steps) != 2 || diag.Steps[1].OK || !strings.Contains(diag.Steps[1].Detail, "smtp connect") {
			t.Fatalf("diag: %+v", diag)
		}
	})
}

// classify is the only send-phase branch not reachable through the relay
// script above (network errors are wrapped as-is); pin the table directly.
func TestClassify(t *testing.T) {
	if se := classify(&textproto.Error{Code: 451, Msg: "try later"}); !se.Temporary() {
		t.Fatal("4xx must be temporary")
	}
	if se := classify(&textproto.Error{Code: 552, Msg: "too big"}); se.Temporary() {
		t.Fatal("5xx must be permanent")
	}
	if se := classify(errors.New("read: connection reset")); !se.Temporary() {
		t.Fatal("network error must be temporary")
	}
}
