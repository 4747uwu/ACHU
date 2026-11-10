package scp

// Well-known DICOM UIDs the SCP recognizes.
//
// Storage SOP Classes accepted by default cover the bulk of clinical
// modalities. The list is intentionally inclusive — when a modality
// proposes a SOP Class we don't have here, the presentation context
// is rejected (result = 3, abstract-syntax-not-supported) but the
// association continues so the modality can negotiate other contexts.
//
// Transfer Syntaxes accepted include the three universally-supported
// uncompressed encodings plus the most common compressed ones. JPEG,
// JPEG-LS, JPEG 2000, RLE all pass through as opaque bytes — we don't
// transcode, just store and forward as received.
const (
	UIDImplicitVRLittleEndian      = "1.2.840.10008.1.2"
	UIDExplicitVRLittleEndian      = "1.2.840.10008.1.2.1"
	UIDExplicitVRBigEndian         = "1.2.840.10008.1.2.2"
	UIDDeflatedExplicitVRLittleEnd = "1.2.840.10008.1.2.1.99"
	UIDJPEGBaseline                = "1.2.840.10008.1.2.4.50"
	UIDJPEGExtended                = "1.2.840.10008.1.2.4.51"
	UIDJPEGLosslessNonHier         = "1.2.840.10008.1.2.4.57"
	UIDJPEGLossless                = "1.2.840.10008.1.2.4.70"
	UIDJPEGLSLossless              = "1.2.840.10008.1.2.4.80"
	UIDJPEGLSLossy                 = "1.2.840.10008.1.2.4.81"
	UIDJPEG2000Lossless            = "1.2.840.10008.1.2.4.90"
	UIDJPEG2000                    = "1.2.840.10008.1.2.4.91"
	UIDRLELossless                 = "1.2.840.10008.1.2.5"
	UIDJPEG2000Part2Lossless       = "1.2.840.10008.1.2.4.92"
	UIDJPEG2000Part2               = "1.2.840.10008.1.2.4.93"
	UIDEncapsulatedUncompressed    = "1.2.840.10008.1.2.1.98"
	// Video. Ultrasound cine loops and endoscopy arrive in these, and a
	// modality that only offers one of them is refused outright without them.
	UIDMPEG2MainLevel              = "1.2.840.10008.1.2.4.100"
	UIDMPEG2HighLevel              = "1.2.840.10008.1.2.4.101"
	UIDMPEG4AVCHighProfile         = "1.2.840.10008.1.2.4.102"
	UIDMPEG4AVCBDCompatible        = "1.2.840.10008.1.2.4.103"

	UIDApplicationContext     = "1.2.840.10008.3.1.1.1"
	UIDImplementationClassTAR = "1.2.826.0.1.3680043.10.1338.1" // ours; pick a real OID for production
	UIDImplementationVersion  = "TARANG_001"

	UIDVerificationSOPClass = "1.2.840.10008.1.1" // C-ECHO
)

// SupportedTransferSyntaxes lists the transfer syntaxes we accept, in
// preference order (most preferred first).
var SupportedTransferSyntaxes = []string{
	UIDExplicitVRLittleEndian,
	UIDImplicitVRLittleEndian,
	UIDDeflatedExplicitVRLittleEnd,
	UIDExplicitVRBigEndian,
	UIDJPEGBaseline,
	UIDJPEGExtended,
	UIDJPEGLossless,
	UIDJPEGLSLossless,
	UIDJPEGLSLossy,
	UIDJPEG2000Lossless,
	UIDJPEG2000,
	UIDRLELossless,

	// Added after a site showed associations being accepted and released with
	// nothing transferred: a context whose transfer syntax we do not list is
	// refused, and the modality simply gives up. Everything here passes
	// through as opaque bytes — the SCP stores and forwards, it does not
	// transcode — so listing one costs nothing and refusing one costs a study.
	UIDJPEGLosslessNonHier,
	UIDJPEG2000Part2Lossless,
	UIDJPEG2000Part2,
	UIDEncapsulatedUncompressed,
	UIDMPEG2MainLevel,
	UIDMPEG2HighLevel,
	UIDMPEG4AVCHighProfile,
	UIDMPEG4AVCBDCompatible,
}

// isSupportedTransferSyntax reports whether ts is in our accepted list.
func isSupportedTransferSyntax(ts string) bool {
	for _, s := range SupportedTransferSyntaxes {
		if s == ts {
			return true
		}
	}
	return false
}

// pickPreferredTransferSyntax returns the first transfer syntax in proposed
// that we also support, honoring our preference order. Returns "" if none
// match.
func pickPreferredTransferSyntax(proposed []string) string {
	// Build a set for O(1) lookup of what was proposed.
	prop := make(map[string]struct{}, len(proposed))
	for _, ts := range proposed {
		prop[ts] = struct{}{}
	}
	for _, pref := range SupportedTransferSyntaxes {
		if _, ok := prop[pref]; ok {
			return pref
		}
	}
	return ""
}
