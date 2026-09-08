package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharatpacs/tarang-sender/internal/transcode"
)

// The shipped per-modality lossless policy: CT, MR, CR and DX all on.
//
// The policy map is now INERT on a default config, because the lossless mode
// itself ships off along with the lossy one — nothing rewrites pixel data
// unless a site opts in. The map is still pinned: it decides WHICH modalities
// transcode the moment an operator switches the mode on, and that list is a
// clinical decision, not an implementation detail.
//
// This is a policy, not a technical limit, so nothing else in the code will
// notice if it drifts — a stray edit to the default map, or a `??` creeping
// back into the Electron bootstrap, would change what a whole fleet transcodes
// without a single test going red. These tests are the thing that notices.
//
// The file exists because CR and DX were once switched OFF here, after every
// transcoded DX series was refused by the receiving Orthanc (HTTP 400, empty
// ReferencedSOPSequence). They are on again to be retested against GDCM 3.2.7,
// which replaced the 3.2.6 build that was in play when that was observed. See
// DefaultLosslessModalityPolicy for the full history — turning these on or off
// should stay a deliberate act, which is the whole point of pinning them.

var wantPolicy = map[string]bool{
	"CT": true,
	"MR": true,
	"CR": true,
	"DX": true,
}

func TestDefaultLosslessModalityPolicy(t *testing.T) {
	got := Default().Compression.Lossless.ModalityEnabled

	for modality, want := range wantPolicy {
		v, ok := got[modality]
		if !ok {
			t.Errorf("%s missing from the default policy — absent means ENABLED, which is wrong for a modality we mean to switch off", modality)
			continue
		}
		if v != want {
			t.Errorf("lossless for %s = %v, want %v", modality, v, want)
		}
	}

	for modality, v := range got {
		if _, expected := wantPolicy[modality]; !expected {
			t.Errorf("unexpected modality %q = %v in the default policy; the map carries exceptions only", modality, v)
		}
	}

	// The mode itself ships OFF, independently of the policy above. The two
	// are separate switches: this one decides whether ANY transcoding
	// happens, the map decides which modalities it applies to once it does.
	if Default().Compression.Lossless.Enabled {
		t.Error("lossless mode is enabled by default; both transcode modes ship off and must be opt-in")
	}
}

// TestDefaultLosslessPolicyIsNotShared guards the reason
// DefaultLosslessModalityPolicy returns a fresh map: Load decodes the on-disk
// file ON TOP of Default(), and encoding/json merges into an existing map. A
// package-level map would accumulate every modality any config ever named, and
// leak one site's settings into the next Load.
func TestDefaultLosslessPolicyIsNotShared(t *testing.T) {
	first := Default().Compression.Lossless.ModalityEnabled
	first["US"] = false

	if _, leaked := Default().Compression.Lossless.ModalityEnabled["US"]; leaked {
		t.Fatal("mutating one Default()'s policy map changed the next one — the map is shared")
	}
}

// TestLosslessPolicyDrivesTranscodePlans is the end of the chain that actually
// matters: what the sender's config actually makes PlanFor return, through the
// same call the queue makes.
//
// Two halves, because the mode and the policy are separate switches:
//
//   - On a DEFAULT config nothing transcodes at all. This is the guarantee the
//     shipped build makes — a study leaves byte-for-byte as the modality sent
//     it — and it must hold for every modality, including the ones the policy
//     map names, because an enabled entry must not resurrect a mode that is
//     switched off.
//   - Once a site opts IN, the policy map takes over and decides which
//     modalities transcode.
func TestLosslessPolicyDrivesTranscodePlans(t *testing.T) {
	cfg := Default()
	settings := transcode.Settings{
		LosslessEnabled:               cfg.Compression.Lossless.Enabled,
		LosslessModalityEnabled:       cfg.Compression.Lossless.ModalityEnabled,
		LosslessSkipAlreadyCompressed: cfg.Compression.Lossless.SkipAlreadyCompressed,
	}

	// The map names CT/MR/CR/DX as enabled; US it does not name, which reads
	// as enabled too. None of that may matter while the mode is off.
	for _, modality := range []string{"CT", "MR", "CR", "DX", "US"} {
		if got := transcode.PlanFor(settings, modality).Mode; got != transcode.ModeNone {
			t.Errorf("PlanFor(%s) on a default config = %v, want %v — transcoding is opt-in",
				modality, got, transcode.ModeNone)
		}
	}

	// Opt in, and the policy is what decides.
	settings.LosslessEnabled = true

	cases := []struct {
		modality string
		want     transcode.Mode
	}{
		{"CT", transcode.ModeLossless},
		{"MR", transcode.ModeLossless},
		{"CR", transcode.ModeLossless},
		{"DX", transcode.ModeLossless},
		// Not named by the policy, so still enabled — the map is exceptions
		// only, and an empty map must never read as "off for everything".
		{"US", transcode.ModeLossless},
	}
	for _, c := range cases {
		if got := transcode.PlanFor(settings, c.modality).Mode; got != c.want {
			t.Errorf("PlanFor(%s) after opt-in = %v, want %v", c.modality, got, c.want)
		}
	}
}

// TestStoredConfigKeepsPolicyForModalitiesItDoesNotName is the half of "it
// lands on existing installs" that the Go side owns.
//
// A config written before the policy existed carries its own modality_enabled
// map. Load decodes it over Default(), and because encoding/json merges into
// the existing map rather than replacing it, the policy survives for every
// modality the stored file does not mention — while an operator's explicit
// choice still wins for the ones it does.
func TestStoredConfigKeepsPolicyForModalitiesItDoesNotName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	stored := `{
	  "lab_id": "L1",
	  "org_id": "O1",
	  "peer": { "name": "ACHYU", "url": "https://router.achyutrs.com" },
	  "compression": {
	    "lossless": {
	      "enabled": true,
	      "modality_enabled": { "US": false }
	    }
	  }
	}`
	if err := os.WriteFile(path, []byte(stored), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m := cfg.Compression.Lossless.ModalityEnabled

	// Never mentioned by the stored file: policy applies.
	if v, ok := m["CR"]; !ok || !v {
		t.Errorf("CR = %v (present=%v), want true — the policy should survive a stored map", v, ok)
	}
	if v, ok := m["CT"]; !ok || !v {
		t.Errorf("CT = %v (present=%v), want true", v, ok)
	}
	// Named by the stored file: the operator's explicit choice wins on load.
	// Electron re-asserts the policy over the top on its next config write;
	// that is the deliberate split, and the reason the bootstrap uses
	// Object.assign. US is the operator's own exception and must survive.
	if v, ok := m["US"]; !ok || v {
		t.Errorf("US = %v (present=%v), want false — an explicit stored value must win on load", v, ok)
	}
}

// TestElectronBootstrapPolicyMatchesGo pins the two copies of the policy to
// each other. main.js writes the config the Go engine reads, so if the lists
// ever disagree the engine's default silently loses to whatever Electron
// wrote, and the drift is invisible until a site transcodes something it
// should not have.
func TestElectronBootstrapPolicyMatchesGo(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "electron", "main", "main.js"))
	if err != nil {
		t.Fatalf("read main.js: %v", err)
	}
	text := string(src)

	const marker = "const LOSSLESS_MODALITY_POLICY = "
	i := strings.Index(text, marker)
	if i < 0 {
		t.Fatal("LOSSLESS_MODALITY_POLICY not found in main.js — did the constant get renamed or inlined?")
	}
	rest := text[i+len(marker):]
	end := strings.Index(rest, ";")
	if end < 0 {
		t.Fatal("could not find the end of the LOSSLESS_MODALITY_POLICY literal")
	}
	// The JS object literal uses bare keys; quote them so it parses as JSON.
	literal := rest[:end]
	for _, k := range []string{"CT", "MR", "CR", "DX"} {
		literal = strings.Replace(literal, k+":", `"`+k+`":`, 1)
	}

	var got map[string]bool
	if err := json.Unmarshal([]byte(literal), &got); err != nil {
		t.Fatalf("parse %q: %v", literal, err)
	}
	if len(got) != len(wantPolicy) {
		t.Fatalf("main.js policy has %d entries, Go has %d: %v vs %v", len(got), len(wantPolicy), got, wantPolicy)
	}
	for modality, want := range wantPolicy {
		if got[modality] != want {
			t.Errorf("main.js policy for %s = %v, Go says %v", modality, got[modality], want)
		}
	}

	// The `??` this replaced only applies when the key is missing entirely, so
	// it never reached an install that already had a stored map.
	if !strings.Contains(text, "modality_enabled: Object.assign(") {
		t.Error("the Electron bootstrap no longer assigns modality_enabled with Object.assign; a `??` default would not reach installs that already carry a stored map")
	}
}
