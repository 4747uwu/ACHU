package scp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bharatpacs/tarang-sender/internal/dicom"
	"github.com/bharatpacs/tarang-sender/internal/log"
	"github.com/bharatpacs/tarang-sender/internal/store"
)

// StudyFirstSeenMeta carries the study metadata available at the moment
// the first instance of a study arrives.
type StudyFirstSeenMeta struct {
	StudyInstanceUID  string
	PatientID         string
	PatientName       string
	StudyDate         string
	StudyTime         string
	StudyDescription  string
	AccessionNumber   string
	Modality          string
}

// IngestHandler is the production Handler implementation.
//
// On each C-STORE-RQ it:
//
//  1. Parses the dataset for indexable metadata (patient/study/series tags)
//  2. Injects the standard set of private tags (matching tagwrite.lua) if
//     tag injection is enabled and the transfer syntax is supported
//  3. Writes the DICOM Part 10 file to <DataDir>/instances/<study>/<series>/<sop>.dcm
//  4. Atomically indexes the instance, series, and study in BoltDB
//  5. Notifies the stability watcher (if configured) that the series saw a new instance
type IngestHandler struct {
	DataDir string
	Store   *store.Store

	// Tag injection — semantics match the legacy tagwrite.lua.
	InjectionEnabled    bool
	PrivateCreator      string
	PrivateOrganisation string

	// OnInstanceStored is invoked after a successful ingest for a series.
	// The stability watcher (M3) sets this to reset its per-series timer.
	// Nil-safe.
	OnInstanceStored func(seriesUID string, imagesInAcquisition int)

	// OnStudyFirstSeen is called exactly once per study when the very first
	// instance arrives. Used by the notifier to pre-register the study on
	// the backend PACS so it appears immediately with an "upload_pending" tag.
	// Nil-safe.
	OnStudyFirstSeen func(studyUID string, meta StudyFirstSeenMeta)
}

// OnCStore handles one C-STORE-RQ.
//
// Returning a non-nil error causes the SCP to reply with a non-success
// DIMSE status, which most modalities respect by re-attempting later.
func (h *IngestHandler) OnCStore(ctx context.Context, msg CStoreMessage) error {
	_ = ctx
	if h.DataDir == "" {
		return errors.New("ingest handler: DataDir not configured")
	}
	if h.Store == nil {
		return errors.New("ingest handler: Store not configured")
	}

	// Parse for full metadata. The function falls back to SCP-provided UIDs
	// for the SOP class/instance if those tags are missing in the dataset
	// (which happens with malformed instances from older modalities).
	meta, err := dicom.ParseDatasetMeta(
		msg.Dataset,
		msg.TransferSyntaxUID,
		msg.SOPClassUID,
		msg.SOPInstanceUID,
	)
	if err != nil {
		log.L().Warn("dataset parse failed",
			"sop_uid", msg.SOPInstanceUID,
			"calling_aet", msg.CallingAET,
			"err", err,
		)
		return fmt.Errorf("parse dataset: %w", err)
	}

	if meta.StudyInstanceUID == "" || meta.SeriesInstanceUID == "" {
		return fmt.Errorf(
			"missing required UIDs (study=%q series=%q sop=%q)",
			meta.StudyInstanceUID, meta.SeriesInstanceUID, meta.SOPInstanceUID,
		)
	}

	// Sanity: the SOP UID in the wire command should match the dataset.
	// Real-world modalities sometimes get this wrong; trust the dataset.
	if msg.SOPInstanceUID != "" && meta.SOPInstanceUID != msg.SOPInstanceUID {
		log.L().Warn("sop-uid mismatch between command and dataset",
			"command", msg.SOPInstanceUID,
			"dataset", meta.SOPInstanceUID,
		)
	}

	// Compute file path: <data_dir>/instances/<study>/<series>/<sop>.dcm.
	// We use UIDs verbatim — they're well-formed identifiers (digits and
	// dots) and safe as path components on Windows and POSIX alike.
	filePath := filepath.Join(
		h.DataDir,
		"instances",
		meta.StudyInstanceUID,
		meta.SeriesInstanceUID,
		meta.SOPInstanceUID+".dcm",
	)

	// Tag injection (M2): match tagwrite.lua semantics. Inject private
	// tags into the dataset bytes BEFORE writing — the on-disk file and
	// the eventually-pushed bytes are identical to what the modality
	// would have seen after the legacy Lua re-upload-to-self dance.
	datasetBytes := msg.Dataset
	modified := false
	if h.InjectionEnabled && h.PrivateCreator != "" && h.PrivateOrganisation != "" {
		tags := dicom.StandardPrivateTags(h.PrivateCreator, h.PrivateOrganisation)
		injected, err := dicom.InjectPrivateTags(datasetBytes, msg.TransferSyntaxUID, tags)
		if err != nil {
			// Unsupported syntax (Big Endian, Deflated) or scan failure.
			// Pass the dataset through unchanged so we don't drop the study.
			log.L().Warn("tag injection skipped",
				"sop_uid", meta.SOPInstanceUID,
				"transfer_syntax", msg.TransferSyntaxUID,
				"err", err,
			)
		} else {
			datasetBytes = injected
			modified = true
		}
	}

	if err := dicom.WriteDICOMFile(
		filePath,
		msg.TransferSyntaxUID,
		meta.SOPClassUID,
		meta.SOPInstanceUID,
		datasetBytes,
	); err != nil {
		return fmt.Errorf("write dicom file %q: %w", filePath, err)
	}

	// Get the on-disk file size for the index. The Part 10 wrapper adds a
	// small fixed overhead (~200 bytes) on top of the dataset bytes; the
	// stat is authoritative.
	var fileSize int64
	if fi, err := os.Stat(filePath); err == nil {
		fileSize = fi.Size()
	} else {
		fileSize = int64(len(msg.Dataset)) // fallback
	}

	rec := store.InstanceRecord{
		SOPInstanceUID:    meta.SOPInstanceUID,
		SeriesInstanceUID: meta.SeriesInstanceUID,
		StudyInstanceUID:  meta.StudyInstanceUID,
		FilePath:          filePath,
		FileSize:          fileSize,
		TransferSyntaxUID: msg.TransferSyntaxUID,
		SOPClassUID:       meta.SOPClassUID,
		ReceivedAt:        time.Now(),
		Modified:          modified,
	}

	seriesMeta := store.SeriesMetaUpdate{
		Modality:          meta.Modality,
		SeriesDescription: meta.SeriesDescription,
		SeriesNumber:      meta.SeriesNumber,
	}
	studyMeta := store.StudyMetaUpdate{
		PatientID:        meta.PatientID,
		PatientName:      meta.PatientName,
		StudyDate:        meta.StudyDate,
		StudyTime:        meta.StudyTime,
		StudyDescription: meta.StudyDescription,
		AccessionNumber:  meta.AccessionNumber,
	}

	if err := h.Store.PutInstance(rec, seriesMeta, studyMeta); err != nil {
		return fmt.Errorf("store put-instance: %w", err)
	}

	// Notify the stability watcher (if configured) so its per-series timer
	// resets. When the series goes quiet for stable_age, the watcher fires
	// and enqueues the series for transfer (M3).
	if h.OnInstanceStored != nil {
		h.OnInstanceStored(meta.SeriesInstanceUID, meta.ImagesInAcquisition)
	}
	if h.OnStudyFirstSeen != nil {
		// Fire only for the first instance of a study. PutInstance returns
		// success even if the study already existed; check the instance count.
		// We check via a lightweight store query; the result is best-effort.
		insts, _ := h.Store.ListInstancesBySeries(meta.SeriesInstanceUID)
		if len(insts) == 1 {
			go h.OnStudyFirstSeen(meta.StudyInstanceUID, StudyFirstSeenMeta{
				StudyInstanceUID: meta.StudyInstanceUID,
				PatientID:        meta.PatientID,
				PatientName:      meta.PatientName,
				StudyDate:        meta.StudyDate,
				StudyTime:        meta.StudyTime,
				StudyDescription: meta.StudyDescription,
				AccessionNumber:  meta.AccessionNumber,
				Modality:         meta.Modality,
			})
		}
	}

	log.L().Info("instance received",
		"sop_uid", meta.SOPInstanceUID,
		"study_uid", meta.StudyInstanceUID,
		"series_uid", meta.SeriesInstanceUID,
		"modality", meta.Modality,
		"patient", meta.PatientName,
		"size", fileSize,
		"transfer_syntax", msg.TransferSyntaxUID,
		"injected", modified,
		"calling_aet", msg.CallingAET,
		"remote", msg.RemoteAddr,
	)

	return nil
}
