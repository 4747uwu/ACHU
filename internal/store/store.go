// Package store wraps BoltDB and exposes all persistence operations the
// sender needs.
//
// Bucket layout:
//
//	instances  : SOP UID                  -> InstanceRecord
//	series     : Series UID               -> SeriesRecord
//	studies    : Study UID                -> StudyRecord
//	queue      : nanosec timestamp + UID  -> QueueEntry  (sortable, FIFO)
//	transfers  : transfer ID              -> TransferRecord
//
//	idx_series_by_study     : studyUID + "/" + seriesUID -> empty
//	idx_instances_by_series : seriesUID + "/" + sopUID   -> empty
//	idx_studies_by_status   : status + "/" + studyUID    -> empty
//
// Index buckets store empty values; the keys themselves are the index. This
// makes prefix scans cheap (BoltDB keys are byte-sorted in their bucket).
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

var (
	bucketInstances        = []byte("instances")
	bucketSeries           = []byte("series")
	bucketStudies          = []byte("studies")
	bucketQueue            = []byte("queue")
	bucketTransfers        = []byte("transfers")
	bucketIdxSeriesByStudy = []byte("idx_series_by_study")
	bucketIdxInstByStudy   = []byte("idx_instances_by_series")
	bucketIdxStudyByStatus = []byte("idx_studies_by_status")

	allBuckets = [][]byte{
		bucketInstances, bucketSeries, bucketStudies,
		bucketQueue, bucketTransfers,
		bucketIdxSeriesByStudy, bucketIdxInstByStudy, bucketIdxStudyByStatus,
	}
)

// ErrNotFound is returned when a record is not present.
var ErrNotFound = errors.New("not found")

// Store is the persistence layer.
type Store struct {
	db *bbolt.DB
}

// Open opens (or creates) a BoltDB at path and ensures all buckets exist.
func Open(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{
		Timeout: 3 * time.Second, // refuse to wait forever for a stale lock

		// NoFreelistSync keeps the free-page list out of every commit.
		//
		// Bolt fsyncs on every write transaction, and by default each one also
		// rewrites the freelist. On an SSD that is invisible. On the 5400rpm
		// drives these clinic machines actually have, an fsync costs 10-20ms
		// against ~0.1ms, and instances arrive every 200-300ms while a study is
		// being pushed — so the write amplification lands squarely on the
		// receive path and shows up as the modality timing out.
		//
		// This does NOT weaken durability of the data. The freelist is derived
		// state: when it is absent Bolt rebuilds it by scanning the pages on
		// open. The only cost is a slower open on a large database, paid once
		// at startup instead of on every instance received.
		NoFreelistSync: true,
	})
	if err != nil {
		return nil, fmt.Errorf("open bolt %q: %w", path, err)
	}

	err = db.Update(func(tx *bbolt.Tx) error {
		for _, name := range allBuckets {
			if _, e := tx.CreateBucketIfNotExists(name); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

// Close releases the DB lock.
func (s *Store) Close() error { return s.db.Close() }

// ---------- instances ----------------------------------------------------

// PutInstance creates or updates an instance record AND updates the parent
// series + study aggregates atomically. This is the single ingest path:
// every received instance flows through this one method.
//
// If the instance already exists with the same SOP UID, this is treated as
// a re-receive (e.g. modality retry); the file path is overwritten and
// aggregates are NOT double-counted.
func (s *Store) PutInstance(rec InstanceRecord, seriesMeta SeriesMetaUpdate, studyMeta StudyMetaUpdate) error {
	if rec.SOPInstanceUID == "" || rec.SeriesInstanceUID == "" || rec.StudyInstanceUID == "" {
		return errors.New("missing UID(s) on instance record")
	}

	return s.db.Update(func(tx *bbolt.Tx) error {
		instances := tx.Bucket(bucketInstances)
		series := tx.Bucket(bucketSeries)
		studies := tx.Bucket(bucketStudies)
		idxSerByStudy := tx.Bucket(bucketIdxSeriesByStudy)
		idxInstBySer := tx.Bucket(bucketIdxInstByStudy)
		idxStudyByStatus := tx.Bucket(bucketIdxStudyByStatus)

		// Did this instance already exist? (idempotent re-receive)
		_, alreadyExisted := getJSON[InstanceRecord](instances, []byte(rec.SOPInstanceUID))

		// Write the instance.
		if err := putJSON(instances, []byte(rec.SOPInstanceUID), rec); err != nil {
			return err
		}
		if err := idxInstBySer.Put(idxKey(rec.SeriesInstanceUID, rec.SOPInstanceUID), nil); err != nil {
			return err
		}

		// ---- update / create series ----
		ser, hadSeries := getJSON[SeriesRecord](series, []byte(rec.SeriesInstanceUID))
		if !hadSeries {
			ser = SeriesRecord{
				SeriesInstanceUID: rec.SeriesInstanceUID,
				StudyInstanceUID:  rec.StudyInstanceUID,
				FirstReceivedAt:   rec.ReceivedAt,
				Status:            StatusReceived,
			}
		}
		applySeriesMeta(&ser, seriesMeta)
		if !alreadyExisted {
			ser.InstanceCount++
			ser.TotalSize += rec.FileSize
		}
		if rec.ReceivedAt.After(ser.LastInstanceAt) {
			ser.LastInstanceAt = rec.ReceivedAt
		}
		if err := putJSON(series, []byte(rec.SeriesInstanceUID), ser); err != nil {
			return err
		}
		if !hadSeries {
			if err := idxSerByStudy.Put(idxKey(rec.StudyInstanceUID, rec.SeriesInstanceUID), nil); err != nil {
				return err
			}
		}

		// ---- update / create study ----
		stu, hadStudy := getJSON[StudyRecord](studies, []byte(rec.StudyInstanceUID))
		oldStatus := stu.Status
		if !hadStudy {
			stu = StudyRecord{
				StudyInstanceUID: rec.StudyInstanceUID,
				FirstReceivedAt:  rec.ReceivedAt,
				Status:           StatusReceived,
			}
		}
		applyStudyMeta(&stu, studyMeta)
		// New series for this study?
		if !hadSeries {
			stu.SeriesCount++
			if seriesMeta.Modality != "" && !contains(stu.Modalities, seriesMeta.Modality) {
				stu.Modalities = append(stu.Modalities, seriesMeta.Modality)
			}
		}
		if !alreadyExisted {
			stu.InstanceCount++
			stu.TotalSize += rec.FileSize
		}
		if rec.ReceivedAt.After(stu.LastInstanceAt) {
			stu.LastInstanceAt = rec.ReceivedAt
		}
		if err := putJSON(studies, []byte(rec.StudyInstanceUID), stu); err != nil {
			return err
		}

		// Maintain status index for the study.
		if hadStudy && oldStatus != "" && oldStatus != stu.Status {
			_ = idxStudyByStatus.Delete(idxKey(oldStatus, rec.StudyInstanceUID))
		}
		if err := idxStudyByStatus.Put(idxKey(stu.Status, rec.StudyInstanceUID), nil); err != nil {
			return err
		}

		return nil
	})
}

// GetInstance fetches by SOP UID.
func (s *Store) GetInstance(sopUID string) (InstanceRecord, error) {
	var out InstanceRecord
	err := s.db.View(func(tx *bbolt.Tx) error {
		v, ok := getJSON[InstanceRecord](tx.Bucket(bucketInstances), []byte(sopUID))
		if !ok {
			return ErrNotFound
		}
		out = v
		return nil
	})
	return out, err
}

// ---------- series -------------------------------------------------------

// GetSeries fetches by Series UID.
func (s *Store) GetSeries(uid string) (SeriesRecord, error) {
	var out SeriesRecord
	err := s.db.View(func(tx *bbolt.Tx) error {
		v, ok := getJSON[SeriesRecord](tx.Bucket(bucketSeries), []byte(uid))
		if !ok {
			return ErrNotFound
		}
		out = v
		return nil
	})
	return out, err
}

// ListSeriesByStudy returns all series belonging to one study, sorted by
// series number (ascending).
func (s *Store) ListSeriesByStudy(studyUID string) ([]SeriesRecord, error) {
	var out []SeriesRecord
	err := s.db.View(func(tx *bbolt.Tx) error {
		idx := tx.Bucket(bucketIdxSeriesByStudy)
		series := tx.Bucket(bucketSeries)
		prefix := []byte(studyUID + "/")
		c := idx.Cursor()
		for k, _ := c.Seek(prefix); k != nil && hasPrefix(k, prefix); k, _ = c.Next() {
			seriesUID := string(k[len(prefix):])
			if v, ok := getJSON[SeriesRecord](series, []byte(seriesUID)); ok {
				out = append(out, v)
			}
		}
		return nil
	})
	return out, err
}

// ListInstancesBySeries returns every instance record belonging to the
// given series. Order follows the lexicographic SOP UID (BoltDB key order),
// which is generally close to but not always exactly the modality's
// acquisition order.
func (s *Store) ListInstancesBySeries(seriesUID string) ([]InstanceRecord, error) {
	var out []InstanceRecord
	err := s.db.View(func(tx *bbolt.Tx) error {
		idx := tx.Bucket(bucketIdxInstByStudy)
		instances := tx.Bucket(bucketInstances)
		prefix := []byte(seriesUID + "/")
		c := idx.Cursor()
		for k, _ := c.Seek(prefix); k != nil && hasPrefix(k, prefix); k, _ = c.Next() {
			sopUID := string(k[len(prefix):])
			if v, ok := getJSON[InstanceRecord](instances, []byte(sopUID)); ok {
				out = append(out, v)
			}
		}
		return nil
	})
	return out, err
}

// SetSeriesStatus updates only the status field of a series, atomically.
func (s *Store) SetSeriesStatus(uid, status string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSeries)
		v, ok := getJSON[SeriesRecord](b, []byte(uid))
		if !ok {
			return ErrNotFound
		}
		v.Status = status
		return putJSON(b, []byte(uid), v)
	})
}

// MarkSOPsDelivered appends the given SOP Instance UIDs to the series'
// delivered checkpoint. Called after each successfully uploaded chunk so
// that a mid-series network failure doesn't force re-uploading everything.
func (s *Store) MarkSOPsDelivered(seriesUID string, sopUIDs []string) error {
	if len(sopUIDs) == 0 {
		return nil
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSeries)
		v, ok := getJSON[SeriesRecord](b, []byte(seriesUID))
		if !ok {
			return ErrNotFound
		}
		v.DeliveredSOPs = append(v.DeliveredSOPs, sopUIDs...)
		return putJSON(b, []byte(seriesUID), v)
	})
}

// ClearDeliveredSOPs removes the checkpoint after a series fully completes,
// freeing the stored SOP UID list.
func (s *Store) ClearDeliveredSOPs(seriesUID string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSeries)
		v, ok := getJSON[SeriesRecord](b, []byte(seriesUID))
		if !ok {
			return ErrNotFound
		}
		v.DeliveredSOPs = nil
		return putJSON(b, []byte(seriesUID), v)
	})
}

// ---------- studies ------------------------------------------------------

// GetStudy fetches by Study UID.
func (s *Store) GetStudy(uid string) (StudyRecord, error) {
	var out StudyRecord
	err := s.db.View(func(tx *bbolt.Tx) error {
		v, ok := getJSON[StudyRecord](tx.Bucket(bucketStudies), []byte(uid))
		if !ok {
			return ErrNotFound
		}
		out = v
		return nil
	})
	return out, err
}

// ListStudies returns all studies, optionally filtered by status. limit=0 means no limit.
func (s *Store) ListStudies(status string, limit int) ([]StudyRecord, error) {
	var out []StudyRecord
	err := s.db.View(func(tx *bbolt.Tx) error {
		studies := tx.Bucket(bucketStudies)
		if status != "" {
			idx := tx.Bucket(bucketIdxStudyByStatus)
			prefix := []byte(status + "/")
			c := idx.Cursor()
			for k, _ := c.Seek(prefix); k != nil && hasPrefix(k, prefix); k, _ = c.Next() {
				if limit > 0 && len(out) >= limit {
					break
				}
				studyUID := string(k[len(prefix):])
				if v, ok := getJSON[StudyRecord](studies, []byte(studyUID)); ok {
					out = append(out, v)
				}
			}
			return nil
		}
		// Full scan, respect limit.
		return studies.ForEach(func(_, v []byte) error {
			if limit > 0 && len(out) >= limit {
				return nil
			}
			var rec StudyRecord
			if err := json.Unmarshal(v, &rec); err == nil {
				out = append(out, rec)
			}
			return nil
		})
	})
	return out, err
}

// SetStudyStatus updates the study's status and maintains the status index.
func (s *Store) SetStudyStatus(uid, status string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		studies := tx.Bucket(bucketStudies)
		idx := tx.Bucket(bucketIdxStudyByStatus)

		v, ok := getJSON[StudyRecord](studies, []byte(uid))
		if !ok {
			return ErrNotFound
		}
		old := v.Status
		v.Status = status
		if status == StatusDelivered && v.DeliveredAt == nil {
			now := time.Now()
			v.DeliveredAt = &now
		}
		if err := putJSON(studies, []byte(uid), v); err != nil {
			return err
		}
		if old != "" && old != status {
			_ = idx.Delete(idxKey(old, uid))
		}
		return idx.Put(idxKey(status, uid), nil)
	})
}

// UpdateStudy overwrites a StudyRecord verbatim and maintains the status
// index against the previous value.
//
// Intended for situations where SetStudyStatus is too coarse (e.g. tests
// that need to backdate DeliveredAt; future migration code). For routine
// status changes, prefer SetStudyStatus.
func (s *Store) UpdateStudy(updated StudyRecord) error {
	if updated.StudyInstanceUID == "" {
		return errors.New("update study: empty UID")
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		studies := tx.Bucket(bucketStudies)
		idx := tx.Bucket(bucketIdxStudyByStatus)

		prev, ok := getJSON[StudyRecord](studies, []byte(updated.StudyInstanceUID))
		if !ok {
			return ErrNotFound
		}
		if err := putJSON(studies, []byte(updated.StudyInstanceUID), updated); err != nil {
			return err
		}
		if prev.Status != updated.Status {
			if prev.Status != "" {
				_ = idx.Delete(idxKey(prev.Status, updated.StudyInstanceUID))
			}
			if updated.Status != "" {
				_ = idx.Put(idxKey(updated.Status, updated.StudyInstanceUID), nil)
			}
		}
		return nil
	})
}

// DeleteStudyCascade removes a study and every series + instance row that
// belongs to it, atomically. Index buckets are updated to match.
//
// On-disk files are NOT touched here — that's the caller's responsibility
// (see internal/retention.DeleteStudy for the full teardown sequence).
//
// Idempotent: deleting a non-existent study returns nil.
func (s *Store) DeleteStudyCascade(studyUID string) error {
	if studyUID == "" {
		return errors.New("delete: empty study UID")
	}

	return s.db.Update(func(tx *bbolt.Tx) error {
		instances := tx.Bucket(bucketInstances)
		series := tx.Bucket(bucketSeries)
		studies := tx.Bucket(bucketStudies)
		idxSerByStudy := tx.Bucket(bucketIdxSeriesByStudy)
		idxInstBySer := tx.Bucket(bucketIdxInstByStudy)
		idxStudyByStatus := tx.Bucket(bucketIdxStudyByStatus)

		study, hadStudy := getJSON[StudyRecord](studies, []byte(studyUID))
		if !hadStudy {
			return nil // idempotent
		}

		// Collect every series UID for this study via the index.
		seriesPrefix := []byte(studyUID + "/")
		var seriesUIDs []string
		c := idxSerByStudy.Cursor()
		for k, _ := c.Seek(seriesPrefix); k != nil && hasPrefix(k, seriesPrefix); k, _ = c.Next() {
			seriesUIDs = append(seriesUIDs, string(k[len(seriesPrefix):]))
		}

		// For each series: delete instances + idx_instances_by_series entries,
		// then delete the series row + idx_series_by_study entry.
		for _, seriesUID := range seriesUIDs {
			instPrefix := []byte(seriesUID + "/")
			ic := idxInstBySer.Cursor()
			var sopUIDs []string
			for k, _ := ic.Seek(instPrefix); k != nil && hasPrefix(k, instPrefix); k, _ = ic.Next() {
				sopUIDs = append(sopUIDs, string(k[len(instPrefix):]))
			}
			for _, sop := range sopUIDs {
				_ = instances.Delete([]byte(sop))
				_ = idxInstBySer.Delete(idxKey(seriesUID, sop))
			}

			_ = series.Delete([]byte(seriesUID))
			_ = idxSerByStudy.Delete(idxKey(studyUID, seriesUID))
		}

		// Delete the study row and its status index entry.
		_ = studies.Delete([]byte(studyUID))
		if study.Status != "" {
			_ = idxStudyByStatus.Delete(idxKey(study.Status, studyUID))
		}
		return nil
	})
}

// ---------- queue --------------------------------------------------------

// Enqueue appends a queue entry. The key is composed of the entry's
// NotBefore timestamp + ID, so a forward iteration produces entries in
// the order they should be processed.
func (s *Store) Enqueue(e QueueEntry) error {
	if e.ID == "" {
		return errors.New("queue entry needs an ID")
	}
	if e.NotBefore.IsZero() {
		e.NotBefore = e.CreatedAt
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket(bucketQueue), queueKey(e.NotBefore, e.ID), e)
	})
}

// PopReadyQueueEntry returns and removes the earliest entry whose NotBefore
// is in the past. Returns ErrNotFound when the queue is empty/nothing ready.
func (s *Store) PopReadyQueueEntry(now time.Time) (QueueEntry, error) {
	var out QueueEntry
	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketQueue)
		c := b.Cursor()
		k, v := c.First()
		if k == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(v, &out); err != nil {
			return err
		}
		if out.NotBefore.After(now) {
			return ErrNotFound // earliest entry is still in the future
		}
		return c.Delete()
	})
	return out, err
}

// QueueDepth returns the number of entries currently queued.
func (s *Store) QueueDepth() (int, error) {
	count := 0
	err := s.db.View(func(tx *bbolt.Tx) error {
		count = tx.Bucket(bucketQueue).Stats().KeyN
		return nil
	})
	return count, err
}

// ---------- transfers ----------------------------------------------------

// PutTransfer creates or updates a transfer record.
func (s *Store) PutTransfer(t TransferRecord) error {
	if t.ID == "" {
		return errors.New("transfer record needs an ID")
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket(bucketTransfers), []byte(t.ID), t)
	})
}

// GetTransfer fetches by transfer ID.
func (s *Store) GetTransfer(id string) (TransferRecord, error) {
	var out TransferRecord
	err := s.db.View(func(tx *bbolt.Tx) error {
		v, ok := getJSON[TransferRecord](tx.Bucket(bucketTransfers), []byte(id))
		if !ok {
			return ErrNotFound
		}
		out = v
		return nil
	})
	return out, err
}

// ---------- helpers ------------------------------------------------------

// SeriesMetaUpdate carries fields scraped from a DICOM dataset that we want
// to back-fill onto the series aggregate when an instance arrives.
type SeriesMetaUpdate struct {
	Modality          string
	SeriesDescription string
	SeriesNumber      string
}

// StudyMetaUpdate carries study-level fields scraped from the dataset.
type StudyMetaUpdate struct {
	PatientID        string
	PatientName      string
	StudyDate        string
	StudyTime        string
	StudyDescription string
	AccessionNumber  string
}

func applySeriesMeta(s *SeriesRecord, m SeriesMetaUpdate) {
	if m.Modality != "" {
		s.Modality = m.Modality
	}
	if m.SeriesDescription != "" {
		s.SeriesDescription = m.SeriesDescription
	}
	if m.SeriesNumber != "" {
		s.SeriesNumber = m.SeriesNumber
	}
}

func applyStudyMeta(s *StudyRecord, m StudyMetaUpdate) {
	if m.PatientID != "" {
		s.PatientID = m.PatientID
	}
	if m.PatientName != "" {
		s.PatientName = m.PatientName
	}
	if m.StudyDate != "" {
		s.StudyDate = m.StudyDate
	}
	if m.StudyTime != "" {
		s.StudyTime = m.StudyTime
	}
	if m.StudyDescription != "" {
		s.StudyDescription = m.StudyDescription
	}
	if m.AccessionNumber != "" {
		s.AccessionNumber = m.AccessionNumber
	}
}

// putJSON marshals v as JSON and stores it under key in b.
func putJSON[T any](b *bbolt.Bucket, key []byte, v T) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return b.Put(key, data)
}

// getJSON reads and decodes a JSON value. Returns the zero value and false
// if the key is absent.
func getJSON[T any](b *bbolt.Bucket, key []byte) (T, bool) {
	var zero T
	v := b.Get(key)
	if v == nil {
		return zero, false
	}
	var out T
	if err := json.Unmarshal(v, &out); err != nil {
		return zero, false
	}
	return out, true
}

// idxKey builds an index bucket key in the form "prefix/secondary".
func idxKey(prefix, secondary string) []byte {
	return []byte(prefix + "/" + secondary)
}

// queueKey produces a sortable key for the queue bucket.
// Format: <RFC3339Nano-padded>/<id>. RFC3339 is lexically sortable when
// padded with leading zeros (which Format("...") does for fixed-width
// fields).
func queueKey(t time.Time, id string) []byte {
	return []byte(t.UTC().Format("2006-01-02T15:04:05.000000000Z") + "/" + id)
}

func hasPrefix(s, prefix []byte) bool {
	return len(s) >= len(prefix) && string(s[:len(prefix)]) == string(prefix)
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.EqualFold(s, needle) {
			return true
		}
	}
	return false
}
