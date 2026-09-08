// Package transcode wraps the gdcmconv CLI tool to re-encode DICOM pixel data
// as JPEG 2000 before upload.
//
// There are two independent modes, mutually exclusive per file — see mode.go
// for how one is chosen:
//
//	Lossless (.90)  ON by default   pixel-for-pixel identical, ~40-60% smaller
//	Lossy    (.91)  opt-in          rate-based N:1, discards image data
//
// Files are converted in place: gdcmconv writes a temp file alongside the
// original and it is renamed over the original only after every integrity
// guard passes, so a bad encode can never reach the PACS. If the tool is
// missing or fails for an individual file, that file is left unchanged and the
// error is surfaced to the caller.
package transcode

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/bharatpacs/tarang-sender/internal/log"
)

const (
	transferSyntaxJP2KLossy    = "1.2.840.10008.1.2.4.91"
	transferSyntaxImplicitVRLE = "1.2.840.10008.1.2"
)

// lossyTransferSyntaxes covers transfer syntaxes that are already lossy-compressed.
// Re-encoding a lossy file would cause a second generation of quality loss, so
// these are skipped when skipAlreadyCompressed is set.
// Lossless syntaxes (.57/.70/.80/.90/.92) are intentionally NOT in this list —
// they should still be re-encoded to lossy to achieve meaningful size reduction.
var lossyTransferSyntaxes = map[string]bool{
	"1.2.840.10008.1.2.4.50": true, // JPEG Baseline (lossy)
	"1.2.840.10008.1.2.4.51": true, // JPEG Extended (lossy)
	"1.2.840.10008.1.2.4.81": true, // JPEG-LS Near-lossless
	"1.2.840.10008.1.2.4.91": true, // JPEG 2000 Lossy
	"1.2.840.10008.1.2.4.93": true, // JPEG 2000 Part 2 Lossy
}

// ResolveGdcmconv returns the path to use for the gdcmconv binary.
//   - If configPath is non-empty it is returned as-is.
//   - Otherwise the directory containing the running executable is checked.
//   - If that fails, "gdcmconv[.exe]" is returned for PATH resolution.
func ResolveGdcmconv(configPath string) string {
	if configPath != "" {
		return configPath
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "gdcmconv"+suffix)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "gdcmconv" + suffix
}

// TranscodeSeries runs gdcmconv on every file path in paths according to plan.
//
// All failures are logged individually; if any file fails the function
// returns a summary error so the caller can retry the whole series.
func TranscodeSeries(ctx context.Context, gdcmconvPath string, paths []string, plan Plan) error {
	if plan.Mode == ModeNone {
		return nil
	}
	logger := log.L()
	failed := 0
	for _, p := range paths {
		if err := TranscodeFile(ctx, gdcmconvPath, p, plan); err != nil {
			logger.Warn("transcode file failed", "path", p, "mode", plan.Mode.String(), "err", err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("transcode: %d/%d files failed", failed, len(paths))
	}
	return nil
}

// TranscodeFile converts one DICOM Part 10 file according to plan. The output
// is written to a sibling .j2k.tmp file and is renamed over the original only
// after every integrity guard has passed, so a bad encode can never reach the
// PACS: a failed guard leaves the original untouched.
func TranscodeFile(ctx context.Context, gdcmconvPath, filePath string, plan Plan) error {
	if plan.Mode == ModeNone {
		return nil
	}
	if plan.Mode == ModeLossy && plan.Rate <= 0 {
		return nil // compression disabled for this modality
	}
	if plan.SkipAlreadyCompressed {
		ts, err := readTransferSyntax(filePath)
		if err == nil && shouldSkip(plan.Mode, ts) {
			return nil
		}
	}

	// An instance with no pixel data has nothing to encode, and gdcmconv exits
	// 1 on it. Checking first turns a "failure" into a skip BEFORE a process
	// is launched — the modality guard cannot catch these, because a Siemens
	// raw-data object rides inside a CT study carrying Modality=CT.
	//
	// Only a definite false suppresses the encoder. An inconclusive probe
	// falls through to gdcmconv, which is the authority on what it can encode;
	// isNonImageFailure below is the backstop for that path.
	if ok, err := hasPixelData(filePath); err == nil && !ok {
		log.L().Info("transcode skipped — instance carries no pixel data", "path", filePath)
		return nil
	}

	// Capture the original pixel representation so we can detect a signed→unsigned
	// flip in the output (a known gdcmconv hazard that corrupts CT Hounsfield units).
	origSigned, origErr := readPixelRepresentation(filePath)
	origSize, sizeErr := fileSize(filePath)

	tmpOut := filePath + ".j2k.tmp"
	defer os.Remove(tmpOut)

	cmd := exec.CommandContext(ctx, gdcmconvPath, gdcmconvArgs(plan, filePath, tmpOut)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		outStr := strings.TrimSpace(string(out))
		// gdcmconv cannot compress non-image objects (protocol docs, raw data,
		// certain vendor private encodings) and exits 1. Treat as a skip —
		// send the file as-is. This is expected, not an error.
		if isNonImageFailure(outStr) {
			log.L().Info("transcode skipped — gdcmconv cannot compress this file type",
				"path", filePath, "gdcmconv", outStr)
			return nil
		}
		return fmt.Errorf("gdcmconv: %w — %s", err, outStr)
	}

	// ---- Post-encode integrity guards -----------------------------------
	// 1. The output must declare exactly the transfer syntax this mode
	//    promised. A lossless plan that produced .91 is a corrupted promise,
	//    not a smaller file.
	wantTS := expectedTransferSyntax(plan.Mode)
	outTS, tsErr := readTransferSyntax(tmpOut)
	if tsErr != nil || outTS != wantTS {
		return fmt.Errorf("transcode integrity: output transfer syntax %q is not %s (%v)", outTS, wantTS, tsErr)
	}

	// 2. Signedness must survive, or CT Hounsfield units are silently wrong.
	if origErr == nil && origSigned {
		outSigned, outErr := readPixelRepresentation(tmpOut)
		if outErr == nil && !outSigned {
			return fmt.Errorf("transcode integrity: signed pixel data flipped to unsigned")
		}
	}

	// 3. Lossless only: keep the original unless the encode actually shrank
	//    it. Entropy coding is not guaranteed to win — on noisy or pre-packed
	//    images JPEG 2000 can come out larger. Because lossless is on by
	//    default for every site, silent inflation would be a fleet-wide
	//    bandwidth regression rather than an occasional oddity.
	if plan.Mode == ModeLossless && sizeErr == nil {
		newSize, err := fileSize(tmpOut)
		if err == nil && newSize >= origSize {
			log.L().Info("lossless encode was not smaller — keeping the original",
				"path", filePath,
				"original_bytes", origSize,
				"encoded_bytes", newSize,
			)
			return nil
		}
	}

	return os.Rename(tmpOut, filePath)
}

// isNonImageFailure reports whether gdcmconv's output says "this object has no
// image to encode" rather than "the encode went wrong".
//
// The distinction decides whether the study can ever succeed. A non-image
// instance is permanent: retrying it re-runs the same failure forever, and
// because one failed file fails its whole series, a single Siemens raw-data
// object parked an entire study at FAILED while every real image series in it
// had already delivered.
//
// Only these known phrasings are treated as skips. Anything else — permission
// denied, no space left, a killed process — stays a retryable error, which is
// what a transient fault needs.
func isNonImageFailure(out string) bool {
	lower := strings.ToLower(out)
	for _, marker := range []string{
		"could not derive",        // the original, and the only one matched before
		"could not read (pixmap)", // GDCM 3.2.7 on a Siemens raw-data object
		"could not find pixmap",   //
		"no pixel data",           //
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// fileSize returns the size of a file in bytes.
func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// readTransferSyntax extracts (0002,0010) from a DICOM Part 10 file header
// without parsing the full dataset.
func readTransferSyntax(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// 128-byte preamble + "DICM"
	preamble := make([]byte, 132)
	if _, err := io.ReadFull(f, preamble); err != nil {
		return "", errors.New("too short for DICOM preamble")
	}
	if string(preamble[128:132]) != "DICM" {
		return "", errors.New("not a DICOM Part 10 file")
	}

	// Walk Explicit VR LE elements in the file-meta group (0x0002).
	for {
		var tagBuf [4]byte
		if _, err := io.ReadFull(f, tagBuf[:]); err != nil {
			return "", fmt.Errorf("read tag: %w", err)
		}
		group := binary.LittleEndian.Uint16(tagBuf[0:2])
		elem := binary.LittleEndian.Uint16(tagBuf[2:4])
		if group != 0x0002 {
			return "", errors.New("(0002,0010) not found")
		}

		var vrBuf [2]byte
		if _, err := io.ReadFull(f, vrBuf[:]); err != nil {
			return "", err
		}
		vr := string(vrBuf[:])

		var valueLen uint32
		if isBigVR(vr) {
			var skip [2]byte
			if _, err := io.ReadFull(f, skip[:]); err != nil {
				return "", err
			}
			var l [4]byte
			if _, err := io.ReadFull(f, l[:]); err != nil {
				return "", err
			}
			valueLen = binary.LittleEndian.Uint32(l[:])
		} else {
			var l [2]byte
			if _, err := io.ReadFull(f, l[:]); err != nil {
				return "", err
			}
			valueLen = uint32(binary.LittleEndian.Uint16(l[:]))
		}

		val := make([]byte, valueLen)
		if _, err := io.ReadFull(f, val); err != nil {
			return "", err
		}

		if group == 0x0002 && elem == 0x0010 {
			return strings.TrimRight(string(val), "\x00 "), nil
		}
	}
}

// skipMetaGroup advances f past the 128-byte preamble, the "DICM" marker, and
// every element of the file-meta group (0x0002), leaving f positioned at the
// first byte of the main dataset.
func skipMetaGroup(f *os.File) error {
	preamble := make([]byte, 132)
	if _, err := io.ReadFull(f, preamble); err != nil {
		return errors.New("too short for DICOM preamble")
	}
	if string(preamble[128:132]) != "DICM" {
		return errors.New("not a DICOM Part 10 file")
	}

	// The file-meta group is always Explicit VR LE. Walk it until we read a
	// tag from a group other than 0x0002, then rewind so the dataset scan can
	// re-read that first dataset element.
	for {
		var tagBuf [4]byte
		if _, err := io.ReadFull(f, tagBuf[:]); err != nil {
			return fmt.Errorf("read tag: %w", err)
		}
		group := binary.LittleEndian.Uint16(tagBuf[0:2])
		if group != 0x0002 {
			if _, err := f.Seek(-4, io.SeekCurrent); err != nil {
				return err
			}
			return nil
		}

		var vrBuf [2]byte
		if _, err := io.ReadFull(f, vrBuf[:]); err != nil {
			return err
		}
		vr := string(vrBuf[:])

		var valueLen uint32
		if isBigVR(vr) {
			var skip [2]byte
			if _, err := io.ReadFull(f, skip[:]); err != nil {
				return err
			}
			var l [4]byte
			if _, err := io.ReadFull(f, l[:]); err != nil {
				return err
			}
			valueLen = binary.LittleEndian.Uint32(l[:])
		} else {
			var l [2]byte
			if _, err := io.ReadFull(f, l[:]); err != nil {
				return err
			}
			valueLen = uint32(binary.LittleEndian.Uint16(l[:]))
		}

		if _, err := f.Seek(int64(valueLen), io.SeekCurrent); err != nil {
			return err
		}
	}
}

// readPixelRepresentation reads (0028,0103) Pixel Representation from the main
// dataset and reports whether the pixel data is signed (value 1). The dataset
// is parsed using the VR mode implied by the file's transfer syntax.
func readPixelRepresentation(path string) (bool, error) {
	ts, _ := readTransferSyntax(path)
	implicitVR := ts == transferSyntaxImplicitVRLE

	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	if err := skipMetaGroup(f); err != nil {
		return false, err
	}

	for {
		var tagBuf [4]byte
		if _, err := io.ReadFull(f, tagBuf[:]); err != nil {
			return false, fmt.Errorf("(0028,0103) not found: %w", err)
		}
		group := binary.LittleEndian.Uint16(tagBuf[0:2])
		elem := binary.LittleEndian.Uint16(tagBuf[2:4])

		// (0028,0103) lives in group 0x0028. Once we are past it, give up.
		if group > 0x0028 || (group == 0x0028 && elem > 0x0103) {
			return false, errors.New("(0028,0103) not found")
		}

		var valueLen uint32
		if implicitVR {
			var l [4]byte
			if _, err := io.ReadFull(f, l[:]); err != nil {
				return false, err
			}
			valueLen = binary.LittleEndian.Uint32(l[:])
		} else {
			var vrBuf [2]byte
			if _, err := io.ReadFull(f, vrBuf[:]); err != nil {
				return false, err
			}
			vr := string(vrBuf[:])
			if isBigVR(vr) {
				var skip [2]byte
				if _, err := io.ReadFull(f, skip[:]); err != nil {
					return false, err
				}
				var l [4]byte
				if _, err := io.ReadFull(f, l[:]); err != nil {
					return false, err
				}
				valueLen = binary.LittleEndian.Uint32(l[:])
			} else {
				var l [2]byte
				if _, err := io.ReadFull(f, l[:]); err != nil {
					return false, err
				}
				valueLen = uint32(binary.LittleEndian.Uint16(l[:]))
			}
		}

		// Undefined length (sequences / encapsulated data) — we cannot skip
		// reliably without full parsing, and (0028,0103) precedes such data.
		if valueLen == 0xFFFFFFFF {
			return false, errors.New("(0028,0103) not found before undefined-length element")
		}

		if group == 0x0028 && elem == 0x0103 {
			val := make([]byte, valueLen)
			if _, err := io.ReadFull(f, val); err != nil {
				return false, err
			}
			if len(val) < 2 {
				return false, errors.New("(0028,0103) value too short")
			}
			return binary.LittleEndian.Uint16(val[:2]) == 1, nil
		}

		if _, err := f.Seek(int64(valueLen), io.SeekCurrent); err != nil {
			return false, err
		}
	}
}

func isBigVR(vr string) bool {
	switch vr {
	case "OB", "OW", "OF", "SQ", "UT", "UN", "OD", "OL", "OV", "SV", "UC", "UR":
		return true
	}
	return false
}
