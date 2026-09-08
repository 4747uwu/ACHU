package transcode

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Builders for minimal but genuinely well-formed Part 10 files. Everything
// here is hand-encoded on purpose: the probe's whole job is to walk bytes
// correctly, so a test that leaned on the same helpers the probe uses would
// prove nothing.

func le16(v uint16) []byte { b := make([]byte, 2); binary.LittleEndian.PutUint16(b, v); return b }
func le32(v uint32) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, v); return b }

// pad makes a value an even number of bytes, as DICOM requires.
func pad(v []byte) []byte {
	if len(v)%2 == 1 {
		return append(v, 0x00)
	}
	return v
}

// explicitElem encodes one element in Explicit VR little-endian.
func explicitElem(group, elem uint16, vr string, value []byte) []byte {
	value = pad(value)
	out := append(le16(group), le16(elem)...)
	out = append(out, vr...)
	if isBigVR(vr) {
		out = append(out, 0x00, 0x00) // reserved
		out = append(out, le32(uint32(len(value)))...)
	} else {
		out = append(out, le16(uint16(len(value)))...)
	}
	return append(out, value...)
}

// implicitElem encodes one element in Implicit VR little-endian.
func implicitElem(group, elem uint16, value []byte) []byte {
	value = pad(value)
	out := append(le16(group), le16(elem)...)
	out = append(out, le32(uint32(len(value)))...)
	return append(out, value...)
}

// rawTag writes a bare tag plus a 4-byte length — the encoding item and
// delimiter tags always use, in either VR mode.
func rawTag(group, elem uint16, length uint32) []byte {
	out := append(le16(group), le16(elem)...)
	return append(out, le32(length)...)
}

// undefinedLengthSQ wraps content in an Explicit VR sequence of undefined
// length holding one undefined-length item.
func undefinedLengthSQ(group, elem uint16, content []byte) []byte {
	out := append(le16(group), le16(elem)...)
	out = append(out, "SQ"...)
	out = append(out, 0x00, 0x00)
	out = append(out, le32(0xFFFFFFFF)...)                   // undefined-length sequence
	out = append(out, rawTag(0xFFFE, 0xE000, 0xFFFFFFFF)...) // undefined-length item
	out = append(out, content...)
	out = append(out, rawTag(0xFFFE, 0xE00D, 0)...) // item delimitation
	out = append(out, rawTag(0xFFFE, 0xE0DD, 0)...) // sequence delimitation
	return out
}

// writeDICOM assembles preamble + DICM + file-meta group + dataset.
func writeDICOM(t *testing.T, transferSyntax string, dataset []byte) string {
	t.Helper()

	var buf bytes.Buffer
	buf.Write(make([]byte, 128))
	buf.WriteString("DICM")
	// File-meta is always Explicit VR LE, whatever the dataset uses.
	buf.Write(explicitElem(0x0002, 0x0010, "UI", []byte(transferSyntax)))
	buf.Write(dataset)

	path := filepath.Join(t.TempDir(), "instance.dcm")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const (
	tsImplicit    = "1.2.840.10008.1.2"
	tsExplicit    = "1.2.840.10008.1.2.1"
	tsJ2KLossless = "1.2.840.10008.1.2.4.90"
)

// ---------------------------------------------------------------- the answers

func TestHasPixelDataFindsIt(t *testing.T) {
	cases := []struct {
		name    string
		ts      string
		dataset []byte
	}{
		{
			name: "explicit VR",
			ts:   tsExplicit,
			dataset: concat(
				explicitElem(0x0008, 0x0060, "CS", []byte("CT")),
				explicitElem(0x0028, 0x0010, "US", le16(64)),
				explicitElem(0x7FE0, 0x0010, "OW", make([]byte, 128)),
			),
		},
		{
			name: "implicit VR",
			ts:   tsImplicit,
			dataset: concat(
				implicitElem(0x0008, 0x0060, []byte("CT")),
				implicitElem(0x0028, 0x0010, le16(64)),
				implicitElem(0x7FE0, 0x0010, make([]byte, 128)),
			),
		},
		{
			name: "after an undefined-length sequence",
			ts:   tsExplicit,
			dataset: concat(
				explicitElem(0x0008, 0x0060, "CS", []byte("CT")),
				undefinedLengthSQ(0x0008, 0x1140, concat(
					explicitElem(0x0008, 0x1150, "UI", []byte("1.2.840.10008.5.1.4.1.1.2")),
					explicitElem(0x0008, 0x1155, "UI", []byte("1.2.3.4")),
				)),
				explicitElem(0x7FE0, 0x0010, "OW", make([]byte, 64)),
			),
		},
		{
			name: "after a nested sequence",
			ts:   tsExplicit,
			dataset: concat(
				undefinedLengthSQ(0x0040, 0x0275, concat(
					explicitElem(0x0040, 0x1001, "SH", []byte("REQ1")),
					undefinedLengthSQ(0x0040, 0x0008, concat(
						explicitElem(0x0040, 0x0009, "SH", []byte("SPS1")),
					)),
				)),
				explicitElem(0x7FE0, 0x0010, "OW", make([]byte, 32)),
			),
		},
		{
			name: "after a defined-length sequence",
			ts:   tsExplicit,
			dataset: concat(
				definedLengthSQ(0x0008, 0x1140, concat(
					rawTag(0xFFFE, 0xE000, uint32(len(explicitElem(0x0008, 0x1155, "UI", []byte("1.2.3"))))),
					explicitElem(0x0008, 0x1155, "UI", []byte("1.2.3")),
				)),
				explicitElem(0x7FE0, 0x0010, "OW", make([]byte, 16)),
			),
		},
		{
			name: "encapsulated JPEG 2000, undefined length",
			ts:   tsJ2KLossless,
			dataset: concat(
				explicitElem(0x0028, 0x0010, "US", le16(64)),
				// (7FE0,0010) OB, undefined length, then fragments.
				concat(le16(0x7FE0), le16(0x0010), []byte("OB"), []byte{0, 0}, le32(0xFFFFFFFF)),
				rawTag(0xFFFE, 0xE000, 0), // empty basic offset table
				rawTag(0xFFFE, 0xE000, 8),
				make([]byte, 8),
				rawTag(0xFFFE, 0xE0DD, 0),
			),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeDICOM(t, c.ts, c.dataset)
			got, err := hasPixelData(path)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !got {
				t.Error("pixel data not found, but the file has (7FE0,0010)")
			}
		})
	}
}

func TestHasPixelDataReportsAbsence(t *testing.T) {
	cases := []struct {
		name    string
		ts      string
		dataset []byte
	}{
		{
			// The shape that started this: a Siemens raw-data object riding
			// inside a CT study, so Modality=CT and the modality guard misses it.
			name: "raw data object with Modality=CT",
			ts:   tsExplicit,
			dataset: concat(
				explicitElem(0x0008, 0x0016, "UI", []byte("1.2.840.10008.5.1.4.1.1.66")),
				explicitElem(0x0008, 0x0060, "CS", []byte("CT")),
				explicitElem(0x0008, 0x103E, "LO", []byte("CSA NON-IMAGE")),
			),
		},
		{
			name: "implicit VR, no pixel data",
			ts:   tsImplicit,
			dataset: concat(
				implicitElem(0x0008, 0x0060, []byte("CT")),
				implicitElem(0x0008, 0x103E, []byte("PROTOCOL")),
			),
		},
		{
			name: "sequences but no pixel data",
			ts:   tsExplicit,
			dataset: concat(
				explicitElem(0x0008, 0x0060, "CS", []byte("CT")),
				undefinedLengthSQ(0x0008, 0x1140, concat(
					explicitElem(0x0008, 0x1155, "UI", []byte("1.2.3.4")),
				)),
				explicitElem(0x0020, 0x000D, "UI", []byte("1.2.3")),
			),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeDICOM(t, c.ts, c.dataset)
			got, err := hasPixelData(path)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got {
				t.Error("reported pixel data in a file that has none")
			}
		})
	}
}

// ------------------------------------------------------- the asymmetric half

// TestHasPixelDataIsUncertainRatherThanWrong is the important one. Every case
// here MUST come back uncertain, never a confident false: a false suppresses
// the encoder, and a wrong one would stop compressing real images fleet-wide
// with nothing going red.
func TestHasPixelDataIsUncertainRatherThanWrong(t *testing.T) {
	t.Run("declared length overruns the file", func(t *testing.T) {
		// Seeking past EOF is legal and succeeds silently, so before the
		// bounds check this walked straight off the end and returned a clean
		// "no pixel data" — the exact dangerous answer.
		dataset := concat(
			explicitElem(0x0008, 0x0060, "CS", []byte("CT")),
			// Claims 4 KB of value with nothing behind it.
			concat(le16(0x0008), le16(0x103E), []byte("OB"), []byte{0, 0}, le32(4096)),
		)
		path := writeDICOM(t, tsExplicit, dataset)
		assertUncertain(t, path)
	})

	t.Run("truncated mid-element", func(t *testing.T) {
		dataset := concat(
			explicitElem(0x0008, 0x0060, "CS", []byte("CT")),
			[]byte{0x28, 0x00, 0x10}, // three bytes of a tag
		)
		path := writeDICOM(t, tsExplicit, dataset)
		assertUncertain(t, path)
	})

	t.Run("explicit walk loses sync", func(t *testing.T) {
		dataset := concat(
			explicitElem(0x0008, 0x0060, "CS", []byte("CT")),
			concat(le16(0x0008), le16(0x0070), []byte{0x01, 0x02}, le16(4), make([]byte, 4)),
		)
		path := writeDICOM(t, tsExplicit, dataset)
		assertUncertain(t, path)
	})

	t.Run("not a DICOM file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "notdicom.bin")
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 400), 0o600); err != nil {
			t.Fatal(err)
		}
		assertUncertain(t, path)
	})

	t.Run("missing file", func(t *testing.T) {
		assertUncertain(t, filepath.Join(t.TempDir(), "gone.dcm"))
	})
}

func assertUncertain(t *testing.T, path string) {
	t.Helper()
	got, err := hasPixelData(path)
	if err == nil {
		t.Fatalf("returned a confident %v; an ambiguous file must be uncertain so the caller still runs gdcmconv", got)
	}
	if !errors.Is(err, errUncertain) {
		t.Fatalf("error %v does not wrap errUncertain", err)
	}
}

// ------------------------------------------------------------ the integration

// TestRawDataInstanceBypassesGdcmconv proves the probe short-circuits before a
// process is launched: the binary path is deliberately bogus, so if
// TranscodeFile tried to run it the call would fail loudly.
func TestRawDataInstanceBypassesGdcmconv(t *testing.T) {
	dataset := concat(
		explicitElem(0x0008, 0x0016, "UI", []byte("1.2.840.10008.5.1.4.1.1.66")),
		explicitElem(0x0008, 0x0060, "CS", []byte("CT")),
	)
	path := writeDICOM(t, tsExplicit, dataset)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	const bogus = "definitely-not-a-real-gdcmconv-binary"
	plan := Plan{Mode: ModeLossless, SkipAlreadyCompressed: true}
	if err := TranscodeFile(context.Background(), bogus, path, plan); err != nil {
		t.Fatalf("a non-image instance must be skipped, not failed: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the file was modified; a skipped instance must pass through untouched")
	}
}

// TestImageInstanceStillReachesGdcmconv keeps the test above honest: with real
// pixel data the same bogus path MUST fail, or the skip proves nothing.
func TestImageInstanceStillReachesGdcmconv(t *testing.T) {
	dataset := concat(
		explicitElem(0x0008, 0x0060, "CS", []byte("CT")),
		explicitElem(0x7FE0, 0x0010, "OW", make([]byte, 64)),
	)
	path := writeDICOM(t, tsExplicit, dataset)

	const bogus = "definitely-not-a-real-gdcmconv-binary"
	plan := Plan{Mode: ModeLossless, SkipAlreadyCompressed: true}
	if err := TranscodeFile(context.Background(), bogus, path, plan); err == nil {
		t.Error("an instance WITH pixel data was not handed to gdcmconv")
	}
}

// ------------------------------------------------------------ the backstop

// TestIsNonImageFailure pins which gdcmconv messages mean "nothing to encode".
// Getting this wrong in either direction is expensive: a missed phrasing parks
// a whole study at FAILED forever, and an over-broad match turns a real,
// transient failure into a silent skip.
func TestIsNonImageFailure(t *testing.T) {
	skips := []string{
		"Could not read (pixmap): C:\\data\\raw.dcm",
		"could not read (pixmap)",
		"gdcmconv: Could not find pixmap",
		"Error: no pixel data in input",
		"could not derive",
		"COULD NOT DERIVE",
	}
	for _, s := range skips {
		if !isNonImageFailure(s) {
			t.Errorf("%q should be treated as a non-image skip", s)
		}
	}

	retryable := []string{
		"Permission denied",
		"No space left on device",
		"Segmentation fault",
		"could not open file for writing",
		"",
	}
	for _, s := range retryable {
		if isNonImageFailure(s) {
			t.Errorf("%q must stay a retryable error, not a skip", s)
		}
	}
}

// concat joins byte slices; it exists only to keep the fixtures readable.
func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// definedLengthSQ encodes an Explicit VR sequence with an explicit length.
func definedLengthSQ(group, elem uint16, content []byte) []byte {
	out := append(le16(group), le16(elem)...)
	out = append(out, "SQ"...)
	out = append(out, 0x00, 0x00)
	out = append(out, le32(uint32(len(content)))...)
	return append(out, content...)
}
