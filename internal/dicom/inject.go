package dicom

// Tag injection — surgical insertion of private DICOM elements into a
// dataset without touching the rest of the byte stream.
//
// Why surgical (vs. full re-encode):
//
//   - We never touch pixel data, so compressed transfer syntaxes (JPEG,
//     JPEG-LS, JPEG 2000, RLE) work transparently — encapsulated PixelData
//     comes much later in the dataset and we don't scan past it.
//   - We preserve the modality's exact bytes for every other element,
//     including private blocks we don't own and odd VRs the parser
//     might have downgraded to UN.
//   - Idempotent: replacing an existing element produces the same output
//     as inserting it the first time.
//
// What's supported:
//
//   - Implicit VR Little Endian          (1.2.840.10008.1.2)
//   - Explicit VR Little Endian          (1.2.840.10008.1.2.1)
//   - All compressed/encapsulated TSes — non-PixelData elements are in
//     Explicit VR LE per PS3.5, so the same encoder path works
//
// What's not supported (returns ErrInjectionUnsupported):
//
//   - Big Endian (retired in DICOM 2007; rare in modern modalities)
//   - Deflated Explicit VR LE (whole dataset is DEFLATE-compressed)

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// UID constants for the transfer syntaxes the injector needs to recognize.
// Duplicated from internal/scp/uids.go to keep this package independent.
const (
	uidImplicitVRLittleEndian  = "1.2.840.10008.1.2"
	uidExplicitVRBigEndian     = "1.2.840.10008.1.2.2"
	uidDeflatedExplicitVRLE    = "1.2.840.10008.1.2.1.99"
)

// PrivateTag is one private DICOM element to inject.
type PrivateTag struct {
	Group   uint16
	Element uint16
	VR      string // typically "LO" for the values we inject
	Value   string
}

// ErrInjectionUnsupported is returned for transfer syntaxes the injector
// can't handle. Callers should fall back to passing the dataset through
// unchanged (logging a warning).
var ErrInjectionUnsupported = errors.New("tag injection not supported for this transfer syntax")

var errUndefinedLengthElement = errors.New("undefined-length element")

// StandardPrivateTags returns the canonical set of tags injected by
// TARANG, matching the legacy tagwrite.lua behavior.
//
// Groups 0x0013/0x0015 use the per-lab private creator (typically the
// lab ID like "UJJ1"). Groups 0x0021/0x0043 use the organization-wide
// private creator (typically "UJJ"). The element 0x1060 in each group
// holds a fixed marker string that downstream tooling looks for.
func StandardPrivateTags(privateCreator, privateOrganisation string) []PrivateTag {
	return []PrivateTag{
		{Group: 0x0013, Element: 0x0010, VR: "LO", Value: privateCreator},
		{Group: 0x0013, Element: 0x1060, VR: "LO", Value: "ab"},
		{Group: 0x0015, Element: 0x0010, VR: "LO", Value: privateCreator},
		{Group: 0x0015, Element: 0x1060, VR: "LO", Value: "xcentic-fallback-1"},
		{Group: 0x0021, Element: 0x0010, VR: "LO", Value: privateOrganisation},
		{Group: 0x0021, Element: 0x1060, VR: "LO", Value: "xcentic-study-key"},
		{Group: 0x0043, Element: 0x0010, VR: "LO", Value: privateOrganisation},
		{Group: 0x0043, Element: 0x1060, VR: "LO", Value: "xcentic-link-uuid"},
	}
}

// InjectPrivateTags returns a copy of dataset with the given tags inserted
// or replaced in tag order. Tags are encoded in the dataset's transfer
// syntax. For unsupported syntaxes, returns ErrInjectionUnsupported.
func InjectPrivateTags(dataset []byte, transferSyntaxUID string, tags []PrivateTag) ([]byte, error) {
	if len(tags) == 0 {
		return dataset, nil
	}

	switch transferSyntaxUID {
	case uidExplicitVRBigEndian:
		return nil, fmt.Errorf("%w: big endian", ErrInjectionUnsupported)
	case uidDeflatedExplicitVRLE:
		return nil, fmt.Errorf("%w: deflated", ErrInjectionUnsupported)
	}

	isImplicit := transferSyntaxUID == uidImplicitVRLittleEndian

	// Sort tags by (group, element) ascending — DICOM datasets must be
	// in tag order.
	sortedTags := append([]PrivateTag(nil), tags...)
	sort.SliceStable(sortedTags, func(i, j int) bool {
		if sortedTags[i].Group != sortedTags[j].Group {
			return sortedTags[i].Group < sortedTags[j].Group
		}
		return sortedTags[i].Element < sortedTags[j].Element
	})

	var out bytes.Buffer
	out.Grow(len(dataset) + 64*len(sortedTags))

	pos := 0
	nextTagIdx := 0

	for pos < len(dataset) {
		if pos+4 > len(dataset) {
			break
		}
		group := binary.LittleEndian.Uint16(dataset[pos : pos+2])
		elem := binary.LittleEndian.Uint16(dataset[pos+2 : pos+4])

		valueLen, headerLen, err := parseElementHeader(dataset, pos, isImplicit)
		if err != nil {
			// If we encounter undefined-length encoding (common with
			// encapsulated/compressed payload structures), we can still
			// complete injection safely when all remaining tags sort
			// before this element. In that case, write pending tags and
			// copy the remainder verbatim.
			if errors.Is(err, errUndefinedLengthElement) {
				// Write any pending tags that sort before this undefined-length element.
				for nextTagIdx < len(sortedTags) {
					nt := sortedTags[nextTagIdx]
					if nt.Group < group || (nt.Group == group && nt.Element < elem) {
						writeInjectedElement(&out, nt, isImplicit)
						nextTagIdx++
						continue
					}
					break
				}

				// Determine the header length so we can find the content start.
				hLen := 8
				if !isImplicit && pos+6 <= len(dataset) {
					if isBigVR(string(dataset[pos+4 : pos+6])) {
						hLen = 12
					}
				}

				// Try to scan past this undefined-length structure (SQ or encapsulated
				// pixel data) by following FFFE delimiter tags. If successful, we copy
				// the element verbatim and continue scanning — allowing tags that sort
				// after it to be injected at their correct positions.
				endPos, skipErr := skipUndefinedLengthContent(dataset, pos+hLen, false, isImplicit)
				if skipErr == nil {
					out.Write(dataset[pos:endPos])
					pos = endPos
					continue
				}

				// Fallback: cannot scan past (malformed or deeply unusual structure).
				// Copy remainder verbatim and append any remaining tags at end.
				out.Write(dataset[pos:])
				for nextTagIdx < len(sortedTags) {
					writeInjectedElement(&out, sortedTags[nextTagIdx], isImplicit)
					nextTagIdx++
				}
				return out.Bytes(), nil
			}

			// We can't safely continue scanning through this dataset.
			return nil, fmt.Errorf("scan element at offset %d: %w", pos, err)
		}

		// Write any tags that should come BEFORE this element.
		for nextTagIdx < len(sortedTags) {
			nt := sortedTags[nextTagIdx]
			if nt.Group < group || (nt.Group == group && nt.Element < elem) {
				writeInjectedElement(&out, nt, isImplicit)
				nextTagIdx++
			} else {
				break
			}
		}

		// If the next tag matches this element exactly, we're replacing —
		// skip the existing element and write the new value.
		if nextTagIdx < len(sortedTags) {
			nt := sortedTags[nextTagIdx]
			if nt.Group == group && nt.Element == elem {
				writeInjectedElement(&out, nt, isImplicit)
				nextTagIdx++
				pos += headerLen + int(valueLen)
				continue
			}
		}

		// Copy the existing element verbatim.
		end := pos + headerLen + int(valueLen)
		if end > len(dataset) {
			end = len(dataset)
		}
		out.Write(dataset[pos:end])
		pos = end
	}

	// Tags that should come after every existing element.
	for nextTagIdx < len(sortedTags) {
		writeInjectedElement(&out, sortedTags[nextTagIdx], isImplicit)
		nextTagIdx++
	}

	return out.Bytes(), nil
}

// skipUndefinedLengthContent scans data starting at contentStart (the first
// byte after the header of an undefined-length element) and finds the matching
// DICOM delimitation item. Returns the offset of the first byte after that
// delimiter so the caller can resume normal scanning.
//
// isItemContent=false → scanning a Sequence body, terminated by (FFFE,E0DD).
// isItemContent=true  → scanning an Item body,     terminated by (FFFE,E00D).
//
// FFFE-group tags (Items and Delimiters) are always 8 bytes with no VR field.
// Nested undefined-length structures are handled recursively.
func skipUndefinedLengthContent(data []byte, contentStart int, isItemContent bool, isImplicit bool) (int, error) {
	pos := contentStart
	for pos < len(data) {
		if pos+4 > len(data) {
			return 0, fmt.Errorf("data truncated at %d while scanning undefined-length content", pos)
		}
		group := binary.LittleEndian.Uint16(data[pos : pos+2])
		elem := binary.LittleEndian.Uint16(data[pos+2 : pos+4])

		if group == 0xFFFE {
			if pos+8 > len(data) {
				return 0, fmt.Errorf("truncated FFFE tag at offset %d", pos)
			}
			itemLen := binary.LittleEndian.Uint32(data[pos+4 : pos+8])
			switch elem {
			case 0xE000: // Item begin
				pos += 8
				if itemLen == 0xFFFFFFFF {
					var err error
					pos, err = skipUndefinedLengthContent(data, pos, true, isImplicit)
					if err != nil {
						return 0, err
					}
				} else {
					pos += int(itemLen)
				}
			case 0xE00D: // Item Delimitation Item
				pos += 8
				if isItemContent {
					return pos, nil
				}
			case 0xE0DD: // Sequence Delimitation Item
				pos += 8
				if !isItemContent {
					return pos, nil
				}
			default:
				pos += 8
			}
			continue
		}

		// Regular element: parse header and skip its value.
		valueLen, headerLen, err := parseElementHeader(data, pos, isImplicit)
		if err != nil {
			if errors.Is(err, errUndefinedLengthElement) {
				// Nested undefined-length element (e.g. a nested SQ).
				hLen := 8
				if !isImplicit && pos+6 <= len(data) {
					if isBigVR(string(data[pos+4 : pos+6])) {
						hLen = 12
					}
				}
				pos += hLen
				pos, err = skipUndefinedLengthContent(data, pos, false, isImplicit)
				if err != nil {
					return 0, err
				}
				continue
			}
			return 0, fmt.Errorf("parse element header at %d: %w", pos, err)
		}
		pos += headerLen + int(valueLen)
	}
	return 0, fmt.Errorf("end of data without delimitation item (contentStart=%d isItem=%v)", contentStart, isItemContent)
}

// parseElementHeader reads one DICOM element header at offset pos and
// returns the value length and total header length.
//
// Returns an error for undefined-length elements (0xFFFFFFFF), which
// the surgical injector can't safely scan past at the dataset top level
// without parsing nested structure. Callers should treat this as
// "injection unsupported for this dataset" and fall back.
func parseElementHeader(data []byte, pos int, isImplicit bool) (valueLen uint32, headerLen int, err error) {
	if pos+4 > len(data) {
		return 0, 0, fmt.Errorf("truncated tag at offset %d", pos)
	}

	if isImplicit {
		if pos+8 > len(data) {
			return 0, 0, fmt.Errorf("truncated implicit element at offset %d", pos)
		}
		valueLen = binary.LittleEndian.Uint32(data[pos+4 : pos+8])
		headerLen = 8
	} else {
		if pos+8 > len(data) {
			return 0, 0, fmt.Errorf("truncated explicit element at offset %d", pos)
		}
		vr := string(data[pos+4 : pos+6])
		if isBigVR(vr) {
			if pos+12 > len(data) {
				return 0, 0, fmt.Errorf("truncated big-VR element at offset %d", pos)
			}
			valueLen = binary.LittleEndian.Uint32(data[pos+8 : pos+12])
			headerLen = 12
		} else {
			valueLen = uint32(binary.LittleEndian.Uint16(data[pos+6 : pos+8]))
			headerLen = 8
		}
	}

	if valueLen == 0xFFFFFFFF {
		return 0, 0, fmt.Errorf("%w at offset %d", errUndefinedLengthElement, pos)
	}

	return valueLen, headerLen, nil
}

// writeInjectedElement encodes one tag in the appropriate transfer-syntax
// element format and writes it to buf.
func writeInjectedElement(buf *bytes.Buffer, t PrivateTag, isImplicit bool) {
	value := []byte(t.Value)
	// Pad to even length per DICOM. UI uses NUL, others use space. For our
	// LO values, space-pad is correct.
	if len(value)%2 == 1 {
		if t.VR == "UI" {
			value = append(value, 0x00)
		} else {
			value = append(value, ' ')
		}
	}

	if isImplicit {
		// tag(4) + length(4 LE) + value
		var hdr [8]byte
		binary.LittleEndian.PutUint16(hdr[0:2], t.Group)
		binary.LittleEndian.PutUint16(hdr[2:4], t.Element)
		binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(value)))
		buf.Write(hdr[:])
		buf.Write(value)
		return
	}

	// Explicit VR LE
	if isBigVR(t.VR) {
		hdr := make([]byte, 12)
		binary.LittleEndian.PutUint16(hdr[0:2], t.Group)
		binary.LittleEndian.PutUint16(hdr[2:4], t.Element)
		copy(hdr[4:6], []byte(t.VR))
		// hdr[6:8] reserved (zero)
		binary.LittleEndian.PutUint32(hdr[8:12], uint32(len(value)))
		buf.Write(hdr)
	} else {
		hdr := make([]byte, 8)
		binary.LittleEndian.PutUint16(hdr[0:2], t.Group)
		binary.LittleEndian.PutUint16(hdr[2:4], t.Element)
		copy(hdr[4:6], []byte(t.VR))
		binary.LittleEndian.PutUint16(hdr[6:8], uint16(len(value)))
		buf.Write(hdr)
	}
	buf.Write(value)
}
