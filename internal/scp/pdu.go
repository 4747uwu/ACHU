package scp

// DICOM Upper Layer Protocol PDU encoding/decoding (PS3.8).
//
// Wire format reference:
//
//	┌──────────────────────────────────────────────────────────────┐
//	│ PDU Header                                                   │
//	│  byte 0   : PDU type (0x01 = A-ASSOCIATE-RQ, ...)            │
//	│  byte 1   : reserved (0x00)                                  │
//	│  bytes 2-5: PDU length (BIG endian, excludes header)         │
//	└──────────────────────────────────────────────────────────────┘
//	│ PDU body (length above)                                      │
//	└──────────────────────────────────────────────────────────────┘
//
// All multi-byte integers in the upper-layer are BIG endian (network order).
// The DIMSE messages carried inside P-DATA-TF PDUs are LITTLE endian
// (negotiated transfer syntax). That asymmetry is in the spec; don't fight it.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// PDU types.
const (
	pduTypeAssociateRQ = 0x01
	pduTypeAssociateAC = 0x02
	pduTypeAssociateRJ = 0x03
	pduTypeData        = 0x04
	pduTypeReleaseRQ   = 0x05
	pduTypeReleaseRP   = 0x06
	pduTypeAbort       = 0x07
)

// Variable item types within associate PDUs.
const (
	itemApplicationContext = 0x10
	itemPresContextRQ      = 0x20
	itemPresContextAC      = 0x21
	itemAbstractSyntax     = 0x30
	itemTransferSyntax     = 0x40
	itemUserInfo           = 0x50
	itemMaxLength          = 0x51
	itemImplClassUID       = 0x52
	itemImplVersionName    = 0x55
)

// Maximum PDU length we'll accept on receive. 16 MB is generous; modalities
// rarely propose more than a few MB.
const maxAcceptablePDULength = 16 * 1024 * 1024

// readPDU reads a single PDU from r. Returns the PDU type and body bytes
// (header already stripped).
func readPDU(r io.Reader) (pduType byte, body []byte, err error) {
	var hdr [6]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	pduType = hdr[0]
	length := binary.BigEndian.Uint32(hdr[2:6])
	if length > maxAcceptablePDULength {
		return 0, nil, fmt.Errorf("pdu length %d exceeds limit", length)
	}
	body = make([]byte, length)
	if _, err = io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return pduType, body, nil
}

// writePDU writes a PDU header + body to w.
func writePDU(w io.Writer, pduType byte, body []byte) error {
	hdr := [6]byte{pduType, 0x00}
	binary.BigEndian.PutUint32(hdr[2:6], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

// ------------------------------------------------------------------------
// A-ASSOCIATE-RQ
// ------------------------------------------------------------------------

// associateRQ is the parsed form of an A-ASSOCIATE-RQ PDU body.
type associateRQ struct {
	ProtocolVersion    uint16
	CalledAETitle      string
	CallingAETitle     string
	ApplicationContext string
	PresentationCtxs   []presContextRQ
	UserInfo           userInformation
}

type presContextRQ struct {
	ID               byte
	AbstractSyntax   string
	TransferSyntaxes []string
}

type presContextAC struct {
	ID             byte
	Result         byte // 0=accept, 1=user-reject, 2=no-reason, 3=abstract-syntax-not-supported, 4=transfer-syntax-not-supported
	TransferSyntax string
}

type userInformation struct {
	MaxPDULength    uint32
	ImplClassUID    string
	ImplVersionName string
}

// parseAssociateRQ decodes the body of an A-ASSOCIATE-RQ PDU.
func parseAssociateRQ(b []byte) (*associateRQ, error) {
	if len(b) < 68 {
		return nil, fmt.Errorf("associate-rq too short: %d bytes", len(b))
	}
	rq := &associateRQ{
		ProtocolVersion: binary.BigEndian.Uint16(b[0:2]),
		CalledAETitle:   trimAE(b[4:20]),
		CallingAETitle:  trimAE(b[20:36]),
	}
	// b[36:68] is reserved
	rest := b[68:]

	for len(rest) >= 4 {
		itemType := rest[0]
		// rest[1] reserved
		itemLen := int(binary.BigEndian.Uint16(rest[2:4]))
		if 4+itemLen > len(rest) {
			return nil, fmt.Errorf("variable item overflows: type=0x%02x len=%d remaining=%d", itemType, itemLen, len(rest)-4)
		}
		payload := rest[4 : 4+itemLen]

		switch itemType {
		case itemApplicationContext:
			rq.ApplicationContext = trimUID(payload)

		case itemPresContextRQ:
			pc, err := parsePresContextRQ(payload)
			if err != nil {
				return nil, err
			}
			rq.PresentationCtxs = append(rq.PresentationCtxs, *pc)

		case itemUserInfo:
			ui, err := parseUserInfo(payload)
			if err != nil {
				return nil, err
			}
			rq.UserInfo = *ui

		default:
			// Unknown items are ignored per spec.
		}

		rest = rest[4+itemLen:]
	}

	return rq, nil
}

func parsePresContextRQ(b []byte) (*presContextRQ, error) {
	if len(b) < 4 {
		return nil, errors.New("pres-context-rq too short")
	}
	pc := &presContextRQ{ID: b[0]}
	// b[1] reserved, b[2] reserved, b[3] reserved
	rest := b[4:]
	for len(rest) >= 4 {
		itemType := rest[0]
		itemLen := int(binary.BigEndian.Uint16(rest[2:4]))
		if 4+itemLen > len(rest) {
			return nil, fmt.Errorf("sub-item overflows in pres-context-rq")
		}
		payload := rest[4 : 4+itemLen]
		switch itemType {
		case itemAbstractSyntax:
			pc.AbstractSyntax = trimUID(payload)
		case itemTransferSyntax:
			pc.TransferSyntaxes = append(pc.TransferSyntaxes, trimUID(payload))
		}
		rest = rest[4+itemLen:]
	}
	return pc, nil
}

func parseUserInfo(b []byte) (*userInformation, error) {
	ui := &userInformation{}
	rest := b
	for len(rest) >= 4 {
		itemType := rest[0]
		itemLen := int(binary.BigEndian.Uint16(rest[2:4]))
		if 4+itemLen > len(rest) {
			return nil, errors.New("sub-item overflows in user-info")
		}
		payload := rest[4 : 4+itemLen]
		switch itemType {
		case itemMaxLength:
			if len(payload) >= 4 {
				ui.MaxPDULength = binary.BigEndian.Uint32(payload[:4])
			}
		case itemImplClassUID:
			ui.ImplClassUID = trimUID(payload)
		case itemImplVersionName:
			ui.ImplVersionName = string(payload)
		}
		rest = rest[4+itemLen:]
	}
	return ui, nil
}

// ------------------------------------------------------------------------
// A-ASSOCIATE-AC
// ------------------------------------------------------------------------

// buildAssociateAC builds the body of an A-ASSOCIATE-AC PDU in response
// to the given RQ, with the given per-context decisions.
func buildAssociateAC(rq *associateRQ, contexts []presContextAC, ourMaxPDU uint32) []byte {
	var body []byte
	// Protocol version (2 bytes big-endian) = 1
	body = append(body, 0x00, 0x01)
	// Reserved 2 bytes
	body = append(body, 0x00, 0x00)
	// Called AE Title — echo back what client sent (16 bytes, space-padded)
	body = append(body, padAE(rq.CalledAETitle)...)
	// Calling AE Title — echo back (16 bytes)
	body = append(body, padAE(rq.CallingAETitle)...)
	// 32 bytes reserved
	body = append(body, make([]byte, 32)...)

	// Application Context Item (echo back the proposed context UID).
	body = append(body, encodeItem(itemApplicationContext, []byte(rq.ApplicationContext))...)

	// Per-context AC items.
	for _, ctx := range contexts {
		body = append(body, encodePresContextAC(ctx)...)
	}

	// User Information item.
	body = append(body, encodeUserInfoAC(ourMaxPDU)...)

	return body
}

func encodeItem(itemType byte, payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	out[0] = itemType
	out[1] = 0x00
	binary.BigEndian.PutUint16(out[2:4], uint16(len(payload)))
	copy(out[4:], payload)
	return out
}

func encodePresContextAC(c presContextAC) []byte {
	// Per-context body: ID, reserved, result, reserved, then transfer-syntax sub-item.
	tsItem := encodeItem(itemTransferSyntax, []byte(c.TransferSyntax))
	body := []byte{c.ID, 0x00, c.Result, 0x00}
	body = append(body, tsItem...)
	return encodeItem(itemPresContextAC, body)
}

func encodeUserInfoAC(maxPDU uint32) []byte {
	var sub []byte

	// Max-length sub-item
	maxBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(maxBuf, maxPDU)
	sub = append(sub, encodeItem(itemMaxLength, maxBuf)...)

	// Implementation Class UID
	sub = append(sub, encodeItem(itemImplClassUID, []byte(UIDImplementationClassTAR))...)

	// Implementation Version Name
	sub = append(sub, encodeItem(itemImplVersionName, []byte(UIDImplementationVersion))...)

	return encodeItem(itemUserInfo, sub)
}

// ------------------------------------------------------------------------
// A-ASSOCIATE-RJ
// ------------------------------------------------------------------------

// buildAssociateRJ builds an A-ASSOCIATE-RJ PDU body.
//
//	result: 1=permanent, 2=transient
//	source: 1=ul-service-user, 2=ul-service-provider-acse, 3=ul-service-provider-presentation
//	reason: depends on source — 1 for "no-reason-given" is always safe.
func buildAssociateRJ(result, source, reason byte) []byte {
	return []byte{0x00, result, source, reason}
}

// ------------------------------------------------------------------------
// A-ABORT
// ------------------------------------------------------------------------

func buildAbort(source, reason byte) []byte {
	return []byte{0x00, 0x00, source, reason}
}

// ------------------------------------------------------------------------
// P-DATA-TF and PDV
// ------------------------------------------------------------------------

// pdv is one Presentation Data Value within a P-DATA-TF PDU.
type pdv struct {
	ContextID byte
	IsCommand bool
	IsLast    bool
	Data      []byte
}

// parsePDataTF splits a P-DATA-TF body into its constituent PDVs.
func parsePDataTF(b []byte) ([]pdv, error) {
	var out []pdv
	for len(b) >= 6 {
		length := binary.BigEndian.Uint32(b[0:4])
		if length < 2 {
			return nil, fmt.Errorf("pdv length %d too short", length)
		}
		if int(4+length) > len(b) {
			return nil, fmt.Errorf("pdv overflows pdu: length=%d remaining=%d", length, len(b)-4)
		}
		ctxID := b[4]
		mch := b[5]
		data := b[6 : 4+length]
		out = append(out, pdv{
			ContextID: ctxID,
			IsCommand: mch&0x01 != 0,
			IsLast:    mch&0x02 != 0,
			Data:      append([]byte(nil), data...), // copy: caller may keep reference past PDU buffer
		})
		b = b[4+length:]
	}
	return out, nil
}

// buildPDataTF wraps a single PDV's data into one P-DATA-TF PDU body.
//
// For replies (which are short — under 1KB) one PDU is plenty. For larger
// outbound messages we'd fragment into multiple PDVs, but the SCP only
// sends short responses so this single-PDV form is sufficient.
func buildPDataTF(ctxID byte, isCommand, isLast bool, data []byte) []byte {
	var mch byte
	if isCommand {
		mch |= 0x01
	}
	if isLast {
		mch |= 0x02
	}
	body := make([]byte, 6+len(data))
	binary.BigEndian.PutUint32(body[0:4], uint32(2+len(data)))
	body[4] = ctxID
	body[5] = mch
	copy(body[6:], data)
	return body
}

// ------------------------------------------------------------------------
// helpers
// ------------------------------------------------------------------------

// trimAE strips trailing spaces and zero bytes from a 16-byte AE title field.
func trimAE(b []byte) string {
	end := len(b)
	for end > 0 && (b[end-1] == ' ' || b[end-1] == 0x00) {
		end--
	}
	return string(b[:end])
}

// padAE produces a 16-byte AE title field, space-padded.
//
// The only rule enforced here is the PS3.8 field width: copy truncates, so an
// over-long title cannot overflow into the next field. Character set and case
// are not policed — the title is echoed back to whatever the modality sent.
func padAE(s string) []byte {
	out := make([]byte, 16)
	for i := range out {
		out[i] = ' '
	}
	copy(out, s)
	return out
}

// trimUID strips trailing zero/space bytes from a UID field.
func trimUID(b []byte) string {
	end := len(b)
	for end > 0 && (b[end-1] == 0x00 || b[end-1] == ' ') {
		end--
	}
	return string(b[:end])
}
