// Package pop3 implements a minimal RFC 1939 client behind the
// domain.POP3Dialer port. Supports implicit TLS (995), STLS upgrade, and —
// for offline / air-gapped networks — plaintext with optional
// authentication. Policy gating for insecure modes happens in the
// application layer, not here.
package pop3

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"postra/internal/domain"
)

type Dialer struct{}

func (Dialer) Dial(ctx context.Context, opts domain.POP3DialOptions) (domain.POP3Session, error) {
	addr := net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port))
	connectTO := secondsOr(opts.ConnectTimeoutSec, 15)
	d := &net.Dialer{Timeout: connectTO}

	var conn net.Conn
	var err error
	tlsCfg := &tls.Config{
		ServerName:         opts.Host,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: opts.InsecureSkipVerify,
	}
	switch opts.Security {
	case domain.SecurityTLS:
		conn, err = (&tls.Dialer{NetDialer: d, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	case domain.SecurityStartTLS, domain.SecurityNone:
		conn, err = d.DialContext(ctx, "tcp", addr)
	default:
		return nil, fmt.Errorf("unknown security mode %q", opts.Security)
	}
	if err != nil {
		return nil, fmt.Errorf("pop3 connect %s: %w", addr, err)
	}

	s := &session{
		conn:      conn,
		text:      textproto.NewConn(conn),
		commandTO: secondsOr(opts.CommandTimeoutSec, 60),
	}
	if _, err := s.readResponse(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("pop3 greeting: %w", err)
	}

	if opts.Security == domain.SecurityStartTLS {
		if _, err := s.cmd("STLS"); err != nil {
			conn.Close()
			return nil, fmt.Errorf("STLS: %w", err)
		}
		tconn := tls.Client(conn, tlsCfg)
		if err := tconn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("STLS handshake: %w", err)
		}
		s.conn = tconn
		s.text = textproto.NewConn(tconn)
	}

	// Authentication is optional: some maildrops on isolated networks accept
	// sessions without USER/PASS.
	if opts.Username != "" {
		if _, err := s.cmd("USER %s", opts.Username); err != nil {
			s.Close()
			return nil, &AuthError{fmt.Errorf("USER: %w", err)}
		}
		pass := ""
		if opts.Password != nil {
			pass = string(opts.Password.Reveal())
		}
		_, err := s.cmd("PASS %s", pass)
		pass = ""
		_ = pass
		if err != nil {
			s.Close()
			return nil, &AuthError{fmt.Errorf("PASS rejected: %w", err)}
		}
	}
	return s, nil
}

// AuthError distinguishes credential failures so the sync layer can move the
// account to credential_error instead of retrying (POP-011).
type AuthError struct{ Err error }

func (e *AuthError) Error() string { return e.Err.Error() }
func (e *AuthError) Unwrap() error { return e.Err }

type session struct {
	conn      net.Conn
	text      *textproto.Conn
	commandTO time.Duration
}

func (s *session) deadline() { s.conn.SetDeadline(time.Now().Add(s.commandTO)) }

func (s *session) readResponse() (string, error) {
	s.deadline()
	line, err := s.text.ReadLine()
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(line, "+OK") {
		return "", fmt.Errorf("server: %s", line)
	}
	return line, nil
}

func (s *session) cmd(format string, args ...any) (string, error) {
	s.deadline()
	if err := s.text.PrintfLine(format, args...); err != nil {
		return "", err
	}
	return s.readResponse()
}

func (s *session) readList() ([]string, error) {
	var lines []string
	for {
		s.deadline()
		line, err := s.text.ReadLine()
		if err != nil {
			return nil, err
		}
		if line == "." {
			return lines, nil
		}
		lines = append(lines, strings.TrimPrefix(line, "."))
	}
}

func (s *session) List(ctx context.Context) ([]domain.RemoteMessage, error) {
	if _, err := s.cmd("LIST"); err != nil {
		return nil, err
	}
	lines, err := s.readList()
	if err != nil {
		return nil, err
	}
	var out []domain.RemoteMessage
	for _, l := range lines {
		var n int
		var size int64
		if _, err := fmt.Sscanf(l, "%d %d", &n, &size); err == nil {
			out = append(out, domain.RemoteMessage{Number: n, Size: size})
		}
	}
	return out, nil
}

func (s *session) UIDL(ctx context.Context) ([]domain.RemoteMessage, error) {
	if _, err := s.cmd("UIDL"); err != nil {
		return nil, err // caller falls back to LIST + content hash (POP-005)
	}
	lines, err := s.readList()
	if err != nil {
		return nil, err
	}
	var out []domain.RemoteMessage
	for _, l := range lines {
		parts := strings.SplitN(strings.TrimSpace(l), " ", 2)
		if len(parts) != 2 {
			continue
		}
		n, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		out = append(out, domain.RemoteMessage{Number: n, UIDL: strings.TrimSpace(parts[1])})
	}
	return out, nil
}

// retrBody reads a multi-line response body with dot-unstuffing.
func (s *session) retrBody() (io.ReadCloser, error) {
	var buf bytes.Buffer
	for {
		s.deadline()
		line, err := s.text.ReadLineBytes()
		if err != nil {
			return nil, err
		}
		if len(line) == 1 && line[0] == '.' {
			break
		}
		if len(line) > 1 && line[0] == '.' {
			line = line[1:]
		}
		buf.Write(line)
		buf.WriteString("\r\n")
	}
	return io.NopCloser(&buf), nil
}

func (s *session) Retrieve(ctx context.Context, number int) (io.ReadCloser, error) {
	if _, err := s.cmd("RETR %d", number); err != nil {
		return nil, err
	}
	return s.retrBody()
}

func (s *session) Top(ctx context.Context, number, lines int) (io.ReadCloser, error) {
	if _, err := s.cmd("TOP %d %d", number, lines); err != nil {
		return nil, err
	}
	return s.retrBody()
}

func (s *session) Delete(ctx context.Context, number int) error {
	_, err := s.cmd("DELE %d", number)
	return err
}

func (s *session) Quit(ctx context.Context) error {
	_, err := s.cmd("QUIT")
	s.conn.Close()
	return err
}

func (s *session) Close() error { return s.conn.Close() }

func secondsOr(v, def int) time.Duration {
	if v <= 0 {
		v = def
	}
	return time.Duration(v) * time.Second
}
