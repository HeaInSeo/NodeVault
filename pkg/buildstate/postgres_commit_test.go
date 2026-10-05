package buildstate

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// safeToRetryErr claims pgconn.SafeToRetry, as pgconn's "conn closed" error does even when the
// connection died after COMMIT was sent.
type safeToRetryErr struct{}

func (safeToRetryErr) Error() string     { return "conn closed" }
func (safeToRetryErr) SafeToRetry() bool { return true }

// Only errors that prove the transaction did not commit are definite; anything that can follow
// a durable commit is ambiguous (TX1-03).
func TestCommitOutcomeKnown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		known bool
	}{
		{"server rolled back", pgx.ErrTxCommitRollback, true},
		{"transaction closed", pgx.ErrTxClosed, true},
		{"serialization failure", &pgconn.PgError{Code: "40001"}, true},
		{"deferred constraint", &pgconn.PgError{Code: "23505"}, true},
		{"admin shutdown", &pgconn.PgError{Code: "57P01"}, false},
		{"connection failure", &pgconn.PgError{Code: "08006"}, false},
		{"internal error", &pgconn.PgError{Code: "XX000"}, false},
		{"connection lost", fmt.Errorf("receive message: %w", io.ErrUnexpectedEOF), false},
		{"conn closed after send", fmt.Errorf("commit: %w", safeToRetryErr{}), false},
		{"pgconn conn closed", pgconn.ErrConnClosed, false},
		{"context canceled", context.Canceled, false},
		{"transaction done", sql.ErrTxDone, false},
	} {
		if got := commitOutcomeKnown(tc.err); got != tc.known {
			t.Errorf("%s: commitOutcomeKnown(%v) = %v, want %v", tc.name, tc.err, got, tc.known)
		}
	}
}

// commitQuery is the simple-protocol Query message pgx sends for COMMIT.
var commitQuery = []byte("Q\x00\x00\x00\x0bcommit\x00")

// ackDropProxy relays TCP between the store and PostgreSQL. Once armed, it forwards the next
// COMMIT to the server, then swallows the server's reply and closes both sides: the commit is
// durable but its acknowledgement is lost.
type ackDropProxy struct {
	dsn     string
	target  string
	armed   atomic.Bool
	dropped chan struct{}
}

func startAckDropProxy(t *testing.T, dsn string) *ackDropProxy {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	target := u.Host
	if u.Port() == "" {
		target = net.JoinHostPort(u.Hostname(), "5432")
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	u.Host = ln.Addr().String()
	p := &ackDropProxy{dsn: u.String(), target: target, dropped: make(chan struct{})}
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			go p.relay(client)
		}
	}()
	return p
}

func (p *ackDropProxy) relay(client net.Conn) {
	server, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", p.target)
	if err != nil {
		_ = client.Close()
		return
	}
	var drop atomic.Bool
	go func() {
		tail := make([]byte, 0, len(commitQuery))
		buf := make([]byte, 32<<10)
		for {
			n, err := client.Read(buf)
			if n > 0 {
				seen := append(append([]byte{}, tail...), buf[:n]...)
				if bytes.Contains(seen, commitQuery) && p.armed.CompareAndSwap(true, false) {
					drop.Store(true)
				}
				tail = append(tail[:0], seen[max(0, len(seen)-len(commitQuery)+1):]...)
				if _, werr := server.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				_ = server.Close()
				return
			}
		}
	}()
	buf := make([]byte, 32<<10)
	for {
		n, err := server.Read(buf)
		if n > 0 && drop.Load() {
			close(p.dropped)
			_ = client.Close()
			_ = server.Close()
			return
		}
		if n > 0 {
			if _, werr := client.Write(buf[:n]); werr != nil {
				_ = server.Close()
				return
			}
		}
		if err != nil {
			_ = client.Close()
			return
		}
	}
}

// TX1-03 / TX1-07 (2): when the connection is lost after the server committed but before the
// client read the reply, the mutation is reported as ErrCommitOutcomeUnknown, not as a definite
// failure; it is durable, and the same call with the same fence converges on it.
func TestPostgres_CommitAckLossIsOutcomeUnknown(t *testing.T) {
	dsn := pgSchemaDSN(t)
	ctx := context.Background()
	direct := openPG(t, dsn)
	_, f1 := mustCreate(t, direct, "b1", "replica-a")

	p := startAckDropProxy(t, dsn)
	s := openPG(t, p.dsn)
	p.armed.Store(true)
	_, _, err := s.Transition(ctx, "b1", f1, StatusBuilding, "")
	select {
	case <-p.dropped:
	default:
		t.Fatalf("proxy did not drop a COMMIT acknowledgement (err=%v)", err)
	}
	if !errors.Is(err, ErrCommitOutcomeUnknown) {
		t.Fatalf("lost COMMIT acknowledgement: want ErrCommitOutcomeUnknown, got %v", err)
	}

	rec, f := mustGet(t, direct, "b1")
	if rec.Status != StatusBuilding || f != (Fence{Owner: "replica-a", Generation: 1, Version: f1.Version + 1}) {
		t.Fatalf("committed mutation not durable: %+v %+v", rec, f)
	}
	rec2, f2, err := s.Transition(ctx, "b1", f1, StatusBuilding, "")
	if err != nil || rec2 != rec || f2 != f {
		t.Fatalf("re-run after unknown outcome must converge: %+v %+v %v", rec2, f2, err)
	}
}
