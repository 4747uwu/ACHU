// Command tarang-sender is the native DICOM receive + forward service that
// replaces client-side Orthanc inside the Bharat PACS Electron app.
//
// Lifecycle:
//
//   1. Read config JSON
//   2. Init logging (stdout + dataDir/logs/)
//   3. Open BoltDB store
//   4. Build the transfer client (M4)
//   5. Build the queue worker (M3) backed by the transfer client
//   6. Build the stability watcher (M3) — its OnStable callback enqueues
//   7. Build the SCP handler (M1+M2) — its OnInstanceStored touches the watcher
//   8. Start: SCP listener, queue worker, HTTP API
//   9. Wait for SIGINT/SIGTERM, drain everything in order
//
// On shutdown we drain in REVERSE startup order so each layer can finish
// what's in flight before its dependency closes:
//
//   API stops → SCP stops → watcher stops → queue worker stops → store closes.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/bharatpacs/tarang-sender/internal/api"
	"github.com/bharatpacs/tarang-sender/internal/config"
	"github.com/bharatpacs/tarang-sender/internal/connectivity"
	"github.com/bharatpacs/tarang-sender/internal/errorreport"
	"github.com/bharatpacs/tarang-sender/internal/labstatus"
	"github.com/bharatpacs/tarang-sender/internal/log"
	"github.com/bharatpacs/tarang-sender/internal/notifier"
	"github.com/bharatpacs/tarang-sender/internal/queue"
	"github.com/bharatpacs/tarang-sender/internal/retention"
	"github.com/bharatpacs/tarang-sender/internal/scp"
	"github.com/bharatpacs/tarang-sender/internal/stability"
	"github.com/bharatpacs/tarang-sender/internal/store"
	"github.com/bharatpacs/tarang-sender/internal/transcode"
	"github.com/bharatpacs/tarang-sender/internal/transfer"
)

// version is overridden at build time via -ldflags "-X main.version=..."
var version = "0.1.0-dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "tarang-sender: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "", "path to config JSON file (required)")
		dataDirFlag = flag.String("data-dir", "", "override storage.data_dir from config")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("tarang-sender %s\n", version)
		return nil
	}
	if *configPath == "" {
		flag.Usage()
		return fmt.Errorf("--config is required")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Re-save immediately, which upgrades a plaintext or foreign-format config
	// to ciphertext on first run.
	//
	// Deliberately BEFORE autoTuneConfig and the --data-dir override, so that
	// RAM-derived tuning and a one-off CLI flag are not baked into the file.
	// Logging is deferred because the logger isn't up yet.
	loadedFormat := cfg.LoadedFormat()
	savedFormat, saveErr := cfg.SaveWithFormat(*configPath)

	if *dataDirFlag != "" {
		cfg.Storage.DataDir = *dataDirFlag
	}
	if cfg.Storage.DataDir == "" {
		return fmt.Errorf("storage.data_dir is required (set in config or via --data-dir)")
	}
	if err := os.MkdirAll(cfg.Storage.DataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir %q: %w", cfg.Storage.DataDir, err)
	}

	// Auto-tune transfer settings based on device RAM.
	autoTuneConfig(&cfg)

	if err := log.Init(cfg.Storage.DataDir, cfg.LogLevel); err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	logger := log.L()

	logger.Info("tarang-sender starting",
		"version", version,
		"lab_id", cfg.LabID,
		"org_id", cfg.OrgID,
		"data_dir", cfg.Storage.DataDir,
		"config", *configPath,
		"dicom_port", cfg.DICOM.Port,
		"http_port", cfg.HTTP.Port,
		"peer", cfg.Peer.Name,
		"injection_enabled", cfg.TagInjection.Enabled,
	)

	// Report on the config-at-rest upgrade now that logging exists.
	switch {
	case saveErr != nil:
		logger.Warn("could not re-save config at rest", "err", saveErr)
	case savedFormat != loadedFormat:
		logger.Info("config re-encrypted at rest", "from", loadedFormat, "to", savedFormat)
	default:
		logger.Info("config encrypted at rest", "format", savedFormat)
	}
	if savedFormat == config.FormatPlaintext && runtime.GOOS == "windows" {
		// Both OSCrypt and DPAPI failed. The API token and the receiver
		// password are now readable by anyone who can open the file.
		logger.Warn("config is stored in PLAINTEXT — OS encryption is unavailable and secrets are readable on disk",
			"path", *configPath,
		)
	}

	// ---- Error reporter (ship error logs to backend) --------------------
	// Captures every error-level log record, spools it under
	// <data_dir>/error-reports, and POSTs it to the backend with this
	// device's and lab's identity. Installed as the log sink before any
	// other subsystem starts so early failures are also reported.
	var errReporter *errorreport.Reporter
	if cfg.ErrorReport.Enabled && cfg.ErrorReport.BackendURL != "" {
		errReporter = errorreport.New(
			cfg.ErrorReport.BackendURL,
			cfg.ErrorReport.APIKey,
			version,
			cfg.Storage.DataDir,
			errorreport.CollectDeviceInfo(),
			errorreport.LabInfo{
				LabID:    cfg.LabID,
				OrgID:    cfg.OrgID,
				PeerName: cfg.Peer.Name,
				PeerURL:  cfg.Peer.URL,
			},
		)
		log.SetErrorSink(errReporter.Report)
		logger.Info("error reporter ready", "url", cfg.ErrorReport.BackendURL)
	} else {
		logger.Info("error reporter disabled")
	}

	// ---- Lab activation gate --------------------------------------------
	// Asked before every notification and every upload. An inactive answer
	// holds transmission (notifications park, queue entries re-enqueue) but
	// never refuses a modality or fails a study — see internal/labstatus.
	hostname, _ := os.Hostname()
	labChecker := labstatus.New(
		labStatusURL(cfg),
		cfg.LabStatus.APIKey,
		cfg.LabID,
		cfg.OrgID,
		hostname,
		version,
	)
	labChecker.PollInterval = time.Duration(cfg.LabStatus.PollSeconds) * time.Second
	labChecker.FailOpen = cfg.LabStatus.FailOpen

	// ---- Study notifier (pre-register on backend) -----------------------
	studyNotifier := notifier.New(
		cfg.Notifier.BackendURL,
		cfg.Notifier.APIKey,
		cfg.LabID,
		cfg.OrgID,
	)
	studyNotifier.Gate = func() (bool, string) {
		return labChecker.Allowed(context.Background(), "notify")
	}

	// ---- Persistent store -----------------------------------------------
	dbPath := filepath.Join(cfg.Storage.DataDir, "tarang.db")
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open store %q: %w", dbPath, err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			logger.Warn("store close", "err", err)
		}
	}()
	logger.Info("store opened", "path", dbPath)

	// ---- Transfer client (M4 / M4.5) ------------------------------------
	progressMgr := api.NewProgressManager()
	transferClient := &transfer.Client{
		PeerURL:     cfg.Peer.URL,
		Username:    cfg.Peer.Username,
		Password:    cfg.Peer.Password,
		CACertPath:  cfg.Peer.CACertPath,
		Timeout:      time.Duration(cfg.Transfer.HTTPTimeoutSeconds) * time.Second,
		Concurrency:  cfg.Transfer.ConcurrentWorkers,
		BucketSizeMB: cfg.Transfer.BucketSizeMB,
		Protocol:     transfer.PushProtocol(cfg.Peer.Protocol),
		CheckpointEnabled: func() bool {
			return cfg.Resilience.InstanceCheckpoint
		},
		// When a study finishes uploading, tell the backend — but only after a
		// 15s settle delay, and ALWAYS (no de-dup), even on a re-delivery.
		OnStudyDelivered: func(studyUID string) {
			stu, err := st.GetStudy(studyUID)
			if err != nil {
				logger.Warn("delivered notify: get study failed", "study_uid", studyUID, "err", err)
				return
			}
			payload := notifier.DeliveredPayload{
				StudyInstanceUID: stu.StudyInstanceUID,
				PatientID:        stu.PatientID,
			}
			time.AfterFunc(15*time.Second, func() {
				studyNotifier.NotifyDelivered(payload)
			})
		},
		OnSeriesProgress: func(p transfer.SeriesProgress) {
			if p.Status == "sending" {
				if progressMgr.Get(p.SeriesUID) == nil {
					progressMgr.Start(p.SeriesUID, p.StudyUID, p.TotalBytes, p.TotalInstances)
				}
				progressMgr.Update(p.SeriesUID, p.BytesSent, p.InstancesSent)
				return
			}
			if p.Status == "complete" {
				progressMgr.Complete(p.SeriesUID)
				return
			}
			if p.Status == "failed" {
				progressMgr.Fail(p.SeriesUID, p.Error)
			}
		},
	}

	// ---- Compression: resolve gdcmconv and build the mode settings ------
	gdcmPath := transcode.ResolveGdcmconv(cfg.Compression.GdcmconvPath)

	// If gdcmconv cannot actually run — missing, or present but unloadable —
	// every series would be routed into the transcode queue, fail, and
	// eventually be marked Failed. Degrade to "send as-is" instead, and say so
	// loudly once at startup. transcode.Available launches the binary rather
	// than just stat-ing it, so this covers the arch/runtime-DLL cases too.
	transcodeSettings := transcode.Settings{
		LossyEnabled:               cfg.Compression.Enabled,
		LossyDefaultRate:           cfg.Compression.DefaultRate,
		LossyModalityRate:          cfg.Compression.ModalityRate,
		LossySkipAlreadyCompressed: cfg.Compression.SkipAlreadyCompressed,

		LosslessEnabled:               cfg.Compression.Lossless.Enabled,
		LosslessModalityEnabled:       cfg.Compression.Lossless.ModalityEnabled,
		LosslessSkipAlreadyCompressed: cfg.Compression.Lossless.SkipAlreadyCompressed,
	}
	gdcmAvailable := transcode.Available(gdcmPath)
	if !gdcmAvailable && (transcodeSettings.LossyEnabled || transcodeSettings.LosslessEnabled) {
		logger.Warn("gdcmconv is not available — all compression disabled, studies will be sent in their original encoding",
			"gdcmconv", gdcmPath,
		)
		transcodeSettings = transcode.Settings{}
	}

	logger.Info("compression config",
		"lossy_enabled", transcodeSettings.LossyEnabled,
		"lossless_enabled", transcodeSettings.LosslessEnabled,
		"gdcmconv", gdcmPath,
		"gdcmconv_available", gdcmAvailable,
		"default_rate", cfg.Compression.DefaultRate,
	)

	// compressionActive reports whether any mode could apply, which is what
	// decides between the transcode queue and the direct transfer queue.
	compressionActive := func() bool {
		return transcodeSettings.LossyEnabled || transcodeSettings.LosslessEnabled
	}

	// ---- Connectivity watchdog ------------------------------------------
	connWatcher := &connectivity.Watcher{
		PeerURL:  cfg.Peer.URL,
		Username: cfg.Peer.Username,
		Password: cfg.Peer.Password,
	}

	// ---- Queue worker (M3) ----------------------------------------------
	// Cap concurrent transcodings so CPU-bound gdcmconv work doesn't starve
	// network-bound upload workers sharing the same pool.
	transcodeWorkers := runtime.NumCPU() / 2
	if transcodeWorkers < 1 {
		transcodeWorkers = 1
	}
	transcodeSlots := make(chan struct{}, transcodeWorkers)

	// Limit simultaneous series uploads to 2. More than 2 simultaneous
	// uploads splits bandwidth too thin on asymmetric connections (e.g.
	// 10 Mbps upload in India) and causes server-side timeouts (EOF).
	// Two uploads saturate the uplink while still keeping the pipe full.
	seriesUploadSlots := make(chan struct{}, 2)

	queueWorker := &queue.Worker{
		Store: st,
		ConnCheck: func(ctx context.Context) error {
			// The gate goes FIRST: an inactive lab must not upload whether or
			// not the peer happens to be reachable. Returning an error here
			// re-enqueues the entry rather than failing it, so the retry
			// budget is untouched while the account is sorted out.
			if err := labChecker.GateUpload(ctx); err != nil {
				return err
			}
			if !cfg.Resilience.ConnectivityWatchdog {
				return nil
			}
			return connWatcher.WaitUntilOnline(ctx)
		},
		NetworkRetryEnabled: func() bool {
			return cfg.Resilience.NetworkRetry
		},
		Process: func(ctx context.Context, entry store.QueueEntry) error {
			switch entry.ResourceLevel {
			case "series":
				select {
				case seriesUploadSlots <- struct{}{}:
				case <-ctx.Done():
					return ctx.Err()
				}
				defer func() { <-seriesUploadSlots }()
				return transferClient.PushSeries(ctx, st, entry.ResourceUID)

			case "transcode":
				// Acquire a transcode slot so CPU-bound gdcmconv work doesn't starve
				// the network-bound upload workers sharing the same pool.
				select {
				case transcodeSlots <- struct{}{}:
				case <-ctx.Done():
					return ctx.Err()
				}
				defer func() { <-transcodeSlots }()
				// Look up the series to determine its modality.
				ser, err := st.GetSeries(entry.ResourceUID)
				if err != nil {
					return fmt.Errorf("get series for transcode: %w", err)
				}
				// Non-image modalities (SR, PR, KO, …) have no pixel data;
				// gdcmconv cannot compress them, so send them as-is.
				if transcode.NoPixelModality(ser.Modality) {
					logger.Info("skipping transcode for non-image modality",
						"series_uid", entry.ResourceUID,
						"modality", ser.Modality,
					)
					return queue.EnqueueSeries(st, entry.ResourceUID)
				}
				plan := transcode.PlanFor(transcodeSettings, ser.Modality)
				if plan.Mode == transcode.ModeNone {
					logger.Info("no compression applies to this modality — sending as-is",
						"series_uid", entry.ResourceUID,
						"modality", ser.Modality,
					)
					return queue.EnqueueSeries(st, entry.ResourceUID)
				}
				// Collect file paths for all instances.
				instances, err := st.ListInstancesBySeries(entry.ResourceUID)
				if err != nil {
					return fmt.Errorf("list instances for transcode: %w", err)
				}
				paths := make([]string, len(instances))
				for i, inst := range instances {
					paths[i] = inst.FilePath
				}
				logger.Info("transcoding series",
					"series_uid", entry.ResourceUID,
					"modality", ser.Modality,
					"mode", plan.Mode.String(),
					"rate", plan.Rate,
					"instances", len(paths),
				)
				if err := transcode.TranscodeSeries(ctx, gdcmPath, paths, plan); err != nil {
					return fmt.Errorf("transcode series %s: %w", entry.ResourceUID, err)
				}
				// Now enqueue for actual transfer.
				return queue.EnqueueSeries(st, entry.ResourceUID)

			default:
				return fmt.Errorf("unsupported queue level %q", entry.ResourceLevel)
			}
		},
		Workers:    cfg.Transfer.ConcurrentWorkers,
		MaxRetries: cfg.Transfer.MaxHTTPRetries,
	}

	// ---- Stability watcher (M3) -----------------------------------------
	stableAge := time.Duration(cfg.DICOM.StableAgeSeconds) * time.Second
	watcher := stability.New(stableAge, func(seriesUID string) {
		if compressionActive() {
			// Compression is on: transcode first, then transfer.
			if err := queue.EnqueueTranscode(st, seriesUID); err != nil {
				logger.Warn("enqueue transcode failed", "series_uid", seriesUID, "err", err)
				return
			}
			logger.Info("series stable, queued for transcoding", "series_uid", seriesUID)
		} else {
			// Compression is off: transfer directly.
			if err := queue.EnqueueSeries(st, seriesUID); err != nil {
				logger.Warn("enqueue stable series failed", "series_uid", seriesUID, "err", err)
				return
			}
			logger.Info("series stable, queued for transfer", "series_uid", seriesUID)
		}
	})

	// ---- SCP handler (M1+M2) --------------------------------------------
	handler := &scp.IngestHandler{
		DataDir:             cfg.Storage.DataDir,
		Store:               st,
		InjectionEnabled:    cfg.TagInjection.Enabled,
		PrivateCreator:      cfg.TagInjection.PrivateCreator,
		PrivateOrganisation: cfg.TagInjection.PrivateOrganisation,
		OnInstanceStored: func(seriesUID string, iac int) {
			watcher.TouchWithHint(seriesUID, iac)
		},
		OnStudyFirstSeen: func(studyUID string, m scp.StudyFirstSeenMeta) {
			studyNotifier.MaybeNotify(studyUID, notifier.StudyPayload{
				StudyInstanceUID: m.StudyInstanceUID,
				PatientID:        m.PatientID,
				PatientName:      m.PatientName,
				StudyDate:        m.StudyDate,
				StudyTime:        m.StudyTime,
				StudyDescription: m.StudyDescription,
				AccessionNumber:  m.AccessionNumber,
				Modality:         m.Modality,
			})
		},
	}
	// ---- Secure DICOM (same port as plaintext) --------------------------
	// A broken TLS setup must not take the listener down: plaintext modalities
	// are the common case, so we log loudly and carry on serving them.
	dicomTLS, err := scp.BuildServerTLSConfig(scp.TLSOptions{
		Enabled:        cfg.DICOM.TLS.Enabled,
		CertFile:       cfg.DICOM.TLS.CertFile,
		KeyFile:        cfg.DICOM.TLS.KeyFile,
		CacheDir:       cfg.Storage.DataDir,
		AllowLegacyRC4: cfg.DICOM.TLS.AllowLegacyRC4,
	})
	if err != nil {
		logger.Error("dicom tls disabled — continuing with plaintext only", "err", err)
		dicomTLS = nil
	}

	scpServer := scp.New(scp.Config{
		Address:     fmt.Sprintf(":%d", cfg.DICOM.Port),
		AETitle:     cfg.DICOM.AET,
		MaxPDU:      1 * 1024 * 1024,
		IdleTimeout: 60 * time.Second,
		TLSConfig:   dicomTLS,
	}, handler)

	// ---- HTTP API (M5) --------------------------------------------------
	apiServer := &api.Server{
		Addr:      fmt.Sprintf("127.0.0.1:%d", cfg.HTTP.Port),
		APIToken:  cfg.HTTP.APIToken,
		Store:     st,
		DataDir:   cfg.Storage.DataDir,
		Version:   version,
		LabID:     cfg.LabID,
		OrgID:     cfg.OrgID,
		DICOMPort: cfg.DICOM.Port,
		HTTPPort:  cfg.HTTP.Port,
		PeerName:  cfg.Peer.Name,
		PeerURL:   cfg.Peer.URL,
		ConfigPath: *configPath,
		Config:     &cfg,
		ProgressManager: progressMgr,
		LabChecker:      labChecker,
		Notifier:        studyNotifier,
		PeerWatcher:     connWatcher,
	}

	// ---- Retention sweeper (M7) ----------------------------------------
	sweeper := retention.New(
		st,
		cfg.Storage.DataDir,
		time.Duration(cfg.Storage.RetentionHours)*time.Hour,
		time.Hour, // sweep cadence
	)

	// ---- Lifecycle: start everything ------------------------------------
	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the error reporter first so it can ship reports (and drain any
	// left over from a previous run) for the whole lifetime of the process.
	if errReporter != nil {
		errReporter.Start(rootCtx)
	}

	// Start connectivity watchdog before the queue worker so it has
	// an initial online/offline reading before the first job is processed.
	connWatcher.Start(rootCtx)

	// Same reasoning for the activation gate: get a verdict before the first
	// study can be announced or uploaded. Then keep retrying notifications
	// parked while the account was inactive.
	labChecker.Start(rootCtx)
	studyNotifier.StartPendingFlusher(rootCtx, 30*time.Second)

	// Queue worker runs in its own goroutine until rootCtx cancels.
	var workerWG sync.WaitGroup
	workerWG.Add(1)
	go func() {
		defer workerWG.Done()
		queueWorker.Run(rootCtx)
	}()

	if err := scpServer.Start(rootCtx); err != nil {
		return fmt.Errorf("start scp: %w", err)
	}
	if err := apiServer.Start(rootCtx); err != nil {
		_ = stopWithTimeout(scpServer.Stop, 5*time.Second)
		return fmt.Errorf("start api: %w", err)
	}
	sweeper.Start(rootCtx)

	logger.Info("ready",
		"dicom", fmt.Sprintf(":%d", cfg.DICOM.Port),
		"http", fmt.Sprintf("127.0.0.1:%d", cfg.HTTP.Port),
		"aet", cfg.DICOM.AET,
		"stable_age", stableAge,
		"lab_gate", labChecker.Enabled(),
		"dicom_tls", dicomTLS != nil,
		"lossy_compression", transcodeSettings.LossyEnabled,
		"lossless_compression", transcodeSettings.LosslessEnabled,
	)

	// ---- Wait for shutdown signal ---------------------------------------
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	logger.Info("shutdown signal received", "signal", sig.String())

	// ---- Graceful shutdown in reverse startup order ---------------------
	if err := stopWithTimeout(apiServer.Stop, 5*time.Second); err != nil {
		logger.Warn("api stop", "err", err)
	}
	sweeper.Stop()
	if err := stopWithTimeout(scpServer.Stop, 20*time.Second); err != nil {
		logger.Warn("scp stop", "err", err)
	}
	watcher.Stop()
	cancel() // stops queue worker
	workerWG.Wait()

	logger.Info("tarang-sender stopped cleanly")
	return nil
}

// autoTuneConfig adjusts transfer parameters based on available system RAM.
// Only applies when the config has zero/default values, so explicit user
// settings always win.
func autoTuneConfig(cfg *config.Config) {
	ram := totalSystemRAMGB()
	if cfg.Transfer.BucketSizeMB == 0 {
		switch {
		case ram >= 12:
			cfg.Transfer.BucketSizeMB = 32
		case ram >= 6:
			cfg.Transfer.BucketSizeMB = 16
		default:
			cfg.Transfer.BucketSizeMB = 8
		}
	}
	// Cap workers on low-RAM devices so memory pressure stays low.
	if cfg.Transfer.ConcurrentWorkers > 4 && ram < 6 {
		cfg.Transfer.ConcurrentWorkers = 4
	}

	// And cap by core count, which RAM does not imply. An older clinic PC can
	// carry 8GB on two cores, and the default six workers then contend for
	// both the CPU and — worse on a spinning disk — one set of drive heads.
	// Never raises the figure, only lowers it, so a configured value stays
	// authoritative wherever the hardware can honour it.
	cpus := runtime.NumCPU()
	if cpus < 2 {
		cpus = 2
	}
	if cfg.Transfer.ConcurrentWorkers > cpus {
		cfg.Transfer.ConcurrentWorkers = cpus
	}

	log.L().Info("auto-tune", "ram_gb", ram, "cpus", runtime.NumCPU(),
		"bucket_mb", cfg.Transfer.BucketSizeMB,
		"workers", cfg.Transfer.ConcurrentWorkers)
}

// labStatusURL returns the activation-gate endpoint, or "" when the gate is
// switched off. An empty URL is what makes labstatus.Checker inert, so
// disabling the gate costs no HTTP traffic rather than merely ignoring the
// answer.
func labStatusURL(cfg config.Config) string {
	if !cfg.LabStatus.Enabled {
		return ""
	}
	return cfg.LabStatus.BackendURL
}

// stopWithTimeout calls a Stop(ctx) function with a deadline.
func stopWithTimeout(stop func(context.Context) error, d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return stop(ctx)
}
