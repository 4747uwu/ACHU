package transcode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Mode selects which JPEG 2000 encoding a file gets, if any.
//
// The two modes are mutually exclusive per file. They exist for genuinely
// different reasons: lossless is a free bandwidth saving that is safe
// everywhere, so it is on by default; lossy discards image data to hit a size
// target, so it is opt-in and per-modality.
type Mode int

const (
	// ModeNone sends the file in its original encoding.
	ModeNone Mode = iota
	// ModeLossy encodes to JPEG 2000 lossy (.91) at a target N:1 rate.
	ModeLossy
	// ModeLossless encodes to JPEG 2000 lossless (.90) — pixel-for-pixel
	// identical, typically 40-60% smaller.
	ModeLossless
)

func (m Mode) String() string {
	switch m {
	case ModeLossy:
		return "lossy"
	case ModeLossless:
		return "lossless"
	}
	return "none"
}

const (
	// transferSyntaxJP2KLossless is JPEG 2000 Image Compression (Lossless Only).
	transferSyntaxJP2KLossless = "1.2.840.10008.1.2.4.90"
)

// Settings is the operator-facing compression configuration, flattened from
// the config file so this package doesn't depend on the config package.
type Settings struct {
	LossyEnabled               bool
	LossyDefaultRate           int
	LossyModalityRate          map[string]int
	LossySkipAlreadyCompressed bool

	LosslessEnabled               bool
	LosslessModalityEnabled       map[string]bool
	LosslessSkipAlreadyCompressed bool
}

// Plan is the decision for one modality.
type Plan struct {
	Mode                  Mode
	Rate                  int // meaningful only for ModeLossy
	SkipAlreadyCompressed bool
}

// PlanFor picks the encoding for a modality.
//
// Lossy wins when it is enabled and the modality resolves to a non-zero rate.
// An operator who configured "CT 10:1" meant it; quietly applying lossless
// instead would ignore an explicit instruction while the UI still showed the
// ratio they set.
func PlanFor(s Settings, modality string) Plan {
	if s.LossyEnabled {
		if rate := lossyRateFor(s, modality); rate > 0 {
			return Plan{
				Mode:                  ModeLossy,
				Rate:                  rate,
				SkipAlreadyCompressed: s.LossySkipAlreadyCompressed,
			}
		}
	}
	if s.LosslessEnabled && losslessEnabledFor(s, modality) {
		return Plan{
			Mode:                  ModeLossless,
			SkipAlreadyCompressed: s.LosslessSkipAlreadyCompressed,
		}
	}
	return Plan{Mode: ModeNone}
}

// lossyRateFor resolves the N:1 rate for a modality. An explicit entry wins,
// including an explicit 0, which is how an operator switches one modality off
// while leaving a default rate in place for the rest.
func lossyRateFor(s Settings, modality string) int {
	if r, ok := s.LossyModalityRate[modality]; ok {
		return r
	}
	return s.LossyDefaultRate
}

// losslessEnabledFor reports whether lossless applies to a modality.
//
// Absent means enabled. Lossless is safe for every modality, so "all of them"
// is the useful default and the map carves out exceptions only — an empty map
// meaning "none" would silently disable the feature for every site that never
// opened the settings page.
func losslessEnabledFor(s Settings, modality string) bool {
	if v, ok := s.LosslessModalityEnabled[modality]; ok {
		return v
	}
	return true
}

// gdcmconvArgs builds the command line for a plan.
//
// The single flag that separates the two modes is --lossy: from gdcmconv's own
// help, "-Y --lossy  Use the lossy (if possible) compressor", and omitting it
// selects the reversible wavelet. --irreversible and -r are lossy-only.
// TestGdcmconvArgsPerMode fails the build if any of the three ever leaks into
// the lossless path — that one slip would make "lossless" a lie while the UI
// still said lossless.
func gdcmconvArgs(plan Plan, in, out string) []string {
	args := []string{"--j2k"}
	if plan.Mode == ModeLossy {
		args = append(args, "--lossy", "--irreversible", "-r", fmt.Sprint(plan.Rate))
	}
	return append(args, "-i", in, "-o", out)
}

// expectedTransferSyntax is what the output must declare for the plan to be
// accepted.
func expectedTransferSyntax(m Mode) string {
	if m == ModeLossy {
		return transferSyntaxJP2KLossy
	}
	return transferSyntaxJP2KLossless
}

// shouldSkip reports whether a file's existing transfer syntax means this plan
// has nothing to gain.
//
// The two modes deliberately consult different sets:
//
//   - Lossy skips only files that are ALREADY LOSSY, because re-encoding
//     those costs a second generation of loss. An uncompressed or
//     losslessly-compressed file is still worth shrinking.
//   - Lossless skips anything already compressed at all: .90 is already the
//     target, another lossless codec gains almost nothing for the CPU, and an
//     already-lossy file cannot be repaired by storing it losslessly — the
//     result is usually larger than what you started with.
func shouldSkip(mode Mode, ts string) bool {
	if mode == ModeLossy {
		return lossyTransferSyntaxes[ts]
	}
	return compressedTransferSyntaxes[ts]
}

// compressedTransferSyntaxes covers every syntax that already carries encoded
// pixel data, lossy or lossless.
var compressedTransferSyntaxes = map[string]bool{
	"1.2.840.10008.1.2.4.50": true, // JPEG Baseline (lossy)
	"1.2.840.10008.1.2.4.51": true, // JPEG Extended (lossy)
	"1.2.840.10008.1.2.4.57": true, // JPEG Lossless, Non-Hierarchical
	"1.2.840.10008.1.2.4.70": true, // JPEG Lossless, First-Order Prediction
	"1.2.840.10008.1.2.4.80": true, // JPEG-LS Lossless
	"1.2.840.10008.1.2.4.81": true, // JPEG-LS Near-lossless
	"1.2.840.10008.1.2.4.90": true, // JPEG 2000 Lossless
	"1.2.840.10008.1.2.4.91": true, // JPEG 2000 Lossy
	"1.2.840.10008.1.2.4.92": true, // JPEG 2000 Part 2 Lossless
	"1.2.840.10008.1.2.4.93": true, // JPEG 2000 Part 2 Lossy
	"1.2.840.10008.1.2.5":    true, // RLE Lossless
}

// noPixelModalities never carry pixel data, so gdcmconv cannot compress them.
// Checking here avoids launching a process per file only to have it exit 1.
var noPixelModalities = map[string]bool{
	"SR": true, "PR": true, "KO": true, "AU": true, "SEG": true, "REG": true,
	"RTSTRUCT": true, "RTPLAN": true, "RTRECORD": true, "RTDOSE": true,
	"DOC": true, "FID": true, "RWV": true, "PLAN": true, "PMAP": true,
}

// NoPixelModality reports whether a modality never carries pixel data.
func NoPixelModality(modality string) bool {
	return noPixelModalities[strings.ToUpper(strings.TrimSpace(modality))]
}

// Available reports whether gdcmconv can actually be run.
//
// This guard is what makes lossless-on-by-default safe. Without it, an install
// where gdcmconv is missing or cannot execute would route every series into
// the transcode queue, fail each one, and eventually mark studies Failed. With
// it, the sender degrades to "send as-is", which is exactly what the previous
// release did anyway.
//
// Existence is not enough, which is the lesson behind the exec probe below.
// The bundled GDCM is now the 32-bit (x86) build precisely so it runs on
// 32-bit Windows as well as 64-bit — but a file that is present and a file
// that the OS will actually load are different things: an arch mismatch or a
// missing MSVC runtime DLL both present as a binary that stats fine and then
// dies on every invocation. So we run it once, at startup, and believe the
// result rather than the directory listing.
func Available(path string) bool {
	if path == "" {
		return false
	}
	if strings.ContainsAny(path, `/\`) {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			return false
		}
	} else if _, err := exec.LookPath(path); err != nil {
		return false
	}
	return runnable(path)
}

// runnable executes "gdcmconv --version" and reports whether it completed.
//
// Called once at startup, so the cost is one short-lived process. The timeout
// is the point of the context: a binary that hangs instead of failing must not
// hang the sender's startup with it.
func runnable(path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A non-zero exit still proves the image loaded and ran, which is all this
	// check is asking; only a failure to start (bad arch, missing DLL) matters.
	if err := exec.CommandContext(ctx, path, "--version").Run(); err != nil {
		var ee *exec.ExitError
		return errors.As(err, &ee)
	}
	return true
}
