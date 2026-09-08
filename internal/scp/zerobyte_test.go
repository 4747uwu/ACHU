package scp

import (
	"context"
	"net"
	"testing"
	"time"
)

// A connection that opens and closes without writing a byte used to kill the
// whole engine.
//
// The sniff in maybeWrapTLS returned (nil, false, err) on EOF, and the caller
// assigned that nil straight over its own conn parameter — `conn, isTLS, err
// := s.maybeWrapTLS(conn)` reuses conn rather than shadowing it, because the
// parameter lives in the same scope. The deferred Close() then ran on a nil
// interface, and the panic was on a per-connection goroutine with nothing to
// recover it, so the process died: listener, queue and every in-flight upload
// with it.
//
// It was found in the field as sixteen starts with zero clean exits, thirteen
// of the restarts landing 1-2s after "sniff first byte: EOF" — Electron's
// restart backoff. One X-ray abandoned a socket at the start of every study;
// any telnet would have done the same thing.
//
// These tests hold the whole chain: a zero-byte connection must not panic, a
// half-open one must not panic, and — the part that actually matters — the
// server must still be serving afterwards.

// dialAndAbandon opens a TCP connection and closes it without writing.
func dialAndAbandon(t *testing.T, addr string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// assertStillServing proves the listener survived, by completing a real
// association on a fresh connection.
func assertStillServing(t *testing.T, addr string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("server stopped accepting connections: %v", err)
	}
	defer func() { _ = c.Close() }()

	if got := associate(t, c, "ACHYU", "TESTSCU"); got != "ACHYU" {
		t.Fatalf("called AE = %q, want %q", got, "ACHYU")
	}
}

// TestZeroByteConnectionDoesNotKillServer is the regression test for the
// crash. Ten abandoned connections in a row, each followed by a real
// association: before the fix the first one took the process down.
func TestZeroByteConnectionDoesNotKillServer(t *testing.T) {
	_, addr := startTestServer(t, TLSOptions{Enabled: true, CacheDir: t.TempDir()})

	for i := 0; i < 10; i++ {
		dialAndAbandon(t, addr)
		assertStillServing(t, addr)
	}
}

// TestHalfOpenConnectionDoesNotKillServer covers the other shape of the same
// bug: the peer half-closes its write side and then goes quiet, so the sniff
// reads EOF while the socket is still open.
func TestHalfOpenConnectionDoesNotKillServer(t *testing.T) {
	_, addr := startTestServer(t, TLSOptions{Enabled: true, CacheDir: t.TempDir()})

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		t.Fatalf("expected *net.TCPConn, got %T", c)
	}
	if err := tcp.CloseWrite(); err != nil {
		t.Fatalf("close write: %v", err)
	}
	// Give the server's sniff time to hit EOF and run its deferred close.
	time.Sleep(200 * time.Millisecond)
	_ = tcp.Close()

	assertStillServing(t, addr)
}

// TestZeroByteConnectionWithTLSDisabled pins the plaintext path too. With no
// TLS config the sniff is skipped entirely, so this never panicked — but it
// documents that the abandoned connection is handled the same way either way.
func TestZeroByteConnectionWithTLSDisabled(t *testing.T) {
	_, addr := startTestServer(t, TLSOptions{Enabled: false})

	dialAndAbandon(t, addr)
	assertStillServing(t, addr)
}

// panickingHandler blows up on every stored instance. It stands in for any
// bug reachable from a single connection — the class of fault that used to
// take the listener, the queue and every in-flight upload down with it.
type panickingHandler struct{}

func (panickingHandler) OnCStore(_ context.Context, _ CStoreMessage) error {
	panic("boom: a bug on the per-connection path")
}

// startPanickingServer is startTestServer with a handler that panics.
func startPanickingServer(t *testing.T) string {
	t.Helper()
	srv := New(Config{
		Address:     "127.0.0.1:0",
		AETitle:     "ACHYU",
		IdleTimeout: 5 * time.Second,
	}, panickingHandler{})

	ctx, cancel := context.WithCancel(context.Background())
	if err := srv.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		shutdown, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Stop(shutdown)
		cancel()
	})

	addr := srv.listenerAddr()
	if addr == "" {
		t.Fatal("listener didn't bind")
	}
	return addr
}

// buildClientCStoreRQ encodes a C-STORE-RQ command set announcing that a
// dataset PDV follows.
func buildClientCStoreRQ(sopClassUID, sopInstanceUID string, messageID uint16) []byte {
	tmp := make([]byte, 0, 128)
	tmp = appendImplicit(tmp, 0x0000, 0x0002, valUI(sopClassUID))
	tmp = appendImplicit(tmp, 0x0000, 0x0100, valUS(cmdFieldCStoreRQ))
	tmp = appendImplicit(tmp, 0x0000, 0x0110, valUS(messageID))
	tmp = appendImplicit(tmp, 0x0000, 0x0700, valUS(0x0000)) // priority MEDIUM
	tmp = appendImplicit(tmp, 0x0000, 0x0800, valUS(0x0102)) // != 0x0101: dataset follows
	tmp = appendImplicit(tmp, 0x0000, 0x1000, valUI(sopInstanceUID))

	var body []byte
	body = appendImplicit(body, 0x0000, 0x0000, valUL(uint32(len(tmp))))
	return append(body, tmp...)
}

// TestPanicInHandlerDoesNotKillServer covers the last of the three layers: a
// panic anywhere on the connection goroutine is recovered, costs that one peer
// its connection, and leaves the listener serving.
func TestPanicInHandlerDoesNotKillServer(t *testing.T) {
	const storageSOPClass = "1.2.840.10008.5.1.4.1.1.7" // Secondary Capture

	addr := startPanickingServer(t)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	rq := buildClientAssociateRQ("ACHYU", "TESTSCU", []proposedContext{
		{ID: 1, AbstractSyntax: storageSOPClass, TransferSyntaxes: []string{UIDImplicitVRLittleEndian}},
	})
	if err := writePDU(conn, pduTypeAssociateRQ, rq); err != nil {
		t.Fatalf("send associate-rq: %v", err)
	}
	if ptype, _, err := readPDU(conn); err != nil || ptype != pduTypeAssociateAC {
		t.Fatalf("associate: ptype=0x%02x err=%v", ptype, err)
	}

	// Command PDV, then a one-element dataset PDV — enough to reach the
	// handler, which panics.
	cmd := buildClientCStoreRQ(storageSOPClass, "1.2.3.4.5.6.7.8", 1)
	if err := writePDU(conn, pduTypeData, buildPDataTF(1, true, true, cmd)); err != nil {
		t.Fatalf("send command pdv: %v", err)
	}
	dataset := appendImplicit(nil, 0x0008, 0x0018, valUI("1.2.3.4.5.6.7.8"))
	if err := writePDU(conn, pduTypeData, buildPDataTF(1, false, true, dataset)); err != nil {
		t.Fatalf("send data pdv: %v", err)
	}

	// The panicking connection is dropped; we don't care how it ends, only
	// that the process is still here to answer the next one.
	_ = conn.Close()

	assertStillServing(t, addr)
}
