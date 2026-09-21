package pop3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"postra/internal/domain"
)

// bodyServer records commands and joins its goroutine before cleanup finishes.
// A stalled body deliberately has neither a newline nor a terminating dot.
func bodyServer(t *testing.T, body string, stall bool) (int, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	closed := make(chan error, 1)
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(6 * time.Second))
		wire := textproto.NewConn(conn)
		_ = wire.PrintfLine("+OK ready")
		for {
			cmd, err := wire.ReadLine()
			if err != nil {
				closed <- err
				return
			}
			switch {
			case cmd == "RETR 1" || cmd == "TOP 1 0":
				_, err = io.WriteString(conn, "+OK body\r\n"+body)
				if err != nil {
					closed <- err
					return
				}
				if stall {
					_, err = wire.ReadLine()
					closed <- err
					return
				}
			case cmd == "DELE 1":
				_ = wire.PrintfLine("+OK deleted")
			case cmd == "QUIT":
				_ = wire.PrintfLine("+OK bye")
				return
			default:
				_ = wire.PrintfLine("-ERR unexpected command")
			}
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		select {
		case <-done:
		case <-time.After(7 * time.Second):
			t.Error("server did not finish")
		}
	})
	return ln.Addr().(*net.TCPAddr).Port, closed
}

func TestPOP3BodyLimit(t *testing.T) {
	for _, top := range []bool{false, true} {
		for _, tc := range []struct {
			name, wire, want string
			limit            int64
			reject, stall    bool
		}{
			{name: "normal", wire: "hello\r\n\r\n..dot\r\n.\r\n", want: "hello\r\n\r\n.dot\r\n", limit: 100},
			{name: "exact", wire: "..x\r\n\r\n.\r\n", want: ".x\r\n\r\n", limit: 6},
			{name: "over", wire: "..x\r\n\r\n.\r\n", limit: 5, reject: true},
			{name: "unlimited", wire: strings.Repeat("x", 8192) + "\r\n.\r\n", want: strings.Repeat("x", 8192) + "\r\n"},
			{name: "long-line", wire: strings.Repeat("x", 128), limit: 32, reject: true, stall: true},
			{name: "empty", wire: ".\r\n", limit: 1},
			{name: "buffer-boundary", wire: strings.Repeat("x", 4095) + "\r\n..tail\r\n.\r\n", want: strings.Repeat("x", 4095) + "\r\n.tail\r\n", limit: 4104},
			{name: "embedded-cr", wire: "..\rX\r\r\n.\r\n", want: ".\rX\r\r\n", limit: 6},
			{name: "dot-only-data", wire: "..\r\n.\r\n", want: ".\r\n", limit: 3},
			{name: "crlf-counts", wire: "x\r\n.\r\n", limit: 2, reject: true},
			{name: "bare-lf", wire: "x\n..y\n.\n", want: "x\r\n.y\r\n", limit: 7},
		} {
			t.Run(fmt.Sprintf("top=%v/%s", top, tc.name), func(t *testing.T) {
				port, closed := bodyServer(t, tc.wire, tc.stall)
				sess, err := (Dialer{}).Dial(context.Background(), domain.POP3DialOptions{Host: "127.0.0.1", Port: port, Security: domain.SecurityNone, CommandTimeoutSec: 3, MaxMessageBytes: tc.limit})
				if err != nil {
					t.Fatal(err)
				}
				defer sess.Close()
				start := time.Now()
				var rc io.ReadCloser
				if top {
					rc, err = sess.Top(context.Background(), 1, 0)
				} else {
					rc, err = sess.Retrieve(context.Background(), 1)
				}
				if tc.reject {
					if err == nil || !strings.Contains(err.Error(), "exceeds") || !strings.Contains(err.Error(), fmt.Sprint(tc.limit)) {
						t.Fatalf("want size error, got %v", err)
					}
					if rc != nil {
						t.Fatal("partial body returned")
					}
					if time.Since(start) >= 2*time.Second {
						t.Fatal("size rejection waited for command timeout")
					}
					select {
					case err := <-closed:
						if err != io.EOF {
							t.Fatalf("server did not observe EOF: %v", err)
						}
					case <-time.After(time.Second):
						t.Fatal("connection not closed")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				raw, err := io.ReadAll(rc)
				_ = rc.Close()
				if err != nil || string(raw) != tc.want {
					t.Fatalf("body mismatch: %q, %v", raw, err)
				}
				if err := sess.Delete(context.Background(), 1); err != nil {
					t.Fatal(err)
				}
				if err := sess.Quit(context.Background()); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestPOP3BodyDeadline(t *testing.T) {
	port, _ := bodyServer(t, "unfinished", true)
	sess, err := (Dialer{}).Dial(context.Background(), domain.POP3DialOptions{
		Host: "127.0.0.1", Port: port, Security: domain.SecurityNone,
		MaxMessageBytes: 100, CommandTimeoutSec: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	rc, err := sess.Retrieve(context.Background(), 1)
	var timeout net.Error
	if rc != nil || !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("want command timeout without partial body, got %v", err)
	}
}
