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
	UIDJPEGLossless                = "1.2.840.10008.1.2.4.70"
	UIDJPEGLSLossless              = "1.2.840.10008.1.2.4.80"
	UIDJPEGLSLossy                 = "1.2.840.10008.1.2.4.81"
	UIDJPEG2000Lossless            = "1.2.840.10008.1.2.4.90"
	UIDJPEG2000                    = "1.2.840.10008.1.2.4.91"
	UIDRLELossless                 = "1.2.840.10008.1.2.5"

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
