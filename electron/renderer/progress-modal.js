/**
 * Progress Modal Component
 * Displays real-time DICOM transfer progress
 */

class ProgressModal {
    constructor() {
        this.isVisible = false;
        this.currentSeriesUid = null;
        this.pollInterval = null;
        this.init();
    }

    init() {
        document.addEventListener('DOMContentLoaded', () => {
            this.createModalHTML();
            this.attachEventListeners();
        });
    }

    createModalHTML() {
        const modalHTML = `
            <div id="progress-modal" class="modal" style="display: none;">
                <div class="modal-backdrop" id="modal-backdrop"></div>
                <div class="modal-content">
                    <div class="modal-header">
                        <h2>Transfer Progress</h2>
                        <button class="modal-close" id="progress-close">&times;</button>
                    </div>
                    <div class="modal-body">
                        <div class="progress-info">
                            <div class="info-row">
                                <span class="label">Study:</span>
                                <span id="progress-study-uid" class="monospace value" title="">...</span>
                            </div>
                            <div class="info-row">
                                <span class="label">Series:</span>
                                <span id="progress-series-uid" class="monospace value" title="">...</span>
                            </div>
                            <div class="info-row">
                                <span class="label">Status:</span>
                                <span id="progress-status" class="status-badge">Initializing</span>
                            </div>
                        </div>

                        <div class="progress-section">
                            <h3>Overall Progress</h3>
                            <div class="progress-bar-container">
                                <div class="progress-bar" id="progress-bar-overall">
                                    <div class="progress-fill" id="progress-fill-overall"></div>
                                </div>
                                <div class="progress-text">
                                    <span id="progress-percent">0%</span>
                                    <span id="progress-size">0 B / 0 B</span>
                                </div>
                            </div>
                        </div>

                        <div class="progress-section">
                            <h3>Instance Progress</h3>
                            <div class="progress-bar-container">
                                <div class="progress-bar" id="progress-bar-instance">
                                    <div class="progress-fill" id="progress-fill-instance"></div>
                                </div>
                                <div class="progress-text">
                                    <span id="progress-instances">0 / 0</span>
                                </div>
                            </div>
                        </div>

                        <div class="progress-stats">
                            <div class="stat-box">
                                <div class="stat-label">Elapsed</div>
                                <div class="stat-value" id="progress-elapsed">0s</div>
                            </div>
                            <div class="stat-box">
                                <div class="stat-label">ETA</div>
                                <div class="stat-value" id="progress-eta">--</div>
                            </div>
                            <div class="stat-box">
                                <div class="stat-label">Speed</div>
                                <div class="stat-value" id="progress-speed">0 MB/s</div>
                            </div>
                        </div>

                        <div id="progress-message" class="progress-message"></div>
                    </div>
                    <div class="modal-footer">
                        <button id="progress-cancel-btn" class="btn btn-danger">Cancel Transfer</button>
                        <button id="progress-close-btn" class="btn btn-secondary">Close</button>
                    </div>
                </div>
            </div>
        `;

        document.body.insertAdjacentHTML('beforeend', modalHTML);
    }

    attachEventListeners() {
        document.getElementById('progress-close').addEventListener('click', () => this.hide());
        document.getElementById('modal-backdrop').addEventListener('click', () => this.hide());
        document.getElementById('progress-close-btn').addEventListener('click', () => this.hide());
        document.getElementById('progress-cancel-btn').addEventListener('click', () => this.cancelTransfer());
    }

    /**
     * Show modal and start polling progress
     */
    show(seriesUid, studyUid) {
        this.currentSeriesUid = seriesUid;
        this.studyUid = studyUid;
        this.isVisible = true;

        document.getElementById('progress-modal').style.display = 'flex';
        document.getElementById('progress-series-uid').textContent = seriesUid;
        document.getElementById('progress-series-uid').title = seriesUid;
        document.getElementById('progress-study-uid').textContent = studyUid;
        document.getElementById('progress-study-uid').title = studyUid;

        this.startPolling();
    }

    /**
     * Hide modal and stop polling
     */
    hide() {
        this.isVisible = false;
        document.getElementById('progress-modal').style.display = 'none';
        this.stopPolling();
        this.currentSeriesUid = null;
    }

    /**
     * Start polling for progress updates
     */
    startPolling() {
        // Poll every 500ms
        this.pollInterval = setInterval(() => {
            this.updateProgress();
        }, 500);

        // Initial update immediately
        this.updateProgress();
    }

    /**
     * Stop polling
     */
    stopPolling() {
        if (this.pollInterval) {
            clearInterval(this.pollInterval);
            this.pollInterval = null;
        }
    }

    /**
     * Fetch and update progress display
     */
    async updateProgress() {
        if (!this.currentSeriesUid) return;

        try {
            const token = this.getToken();
            const url = `http://localhost:9042/api/transfer/${this.currentSeriesUid}/progress`;
            const response = await fetch(url, {
                headers: { 'Authorization': `Bearer ${token}` }
            });

            if (!response.ok) {
                if (response.status === 404) {
                    this.setMessage('Transfer not found or complete', 'info');
                    this.stopPolling();
                }
                return;
            }

            const progress = await response.json();
            this.displayProgress(progress);

            // Stop polling if complete or failed
            if (progress.status === 'complete' || progress.status === 'failed') {
                this.stopPolling();
                if (progress.status === 'failed') {
                    this.setMessage(`Transfer failed: ${progress.error || 'Unknown error'}`, 'error');
                } else {
                    this.setMessage('Transfer completed successfully', 'success');
                }
            }

        } catch (error) {
            console.error('[PROGRESS ERROR]', error);
        }
    }

    /**
     * Display progress information
     */
    displayProgress(progress) {
        // Overall progress bar
        const percent = progress.percent || 0;
        document.getElementById('progress-fill-overall').style.width = `${percent}%`;
        document.getElementById('progress-percent').textContent = `${percent}%`;
        document.getElementById('progress-size').textContent = 
            `${this.formatBytes(progress.bytes_sent || 0)} / ${this.formatBytes(progress.total_bytes || 0)}`;

        // Instance progress bar
        const instanceCount = progress.total_instances || 1;
        const instanceSent = progress.instances_sent || 0;
        const instancePercent = Math.round((instanceSent / instanceCount) * 100);
        document.getElementById('progress-fill-instance').style.width = `${instancePercent}%`;
        document.getElementById('progress-instances').textContent = 
            `${instanceSent} / ${instanceCount}`;

        // Status badge
        const statusEl = document.getElementById('progress-status');
        statusEl.textContent = (progress.status || 'sending').toUpperCase();
        statusEl.className = `status-badge status-${progress.status || 'sending'}`;

        // Stats
        document.getElementById('progress-elapsed').textContent = 
            this.formatSeconds(progress.elapsed_s || 0);
        document.getElementById('progress-eta').textContent = 
            progress.eta_s ? this.formatSeconds(progress.eta_s) : '--';
        
        const speed = progress.elapsed_s > 0 
            ? (progress.bytes_sent || 0) / (progress.elapsed_s * 1024 * 1024)
            : 0;
        document.getElementById('progress-speed').textContent = 
            `${speed.toFixed(2)} MB/s`;
    }

    /**
     * Request transfer cancellation
     */
    async cancelTransfer() {
        if (!confirm('Cancel this transfer?')) return;

        try {
            const token = this.getToken();
            const response = await fetch(
                `http://localhost:9042/api/transfer/${this.currentSeriesUid}/cancel`,
                {
                    method: 'POST',
                    headers: { 'Authorization': `Bearer ${token}` }
                }
            );

            if (response.ok) {
                this.setMessage('Transfer cancelled', 'info');
                this.stopPolling();
                setTimeout(() => this.hide(), 1500);
            } else {
                this.setMessage('Failed to cancel transfer', 'error');
            }
        } catch (error) {
            this.setMessage(`Error: ${error.message}`, 'error');
        }
    }

    /**
     * Set status message
     */
    setMessage(message, type = 'info') {
        const msgEl = document.getElementById('progress-message');
        msgEl.textContent = message;
        msgEl.className = `progress-message status-${type}`;
    }

    /**
     * Utility: Format bytes to human-readable
     */
    formatBytes(bytes) {
        if (bytes === 0) return '0 B';
        const k = 1024;
        const sizes = ['B', 'KB', 'MB', 'GB'];
        const i = Math.floor(Math.log(bytes) / Math.log(k));
        return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
    }

    /**
     * Utility: Format seconds to human-readable
     */
    formatSeconds(seconds) {
        if (seconds < 60) return `${Math.round(seconds)}s`;
        const mins = Math.floor(seconds / 60);
        const secs = Math.round(seconds % 60);
        return `${mins}m ${secs}s`;
    }

    /**
     * Get authentication token
     */
    getToken() {
        if (window.tarang && window.tarang.token) return window.tarang.token;
        const auth = localStorage.getItem('tarang_auth');
        if (auth) return JSON.parse(auth).token;
        return 'dev-token';
    }
}

// Initialize
const progressModal = new ProgressModal();
