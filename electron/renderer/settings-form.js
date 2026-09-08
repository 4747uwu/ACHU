/**
 * Settings Form Manager
 * Handles editable configuration UI with validation and save
 */

class SettingsForm {
    constructor() {
        this.formElement = null;
        this.isEditing = false;
        this.currentConfig = null;
        this.init();
    }

    async init() {
        // Create form structure when DOM is ready
        document.addEventListener('DOMContentLoaded', () => {
            this.createFormHTML();
            this.attachEventListeners();
            this.loadSettings();
        });
    }

    createFormHTML() {
        // This form will be injected into the Settings pane
        // Placeholder for now - will be populated by app.js
        const formHTML = `
            <div id="settings-form-container" style="padding: 20px;">
                <div class="form-section">
                    <h3>Lab Identity</h3>
                    <div class="form-group">
                        <label>Lab ID</label>
                        <input type="text" id="lab_id" readonly class="readonly-field">
                        <small>Configured at login, cannot be changed</small>
                    </div>
                    <div class="form-group">
                        <label>Organization ID</label>
                        <input type="text" id="org_id" readonly class="readonly-field">
                    </div>
                </div>

                <div class="form-section">
                    <h3>DICOM Server</h3>
                    <div class="form-group">
                        <label>DICOM Port</label>
                        <input type="number" id="dicom_port" min="1024" max="65535">
                        <small>C-STORE listener port (default: 1007)</small>
                    </div>
                    <div class="form-group">
                        <label>AE Title</label>
                        <input type="text" id="dicom_aet" maxlength="16" placeholder="ACHYU">
                        <small>Application Entity Title (max 16 chars)</small>
                    </div>
                    <div class="form-group">
                        <label>Stable Age (seconds)</label>
                        <input type="number" id="stable_age" min="1" max="300">
                        <small>Wait time before queuing (default: 15s)</small>
                    </div>
                </div>

                <div class="form-section">
                    <h3>HTTP API</h3>
                    <div class="form-group">
                        <label>HTTP Port</label>
                        <input type="number" id="http_port" min="1024" max="65535">
                        <small>REST API listener port (default: 9042)</small>
                    </div>
                    <div class="form-group">
                        <label>API Token</label>
                        <input type="password" id="http_token" placeholder="Click 'Edit' to change">
                        <small>Bearer token for API auth</small>
                    </div>
                </div>

                <div class="form-section">
                    <h3>Storage</h3>
                    <div class="form-group">
                        <label>Data Directory</label>
                        <input type="text" id="storage_dir" readonly>
                        <small>Read-only, configured at setup</small>
                    </div>
                    <div class="form-group">
                        <label>Retention (hours)</label>
                        <input type="number" id="retention_hours" min="1" max="720">
                        <small>Auto-delete delivered studies after (default: 24h)</small>
                    </div>
                    <div class="form-group">
                        <label>Max Disk (GB)</label>
                        <input type="number" id="max_disk_gb" min="1">
                        <small>Stop ingest if storage exceeds this (default: 50GB)</small>
                    </div>
                </div>

                <div class="form-section">
                    <h3>Central PACS Peer</h3>
                    <div class="form-group">
                        <label>Peer Name</label>
                        <input type="text" id="peer_name" placeholder="e.g., Central Orthanc">
                        <small>Friendly name for central repository</small>
                    </div>
                    <div class="form-group">
                        <label>Peer URL</label>
                        <input type="url" id="peer_url" placeholder="http://example.com:8042">
                        <small>STOW-RS endpoint URL</small>
                    </div>
                    <div class="form-group">
                        <label>Username</label>
                        <input type="text" id="peer_username" placeholder="alice">
                    </div>
                    <div class="form-group">
                        <label>Password</label>
                        <input type="password" id="peer_password" placeholder="Click 'Edit' to change">
                    </div>
                    <div class="form-group">
                        <label>Protocol</label>
                        <select id="peer_protocol">
                            <option value="stow-rs">STOW-RS (DICOMweb)</option>
                            <option value="dicom-web">DICOM Web (alt)</option>
                        </select>
                    </div>
                    <div class="form-group">
                        <label>Compression</label>
                        <select id="peer_compression">
                            <option value="none">None</option>
                            <option value="gzip">GZIP</option>
                            <option value="deflate">Deflate</option>
                        </select>
                    </div>
                </div>

                <div class="form-section">
                    <h3>Tag Injection</h3>
                    <div class="form-group">
                        <label>
                            <input type="checkbox" id="injection_enabled">
                            Enable Tag Injection
                        </label>
                        <small>Inject lab/org private tags into DICOM instances</small>
                    </div>
                    <div class="form-group">
                        <label>Private Creator</label>
                        <input type="text" id="injection_creator" maxlength="64">
                        <small>DICOM private creator identifier</small>
                    </div>
                    <div class="form-group">
                        <label>Private Organization</label>
                        <input type="text" id="injection_org" maxlength="64">
                    </div>
                </div>

                <div class="form-section">
                    <h3>Transfer</h3>
                    <div class="form-group">
                        <label>Concurrent Workers</label>
                        <input type="number" id="transfer_workers" min="1" max="32">
                        <small>Parallel study transfer threads</small>
                    </div>
                    <div class="form-group">
                        <label>Max HTTP Retries</label>
                        <input type="number" id="transfer_retries" min="0" max="10">
                        <small>Retry failed transfers (default: 3)</small>
                    </div>
                    <div class="form-group">
                        <label>HTTP Timeout (seconds)</label>
                        <input type="number" id="transfer_timeout" min="10" max="600">
                        <small>Transfer operation timeout (default: 120s)</small>
                    </div>
                </div>

                <div class="form-actions">
                    <button id="settings-edit-btn" class="btn btn-primary">Edit</button>
                    <button id="settings-save-btn" class="btn btn-success" style="display:none;">Save Changes</button>
                    <button id="settings-cancel-btn" class="btn btn-secondary" style="display:none;">Cancel</button>
                    <button id="settings-reset-btn" class="btn btn-warning" style="display:none;">Reset to Defaults</button>
                </div>

                <div id="settings-status" class="status-message" style="margin-top: 20px;"></div>
            </div>
        `;

        // Inject into settings pane (app.js will replace static form with this)
        const settingsPane = document.querySelector('[data-pane="settings"]');
        if (settingsPane) {
            settingsPane.innerHTML = formHTML;
        }
    }

    attachEventListeners() {
        const editBtn = document.getElementById('settings-edit-btn');
        const saveBtn = document.getElementById('settings-save-btn');
        const cancelBtn = document.getElementById('settings-cancel-btn');
        const resetBtn = document.getElementById('settings-reset-btn');

        if (editBtn) {
            editBtn.addEventListener('click', () => this.enableEditing());
        }
        if (saveBtn) {
            saveBtn.addEventListener('click', () => this.saveSettings());
        }
        if (cancelBtn) {
            cancelBtn.addEventListener('click', () => this.cancelEditing());
        }
        if (resetBtn) {
            resetBtn.addEventListener('click', () => this.resetToDefaults());
        }
    }

    async loadSettings() {
        try {
            const headers = { 'Authorization': `Bearer ${this.getToken()}` };
            const response = await fetch('http://localhost:9042/api/config', { headers });

            if (!response.ok) throw new Error('Failed to load config');

            this.currentConfig = await response.json();
            this.populateForm(this.currentConfig);
            this.disableEditing();
        } catch (error) {
            this.showStatus(`Error loading settings: ${error.message}`, 'error');
        }
    }

    populateForm(config) {
        // Map config object to form fields
        document.getElementById('lab_id').value = config.lab_id || '';
        document.getElementById('org_id').value = config.org_id || '';
        document.getElementById('dicom_port').value = config.dicom?.port || 1007;
        document.getElementById('dicom_aet').value = config.dicom?.aet || 'ACHYU';
        document.getElementById('stable_age').value = config.dicom?.stable_age_seconds || 15;
        document.getElementById('http_port').value = config.http?.port || 9042;
        document.getElementById('http_token').value = '••••••••'; // Don't show actual token
        document.getElementById('storage_dir').value = config.storage?.data_dir || '';
        document.getElementById('retention_hours').value = config.storage?.retention_hours || 24;
        document.getElementById('max_disk_gb').value = config.storage?.max_disk_gb || 50;
        document.getElementById('peer_name').value = config.peer?.name || '';
        document.getElementById('peer_url').value = config.peer?.url || '';
        document.getElementById('peer_username').value = config.peer?.username || '';
        document.getElementById('peer_password').value = '••••••••';
        document.getElementById('peer_protocol').value = config.peer?.protocol || 'stow-rs';
        document.getElementById('peer_compression').value = config.peer?.compression || 'gzip';
        document.getElementById('injection_enabled').checked = config.tag_injection?.enabled || false;
        document.getElementById('injection_creator').value = config.tag_injection?.private_creator || '';
        document.getElementById('injection_org').value = config.tag_injection?.private_organisation || '';
        document.getElementById('transfer_workers').value = config.transfer?.concurrent_workers || 6;
        document.getElementById('transfer_retries').value = config.transfer?.max_http_retries || 3;
        document.getElementById('transfer_timeout').value = config.transfer?.http_timeout_seconds || 120;
    }

    enableEditing() {
        this.isEditing = true;
        document.querySelectorAll('#settings-form-container input, #settings-form-container select').forEach(input => {
            if (!input.readOnly) input.disabled = false;
        });

        document.getElementById('settings-edit-btn').style.display = 'none';
        document.getElementById('settings-save-btn').style.display = 'inline-block';
        document.getElementById('settings-cancel-btn').style.display = 'inline-block';
        document.getElementById('settings-reset-btn').style.display = 'inline-block';
    }

    disableEditing() {
        this.isEditing = false;
        document.querySelectorAll('#settings-form-container input, #settings-form-container select').forEach(input => {
            input.disabled = true;
        });

        document.getElementById('settings-edit-btn').style.display = 'inline-block';
        document.getElementById('settings-save-btn').style.display = 'none';
        document.getElementById('settings-cancel-btn').style.display = 'none';
        document.getElementById('settings-reset-btn').style.display = 'none';
    }

    cancelEditing() {
        this.populateForm(this.currentConfig);
        this.disableEditing();
        this.showStatus('Changes cancelled', 'info');
    }

    async saveSettings() {
        // Validate form
        if (!this.validateForm()) return;

        // Build config update object
        const updatedConfig = {
            dicom: {
                port: parseInt(document.getElementById('dicom_port').value),
                aet: document.getElementById('dicom_aet').value,
                stable_age_seconds: parseInt(document.getElementById('stable_age').value)
            },
            http: {
                port: parseInt(document.getElementById('http_port').value)
            },
            storage: {
                retention_hours: parseInt(document.getElementById('retention_hours').value),
                max_disk_gb: parseInt(document.getElementById('max_disk_gb').value)
            },
            peer: {
                name: document.getElementById('peer_name').value,
                url: document.getElementById('peer_url').value,
                username: document.getElementById('peer_username').value,
                protocol: document.getElementById('peer_protocol').value,
                compression: document.getElementById('peer_compression').value
            },
            tag_injection: {
                enabled: document.getElementById('injection_enabled').checked,
                private_creator: document.getElementById('injection_creator').value,
                private_organisation: document.getElementById('injection_org').value
            },
            transfer: {
                concurrent_workers: parseInt(document.getElementById('transfer_workers').value),
                max_http_retries: parseInt(document.getElementById('transfer_retries').value),
                http_timeout_seconds: parseInt(document.getElementById('transfer_timeout').value)
            }
        };

        // Handle password change
        const newPassword = document.getElementById('peer_password').value;
        if (newPassword && newPassword !== '••••••••') {
            updatedConfig.peer.password = newPassword;
        }

        try {
            this.showStatus('Saving settings...', 'info');

            const response = await fetch('http://localhost:9042/api/config', {
                method: 'PATCH',
                headers: {
                    'Content-Type': 'application/json',
                    'Authorization': `Bearer ${this.getToken()}`
                },
                body: JSON.stringify(updatedConfig)
            });

            if (!response.ok) {
                const error = await response.json();
                throw new Error(error.error || 'Failed to save config');
            }

            const result = await response.json();
            this.currentConfig = result;
            this.populateForm(result);
            this.disableEditing();
            this.showStatus('Settings saved successfully. DICOM server restarting...', 'success');

            // Reload after brief delay to allow server restart
            setTimeout(() => window.location.reload(), 2000);

        } catch (error) {
            this.showStatus(`Error: ${error.message}`, 'error');
        }
    }

    async resetToDefaults() {
        if (!confirm('Reset all settings to defaults?')) return;

        try {
            this.showStatus('Resetting to defaults...', 'info');

            const response = await fetch('http://localhost:9042/api/config/reset', {
                method: 'POST',
                headers: {
                    'Authorization': `Bearer ${this.getToken()}`
                }
            });

            if (!response.ok) throw new Error('Failed to reset config');

            const result = await response.json();
            this.currentConfig = result;
            this.populateForm(result);
            this.disableEditing();
            this.showStatus('Settings reset to defaults. Restarting...', 'success');

            setTimeout(() => window.location.reload(), 2000);

        } catch (error) {
            this.showStatus(`Error: ${error.message}`, 'error');
        }
    }

    validateForm() {
        const dicomPort = parseInt(document.getElementById('dicom_port').value);
        const httpPort = parseInt(document.getElementById('http_port').value);
        const peerUrl = document.getElementById('peer_url').value;

        if (dicomPort < 1024 || dicomPort > 65535) {
            this.showStatus('DICOM port must be between 1024 and 65535', 'error');
            return false;
        }

        if (httpPort < 1024 || httpPort > 65535) {
            this.showStatus('HTTP port must be between 1024 and 65535', 'error');
            return false;
        }

        if (dicomPort === httpPort) {
            this.showStatus('DICOM and HTTP ports must be different', 'error');
            return false;
        }

        if (peerUrl && !peerUrl.match(/^https?:\/\/.+/)) {
            this.showStatus('Invalid peer URL format', 'error');
            return false;
        }

        return true;
    }

    showStatus(message, type = 'info') {
        const statusEl = document.getElementById('settings-status');
        if (statusEl) {
            statusEl.textContent = message;
            statusEl.className = `status-message status-${type}`;
        }
    }

    getToken() {
        // Get token from localStorage or window.tarang
        if (window.tarang && window.tarang.token) return window.tarang.token;
        const auth = localStorage.getItem('tarang_auth');
        if (auth) return JSON.parse(auth).token;
        return 'dev-token'; // fallback for dev
    }
}

// Initialize on page load
const settingsForm = new SettingsForm();
