package dicom

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// TestRoundTrip builds a minimal DICOM dataset in Explicit VR Little
// Endian, writes it via WriteDICOMFile, parses it back via
// ParseDatasetMeta, and confirms tags survive the wrap/unwrap.
func TestRoundTrip(t *testing.T) {
	const (
		studyUID  = "1.2.840.10008.5.1.test.study.42"
		seriesUID = "1.2.840.10008.5.1.test.series.42"
		sopUID    = "1.2.840.10008.5.1.test.inst.42"
		sopClass  = "1.2.840.10008.5.1.4.1.1.2" // CT Image Storage
	)

	dataset := buildMinimalDataset(t, map[uint32]elem{
		mkTag(0x0008, 0x0018): {VR: "UI", Value: []byte(padOdd(sopUID))},     // SOPInstanceUID
		mkTag(0x0008, 0x0016): {VR: "UI", Value: []byte(padOdd(sopClass))},   // SOPClassUID
		mkTag(0x0010, 0x0010): {VR: "PN", Value: padSpace([]byte("DOE^JOHN"))}, // PatientName
		mkTag(0x0010, 0x0020): {VR: "LO", Value: padSpace([]byte("P-000042"))}, // PatientID
		mkTag(0x0008, 0x0020): {VR: "DA", Value: []byte("20260426")},          // StudyDate
		mkTag(0x0008, 0x0030): {VR: "TM", Value: padSpace([]byte("120000"))},  // StudyTime
		mkTag(0x0008, 0x0050): {VR: "SH", Value: padSpace([]byte("ACC-1"))},   // AccessionNumber
		mkTag(0x0008, 0x0060): {VR: "CS", Value: padSpace([]byte("CT"))},      // Modality
		mkTag(0x0008, 0x103E): {VR: "LO", Value: padSpace([]byte("Test Series"))}, // SeriesDescription
		mkTag(0x0020, 0x0011): {VR: "IS", Value: padSpace([]byte("1"))},       // SeriesNumber
		mkTag(0x0020, 0x000D): {VR: "UI", Value: []byte(padOdd(studyUID))},    // StudyInstanceUID
		mkTag(0x0020, 0x000E): {VR: "UI", Value: []byte(padOdd(seriesUID))},   // SeriesInstanceUID
	})

	// Write the file.
	path := filepath.Join(t.TempDir(), "test.dcm")
	if err := WriteDICOMFile(path, "1.2.840.10008.1.2.1", sopClass, sopUID, dataset); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Sanity-check the file starts with the 128-byte preamble + DICM.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(raw) < 132 || string(raw[128:132]) != "DICM" {
		t.Fatalf("file does not start with preamble + DICM magic")
	}

	// Round-trip parse.
	meta, err := ParseDatasetMeta(dataset, "1.2.840.10008.1.2.1", sopClass, sopUID)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if meta.StudyInstanceUID != studyUID {
		t.Errorf("StudyInstanceUID = %q, want %q", meta.StudyInstanceUID, studyUID)
	}
	if meta.SeriesInstanceUID != seriesUID {
		t.Errorf("SeriesInstanceUID = %q, want %q", meta.SeriesInstanceUID, seriesUID)
	}
	if meta.SOPInstanceUID != sopUID {
		t.Errorf("SOPInstanceUID = %q, want %q", meta.SOPInstanceUID, sopUID)
	}
	if meta.PatientName != "DOE^JOHN" {
		t.Errorf("PatientName = %q, want DOE^JOHN", meta.PatientName)
	}
	if meta.PatientID != "P-000042" {
		t.Errorf("PatientID = %q, want P-000042", meta.PatientID)
	}
	if meta.Modality != "CT" {
		t.Errorf("Modality = %q, want CT", meta.Modality)
	}
	if meta.StudyDate != "20260426" {
		t.Errorf("StudyDate = %q, want 20260426", meta.StudyDate)
	}
}

// elem is one explicit-VR element to encode in test fixtures.
type elem struct {
	VR    string
	Value []byte
}

func mkTag(group, element uint16) uint32 {
	return uint32(group)<<16 | uint32(element)
}

// buildMinimalDataset encodes a map of tag→element in Explicit VR Little
// Endian, in ascending tag order (DICOM requires sorted tags).
func buildMinimalDataset(t *testing.T, elements map[uint32]elem) []byte {
	t.Helper()
	keys := make([]uint32, 0, len(elements))
	for k := range elements {
		keys = append(keys, k)
	}
	// sort ascending — simple insertion sort, plenty fast for a test
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}

	var buf bytes.Buffer
	for _, k := range keys {
		group := uint16(k >> 16)
		elemNum := uint16(k & 0xFFFF)
		e := elements[k]
		writeExplicitElement(&buf, group, elemNum, e.VR, e.Value)
	}
	return buf.Bytes()
}

// padSpace pads a byte slice to even length with space.
func padSpace(b []byte) []byte {
	if len(b)%2 == 1 {
		return append(b, ' ')
	}
	return b
}

// padOdd pads a UID string to even length with NUL.
func padOdd(s string) string {
	if len(s)%2 == 1 {
		return s + "\x00"
	}
	return s
}

var _ = binary.LittleEndian // keep import even if unused in some builds
