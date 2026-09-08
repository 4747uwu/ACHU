package scp

// DIMSE message encoding/decoding (PS3.7).
//
// DIMSE command sets are always encoded in Implicit VR Little Endian
// regardless of the negotiated transfer syntax for the dataset. This is
// fixed by the spec — don't try to negotiate it away.
//
// Implicit VR LE element format:
//
//	bytes 0-1 : group number   (little endian)
//	bytes 2-3 : element number (little endian)
//	bytes 4-7 : value length   (little endian, 4-byte uint)
//	bytes 8+  : value          (length above)
//
// For most VRs, even-length values are required (pad with NUL or space
// per VR rules; UI uses NUL, others use space). We pad to even.

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// Command field values from PS3.7.
const (
	cmdFieldCStoreRQ = 0x0001
	cmdFieldCStoreRS = 0x8001
	cmdFieldCEchoRQ  = 0x0030
	cmdFieldCEchoRS  = 0x8030
)

// DIMSE status codes (PS3.7 Annex C). Only the ones we send.
const (
	dimseStatusSuccess          = 0x0000
	dimseStatusProcessingFailed = 0x0110
)

// dimseCommand is a parsed DIMSE command set.
type dimseCommand struct {
	GroupLength       uint32
	AffectedSOPClass  string
	CommandField      uint16
	MessageID         uint16
	MessageIDRespTo   uint16
	Priority          uint16
	DataSetType       uint16
	Status            uint16
	AffectedSOPInst   string
}

// hasDataSet reports whether a dataset PDV stream follows this command.
// Per PS3.7: DataSetType != 0x0101 means a dataset follows.
func (c dimseCommand) hasDataSet() bool {
	return c.DataSetType != 0x0101
}

// parseDIMSECommand decodes a DIMSE command set encoded in implicit-VR LE.
func parseDIMSECommand(b []byte) (*dimseCommand, error) {
	cmd := &dimseCommand{}
	for len(b) >= 8 {
		group := binary.LittleEndian.Uint16(b[0:2])
		elem := binary.LittleEndian.Uint16(b[2:4])
		length := binary.LittleEndian.Uint32(b[4:8])
		if 8+int(length) > len(b) {
			return nil, fmt.Errorf("dimse element overflows: (%04x,%04x) len=%d remaining=%d", group, elem, length, len(b)-8)
		}
		val := b[8 : 8+length]
		if group == 0x0000 {
			switch elem {
			case 0x0000:
				if length >= 4 {
					cmd.GroupLength = binary.LittleEndian.Uint32(val[:4])
				}
			case 0x0002:
				cmd.AffectedSOPClass = trimUID(val)
			case 0x0100:
				if length >= 2 {
					cmd.CommandField = binary.LittleEndian.Uint16(val[:2])
				}
			case 0x0110:
				if length >= 2 {
					cmd.MessageID = binary.LittleEndian.Uint16(val[:2])
				}
			case 0x0120:
				if length >= 2 {
					cmd.MessageIDRespTo = binary.LittleEndian.Uint16(val[:2])
				}
			case 0x0700:
				if length >= 2 {
					cmd.Priority = binary.LittleEndian.Uint16(val[:2])
				}
			case 0x0800:
				if length >= 2 {
					cmd.DataSetType = binary.LittleEndian.Uint16(val[:2])
				}
			case 0x0900:
				if length >= 2 {
					cmd.Status = binary.LittleEndian.Uint16(val[:2])
				}
			case 0x1000:
				cmd.AffectedSOPInst = trimUID(val)
			}
		}
		b = b[8+length:]
	}
	return cmd, nil
}

// buildCStoreResponse encodes a C-STORE-RSP command set with the given
// status and references back to the original C-STORE-RQ.
func buildCStoreResponse(reqCmd *dimseCommand, status uint16) []byte {
	var body bytes.Buffer

	// We build the body without the group-length element first, then
	// prepend the group-length element pointing at the body's size.
	writeImplicitElement(&body, 0x0000, 0x0002, valUI(reqCmd.AffectedSOPClass))
	writeImplicitElement(&body, 0x0000, 0x0100, valUS(cmdFieldCStoreRS))
	writeImplicitElement(&body, 0x0000, 0x0120, valUS(reqCmd.MessageID))
	writeImplicitElement(&body, 0x0000, 0x0800, valUS(0x0101)) // no dataset follows
	writeImplicitElement(&body, 0x0000, 0x0900, valUS(status))
	writeImplicitElement(&body, 0x0000, 0x1000, valUI(reqCmd.AffectedSOPInst))

	var out bytes.Buffer
	writeImplicitElement(&out, 0x0000, 0x0000, valUL(uint32(body.Len())))
	out.Write(body.Bytes())
	return out.Bytes()
}

// buildCEchoResponse encodes a C-ECHO-RSP. C-ECHO is the DICOM "ping" —
// we must support it for connectivity verification (storescu/orthanc/etc.
// often probe with it before initiating C-STORE).
func buildCEchoResponse(reqCmd *dimseCommand) []byte {
	var body bytes.Buffer
	writeImplicitElement(&body, 0x0000, 0x0002, valUI(reqCmd.AffectedSOPClass))
	writeImplicitElement(&body, 0x0000, 0x0100, valUS(cmdFieldCEchoRS))
	writeImplicitElement(&body, 0x0000, 0x0120, valUS(reqCmd.MessageID))
	writeImplicitElement(&body, 0x0000, 0x0800, valUS(0x0101))
	writeImplicitElement(&body, 0x0000, 0x0900, valUS(dimseStatusSuccess))

	var out bytes.Buffer
	writeImplicitElement(&out, 0x0000, 0x0000, valUL(uint32(body.Len())))
	out.Write(body.Bytes())
	return out.Bytes()
}

// writeImplicitElement appends one element in implicit-VR LE form to buf.
func writeImplicitElement(buf *bytes.Buffer, group, elem uint16, value []byte) {
	var hdr [8]byte
	binary.LittleEndian.PutUint16(hdr[0:2], group)
	binary.LittleEndian.PutUint16(hdr[2:4], elem)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(value)))
	buf.Write(hdr[:])
	buf.Write(value)
}

// valUS encodes a 16-bit unsigned (US VR).
func valUS(v uint16) []byte {
	out := make([]byte, 2)
	binary.LittleEndian.PutUint16(out, v)
	return out
}

// valUL encodes a 32-bit unsigned (UL VR).
func valUL(v uint32) []byte {
	out := make([]byte, 4)
	binary.LittleEndian.PutUint32(out, v)
	return out
}

// valUI encodes a UID with NUL padding to even length.
func valUI(s string) []byte {
	b := []byte(s)
	if len(b)%2 == 1 {
		b = append(b, 0x00)
	}
	return b
}
