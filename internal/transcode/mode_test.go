package transcode

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPlanForPrecedence pins which mode a modality gets. The load-bearing rule
// is that an explicitly configured lossy rate beats lossless: an operator who
// set "CT 10:1" meant it, and quietly applying lossless instead would ignore
// an explicit instruction while the UI still showed their ratio.
func TestPlanForPrecedence(t *testing.T) {
	both := Settings{
		LossyEnabled:            true,
		LossyDefaultRate:        0,
		LossyModalityRate:       map[string]int{"CT": 10, "MR": 0},
		LosslessEnabled:         true,
		LosslessModalityEnabled: map[string]bool{"US": false},
	}

	cases := []struct {
		name     string
		settings Settings
		modality string
		wantMode Mode
		wantRate int
	}{
		{"both off means send as-is", Settings{}, "CT", ModeNone, 0},

		{"lossy wins where a rate is configured", both, "CT", ModeLossy, 10},
		{"an explicit rate of 0 falls through to lossless", both, "MR", ModeLossless, 0},
		{"no rate and no default falls through to lossless", both, "DX", ModeLossless, 0},
		{"a lossless exception with no lossy rate means no compression", both, "US", ModeNone, 0},

		{"lossless alone, absent modality is enabled",
			Settings{LosslessEnabled: true}, "CT", ModeLossless, 0},
		{"lossless alone, empty exception map still enables everything",
			Settings{LosslessEnabled: true, LosslessModalityEnabled: map[string]bool{}}, "XA", ModeLossless, 0},
		{"lossless alone, explicit exception disables",
			Settings{LosslessEnabled: true, LosslessModalityEnabled: map[string]bool{"XA": false}}, "XA", ModeNone, 0},

		{"lossy alone with a default rate",
			Settings{LossyEnabled: true, LossyDefaultRate: 8}, "MG", ModeLossy, 8},
		{"lossy alone with no rate at all is no compression",
			Settings{LossyEnabled: true}, "MG", ModeNone, 0},
		{"a modality rate overrides the default",
			Settings{LossyEnabled: true, LossyDefaultRate: 8, LossyModalityRate: map[string]int{"MG": 4}}, "MG", ModeLossy, 4},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := PlanFor(tc.settings, tc.modality)
			if got.Mode != tc.wantMode {
				t.Fatalf("Mode = %s, want %s", got.Mode, tc.wantMode)
			}
			if got.Rate != tc.wantRate {
				t.Fatalf("Rate = %d, want %d", got.Rate, tc.wantRate)
			}
		})
	}
}

// TestStoredRateStaysInertWhileLossyIsOff is the safety property behind
// shipping a default "CT: 10" in the config: a leftover ratio must not be able
// to quietly start discarding image data when somebody enables lossless.
func TestStoredRateStaysInertWhileLossyIsOff(t *testing.T) {
	s := Settings{
		LossyEnabled:      false,
		LossyModalityRate: map[string]int{"CT": 10},
		LosslessEnabled:   true,
	}
	if got := PlanFor(s, "CT"); got.Mode != ModeLossless {
		t.Fatalf("a stored lossy rate produced %s with the lossy toggle off", got.Mode)
	}
}

// TestGdcmconvArgsPerMode is the single most important test in this package.
// --lossy, --irreversible and -r are what make gdcmconv discard image data;
// if any of them ever leaks into the lossless path, "lossless" becomes a lie
// while the UI still says lossless and nobody finds out from the output.
func TestGdcmconvArgsPerMode(t *testing.T) {
	lossless := gdcmconvArgs(Plan{Mode: ModeLossless}, "in.dcm", "out.tmp")
	joined := strings.Join(lossless, " ")

	for _, forbidden := range []string{"--lossy", "--irreversible", "-r"} {
		for _, arg := range lossless {
			if arg == forbidden {
				t.Fatalf("lossless args contain %q: %s", forbidden, joined)
			}
		}
	}
	if want := "--j2k -i in.dcm -o out.tmp"; joined != want {
		t.Fatalf("lossless args = %q, want %q", joined, want)
	}

	lossy := strings.Join(gdcmconvArgs(Plan{Mode: ModeLossy, Rate: 10}, "in.dcm", "out.tmp"), " ")
	if want := "--j2k --lossy --irreversible -r 10 -i in.dcm -o out.tmp"; lossy != want {
		t.Fatalf("lossy args = %q, want %q", lossy, want)
	}
}

// TestExpectedTransferSyntaxPerMode: the post-encode guard compares against
// these, so mixing them up would accept the wrong encoding.
func TestExpectedTransferSyntaxPerMode(t *testing.T) {
	if got := expectedTransferSyntax(ModeLossless); got != "1.2.840.10008.1.2.4.90" {
		t.Fatalf("lossless expects %q, want .90", got)
	}
	if got := expectedTransferSyntax(ModeLossy); got != "1.2.840.10008.1.2.4.91" {
		t.Fatalf("lossy expects %q, want .91", got)
	}
}

// TestSkipSetsDifferPerMode pins the asymmetry between the two skip sets,
// which is deliberate and easy to "simplify" into a bug.
func TestSkipSetsDifferPerMode(t *testing.T) {
	cases := []struct {
		ts              string
		what            string
		skipForLossy    bool
		skipForLossless bool
	}{
		{"1.2.840.10008.1.2", "implicit VR LE (uncompressed)", false, false},
		{"1.2.840.10008.1.2.1", "explicit VR LE (uncompressed)", false, false},
		{"1.2.840.10008.1.2.4.50", "JPEG baseline (lossy)", true, true},
		{"1.2.840.10008.1.2.4.91", "JPEG 2000 lossy", true, true},
		// The interesting rows: already-lossless files are still worth
		// re-encoding to lossy, but not worth re-encoding to lossless.
		{"1.2.840.10008.1.2.4.70", "JPEG lossless", false, true},
		{"1.2.840.10008.1.2.4.90", "JPEG 2000 lossless", false, true},
		{"1.2.840.10008.1.2.5", "RLE lossless", false, true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.what, func(t *testing.T) {
			if got := shouldSkip(ModeLossy, tc.ts); got != tc.skipForLossy {
				t.Errorf("lossy skip = %v, want %v", got, tc.skipForLossy)
			}
			if got := shouldSkip(ModeLossless, tc.ts); got != tc.skipForLossless {
				t.Errorf("lossless skip = %v, want %v", got, tc.skipForLossless)
			}
		})
	}
}

// TestAvailable covers the guard that makes lossless-on-by-default safe. If it
// wrongly reports true, every series is routed into a transcode queue that
// cannot succeed and studies end up Failed.
func TestAvailable(t *testing.T) {
	if Available("") {
		t.Error("an empty path reported available")
	}

	dir := t.TempDir()
	missing := filepath.Join(dir, "gdcmconv.exe")
	if Available(missing) {
		t.Error("a non-existent path reported available")
	}

	// A real, launchable binary at a full path. The test binary itself is the
	// one executable guaranteed to exist on every platform the sender builds
	// for; "--version" is not a flag it defines, so it exits non-zero during
	// flag parsing — which still proves the OS loaded and ran it.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !Available(self) {
		t.Error("a runnable binary reported unavailable")
	}

	// Existence is not runnability. This file has the name and the executable
	// bit but no valid executable image, which is how an architecture mismatch
	// presents — the case the bundled 32-bit GDCM exists to avoid. Reporting it
	// available would fail every study instead of falling back to sending them
	// in their original encoding.
	notExec := filepath.Join(dir, "gdcmconv-not-an-executable")
	if err := os.WriteFile(notExec, []byte("this is not a program"), 0o755); err != nil {
		t.Fatal(err)
	}
	if Available(notExec) {
		t.Error("a file that cannot be executed reported available")
	}

	if Available(dir) {
		t.Error("a directory reported available")
	}

	// A bare name is resolved through PATH.
	bare := "go"
	if runtime.GOOS == "windows" {
		bare = "go.exe"
	}
	if !Available(bare) {
		t.Errorf("%q on PATH reported unavailable", bare)
	}
	if Available("definitely-not-a-real-binary-xyz") {
		t.Error("a bogus PATH name reported available")
	}
}

// TestNoPixelModality: these are skipped before gdcmconv is launched at all,
// because it cannot compress them and exits non-zero.
func TestNoPixelModality(t *testing.T) {
	for _, m := range []string{"SR", "PR", "KO", "AU", "SEG", "REG",
		"RTSTRUCT", "RTPLAN", "RTRECORD", "RTDOSE", "DOC", "FID", "RWV", "PLAN", "PMAP"} {
		if !NoPixelModality(m) {
			t.Errorf("%s should be treated as non-image", m)
		}
	}
	for _, m := range []string{"CT", "MR", "US", "DX", "CR", "XA", "MG", "PT", "NM"} {
		if NoPixelModality(m) {
			t.Errorf("%s must not be treated as non-image", m)
		}
	}
	if !NoPixelModality("sr") {
		t.Error("modality matching should be case-insensitive")
	}
}

// TestPlanNoneIsANoOp: with no mode selected, transcoding must not touch the
// file or invoke anything.
func TestPlanNoneIsANoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.dcm")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A deliberately bogus gdcmconv path: if the plan were acted on, this
	// would fail loudly rather than silently succeeding.
	if err := TranscodeFile(nil, "/nonexistent/gdcmconv", path, Plan{Mode: ModeNone}); err != nil {
		t.Fatalf("ModeNone returned %v, want nil", err)
	}
	if err := TranscodeSeries(nil, "/nonexistent/gdcmconv", []string{path}, Plan{Mode: ModeNone}); err != nil {
		t.Fatalf("ModeNone series returned %v, want nil", err)
	}

	b, err := os.ReadFile(path)
	if err != nil || string(b) != "original" {
		t.Fatalf("file was modified: %q (%v)", string(b), err)
	}
}

// TestLossyRateZeroIsANoOp: rate 0 is the documented "disabled for this
// modality" value and must not reach gdcmconv.
func TestLossyRateZeroIsANoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.dcm")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := TranscodeFile(nil, "/nonexistent/gdcmconv", path, Plan{Mode: ModeLossy, Rate: 0}); err != nil {
		t.Fatalf("rate 0 returned %v, want nil", err)
	}
}
