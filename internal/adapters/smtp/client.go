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

func (Client) connect(ctx context.Context, opts domain.SMTPSendOptions) (*smtp.Client, error) {
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
	c, err := smtp.NewClient(conn, opts.Host)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err := c.Hello(localName()); err != nil {
		c.Close()
		return nil, err
	}
	if opts.Security == domain.SecurityStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			c.Close()
			return nil, errors.New("server does not offer STARTTLS")
		}
		if err := c.StartTLS(dialTLSConfig(opts)); err != nil {
			c.Close()
			return nil, fmt.Errorf("STARTTLS: %w", err)
		}
	}
	if err := authenticate(c, opts); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
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
	c, err := cl.connect(ctx, opts)
	if !step("smtp_ehlo_auth", err) {
		return diag, nil
	}
	c.Quit()
	diag.OK = true
	return diag, nil
}

func (cl Client) Send(ctx context.Context, opts domain.SMTPSendOptions, env domain.Envelope, message io.Reader) (domain.SendReceipt, error) {
	defer func() {
		if opts.Password != nil {
			opts.Password.Zero()
		}
	}()
	c, err := cl.connect(ctx, opts)
	if err != nil {
		// Auth failures are permanent; connection issues are retryable.
		var ae *AuthError
		if errors.As(err, &ae) {
			return domain.SendReceipt{}, &SendError{Err: err, temp: false}
		}
		return domain.SendReceipt{}, &SendError{Err: err, temp: true}
	}
	defer c.Close()

	if err := c.Mail(env.From); err != nil {
		return domain.SendReceipt{}, classify(fmt.Errorf("MAIL FROM: %w", err))
	}
	for _, rcpt := range env.To {
		if err := c.Rcpt(rcpt); err != nil {
			return domain.SendReceipt{}, classify(fmt.Errorf("RCPT TO %s: %w", rcpt, err))
		}
	}
	w, err := c.Data()
	if err != nil {
		return domain.SendReceipt{}, classify(fmt.Errorf("DATA: %w", err))
	}
	if _, err := io.Copy(w, message); err != nil {
		w.Close()
		return domain.SendReceipt{}, &SendError{Err: fmt.Errorf("DATA write: %w", err), temp: true}
	}
	// After the payload is fully handed over, a lost final response means the
	// server may have accepted the message: report uncertain, never retried
	// automatically (SMTP-008/009).
	if err := w.Close(); err != nil {
		if isConnectionLost(err) {
			return domain.SendReceipt{Uncertain: true, ServerResponse: err.Error()}, nil
		}
		return domain.SendReceipt{}, fmt.Errorf("DATA close: %w", err)
	}
	resp := "250 accepted"
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
