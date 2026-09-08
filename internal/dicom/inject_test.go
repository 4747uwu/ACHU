package dicom

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// TestInjectExplicit verifies tag injection in Explicit VR Little Endian.
func TestInjectExplicit(t *testing.T) {
	// Build a minimal dataset with elements before and after the injection points.
	// Tags 0008,0008 (PatientName) and 0010,0010 (PatientID) come before group 0013.
	// Tag 0020,000D (StudyInstanceUID) comes after group 0013.
	dataset := buildMinimalDataset(t, map[uint32]elem{
		mkTag(0x0008, 0x0050): {VR: "SH", Value: padSpace([]byte("ACC-001"))},
		mkTag(0x0010, 0x0010): {VR: "PN", Value: padSpace([]byte("DOE^JOHN"))},
		mkTag(0x0020, 0x000D): {VR: "UI", Value: []byte(padOdd("1.2.3.4.5"))},
	})

	tags := StandardPrivateTags("UJJ1", "UJJ")

	out, err := InjectPrivateTags(dataset, "1.2.840.10008.1.2.1", tags)
	if err != nil {
		t.Fatalf("inject: %v", err)
	}

	// Walk the output and collect tags that appear.
	got := walkExplicit(t, out)
	want := []uint32{
		mkTag(0x0008, 0x0050),
		mkTag(0x0010, 0x0010),
		mkTag(0x0013, 0x0010),
		mkTag(0x0013, 0x1060),
		mkTag(0x0015, 0x0010),
		mkTag(0x0015, 0x1060),
		mkTag(0x0020, 0x000D),
		mkTag(0x0021, 0x0010),
		mkTag(0x0021, 0x1060),
		mkTag(0x0043, 0x0010),
		mkTag(0x0043, 0x1060),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d elements, want %d: got=%v", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("element[%d] = %08x, want %08x", i, got[i], want[i])
		}
	}

	// Verify the injected values are present.
	if !containsValue(out, []byte("UJJ1")) {
		t.Error("missing private creator UJJ1")
	}
	if !containsValue(out, []byte("UJJ ")) { // padded
		t.Error("missing private creator UJJ")
	}
	if !containsValue(out, []byte("xcentic-fallback-1")) {
		t.Error("missing xcentic-fallback-1")
	}
	if !containsValue(out, []byte("xcentic-study-key")) {
		t.Error("missing xcentic-study-key")
	}
	if !containsValue(out, []byte("xcentic-link-uuid")) {
		t.Error("missing xcentic-link-uuid")
	}
}

// TestInjectImplicit verifies tag injection in Implicit VR Little Endian.
func TestInjectImplicit(t *testing.T) {
	// Build implicit-VR dataset.
	var buf bytes.Buffer
	writeImplicit(&buf, 0x0008, 0x0050, padSpace([]byte("ACC-001")))
	writeImplicit(&buf, 0x0010, 0x0010, padSpace([]byte("DOE^JOHN")))
	writeImplicit(&buf, 0x0020, 0x000D, []byte(padOdd("1.2.3.4.5")))

	tags := StandardPrivateTags("UJJ1", "UJJ")

	out, err := InjectPrivateTags(buf.Bytes(), "1.2.840.10008.1.2", tags)
	if err != nil {
		t.Fatalf("inject: %v", err)
	}

	got := walkImplicit(t, out)
	want := []uint32{
		mkTag(0x0008, 0x0050),
		mkTag(0x0010, 0x0010),
		mkTag(0x0013, 0x0010),
		mkTag(0x0013, 0x1060),
		mkTag(0x0015, 0x0010),
		mkTag(0x0015, 0x1060),
		mkTag(0x0020, 0x000D),
		mkTag(0x0021, 0x0010),
		mkTag(0x0021, 0x1060),
		mkTag(0x0043, 0x0010),
		mkTag(0x0043, 0x1060),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d elements, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("element[%d] = %08x, want %08x", i, got[i], want[i])
		}
	}
}

// TestInjectReplaces verifies that an existing element with the same tag
// is replaced (not duplicated).
func TestInjectReplaces(t *testing.T) {
	// Pre-existing element at (0013,0010) with old value.
	dataset := buildMinimalDataset(t, map[uint32]elem{
		mkTag(0x0010, 0x0010): {VR: "PN", Value: padSpace([]byte("DOE^JOHN"))},
		mkTag(0x0013, 0x0010): {VR: "LO", Value: padSpace([]byte("OLD_LAB"))},
	})

	tags := []PrivateTag{
		{Group: 0x0013, Element: 0x0010, VR: "LO", Value: "NEW_LAB"},
	}

	out, err := InjectPrivateTags(dataset, "1.2.840.10008.1.2.1", tags)
	if err != nil {
		t.Fatalf("inject: %v", err)
	}

	if containsValue(out, []byte("OLD_LAB")) {
		t.Error("OLD_LAB should have been replaced")
	}
	if !containsValue(out, []byte("NEW_LAB")) {
		t.Error("NEW_LAB should be present")
	}

	// Should still have exactly 2 elements (PN at 0010,0010 and LO at 0013,0010).
	got := walkExplicit(t, out)
	if len(got) != 2 {
		t.Errorf("got %d elements, want 2", len(got))
	}
}

// TestInjectUnsupportedSyntax verifies the unsupported-syntax error path.
func TestInjectUnsupportedSyntax(t *testing.T) {
	dataset := buildMinimalDataset(t, map[uint32]elem{
		mkTag(0x0008, 0x0050): {VR: "SH", Value: padSpace([]byte("X"))},
	})
	tags := StandardPrivateTags("X", "Y")

	for _, ts := range []string{
		"1.2.840.10008.1.2.2",      // Big Endian
		"1.2.840.10008.1.2.1.99",   // Deflated
	} {
		_, err := InjectPrivateTags(dataset, ts, tags)
		if !errors.Is(err, ErrInjectionUnsupported) {
			t.Errorf("transfer syntax %s: got err=%v, want ErrInjectionUnsupported", ts, err)
		}
	}
}

// TestInjectUndefinedLengthLate verifies we still inject when an
// undefined-length element appears after our private-tag groups.
func TestInjectUndefinedLengthLate(t *testing.T) {
	var buf bytes.Buffer
	writeExplicit(&buf, 0x0008, 0x0050, "SH", padSpace([]byte("ACC-001")))
	writeExplicit(&buf, 0x0010, 0x0010, "PN", padSpace([]byte("DOE^JOHN")))

	// (3006,0010) SQ with undefined length; then immediate sequence delimiter.
	writeExplicitUndefinedLengthSQWithDelimiter(&buf, 0x3006, 0x0010)

	tags := StandardPrivateTags("UJJ1", "UJJ")
	out, err := InjectPrivateTags(buf.Bytes(), "1.2.840.10008.1.2.1", tags)
	if err != nil {
		t.Fatalf("inject with late undefined-length element: %v", err)
	}

	if !containsValue(out, []byte("UJJ1")) {
		t.Fatal("missing injected private creator UJJ1")
	}
	if !containsValue(out, []byte("xcentic-link-uuid")) {
		t.Fatal("missing injected marker xcentic-link-uuid")
	}
}

// TestInjectUndefinedLengthEarlyStillFails verifies we fail safely when
// undefined-length appears before insertion point.
func TestInjectUndefinedLengthEarlyStillFails(t *testing.T) {
	var buf bytes.Buffer
	// (0008,1111) Referenced Performed Procedure Step Sequence (SQ, undef).
	writeExplicitUndefinedLengthSQWithDelimiter(&buf, 0x0008, 0x1111)

	tags := StandardPrivateTags("UJJ1", "UJJ")
	_, err := InjectPrivateTags(buf.Bytes(), "1.2.840.10008.1.2.1", tags)
	if err == nil {
		t.Fatal("expected error for early undefined-length element, got nil")
	}
}

// ------------------------------------------------------------------------
// helpers
// ------------------------------------------------------------------------

// walkExplicit walks an explicit-VR LE dataset and returns each element's tag.
func walkExplicit(t *testing.T, data []byte) []uint32 {
	t.Helper()
	var tags []uint32
	pos := 0
	for pos+8 <= len(data) {
		group := binary.LittleEndian.Uint16(data[pos : pos+2])
		elem := binary.LittleEndian.Uint16(data[pos+2 : pos+4])
		vr := string(data[pos+4 : pos+6])

		var valueLen uint32
		var headerLen int
		if isBigVR(vr) {
			if pos+12 > len(data) {
				return tags
			}
			valueLen = binary.LittleEndian.Uint32(data[pos+8 : pos+12])
			headerLen = 12
		} else {
			valueLen = uint32(binary.LittleEndian.Uint16(data[pos+6 : pos+8]))
			headerLen = 8
		}
		tags = append(tags, mkTag(group, elem))
		pos += headerLen + int(valueLen)
	}
	return tags
}

// walkImplicit walks an implicit-VR LE dataset and returns each element's tag.
func walkImplicit(t *testing.T, data []byte) []uint32 {
	t.Helper()
	var tags []uint32
	pos := 0
	for pos+8 <= len(data) {
		group := binary.LittleEndian.Uint16(data[pos : pos+2])
		elem := binary.LittleEndian.Uint16(data[pos+2 : pos+4])
		valueLen := binary.LittleEndian.Uint32(data[pos+4 : pos+8])
		tags = append(tags, mkTag(group, elem))
		pos += 8 + int(valueLen)
	}
	return tags
}

func writeImplicit(buf *bytes.Buffer, group, elem uint16, value []byte) {
	var hdr [8]byte
	binary.LittleEndian.PutUint16(hdr[0:2], group)
	binary.LittleEndian.PutUint16(hdr[2:4], elem)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(value)))
	buf.Write(hdr[:])
	buf.Write(value)
}

func writeExplicit(buf *bytes.Buffer, group, elem uint16, vr string, value []byte) {
	var tag [4]byte
	binary.LittleEndian.PutUint16(tag[0:2], group)
	binary.LittleEndian.PutUint16(tag[2:4], elem)
	buf.Write(tag[:])
	buf.WriteString(vr)

	if isBigVR(vr) {
		buf.Write([]byte{0x00, 0x00})
		var len4 [4]byte
		binary.LittleEndian.PutUint32(len4[:], uint32(len(value)))
		buf.Write(len4[:])
	} else {
		var len2 [2]byte
		binary.LittleEndian.PutUint16(len2[:], uint16(len(value)))
		buf.Write(len2[:])
	}
	buf.Write(value)
}

func writeExplicitUndefinedLengthSQWithDelimiter(buf *bytes.Buffer, group, elem uint16) {
	var tag [4]byte
	binary.LittleEndian.PutUint16(tag[0:2], group)
	binary.LittleEndian.PutUint16(tag[2:4], elem)
	buf.Write(tag[:])
	buf.WriteString("SQ")
	buf.Write([]byte{0x00, 0x00})
	buf.Write([]byte{0xFF, 0xFF, 0xFF, 0xFF})

	// Sequence Delimitation Item (FFFE,E0DD), length 0.
	buf.Write([]byte{0xFE, 0xFF, 0xDD, 0xE0, 0x00, 0x00, 0x00, 0x00})
}

func containsValue(data, needle []byte) bool {
	return bytes.Contains(data, needle)
}
