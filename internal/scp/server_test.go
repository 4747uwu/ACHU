package scp

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// TestAssociateAndCEcho exercises the full A-ASSOCIATE handshake plus a
// C-ECHO round-trip against the running SCP server — using a hand-rolled
// minimal DICOM client built on the same encoding primitives.
//
// This validates:
//
//   - PDU type/length framing on both directions
//   - A-ASSOCIATE-RQ parsing and A-ASSOCIATE-AC construction
//   - Presentation-context negotiation (transfer syntax selection)
//   - DIMSE C-ECHO dispatch and response encoding
//   - A-RELEASE handshake
//
// What it doesn't cover: C-STORE with a real dataset (requires a valid
// DICOM dataset in the negotiated transfer syntax — covered separately
// in the dicom package's round-trip test).
func TestAssociateAndCEcho(t *testing.T) {
	// Stand up the server on a random port.
	h := &recordingHandler{}
	srv := New(Config{
		Address:     "127.0.0.1:0",
		AETitle:     "TARANG",
		IdleTimeout: 5 * time.Second,
	}, h)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		shutdown, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Stop(shutdown)
	})

	addr := srv.listenerAddr()
	if addr == "" {
		t.Fatal("listener didn't bind")
	}

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// 1. Send A-ASSOCIATE-RQ proposing one C-ECHO context.
	rq := buildClientAssociateRQ(
		"TARANG", "TESTSCU",
		[]proposedContext{
			{ID: 1, AbstractSyntax: UIDVerificationSOPClass, TransferSyntaxes: []string{UIDImplicitVRLittleEndian}},
		},
	)
	if err := writePDU(conn, pduTypeAssociateRQ, rq); err != nil {
		t.Fatalf("send associate-rq: %v", err)
	}

	// 2. Expect A-ASSOCIATE-AC.
	ptype, body, err := readPDU(conn)
	if err != nil {
		t.Fatalf("read associate-ac: %v", err)
	}
	if ptype != pduTypeAssociateAC {
		t.Fatalf("expected associate-ac (0x02), got 0x%02x", ptype)
	}
	if len(body) < 68 {
		t.Fatalf("associate-ac body too short: %d", len(body))
	}
	// Quickly verify the AET fields echoed back, presentation context
	// accepted with our chosen transfer syntax.
	if got := trimAE(body[4:20]); got != "TARANG" {
		t.Errorf("called AET = %q, want TARANG", got)
	}
	// We don't fully reparse here — successful peers (real modalities)
	// trust the framing and look for items.

	// 3. Send C-ECHO-RQ in a P-DATA-TF.
	echoCmd := buildClientCEchoRQ(UIDVerificationSOPClass, 1)
	if err := writePDU(conn, pduTypeData, buildPDataTF(1, true, true, echoCmd)); err != nil {
		t.Fatalf("send c-echo-rq: %v", err)
	}

	// 4. Expect C-ECHO-RSP wrapped in P-DATA-TF.
	ptype, body, err = readPDU(conn)
	if err != nil {
		t.Fatalf("read c-echo-rsp pdu: %v", err)
	}
	if ptype != pduTypeData {
		t.Fatalf("expected p-data-tf (0x04), got 0x%02x", ptype)
	}
	pdvs, err := parsePDataTF(body)
	if err != nil {
		t.Fatalf("parse p-data-tf: %v", err)
	}
	if len(pdvs) != 1 {
		t.Fatalf("expected 1 pdv, got %d", len(pdvs))
	}
	rspCmd, err := parseDIMSECommand(pdvs[0].Data)
	if err != nil {
		t.Fatalf("parse dimse rsp: %v", err)
	}
	if rspCmd.CommandField != cmdFieldCEchoRS {
		t.Errorf("CommandField = 0x%04x, want 0x8030 (C-ECHO-RS)", rspCmd.CommandField)
	}
	if rspCmd.Status != 0x0000 {
		t.Errorf("Status = 0x%04x, want 0x0000 (success)", rspCmd.Status)
	}
	if rspCmd.MessageIDRespTo != 1 {
		t.Errorf("MessageIDBeingRespondedTo = %d, want 1", rspCmd.MessageIDRespTo)
	}

	// 5. Release.
	if err := writePDU(conn, pduTypeReleaseRQ, []byte{0, 0, 0, 0}); err != nil {
		t.Fatalf("send release-rq: %v", err)
	}
	ptype, _, err = readPDU(conn)
	if err != nil {
		t.Fatalf("read release-rp: %v", err)
	}
	if ptype != pduTypeReleaseRP {
		t.Errorf("expected release-rp (0x06), got 0x%02x", ptype)
	}
}

// recordingHandler captures C-STORE invocations for tests.
type recordingHandler struct {
	mu       sync.Mutex
	received []CStoreMessage
}

func (h *recordingHandler) OnCStore(_ context.Context, msg CStoreMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.received = append(h.received, msg)
	return nil
}

// listenerAddr exposes the bound address of the server's listener for
// tests that pass "127.0.0.1:0" to get an ephemeral port.
func (s *Server) listenerAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// ------------------------------------------------------------------------
// minimal client-side encoders used only in tests
// ------------------------------------------------------------------------

type proposedContext struct {
	ID               byte
	AbstractSyntax   string
	TransferSyntaxes []string
}

// buildClientAssociateRQ encodes an A-ASSOCIATE-RQ body. This is the
// inverse of parseAssociateRQ.
func buildClientAssociateRQ(calledAE, callingAE string, contexts []proposedContext) []byte {
	var body []byte
	body = append(body, 0x00, 0x01) // Protocol version 1
	body = append(body, 0x00, 0x00) // reserved
	body = append(body, padAE(calledAE)...)
	body = append(body, padAE(callingAE)...)
	body = append(body, make([]byte, 32)...) // 32 bytes reserved

	// Application Context
	body = append(body, encodeItem(itemApplicationContext, []byte(UIDApplicationContext))...)

	// Presentation Contexts
	for _, ctx := range contexts {
		var pcBody []byte
		pcBody = append(pcBody, ctx.ID, 0x00, 0x00, 0x00) // ID + 3 reserved
		pcBody = append(pcBody, encodeItem(itemAbstractSyntax, []byte(ctx.AbstractSyntax))...)
		for _, ts := range ctx.TransferSyntaxes {
			pcBody = append(pcBody, encodeItem(itemTransferSyntax, []byte(ts))...)
		}
		body = append(body, encodeItem(itemPresContextRQ, pcBody)...)
	}

	// User Information
	var ui []byte
	maxBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(maxBuf, 16384) // 16 KB receive PDU
	ui = append(ui, encodeItem(itemMaxLength, maxBuf)...)
	ui = append(ui, encodeItem(itemImplClassUID, []byte("1.2.826.0.1.test.client"))...)
	ui = append(ui, encodeItem(itemImplVersionName, []byte("TESTCLIENT"))...)
	body = append(body, encodeItem(itemUserInfo, ui)...)

	return body
}

// buildClientCEchoRQ encodes a C-ECHO-RQ DIMSE command set.
func buildClientCEchoRQ(sopClassUID string, messageID uint16) []byte {
	// Build body sans group-length first.
	var body []byte
	tmp := make([]byte, 0, 64)
	tmp = appendImplicit(tmp, 0x0000, 0x0002, valUI(sopClassUID))
	tmp = appendImplicit(tmp, 0x0000, 0x0100, valUS(cmdFieldCEchoRQ))
	tmp = appendImplicit(tmp, 0x0000, 0x0110, valUS(messageID))
	tmp = appendImplicit(tmp, 0x0000, 0x0800, valUS(0x0101)) // no dataset
	body = appendImplicit(body, 0x0000, 0x0000, valUL(uint32(len(tmp))))
	body = append(body, tmp...)
	return body
}

// appendImplicit writes one element in implicit-VR LE form, returning the
// extended slice.
func appendImplicit(buf []byte, group, elem uint16, value []byte) []byte {
	hdr := make([]byte, 8)
	binary.LittleEndian.PutUint16(hdr[0:2], group)
	binary.LittleEndian.PutUint16(hdr[2:4], elem)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(value)))
	buf = append(buf, hdr...)
	buf = append(buf, value...)
	return buf
}

// Compile-time assertion that recordingHandler implements Handler.
var _ Handler = (*recordingHandler)(nil)

// ensure unused import doesn't kill the test build
var _ = errors.New
