package scp

import "testing"

// A site reported associations being accepted and released with nothing
// transferred. One cause is a transfer syntax we do not list: the context is
// refused, the modality gives up, and at info level the log is identical to a
// healthy C-ECHO. These pin the syntaxes a clinic actually sends.
func TestCommonModalitySyntaxesAreAccepted(t *testing.T) {
	for _, tc := range []struct{ name, uid string }{
		{"implicit VR LE", UIDImplicitVRLittleEndian},
		{"explicit VR LE", UIDExplicitVRLittleEndian},
		{"explicit VR BE", UIDExplicitVRBigEndian},
		{"deflated", UIDDeflatedExplicitVRLittleEnd},
		{"JPEG baseline", UIDJPEGBaseline},
		{"JPEG extended", UIDJPEGExtended},
		{"JPEG lossless non-hier (.57)", UIDJPEGLosslessNonHier},
		{"JPEG lossless SV1 (.70)", UIDJPEGLossless},
		{"JPEG-LS lossless", UIDJPEGLSLossless},
		{"JPEG-LS lossy", UIDJPEGLSLossy},
		{"JPEG2000 lossless", UIDJPEG2000Lossless},
		{"JPEG2000", UIDJPEG2000},
		{"JPEG2000 part2 lossless", UIDJPEG2000Part2Lossless},
		{"JPEG2000 part2", UIDJPEG2000Part2},
		{"encapsulated uncompressed", UIDEncapsulatedUncompressed},
		{"MPEG2 main", UIDMPEG2MainLevel},
		{"MPEG2 high", UIDMPEG2HighLevel},
		{"MPEG4 AVC high", UIDMPEG4AVCHighProfile},
		{"MPEG4 AVC BD", UIDMPEG4AVCBDCompatible},
		{"RLE lossless", UIDRLELossless},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickPreferredTransferSyntax([]string{tc.uid}); got != tc.uid {
				t.Fatalf("%s (%s) REFUSED — a modality offering only this is turned away", tc.name, tc.uid)
			}
		})
	}
}

// Preference must still favour uncompressed when a modality offers a choice,
// so the stored bytes stay the easiest thing for the receiver to handle.
func TestUncompressedIsStillPreferred(t *testing.T) {
	got := pickPreferredTransferSyntax([]string{UIDJPEG2000, UIDExplicitVRLittleEndian})
	if got != UIDExplicitVRLittleEndian {
		t.Fatalf("picked %q, want explicit VR little endian", got)
	}
}

// Something genuinely unknown must still be refused rather than accepted and
// then failed downstream.
func TestUnknownSyntaxIsStillRefused(t *testing.T) {
	if got := pickPreferredTransferSyntax([]string{"1.2.3.4.5.not.a.real.syntax"}); got != "" {
		t.Fatalf("accepted an unknown transfer syntax: %q", got)
	}
}
