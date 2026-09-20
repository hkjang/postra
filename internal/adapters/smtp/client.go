// Package smtp implements the domain.SMTPClient port. net/smtp is frozen
// upstream, so all use goes through this adapter; swapping in a maintained
// third-party library later only touches this package.
//
// Supported modes: implicit TLS (465), STARTTLS (587), and — for offline /
// air-gapped networks — plaintext without AUTH. Insecure modes are gated by
// the application-layer policy, not here.
package smtp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"postra/internal/domain"
)

type Client struct{}

func dialTLSConfig(opts domain.SMTPSendOptions) *tls.Config {
	return &tls.Config{
		ServerName: opts.Host,
		MinVersion: tls.VersionTLS12,
		// #nosec G402 -- offline/self-hosted relays may use self-signed certs;
		// skipping verification is an explicit per-account opt-in (default false),
		// required by the offline-network mail support in the spec.
		InsecureSkipVerify: opts.InsecureSkipVerify,
	}
}

// session is one relay conversation. net/smtp has no notion of ctx or
// per-command deadlines, so the raw conn is kept: every command arms a fresh
// deadline on it (the same pattern as the POP3/IMAP adapters), and ctx
// cancellation closes it so a blocked read returns immediately. After
// STARTTLS the deadline still goes on the raw conn — tls.Conn delegates
// SetDeadline to it anyway.
type session struct {
	c         *smtp.Client
	conn      net.Conn
	ctx       context.Context
	commandTO time.Duration
	stop      func() bool // detaches the ctx watcher; safe to call twice
}

func (s *session) deadline() { _ = s.conn.SetDeadline(time.Now().Add(s.commandTO)) }

// cause annotates a command error with the ctx error when cancellation is
// what closed the socket, so the outbox log shows why instead of a bare
// "use of closed network connection".
func (s *session) cause(err error) error {
	if cerr := s.ctx.Err(); cerr != nil {
		return fmt.Errorf("%w (%w)", err, cerr)
	}
	return err
}

func (s *session) close() {
	s.stop()
	s.c.Close()
}

// deadlineWriter arms the command deadline before each chunk so a large DATA
// payload is bounded per write, not for the whole upload.
type deadlineWriter struct {
	s *session
	w io.Writer
}

func (d deadlineWriter) Write(p []byte) (int, error) {
	d.s.deadline()
	return d.w.Write(p)
}

func (Client) connect(ctx context.Context, opts domain.SMTPSendOptions) (*session, error) {
	addr := net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port))
	d := &net.Dialer{Timeout: secondsOr(opts.ConnectTimeoutSec, 15)}

	var conn net.Conn
	var err error
	if opts.Security == domain.SecurityTLS {
		conn, err = (&tls.Dialer{NetDialer: d, Config: dialTLSConfig(opts)}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("smtp connect %s: %w", addr, err)
	}
	s := &session{conn: conn, ctx: ctx, commandTO: secondsOr(opts.CommandTimeoutSec, 60)}
	s.stop = context.AfterFunc(ctx, func() { _ = conn.Close() })
	s.deadline()
	c, err := smtp.NewClient(conn, opts.Host)
	if err != nil {
		s.stop()
		conn.Close()
		return nil, s.cause(err)
	}
	s.c = c
	s.deadline()
	if err := c.Hello(localName()); err != nil {
		s.close()
		return nil, s.cause(err)
	}
	if opts.Security == domain.SecurityStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			s.close()
			return nil, errors.New("server does not offer STARTTLS")
		}
		s.deadline()
		if err := c.StartTLS(dialTLSConfig(opts)); err != nil {
			s.close()
			return nil, fmt.Errorf("STARTTLS: %w", s.cause(err))
		}
	}
	s.deadline()
	if err := authenticate(c, opts); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func authenticate(c *smtp.Client, opts domain.SMTPSendOptions) error {
	if opts.AuthMethod == "none" || opts.Username == "" {
		return nil // open submission on isolated networks
	}
	pass := ""
	if opts.Password != nil {
		pass = string(opts.Password.Reveal())
	}
	ok, ext := c.Extension("AUTH")
	if !ok {
		if opts.AuthMethod == "auto" {
			return nil // server offers no AUTH; proceed unauthenticated
		}
		return errors.New("server does not offer AUTH")
	}
	var auth smtp.Auth
	switch {
	case strings.Contains(ext, "PLAIN"):
		auth = smtp.PlainAuth("", opts.Username, pass, opts.Host)
	case strings.Contains(ext, "LOGIN"):
		auth = &loginAuth{username: opts.Username, password: pass}
	case strings.Contains(ext, "CRAM-MD5"):
		auth = smtp.CRAMMD5Auth(opts.Username, pass)
	default:
		return fmt.Errorf("no supported AUTH mechanism in %q", ext)
	}
	if err := c.Auth(auth); err != nil {
		return &AuthError{err}
	}
	return nil
}

type AuthError struct{ Err error }

func (e *AuthError) Error() string { return "smtp auth: " + e.Err.Error() }
func (e *AuthError) Unwrap() error { return e.Err }

// SendError classifies a send failure as temporary (retryable, e.g. 4xx or a
// network blip) or permanent (5xx), so the outbox can retry only what makes
// sense (SMTP-010/011).
type SendError struct {
	Err  error
	temp bool
}

func (e *SendError) Error() string   { return e.Err.Error() }
func (e *SendError) Unwrap() error   { return e.Err }
func (e *SendError) Temporary() bool { return e.temp }

// classify wraps a send-phase error. A 4xx SMTP reply or a non-SMTP
// (network) error is temporary; a 5xx reply is permanent.
func classify(err error) *SendError {
	var te *textproto.Error
	if errors.As(err, &te) {
		return &SendError{Err: err, temp: te.Code/100 == 4}
	}
	return &SendError{Err: err, temp: true}
}

// loginAuth implements the legacy AUTH LOGIN mechanism still common on
// intranet mail servers.
type loginAuth struct{ username, password string }

func (a *loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(fromServer))) {
	case "username:":
		return []byte(a.username), nil
	case "password:":
		return []byte(a.password), nil
	}
	return nil, fmt.Errorf("unexpected LOGIN challenge")
}

func (cl Client) TestConnection(ctx context.Context, opts domain.SMTPSendOptions) (*domain.ConnDiagnostics, error) {
	diag := &domain.ConnDiagnostics{Target: "smtp"}
	step := func(name string, err error) bool {
		st := domain.ConnStep{Step: name, OK: err == nil}
		if err != nil {
			st.Detail = err.Error()
		}
		diag.Steps = append(diag.Steps, st)
		return err == nil
	}
	_, err := net.DefaultResolver.LookupHost(ctx, opts.Host)
	if !step("dns", err) {
		return diag, nil
	}
	s, err := cl.connect(ctx, opts)
	if !step("smtp_ehlo_auth", err) {
		return diag, nil
	}
	defer s.close()
	s.deadline()
	s.c.Quit()
	diag.OK = true
	return diag, nil
}

func (cl Client) Send(ctx context.Context, opts domain.SMTPSendOptions, env domain.Envelope, message io.Reader) (domain.SendReceipt, error) {
	defer func() {
		if opts.Password != nil {
			opts.Password.Zero()
		}
	}()
	s, err := cl.connect(ctx, opts)
	if err != nil {
		// Auth failures are permanent; connection issues are retryable.
		var ae *AuthError
		if errors.As(err, &ae) {
			return domain.SendReceipt{}, &SendError{Err: err, temp: false}
		}
		return domain.SendReceipt{}, &SendError{Err: err, temp: true}
	}
	defer s.close()
	c := s.c

	s.deadline()
	if err := c.Mail(env.From); err != nil {
		return domain.SendReceipt{}, classify(fmt.Errorf("MAIL FROM: %w", s.cause(err)))
	}
	for _, rcpt := range env.To {
		s.deadline()
		if err := c.Rcpt(rcpt); err != nil {
			return domain.SendReceipt{}, classify(fmt.Errorf("RCPT TO %s: %w", rcpt, s.cause(err)))
		}
	}
	s.deadline()
	w, err := c.Data()
	if err != nil {
		return domain.SendReceipt{}, classify(fmt.Errorf("DATA: %w", s.cause(err)))
	}
	// Hide any WriterTo on the source so io.Copy chunks through deadlineWriter
	// instead of handing the whole body over in one write.
	if _, err := io.Copy(deadlineWriter{s, w}, struct{ io.Reader }{message}); err != nil {
		w.Close()
		return domain.SendReceipt{}, &SendError{Err: fmt.Errorf("DATA write: %w", s.cause(err)), temp: true}
	}
	// After the payload is fully handed over, a lost final response means the
	// server may have accepted the message: report uncertain, never retried
	// automatically (SMTP-008/009). A deadline or ctx close here counts as
	// lost for the same reason.
	s.deadline()
	if err := w.Close(); err != nil {
		if isConnectionLost(err) {
			return domain.SendReceipt{Uncertain: true, ServerResponse: err.Error()}, nil
		}
		return domain.SendReceipt{}, fmt.Errorf("DATA close: %w", err)
	}
	resp := "250 accepted"
	s.deadline()
	if err := c.Quit(); err != nil && !isConnectionLost(err) {
		resp = "accepted (QUIT: " + err.Error() + ")"
	}
	return domain.SendReceipt{ServerResponse: resp}, nil
}

func isConnectionLost(err error) bool {
	var nerr net.Error
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) ||
		(errors.As(err, &nerr) && nerr.Timeout()) ||
		strings.Contains(err.Error(), "connection reset")
}

func localName() string { return "postra.local" }

func secondsOr(v, def int) time.Duration {
	if v <= 0 {
		v = def
	}
	return time.Duration(v) * time.Second
}

var _ domain.SMTPClient = Client{}
