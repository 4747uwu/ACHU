package transcode

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// Deciding whether an instance has pixel data at all, before gdcmconv is
// launched at it.
//
// The bug this exists for: Siemens raw-data objects (Raw Data Storage, CSA
// non-image) ride inside an ordinary CT study carrying Modality=CT, so the
// modality-level NoPixelModality guard never sees them. They have no pixmap,
// gdcmconv exits 1 with "Could not read (pixmap)", and that used to fall
// through to a returned error — a retryable failure. The whole study then
// parked at FAILED forever, retrying two instances that can never succeed,
// while every real image series in it had already delivered.
//
// A SOP-class allowlist was considered and rejected. The .66 family is a trap:
// 1.2.840.10008.5.1.4.1.1.66 is Raw Data (no pixels) but .66.4 Segmentation
// DOES carry pixel data, so a prefix match would silently switch compression
// off for valid images.

// errUncertain means the walk could not reach a confident answer. The caller
// must fall through to gdcmconv rather than assume anything.
var errUncertain = errors.New("pixel data probe: inconclusive")

const (
	tagPixelDataGroup = 0x7FE0
	tagPixelDataElem  = 0x0010

	// Group 0xFFFE carries item and delimiter tags. These are ALWAYS encoded
	// implicit-style — bare 4-byte length, no VR — even inside an Explicit VR
	// dataset. This is the detail that desynchronises a naive walker the
	// moment it steps into a sequence.
	tagDelimiterGroup = 0xFFFE

	undefinedLength = 0xFFFFFFFF
)

// hasPixelData reports whether the dataset contains (7FE0,0010).
//
// The contract is deliberately ASYMMETRIC, and simplifying it would be a
// mistake: a definite false is the only answer that suppresses the encoder, so
// it is returned only after a clean walk all the way to EOF. Anything
// ambiguous — an encoding this does not read, a truncated file, a length that
// overruns, an unexpected VR — returns errUncertain, and the caller hands the
// file to gdcmconv, which is the authority on what it can encode.
//
// The asymmetry is the whole design. A wrong false silently stops compressing
// real images fleet-wide, and nothing would go red. A wrong "uncertain" costs
// one process launch.
func hasPixelData(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("%w: %v", errUncertain, err)
	}
	size := info.Size()

	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("%w: %v", errUncertain, err)
	}
	defer f.Close()

	if err := skipMetaGroup(f); err != nil {
		return false, fmt.Errorf("%w: %v", errUncertain, err)
	}

	ts, err := readTransferSyntax(path)
	if err != nil {
		return false, fmt.Errorf("%w: %v", errUncertain, err)
	}
	implicitVR := ts == transferSyntaxImplicitVRLE

	// seek advances by n bytes, but only when the file is actually that long.
	// Seeking past EOF is legal and succeeds silently, so an element whose
	// declared length overruns the file would otherwise walk straight off the
	// end and report a clean "no pixel data" — the exact dangerous answer.
	seek := func(n int64) error {
		pos, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		if n < 0 || pos+n > size {
			return fmt.Errorf("%w: element length %d overruns the file at offset %d", errUncertain, n, pos)
		}
		_, err = f.Seek(n, io.SeekCurrent)
		return err
	}

	for {
		var tagBuf [4]byte
		if _, err := io.ReadFull(f, tagBuf[:]); err != nil {
			if errors.Is(err, io.EOF) {
				// Clean end, exactly on an element boundary: the dataset
				// genuinely has no (7FE0,0010).
				return false, nil
			}
			// A partial tag means the file is truncated mid-element.
			return false, fmt.Errorf("%w: truncated element header: %v", errUncertain, err)
		}
		group := binary.LittleEndian.Uint16(tagBuf[0:2])
		elem := binary.LittleEndian.Uint16(tagBuf[2:4])

		if group == tagPixelDataGroup && elem == tagPixelDataElem {
			return true, nil
		}

		// Item and delimiter tags: implicit-style length, no VR, whatever the
		// dataset's own encoding.
		if group == tagDelimiterGroup {
			var l [4]byte
			if _, err := io.ReadFull(f, l[:]); err != nil {
				return false, fmt.Errorf("%w: truncated item length: %v", errUncertain, err)
			}
			length := binary.LittleEndian.Uint32(l[:])
			if length == undefinedLength {
				// Undefined-length item: its content is a run of elements,
				// terminated by an item-delimitation tag. Keep walking.
				continue
			}
			// A defined-length item is skipped whole rather than walked into.
			// Pixel Data is a top-level element, so nothing is lost — and the
			// items inside encapsulated pixel data hold raw fragments, not
			// elements, which is precisely what desynchronises a walker that
			// steps into them.
			if err := seek(int64(length)); err != nil {
				return false, err
			}
			continue
		}

		var length uint32
		if implicitVR {
			var l [4]byte
			if _, err := io.ReadFull(f, l[:]); err != nil {
				return false, fmt.Errorf("%w: truncated element length: %v", errUncertain, err)
			}
			length = binary.LittleEndian.Uint32(l[:])
		} else {
			var vrBuf [2]byte
			if _, err := io.ReadFull(f, vrBuf[:]); err != nil {
				return false, fmt.Errorf("%w: truncated VR: %v", errUncertain, err)
			}
			vr := string(vrBuf[:])
			if !plausibleVR(vr) {
				// Two bytes that are not a VR mean the walk has lost sync.
				// Guessing from here is how a wrong false gets produced.
				return false, fmt.Errorf("%w: implausible VR %q", errUncertain, vr)
			}
			if isBigVR(vr) {
				var skip [2]byte
				if _, err := io.ReadFull(f, skip[:]); err != nil {
					return false, fmt.Errorf("%w: truncated reserved bytes: %v", errUncertain, err)
				}
				var l [4]byte
				if _, err := io.ReadFull(f, l[:]); err != nil {
					return false, fmt.Errorf("%w: truncated element length: %v", errUncertain, err)
				}
				length = binary.LittleEndian.Uint32(l[:])
			} else {
				var l [2]byte
				if _, err := io.ReadFull(f, l[:]); err != nil {
					return false, fmt.Errorf("%w: truncated element length: %v", errUncertain, err)
				}
				length = uint32(binary.LittleEndian.Uint16(l[:]))
			}
		}

		// Undefined length is a sequence (or encapsulated pixel data, which is
		// the tag we already returned on). Its content is items, so walk in —
		// the group 0xFFFE branch above handles them.
		if length == undefinedLength {
			continue
		}

		if err := seek(int64(length)); err != nil {
			return false, err
		}
	}
}

// plausibleVR reports whether two bytes look like a DICOM value representation.
// Used only to detect that an Explicit VR walk has lost sync.
func plausibleVR(vr string) bool {
	if len(vr) != 2 {
		return false
	}
	for i := 0; i < 2; i++ {
		if vr[i] < 'A' || vr[i] > 'Z' {
			return false
		}
	}
	return true
}
