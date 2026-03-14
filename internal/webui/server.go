package webui

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/verssache/chatgpt-creator/internal/config"
	"github.com/verssache/chatgpt-creator/internal/register"
)

const maxLogLines = 400

type Server struct {
	cfg  *config.Config
	mu   sync.Mutex
	job  jobState
	tmpl *template.Template
}

type jobState struct {
	Running      bool                   `json:"running"`
	Message      string                 `json:"message"`
	Error        string                 `json:"error"`
	StartedAt    string                 `json:"started_at"`
	FinishedAt   string                 `json:"finished_at"`
	Config       jobConfig              `json:"config"`
	LastSummary  *register.BatchSummary `json:"last_summary,omitempty"`
	LogLines     []string               `json:"log_lines"`
	DownloadURL  string                 `json:"download_url,omitempty"`
	DownloadName string                 `json:"download_name,omitempty"`
	TerminalHint string                 `json:"terminal_hint"`
}

type jobConfig struct {
	Proxy           string `json:"proxy"`
	TotalAccounts   int    `json:"total_accounts"`
	MaxWorkers      int    `json:"max_workers"`
	DefaultPassword string `json:"default_password"`
	DefaultDomain   string `json:"default_domain"`
	OutputFile      string `json:"output_file"`
}

type pageData struct {
	ListenAddr string
	Config     pageConfig
	State      template.JS
}

type pageConfig struct {
	Proxy           string
	OutputFile      string
	DefaultPassword string
	DefaultDomain   string
}

func New(cfg *config.Config) (*Server, error) {
	tmpl, err := template.New("index").Parse(indexHTML)
	if err != nil {
		return nil, err
	}

	return &Server{
		cfg:  cfg,
		tmpl: tmpl,
		job: jobState{
			Message:      "Belum ada proses yang dijalankan.",
			TerminalHint: "Log detail tampil di web dan tetap dicetak ke terminal server.",
		},
	}, nil
}

func (s *Server) Handler(listenAddr string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex(listenAddr))
	mux.HandleFunc("/start", s.handleStart)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/download", s.handleDownload)
	return mux
}

func (s *Server) handleIndex(listenAddr string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		stateBytes, err := json.Marshal(s.snapshot())
		if err != nil {
			http.Error(w, "failed to render page", http.StatusInternalServerError)
			return
		}

		data := pageData{
			ListenAddr: listenAddr,
			Config: pageConfig{
				Proxy:           s.cfg.Proxy,
				OutputFile:      s.cfg.OutputFile,
				DefaultPassword: s.cfg.DefaultPassword,
				DefaultDomain:   s.cfg.DefaultDomain,
			},
			State: template.JS(stateBytes),
		}

		if err := s.tmpl.Execute(w, data); err != nil {
			http.Error(w, "failed to render page", http.StatusInternalServerError)
		}
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.snapshot())
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state := s.snapshot()
	outputFile := strings.TrimSpace(state.Config.OutputFile)
	if outputFile == "" {
		http.Error(w, "no output file available", http.StatusNotFound)
		return
	}

	info, err := os.Stat(outputFile)
	if err != nil || info.IsDir() {
		http.Error(w, "output file not found", http.StatusNotFound)
		return
	}

	filename := filepath.Base(outputFile)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	http.ServeFile(w, r, outputFile)
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}

	req, err := s.parseRequest(r)
	if err != nil {
		s.setError(err.Error())
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	s.mu.Lock()
	if s.job.Running {
		s.job.Error = "Masih ada proses berjalan. Tunggu sampai selesai."
		s.mu.Unlock()
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	s.job = jobState{
		Running:      true,
		Message:      "Proses registrasi sedang berjalan.",
		StartedAt:    time.Now().Format(time.RFC3339),
		Config:       req,
		LogLines:     nil,
		TerminalHint: "Log detail tampil di web dan tetap dicetak ke terminal server.",
	}
	s.appendLogLocked("Job started with output file: " + req.OutputFile)
	s.mu.Unlock()

	go s.runJob(req)

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) parseRequest(r *http.Request) (jobConfig, error) {
	totalAccounts, err := strconv.Atoi(strings.TrimSpace(r.FormValue("total_accounts")))
	if err != nil || totalAccounts <= 0 {
		return jobConfig{}, fmt.Errorf("total akun harus angka lebih dari 0")
	}

	maxWorkersText := strings.TrimSpace(r.FormValue("max_workers"))
	maxWorkers := 3
	if maxWorkersText != "" {
		val, convErr := strconv.Atoi(maxWorkersText)
		if convErr != nil || val <= 0 {
			return jobConfig{}, fmt.Errorf("max worker harus angka lebih dari 0")
		}
		maxWorkers = val
	}

	password := strings.TrimSpace(r.FormValue("default_password"))
	if password != "" && len(password) < 12 {
		return jobConfig{}, fmt.Errorf("password minimal 12 karakter")
	}

	outputFile := strings.TrimSpace(r.FormValue("output_file"))
	if outputFile == "" {
		outputFile = s.cfg.OutputFile
	}

	return jobConfig{
		Proxy:           strings.TrimSpace(r.FormValue("proxy")),
		TotalAccounts:   totalAccounts,
		MaxWorkers:      maxWorkers,
		DefaultPassword: password,
		DefaultDomain:   strings.TrimSpace(r.FormValue("default_domain")),
		OutputFile:      outputFile,
	}, nil
}

func (s *Server) runJob(req jobConfig) {
	summary := register.RunBatchWithLogger(
		req.TotalAccounts,
		req.OutputFile,
		req.MaxWorkers,
		req.Proxy,
		req.DefaultPassword,
		req.DefaultDomain,
		s.logCollector,
	)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.job.Running = false
	s.job.Message = "Proses registrasi selesai."
	s.job.Error = ""
	s.job.FinishedAt = time.Now().Format(time.RFC3339)
	s.job.LastSummary = &summary
	s.appendLogLocked("Job finished.")
}

func (s *Server) logCollector(line string) {
	fmt.Print(line)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendLogLocked(line)
}

func (s *Server) appendLogLocked(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}

	s.job.LogLines = append(s.job.LogLines, line)
	if len(s.job.LogLines) > maxLogLines {
		s.job.LogLines = append([]string(nil), s.job.LogLines[len(s.job.LogLines)-maxLogLines:]...)
	}
}

func (s *Server) setError(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.job.Error = msg
}

func buildDownloadURL(outputFile string, modifiedAt time.Time) string {
	version := modifiedAt.UTC().UnixNano()
	if version <= 0 {
		version = time.Now().UTC().UnixNano()
	}

	return fmt.Sprintf("/download?v=%d", version)
}

func (s *Server) snapshot() jobState {
	s.mu.Lock()
	defer s.mu.Unlock()

	state := s.job
	state.LogLines = append([]string(nil), s.job.LogLines...)

	outputFile := strings.TrimSpace(state.Config.OutputFile)
	if outputFile != "" {
		if info, err := os.Stat(outputFile); err == nil && !info.IsDir() {
			state.DownloadURL = buildDownloadURL(outputFile, info.ModTime())
			state.DownloadName = filepath.Base(outputFile)
		}
	}

	return state
}

const indexHTML = `<!DOCTYPE html>
<html lang="id">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>ChatGPT Creator</title>
  <style>
    :root {
      --panel: rgba(255, 251, 243, 0.92);
      --line: rgba(75, 54, 33, 0.12);
      --text: #2b2118;
      --muted: #6a5b4b;
      --accent: #b55233;
      --accent-dark: #7a351f;
      --ok: #2a7a4d;
      --err: #b42318;
      --shadow: 0 24px 60px rgba(79, 46, 24, 0.14);
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: Georgia, "Times New Roman", serif;
      color: var(--text);
      background:
        radial-gradient(circle at top left, rgba(181, 82, 51, 0.18), transparent 32%),
        radial-gradient(circle at bottom right, rgba(122, 53, 31, 0.12), transparent 28%),
        linear-gradient(135deg, #f4ecdf 0%, #efe2cd 48%, #f9f5ee 100%);
      min-height: 100vh;
    }
    .wrap {
      width: min(1180px, calc(100% - 32px));
      margin: 0 auto;
      padding: 36px 0 48px;
    }
    .hero {
      display: grid;
      grid-template-columns: 1.05fr 0.95fr;
      gap: 24px;
      align-items: start;
    }
    .panel {
      background: var(--panel);
      border: 1px solid var(--line);
      border-radius: 26px;
      box-shadow: var(--shadow);
      backdrop-filter: blur(10px);
    }
    .intro, .form-card, .status-card, .log-card {
      padding: 24px;
    }
    .eyebrow {
      letter-spacing: 0.12em;
      text-transform: uppercase;
      font-size: 12px;
      color: var(--accent-dark);
      margin-bottom: 12px;
    }
    h1 {
      font-size: clamp(2rem, 5vw, 4rem);
      line-height: 0.96;
      margin: 0 0 16px;
      font-weight: 700;
    }
    .intro p {
      margin: 0;
      max-width: 48ch;
      color: var(--muted);
      font-size: 1rem;
      line-height: 1.7;
    }
    .meta {
      display: flex;
      gap: 12px;
      flex-wrap: wrap;
      margin-top: 24px;
    }
    .chip {
      padding: 10px 14px;
      border-radius: 999px;
      border: 1px solid var(--line);
      background: rgba(255, 255, 255, 0.6);
      font-size: 14px;
    }
    h2 {
      margin: 0 0 18px;
      font-size: 1.2rem;
    }
    form {
      display: grid;
      gap: 14px;
    }
    .grid {
      display: grid;
      grid-template-columns: repeat(2, minmax(0, 1fr));
      gap: 14px;
    }
    label {
      display: grid;
      gap: 8px;
      font-size: 14px;
      color: var(--muted);
    }
    input {
      width: 100%;
      border: 1px solid rgba(75, 54, 33, 0.18);
      border-radius: 14px;
      padding: 13px 14px;
      background: rgba(255, 255, 255, 0.85);
      font: inherit;
      color: var(--text);
    }
    input:focus {
      outline: none;
      border-color: rgba(181, 82, 51, 0.7);
      box-shadow: 0 0 0 4px rgba(181, 82, 51, 0.12);
    }
    button, .download-link {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      border: 0;
      border-radius: 999px;
      padding: 14px 18px;
      font: inherit;
      font-weight: 700;
      color: #fff7f0;
      background: linear-gradient(135deg, var(--accent) 0%, var(--accent-dark) 100%);
      cursor: pointer;
      text-decoration: none;
    }
    button:disabled {
      opacity: 0.6;
      cursor: not-allowed;
    }
    .stack {
      display: grid;
      gap: 24px;
      margin-top: 24px;
    }
    .status-head {
      display: flex;
      justify-content: space-between;
      gap: 12px;
      align-items: center;
      margin-bottom: 18px;
    }
    .actions {
      display: flex;
      gap: 12px;
      flex-wrap: wrap;
      align-items: center;
    }
    .badge {
      padding: 8px 12px;
      border-radius: 999px;
      font-size: 13px;
      font-weight: 700;
      background: rgba(122, 53, 31, 0.08);
      color: var(--accent-dark);
    }
    .badge.running {
      background: rgba(181, 82, 51, 0.12);
    }
    .badge.done {
      background: rgba(42, 122, 77, 0.12);
      color: var(--ok);
    }
    .badge.error {
      background: rgba(180, 35, 24, 0.12);
      color: var(--err);
    }
    .summary {
      display: grid;
      grid-template-columns: repeat(4, minmax(0, 1fr));
      gap: 12px;
      margin-top: 18px;
    }
    .stat {
      border: 1px solid var(--line);
      border-radius: 18px;
      padding: 16px;
      background: rgba(255, 255, 255, 0.54);
    }
    .stat small {
      display: block;
      color: var(--muted);
      margin-bottom: 10px;
      font-size: 12px;
      text-transform: uppercase;
      letter-spacing: 0.08em;
    }
    .stat strong {
      font-size: 1.35rem;
    }
    .details {
      margin-top: 16px;
      display: grid;
      gap: 10px;
      color: var(--muted);
      font-size: 14px;
    }
    .error {
      margin-top: 14px;
      padding: 12px 14px;
      border-radius: 14px;
      background: rgba(180, 35, 24, 0.08);
      color: var(--err);
    }
    .download-note {
      color: var(--muted);
      font-size: 14px;
    }
    .log-card pre {
      margin: 0;
      min-height: 260px;
      max-height: 420px;
      overflow: auto;
      padding: 18px;
      border-radius: 18px;
      background: #211910;
      color: #f1e4d4;
      font-family: "Courier New", monospace;
      font-size: 13px;
      line-height: 1.55;
      white-space: pre-wrap;
      word-break: break-word;
    }
    code {
      font-family: "Courier New", monospace;
      background: rgba(255, 255, 255, 0.6);
      padding: 2px 6px;
      border-radius: 6px;
    }
    @media (max-width: 920px) {
      .hero {
        grid-template-columns: 1fr;
      }
      .summary {
        grid-template-columns: repeat(2, minmax(0, 1fr));
      }
    }
    @media (max-width: 640px) {
      .wrap {
        width: min(100% - 20px, 1180px);
        padding-top: 22px;
      }
      .intro, .form-card, .status-card, .log-card {
        padding: 20px;
      }
      .grid, .summary {
        grid-template-columns: 1fr;
      }
    }
  </style>
</head>
<body>
  <div class="wrap">
    <section class="hero">
      <article class="panel intro">
        <div class="eyebrow">Web Control Panel</div>
        <h1>Daftar, pantau log, lalu download hasilnya.</h1>
        <p>Flow registrasi tetap sama, tapi sekarang form, status, log proses, dan export file hasil bisa diakses langsung dari browser.</p>
        <div class="meta">
          <div class="chip">Jalankan: <code>go run cmd/register/main.go</code></div>
          <div class="chip">Akses: <code>http://{{.ListenAddr}}</code></div>
        </div>
      </article>

      <aside class="panel form-card">
        <h2>Form Registrasi</h2>
        <form method="post" action="/start" id="register-form">
          <div class="grid">
            <label>
              Total akun
              <input type="number" min="1" name="total_accounts" value="1" required>
            </label>
            <label>
              Max worker
              <input type="number" min="1" name="max_workers" value="3" required>
            </label>
          </div>

          <label>
            Proxy
            <input type="text" name="proxy" value="{{.Config.Proxy}}" placeholder="http://user:pass@host:port">
          </label>

          <label>
            Output file
            <input type="text" name="output_file" value="{{.Config.OutputFile}}" placeholder="results.txt">
          </label>

          <label>
            Password default
            <input type="text" name="default_password" value="{{.Config.DefaultPassword}}" placeholder="Kosongkan untuk random">
          </label>

          <label>
            Domain default
            <input type="text" name="default_domain" value="{{.Config.DefaultDomain}}" placeholder="Kosongkan untuk generator.email">
          </label>

          <button type="submit" id="submit-btn">Mulai registrasi</button>
        </form>
      </aside>
    </section>

    <div class="stack">
      <section class="panel status-card">
        <div class="status-head">
          <h2>Status Proses</h2>
          <div class="actions">
            <span class="badge" id="status-badge">Idle</span>
            <a class="download-link" id="download-link" href="/download" hidden>Download hasil</a>
          </div>
        </div>

        <div id="message"></div>
        <div class="error" id="error-box" hidden></div>

        <div class="summary">
          <div class="stat">
            <small>Target</small>
            <strong id="target">-</strong>
          </div>
          <div class="stat">
            <small>Success</small>
            <strong id="success">-</strong>
          </div>
          <div class="stat">
            <small>Attempts</small>
            <strong id="attempts">-</strong>
          </div>
          <div class="stat">
            <small>Failures</small>
            <strong id="failures">-</strong>
          </div>
        </div>

        <div class="details">
          <div id="elapsed"></div>
          <div id="started-at"></div>
          <div id="finished-at"></div>
          <div id="config-view"></div>
          <div id="download-note" class="download-note"></div>
          <div id="terminal-hint"></div>
        </div>
      </section>

      <section class="panel log-card">
        <div class="status-head">
          <h2>Live Log</h2>
          <span class="badge" id="log-count">0 baris</span>
        </div>
        <pre id="log-output">Belum ada log.</pre>
      </section>
    </div>
  </div>

  <script>
    const initialState = {{.State}};
    const statusBadge = document.getElementById('status-badge');
    const submitBtn = document.getElementById('submit-btn');
    const message = document.getElementById('message');
    const errorBox = document.getElementById('error-box');
    const target = document.getElementById('target');
    const success = document.getElementById('success');
    const attempts = document.getElementById('attempts');
    const failures = document.getElementById('failures');
    const elapsed = document.getElementById('elapsed');
    const startedAt = document.getElementById('started-at');
    const finishedAt = document.getElementById('finished-at');
    const configView = document.getElementById('config-view');
    const terminalHint = document.getElementById('terminal-hint');
    const logOutput = document.getElementById('log-output');
    const logCount = document.getElementById('log-count');
    const downloadLink = document.getElementById('download-link');
    const downloadNote = document.getElementById('download-note');

    function setText(node, label, value) {
      node.textContent = value ? (label + ': ' + value) : '';
    }

    function render(state) {
      statusBadge.className = 'badge';
      if (state.running) {
        statusBadge.textContent = 'Running';
        statusBadge.classList.add('running');
        submitBtn.disabled = true;
      } else if (state.error) {
        statusBadge.textContent = 'Error';
        statusBadge.classList.add('error');
        submitBtn.disabled = false;
      } else if (state.last_summary) {
        statusBadge.textContent = 'Completed';
        statusBadge.classList.add('done');
        submitBtn.disabled = false;
      } else {
        statusBadge.textContent = 'Idle';
        submitBtn.disabled = false;
      }

      message.textContent = state.message || '';
      if (state.error) {
        errorBox.hidden = false;
        errorBox.textContent = state.error;
      } else {
        errorBox.hidden = true;
        errorBox.textContent = '';
      }

      const summary = state.last_summary || {};
      const cfg = state.config || {};

      target.textContent = summary.target ?? cfg.total_accounts ?? '-';
      success.textContent = summary.success ?? '-';
      attempts.textContent = summary.attempts ?? '-';
      failures.textContent = summary.failures ?? '-';

      setText(elapsed, 'Elapsed', summary.elapsed || '');
      setText(startedAt, 'Started', state.started_at || '');
      setText(finishedAt, 'Finished', state.finished_at || '');

      const parts = [];
      if (cfg.proxy) parts.push('proxy=' + cfg.proxy);
      if (cfg.max_workers) parts.push('workers=' + cfg.max_workers);
      if (cfg.output_file) parts.push('output=' + cfg.output_file);
      if (cfg.default_domain) parts.push('domain=' + cfg.default_domain);
      parts.push(cfg.default_password ? 'password=custom' : 'password=random');
      configView.textContent = parts.length ? ('Config: ' + parts.join(' | ')) : '';

      if (state.download_url) {
        downloadLink.hidden = false;
        downloadLink.href = state.download_url;
        downloadLink.textContent = state.download_name ? ('Download ' + state.download_name) : 'Download hasil';
        downloadNote.textContent = 'File hasil bisa diunduh langsung dari web.';
      } else {
        downloadLink.hidden = true;
        downloadNote.textContent = '';
      }

      terminalHint.textContent = state.terminal_hint || '';

      const logs = state.log_lines || [];
      logCount.textContent = logs.length + ' baris';
      logOutput.textContent = logs.length ? logs.join('\n') : 'Belum ada log.';
      logOutput.scrollTop = logOutput.scrollHeight;
    }

    async function refreshStatus() {
      try {
        const response = await fetch('/status', { cache: 'no-store' });
        if (!response.ok) return;
        const state = await response.json();
        render(state);
      } catch (_) {
      }
    }

    render(initialState);
    setInterval(refreshStatus, 2000);
  </script>
</body>
</html>`
