// Package dicom holds DICOM read/write helpers shared across the sender.
//
// We parse received datasets to extract a small set of tags needed to
// populate StudyRecord/SeriesRecord (PatientName, StudyDate, Modality, etc.).
// We do NOT decode pixel data — we only walk the top-level dataset.
//
// We use github.com/suyashkumar/dicom for parsing because it handles the
// real-world weirdness in DICOM datasets (private blocks, sequence items,
// odd VR rules) that we don't want to reimplement.
//
// Writing to disk wraps the raw dataset bytes received over the wire with
// a proper DICOM Part 10 file meta information header (preamble + DICM +
// (0002,xxxx) elements in Explicit VR LE). The receiver-side Orthanc and
// our downstream tools all expect Part 10 files, not raw datasets.
package dicom

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	suyashdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// DatasetMeta is a small projection of the tags we care about for
// indexing in BoltDB. Values are best-effort: missing tags become "".
type DatasetMeta struct {
	StudyInstanceUID  string
	SeriesInstanceUID string
	SOPInstanceUID    string
	SOPClassUID       string

	PatientID         string
	PatientName       string
	StudyDate         string
	StudyTime         string
	StudyDescription  string
	AccessionNumber   string

	Modality          string
	SeriesNumber      string
	SeriesDescription string
	ImagesInAcquisition int
}

// ParseDatasetMeta extracts the indexable tags from a raw dataset
// encoded in the given transfer syntax.
//
// transferSyntaxUID identifies how the dataset is encoded; pass the value
// the SCP handed you from the negotiated presentation context.
//
// suyashkumar/dicom expects a Part 10 file as input, so we synthesize a
// minimal file-meta header in memory before parsing. This is ~150 bytes
// of overhead per parse, which is fine.
func ParseDatasetMeta(dataset []byte, transferSyntaxUID, sopClassUID, sopInstanceUID string) (DatasetMeta, error) {
	var out DatasetMeta
	if transferSyntaxUID == "" || sopClassUID == "" || sopInstanceUID == "" {
		return out, errors.New("dicom parse: missing transfer-syntax, sop-class, or sop-instance UID")
	}

	wrapped := buildPart10(transferSyntaxUID, sopClassUID, sopInstanceUID, dataset)

	r := bytes.NewReader(wrapped)
	ds, err := suyashdicom.Parse(r, int64(len(wrapped)), nil)
	if err != nil {
		return out, err
	}

	out.StudyInstanceUID = stringTag(ds, tag.StudyInstanceUID)
	out.SeriesInstanceUID = stringTag(ds, tag.SeriesInstanceUID)
	out.SOPInstanceUID = stringTag(ds, tag.SOPInstanceUID)
	out.SOPClassUID = stringTag(ds, tag.SOPClassUID)

	out.PatientID = stringTag(ds, tag.PatientID)
	out.PatientName = stringTag(ds, tag.PatientName)
	out.StudyDate = stringTag(ds, tag.StudyDate)
	out.StudyTime = stringTag(ds, tag.StudyTime)
	out.StudyDescription = stringTag(ds, tag.StudyDescription)
	out.AccessionNumber = stringTag(ds, tag.AccessionNumber)

	out.Modality = stringTag(ds, tag.Modality)
	out.SeriesNumber = stringTag(ds, tag.SeriesNumber)
	out.SeriesDescription = stringTag(ds, tag.SeriesDescription)

	// (0020,1002) — how many images are expected in this acquisition.
	// Non-zero means the modality told us; used by the stability watcher to
	// fire immediately when all expected instances have arrived.
	if el, err := ds.FindElementByTag(tag.Tag{Group: 0x0020, Element: 0x1002}); err == nil {
		if el.Value != nil {
			if vals, ok := el.Value.GetValue().([]int); ok && len(vals) > 0 && vals[0] > 0 {
				out.ImagesInAcquisition = vals[0]
			}
		}
	}

	// Fall back to the SCP-provided values if the dataset is missing them.
	// This happens with malformed instances; we'd rather index something
	// than refuse the file.
	if out.SOPInstanceUID == "" {
		out.SOPInstanceUID = sopInstanceUID
	}
	if out.SOPClassUID == "" {
		out.SOPClassUID = sopClassUID
	}

	return out, nil
}

// stringTag pulls the first string value of an element. Returns "" for
// missing elements or non-string VRs.
func stringTag(ds suyashdicom.Dataset, t tag.Tag) string {
	el, err := ds.FindElementByTag(t)
	if err != nil {
		return ""
	}
	if el.Value == nil {
		return ""
	}
	v := el.Value.GetValue()
	switch x := v.(type) {
	case []string:
		if len(x) == 0 {
			return ""
		}
		return strings.TrimSpace(x[0])
	case string:
		return strings.TrimSpace(x)
	}
	return ""
}

// WriteDICOMFile constructs a DICOM Part 10 file at path, wrapping the
// given dataset bytes with a freshly-built file meta information header.
func WriteDICOMFile(path string, transferSyntaxUID, sopClassUID, sopInstanceUID string, dataset []byte) error {
	if transferSyntaxUID == "" || sopClassUID == "" || sopInstanceUID == "" {
		return errors.New("dicom write: missing transfer-syntax, sop-class, or sop-instance UID")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	wrapped := buildPart10(transferSyntaxUID, sopClassUID, sopInstanceUID, dataset)

	// Atomic write: temp file + rename.
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, bytes.NewReader(wrapped)); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// buildPart10 composes preamble + DICM + file meta group + dataset bytes
// into a complete DICOM Part 10 byte stream.
//
// File meta is encoded in Explicit VR Little Endian regardless of the
// transfer syntax of the inner dataset (per Part 10).
func buildPart10(transferSyntaxUID, sopClassUID, sopInstanceUID string, dataset []byte) []byte {
	var buf bytes.Buffer

	// 128-byte preamble (zeros) + "DICM" magic.
	buf.Write(make([]byte, 128))
	buf.WriteString("DICM")

	// Build the file-meta group body, then prepend the group-length element.
	var meta bytes.Buffer
	writeExplicitElement(&meta, 0x0002, 0x0001, "OB", []byte{0x00, 0x01}) // FileMetaInformationVersion
	writeExplicitElement(&meta, 0x0002, 0x0002, "UI", padUI(sopClassUID))
	writeExplicitElement(&meta, 0x0002, 0x0003, "UI", padUI(sopInstanceUID))
	writeExplicitElement(&meta, 0x0002, 0x0010, "UI", padUI(transferSyntaxUID))
	writeExplicitElement(&meta, 0x0002, 0x0012, "UI", padUI(implementationClassUID))
	writeExplicitElement(&meta, 0x0002, 0x0013, "SH", padEven(implementationVersionName))

	// Group length element: (0002,0000) UL pointing at meta length
	groupLen := make([]byte, 4)
	binary.LittleEndian.PutUint32(groupLen, uint32(meta.Len()))
	writeExplicitElement(&buf, 0x0002, 0x0000, "UL", groupLen)
	buf.Write(meta.Bytes())

	// Dataset body — already encoded in the negotiated transfer syntax.
	buf.Write(dataset)

	return buf.Bytes()
}

const (
	// Pick a real OID for production. This one is in our private OID arc.
	implementationClassUID    = "1.2.826.0.1.3680043.10.1338.1"
	implementationVersionName = "TARANG_001"
)

// writeExplicitElement appends one element in Explicit VR Little Endian.
//
//   2-byte VRs (most): tag(4) + VR(2) + length(2 LE) + value
//   "Big" VRs (OB OW OF SQ UT UN OD OL OV SV UC UR): tag(4) + VR(2) + reserved(2) + length(4 LE) + value
func writeExplicitElement(buf *bytes.Buffer, group, elem uint16, vr string, value []byte) {
	hdr := make([]byte, 4)
	binary.LittleEndian.PutUint16(hdr[0:2], group)
	binary.LittleEndian.PutUint16(hdr[2:4], elem)
	buf.Write(hdr)
	buf.WriteString(vr)

	if isBigVR(vr) {
		buf.Write([]byte{0x00, 0x00}) // reserved
		var lenBuf [4]byte
		binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(value)))
		buf.Write(lenBuf[:])
	} else {
		var lenBuf [2]byte
		binary.LittleEndian.PutUint16(lenBuf[:], uint16(len(value)))
		buf.Write(lenBuf[:])
	}
	buf.Write(value)
}

func isBigVR(vr string) bool {
	switch vr {
	case "OB", "OW", "OF", "SQ", "UT", "UN", "OD", "OL", "OV", "SV", "UC", "UR":
		return true
	}
	return false
}

// padUI pads a UID to even length with NUL.
func padUI(s string) []byte {
	b := []byte(s)
	if len(b)%2 == 1 {
		b = append(b, 0x00)
	}
	return b
}

// padEven pads a string to even length with space (for SH/LO/etc.).
func padEven(s string) []byte {
	b := []byte(s)
	if len(b)%2 == 1 {
		b = append(b, ' ')
	}
	return b
}
