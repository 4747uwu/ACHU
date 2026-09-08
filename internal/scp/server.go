// Package scp implements a minimal DICOM C-STORE / C-ECHO SCP from
// scratch over PS3.8 (DICOM Upper Layer) and PS3.7 (DIMSE).
//
// Why hand-rolled: the Go DICOM networking ecosystem is unmaintained
// (grailbio/go-netdicom is archived; forks have broken transitive deps)
// and the surface area we need is small enough that owning it is a net
// reduction in risk.
//
// What we support:
//   - A-ASSOCIATE handshake with multiple presentation contexts
//   - C-STORE-RQ for any storage SOP class proposed (presentation contexts
//     for unrecognized SOP classes are rejected, but the association
//     proceeds for the rest)
//   - C-ECHO (verification SOP class) — modalities probe with this
//   - All transfer syntaxes in SupportedTransferSyntaxes (uncompressed
//     and common compressed; we don't transcode, just store-and-forward)
//   - A-RELEASE handshake
//   - A-ABORT (we send on errors, accept from peer)
//
// What we don't support yet:
//   - Asynchronous operations, role negotiation, extended negotiation
//   - C-FIND / C-MOVE / C-GET as SCP (sender doesn't need these)
//   - DICOM TLS (Phase 2; gateway typically terminates TLS upstream)
package scp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"sync"
	"time"

	"github.com/bharatpacs/tarang-sender/internal/log"
)

// Handler processes a fully-received DICOM instance.
//
// Implementations should not block longer than a few seconds — the SCP
// holds the association open until this returns. Heavy work (tag
// injection, queueing) should kick off background tasks.
type Handler interface {
	OnCStore(ctx context.Context, msg CStoreMessage) error
}

// CStoreMessage is what we hand to the handler when an instance arrives.
type CStoreMessage struct {
	TransferSyntaxUID string
	SOPClassUID       string
	SOPInstanceUID    string
	Dataset           []byte // raw dataset bytes in the negotiated transfer syntax
	CallingAET        string
	CalledAET         string
	RemoteAddr        string
}

// Server is the C-STORE SCP. Construct with New, start with Start, stop with Stop.
type Server struct {
	cfg     Config
	handler Handler

	mu       sync.Mutex
	listener net.Listener
	wg       sync.WaitGroup
	cancel   context.CancelFunc
}

// Config tunes the server's behavior.
type Config struct {
	Address    string        // bind address, e.g. ":1007"
	AETitle    string        // display/logging only — never gates a connection
	MaxPDU     uint32        // max PDU size we'll accept on receive
	IdleTimeout time.Duration // close associations idle for this long

	// TLSConfig enables Secure DICOM on the SAME port as plaintext: each
	// connection is sniffed and wrapped only if the peer opened with a TLS
	// ClientHello. nil disables TLS entirely. Build one with
	// BuildServerTLSConfig.
	TLSConfig *tls.Config
}

// New constructs a Server. Handler is required.
func New(cfg Config, h Handler) *Server {
	if cfg.MaxPDU == 0 {
		cfg.MaxPDU = 1 * 1024 * 1024 // 1 MB default
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 60 * time.Second
	}
	return &Server{cfg: cfg, handler: h}
}

// Start begins accepting connections. Returns when the listener is bound.
// The accept loop runs in a goroutine; use Stop to shut it down cleanly.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.listener != nil {
		s.mu.Unlock()
		return errors.New("scp: server already started")
	}
	l, err := net.Listen("tcp", s.cfg.Address)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("scp listen %q: %w", s.cfg.Address, err)
	}
	s.listener = l
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()

	log.L().Info("scp listening",
		"addr", l.Addr().String(),
		"aet", s.cfg.AETitle,
		"tls", s.cfg.TLSConfig != nil,
	)

	s.wg.Add(1)
	go s.acceptLoop(ctx)
	return nil
}

// Stop closes the listener and waits for in-flight associations to drain.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.listener == nil {
		s.mu.Unlock()
		return nil
	}
	_ = s.listener.Close()
	s.listener = nil
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) acceptLoop(ctx context.Context) {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return // graceful shutdown
			}
			// Common case: net.Listener closed because Stop was called.
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.L().Warn("scp accept error", "err", err)
			// Brief backoff before retrying
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
				return
			}
			continue
		}
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			// A panic on a per-connection goroutine kills the whole process,
			// and this one is the process: the listener, the queue and every
			// in-flight upload go with it. Whatever a single peer manages to
			// provoke, it may cost that peer its connection and nothing more.
			// Logged at error level so it reaches the backend error reporter
			// instead of dying in a log file nobody reads.
			defer func() {
				if r := recover(); r != nil {
					log.L().Error("scp connection handler panicked — connection dropped, listener still serving",
						"remote", c.RemoteAddr().String(),
						"panic", fmt.Sprint(r),
						"stack", string(debug.Stack()),
					)
					_ = c.Close()
				}
			}()
			s.handleConnection(ctx, c)
		}(conn)
	}
}

// handleConnection runs the full lifecycle for one TCP connection:
// associate handshake, message loop, release.
func (s *Server) handleConnection(ctx context.Context, conn net.Conn) {
	remote := conn.RemoteAddr().String()
	log.L().Debug("scp connection accepted", "remote", remote)

	// accepted is the socket as handed to us. maybeWrapTLS below REASSIGNS
	// conn — `conn, isTLS, err := ...` reuses the parameter rather than
	// shadowing it, because a parameter shares the function's outermost scope
	// — so a defer that read conn at return time would close whatever conn had
	// become by then. When the sniff failed that was nil, and Close() on a nil
	// interface panicked on a goroutine with nothing to recover it.
	//
	// Holding the accepted socket separately means the thing that must always
	// come down always does, no matter what the identification step returned.
	accepted := conn
	defer func() {
		// Close the wrapper first when there is one, so a TLS session still
		// gets its close_notify; then the accepted socket. A second Close on
		// an already-closed conn just returns an error, which is why this can
		// be unconditional.
		if c := conn; c != nil && c != accepted {
			_ = c.Close()
		}
		_ = accepted.Close()
		log.L().Debug("scp connection closed", "remote", remote)
	}()

	// Secure DICOM and plaintext share this port; one peeked byte decides.
	conn, isTLS, err := s.maybeWrapTLS(conn)
	if err != nil {
		log.L().Warn("scp connection identification failed", "remote", remote, "err", err)
		return
	}
	if isTLS {
		tlsConn, ok := conn.(*tls.Conn)
		if !ok {
			log.L().Warn("scp tls wrap produced an unexpected connection type", "remote", remote)
			return
		}
		// Handshake explicitly so a failure is reported as a handshake
		// failure, instead of surfacing later as a garbled PDU that looks
		// like a DICOM bug.
		_ = tlsConn.SetDeadline(time.Now().Add(s.cfg.IdleTimeout))
		if err := tlsConn.Handshake(); err != nil {
			log.L().Warn("scp tls handshake failed", "remote", remote, "err", err)
			return
		}
		st := tlsConn.ConnectionState()
		log.L().Info("scp tls association",
			"remote", remote,
			"tls_version", tlsVersionName(st.Version),
			"cipher_suite", tls.CipherSuiteName(st.CipherSuite),
		)
	}

	// Set per-connection deadlines so a wedged peer can't hold a goroutine
	// forever. The deadline is reset on every successful PDU read/write.
	resetDeadline := func() {
		_ = conn.SetDeadline(time.Now().Add(s.cfg.IdleTimeout))
	}
	resetDeadline()

	// Step 1: read A-ASSOCIATE-RQ
	pduType, body, err := readPDU(conn)
	if err != nil {
		log.L().Warn("scp read associate-rq", "remote", remote, "err", err)
		return
	}
	if pduType != pduTypeAssociateRQ {
		log.L().Warn("scp expected associate-rq", "remote", remote, "got", fmt.Sprintf("0x%02x", pduType))
		_ = writePDU(conn, pduTypeAbort, buildAbort(2, 2)) // unexpected pdu
		return
	}
	rq, err := parseAssociateRQ(body)
	if err != nil {
		log.L().Warn("scp parse associate-rq", "remote", remote, "err", err)
		_ = writePDU(conn, pduTypeAbort, buildAbort(2, 6)) // invalid pdu parameter
		return
	}

	// The called AE title is deliberately NOT checked. It is caller-supplied
	// and unauthenticated, so enforcing it buys no security — all it ever
	// produced in the field was called-AE-title-not-recognized rejections
	// when a site's engineer typed the destination name differently from the
	// config. Any title associates, and buildAssociateAC echoes it back
	// verbatim. The two things that must actually be right on the modality
	// are the IP and the port; s.cfg.AETitle is for display and logging.
	if s.cfg.AETitle != "" && rq.CalledAETitle != s.cfg.AETitle {
		log.L().Debug("scp called-ae differs from configured title — accepting anyway",
			"remote", remote,
			"configured", s.cfg.AETitle,
			"got", rq.CalledAETitle,
		)
	}

	// Step 2: build A-ASSOCIATE-AC. Accept all known SOP classes with
	// their best supported transfer syntax.
	ctxByID := make(map[byte]presContextRQ, len(rq.PresentationCtxs))
	acDecisions := make([]presContextAC, 0, len(rq.PresentationCtxs))
	for _, pc := range rq.PresentationCtxs {
		ctxByID[pc.ID] = pc
		ts := pickPreferredTransferSyntax(pc.TransferSyntaxes)
		if ts == "" {
			// transfer-syntaxes-not-supported (4)
			acDecisions = append(acDecisions, presContextAC{
				ID:             pc.ID,
				Result:         4,
				TransferSyntax: "",
			})
			continue
		}
		// For C-STORE-style SCP we accept any abstract syntax — the
		// receiver-side Orthanc decides whether it actually wants to
		// store it. C-ECHO is also implicitly accepted via this path.
		acDecisions = append(acDecisions, presContextAC{
			ID:             pc.ID,
			Result:         0, // accept
			TransferSyntax: ts,
		})
	}

	// Step 3: write A-ASSOCIATE-AC
	resetDeadline()
	if err := writePDU(conn, pduTypeAssociateAC, buildAssociateAC(rq, acDecisions, s.cfg.MaxPDU)); err != nil {
		log.L().Warn("scp write associate-ac", "remote", remote, "err", err)
		return
	}

	log.L().Info("scp association accepted",
		"remote", remote,
		"calling_aet", rq.CallingAETitle,
		"called_aet", rq.CalledAETitle,
		"contexts", len(rq.PresentationCtxs),
		"impl_class", rq.UserInfo.ImplClassUID,
	)

	// Step 4: per-context state. A single association can interleave
	// messages across contexts; track one accumulator per context.
	acceptedCtx := make(map[byte]presContextAC, len(acDecisions))
	for _, d := range acDecisions {
		if d.Result == 0 {
			acceptedCtx[d.ID] = d
		}
	}

	// One DIMSE message at a time per context (DICOM doesn't allow
	// interleaving fragments of multiple messages on one context).
	type msgState struct {
		commandBuf []byte
		dataBuf    []byte
		commandDone bool
	}
	state := make(map[byte]*msgState)

	// Step 5: message loop
	for {
		resetDeadline()
		pduType, body, err := readPDU(conn)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				log.L().Warn("scp read pdu", "remote", remote, "err", err)
			}
			return
		}

		switch pduType {
		case pduTypeData:
			pdvs, err := parsePDataTF(body)
			if err != nil {
				log.L().Warn("scp parse p-data-tf", "remote", remote, "err", err)
				_ = writePDU(conn, pduTypeAbort, buildAbort(2, 6))
				return
			}
			for _, p := range pdvs {
				ctx, ok := acceptedCtx[p.ContextID]
				if !ok {
					log.L().Warn("scp pdv on rejected context", "remote", remote, "ctx", p.ContextID)
					_ = writePDU(conn, pduTypeAbort, buildAbort(2, 4))
					return
				}
				st := state[p.ContextID]
				if st == nil {
					st = &msgState{}
					state[p.ContextID] = st
				}
				if p.IsCommand {
					st.commandBuf = append(st.commandBuf, p.Data...)
					if p.IsLast {
						st.commandDone = true
					}
				} else {
					st.dataBuf = append(st.dataBuf, p.Data...)
				}

				// A complete DIMSE message is: full command + (if dataset
				// follows) full dataset. We dispatch when we have both.
				if !st.commandDone {
					continue
				}
				cmd, err := parseDIMSECommand(st.commandBuf)
				if err != nil {
					log.L().Warn("scp parse dimse", "remote", remote, "err", err)
					_ = writePDU(conn, pduTypeAbort, buildAbort(2, 6))
					return
				}
				if cmd.hasDataSet() && !p.IsLast {
					continue // wait for last data fragment
				}
				if cmd.hasDataSet() && !(p.IsCommand == false && p.IsLast) {
					continue
				}

				// Dispatch.
				if err := s.dispatchMessage(ctx2(ctx, rq, remote), conn, ctx, cmd, st.dataBuf); err != nil {
					log.L().Warn("scp dispatch", "remote", remote, "err", err)
					// Best-effort response was attempted by dispatch; abort if it bubbled up.
					_ = writePDU(conn, pduTypeAbort, buildAbort(2, 0))
					return
				}
				delete(state, p.ContextID)
			}

		case pduTypeReleaseRQ:
			// 4 reserved bytes per spec; reply with A-RELEASE-RP and close.
			_ = writePDU(conn, pduTypeReleaseRP, []byte{0x00, 0x00, 0x00, 0x00})
			log.L().Info("scp association released", "remote", remote)
			return

		case pduTypeAbort:
			source := byte(0)
			reason := byte(0)
			if len(body) >= 4 {
				source = body[2]
				reason = body[3]
			}
			log.L().Info("scp peer abort", "remote", remote, "source", source, "reason", reason)
			return

		default:
			log.L().Warn("scp unexpected pdu", "remote", remote, "type", fmt.Sprintf("0x%02x", pduType))
			_ = writePDU(conn, pduTypeAbort, buildAbort(2, 2))
			return
		}
	}
}

// dispatchMessage handles one fully-received DIMSE message.
func (s *Server) dispatchMessage(ctx context.Context, conn net.Conn, pc presContextAC, cmd *dimseCommand, dataset []byte) error {
	switch cmd.CommandField {
	case cmdFieldCEchoRQ:
		log.L().Debug("scp c-echo-rq", "msg_id", cmd.MessageID)
		body := buildCEchoResponse(cmd)
		if err := sendDIMSEResponse(conn, pc.ID, body); err != nil {
			return err
		}

	case cmdFieldCStoreRQ:
		// Hand the dataset to the handler.
		msg := CStoreMessage{
			TransferSyntaxUID: pc.TransferSyntax,
			SOPClassUID:       cmd.AffectedSOPClass,
			SOPInstanceUID:    cmd.AffectedSOPInst,
			Dataset:           dataset,
			RemoteAddr:        conn.RemoteAddr().String(),
		}
		// Pull AE titles if we stashed them in ctx.
		if v, ok := ctx.Value(ctxKeyCallingAET).(string); ok {
			msg.CallingAET = v
		}
		if v, ok := ctx.Value(ctxKeyCalledAET).(string); ok {
			msg.CalledAET = v
		}

		status := uint16(dimseStatusSuccess)
		if err := s.handler.OnCStore(ctx, msg); err != nil {
			log.L().Error("scp handler OnCStore", "sop_uid", cmd.AffectedSOPInst, "err", err)
			status = dimseStatusProcessingFailed
		}

		body := buildCStoreResponse(cmd, status)
		if err := sendDIMSEResponse(conn, pc.ID, body); err != nil {
			return err
		}

	default:
		log.L().Warn("scp unsupported dimse command", "cmd_field", fmt.Sprintf("0x%04x", cmd.CommandField))
		// For unrecognized commands we technically should send a status-rsp;
		// for now, just abort. Real SCPs should be more graceful.
		return fmt.Errorf("unsupported DIMSE command 0x%04x", cmd.CommandField)
	}

	return nil
}

func sendDIMSEResponse(conn net.Conn, ctxID byte, commandBytes []byte) error {
	pdu := buildPDataTF(ctxID, true, true, commandBytes)
	return writePDU(conn, pduTypeData, pdu)
}

// Context keys (typed to avoid collision).
type ctxKey int

const (
	ctxKeyCallingAET ctxKey = iota
	ctxKeyCalledAET
)

func ctx2(_ presContextAC, rq *associateRQ, remote string) context.Context {
	ctx := context.Background()
	ctx = context.WithValue(ctx, ctxKeyCallingAET, rq.CallingAETitle)
	ctx = context.WithValue(ctx, ctxKeyCalledAET, rq.CalledAETitle)
	_ = remote
	return ctx
}
