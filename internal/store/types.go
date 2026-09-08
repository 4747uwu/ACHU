package store

import "time"

// Status values describe where a study/series/instance is in the pipeline.
//
// Lifecycle: Incoming → Received → Stable → Queued → Sending → Delivered.
// On error we land in Failed; manual or scheduled retry returns to Queued.
const (
	StatusIncoming        = "incoming"
	StatusReceived        = "received"
	StatusStable          = "stable"
	StatusPendingTranscode = "transcoding"
	StatusQueued          = "queued"
	StatusSending         = "sending"
	StatusDelivered       = "delivered"
	StatusFailed          = "failed"
)

// InstanceRecord is one DICOM instance on disk.
type InstanceRecord struct {
	SOPInstanceUID    string    `json:"sop_instance_uid"`
	SeriesInstanceUID string    `json:"series_instance_uid"`
	StudyInstanceUID  string    `json:"study_instance_uid"`
	FilePath          string    `json:"file_path"`
	FileSize          int64     `json:"file_size"`
	TransferSyntaxUID string    `json:"transfer_syntax_uid"`
	SOPClassUID       string    `json:"sop_class_uid"`
	ReceivedAt        time.Time `json:"received_at"`
	Modified          bool      `json:"modified"` // true once tag-injection has run
}

// SeriesRecord aggregates instances of one series.
type SeriesRecord struct {
	SeriesInstanceUID string    `json:"series_instance_uid"`
	StudyInstanceUID  string    `json:"study_instance_uid"`
	Modality          string    `json:"modality"`
	SeriesDescription string    `json:"series_description"`
	SeriesNumber      string    `json:"series_number"`
	InstanceCount     int       `json:"instance_count"`
	TotalSize         int64     `json:"total_size"`
	FirstReceivedAt   time.Time `json:"first_received_at"`
	LastInstanceAt    time.Time `json:"last_instance_at"`
	Status            string    `json:"status"`
	// DeliveredSOPs tracks SOP Instance UIDs confirmed delivered to the peer.
	// Populated chunk-by-chunk during upload; used to skip already-sent
	// instances when the transfer is retried after a network failure.
	// Cleared on successful series completion.
	DeliveredSOPs     []string  `json:"delivered_sops,omitempty"`
}

// StudyRecord aggregates series of one study.
type StudyRecord struct {
	StudyInstanceUID  string    `json:"study_instance_uid"`
	PatientID         string    `json:"patient_id"`
	PatientName       string    `json:"patient_name"`
	StudyDate         string    `json:"study_date"` // raw DICOM "YYYYMMDD"
	StudyTime         string    `json:"study_time"`
	StudyDescription  string    `json:"study_description"`
	AccessionNumber   string    `json:"accession_number"`
	Modalities        []string  `json:"modalities"` // distinct list across series
	SeriesCount       int       `json:"series_count"`
	InstanceCount     int       `json:"instance_count"`
	TotalSize         int64     `json:"total_size"`
	FirstReceivedAt   time.Time `json:"first_received_at"`
	LastInstanceAt    time.Time `json:"last_instance_at"`
	Status            string    `json:"status"`
	DeliveredAt       *time.Time `json:"delivered_at,omitempty"`
}

// QueueEntry represents a unit of work waiting to be transferred.
type QueueEntry struct {
	ID             string    `json:"id"`
	ResourceLevel  string    `json:"resource_level"` // "series" or "study"
	ResourceUID    string    `json:"resource_uid"`
	Retries        int       `json:"retries"`         // data/server-error retries (capped by MaxRetries)
	NetworkRetries int       `json:"network_retries"` // network-error retries (uncapped, separate budget)
	LastError      string    `json:"last_error,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	NotBefore      time.Time `json:"not_before"` // for backoff scheduling
}

// TransferRecord tracks an in-flight or completed push transfer.
type TransferRecord struct {
	ID            string     `json:"id"`
	ResourceLevel string     `json:"resource_level"`
	ResourceUID   string     `json:"resource_uid"`
	PeerName      string     `json:"peer_name"`
	Status        string     `json:"status"`
	BytesSent     int64      `json:"bytes_sent"`
	TotalBytes    int64      `json:"total_bytes"`
	StartedAt     time.Time  `json:"started_at"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	Error         string     `json:"error,omitempty"`
}
