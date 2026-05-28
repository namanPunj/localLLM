// ============================================================
//  Assistant Web UI (Stream-Resilient Overhaul)
//  • Background-safe streaming (no abort on session switch)
//  • Real-time localStorage caching per session
//  • Instant restore on reload/return
//  • Latest-first history sorting
//  • Project mode with persistent file associations
// ============================================================
const $ = (id) => document.getElementById(id);
const chatContainer = $('chatContainer');
const userInput     = $('userInput');
const sendBtn       = $('sendBtn');
const sidebar       = $('sidebar');
// Sidebar section containers are accessed via $('workspaceList') and $('chatList')
const fileInput     = $('fileInput');
const fileChipRow   = $('fileChipRow');

let currentSessionID = '';
let attachedFiles     = [];
let ghostMode        = false;
let inFlight         = false;
let modeSwitching    = false; // true while model swap in progress — blocks all input
let pendingModeSwitch = false; // queued toggle while stream is running
let projectMode      = false;
let userScrolledUp   = false; // smart-scroll: true when user scrolled up intentionally
let _chatAutoScrolling = false; // true while autoScroll() is driving the scroll
let currentNumCtx    = 4096; // tracks which context size Ollama is currently loaded with

// ============================================================
//  THEME — light / dark toggle with localStorage persistence
// ============================================================
const THEME_KEY = 'assistant_theme';
function applyTheme(theme) {
  document.body.classList.toggle('light-theme', theme === 'light');
  try { localStorage.setItem(THEME_KEY, theme); } catch {}
  // Update meta theme-color for mobile browsers
  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.content = theme === 'light' ? '#f4f6f9' : '#0b0f14';
}
// Restore on load (before paint via class)
(function() {
  let saved = '';
  try { saved = localStorage.getItem(THEME_KEY) || ''; } catch {}
  if (saved === 'light') document.body.classList.add('light-theme');
})();

$('themeToggleBtn').onclick = () => {
  const isLight = document.body.classList.contains('light-theme');
  applyTheme(isLight ? 'dark' : 'light');
};

// ============================================================
//  SMART SCROLL — intent-based: only user wheel/touch triggers lock
// ============================================================
// Wheel up = user wants to scroll back → take control
chatContainer.addEventListener('wheel', (e) => {
  if (e.deltaY < 0) userScrolledUp = true;
}, { passive: true });

// Touch swipe down (scroll up in content) = user interrupt
let _touchStartY = 0;
chatContainer.addEventListener('touchstart', (e) => {
  _touchStartY = e.touches[0].clientY;
}, { passive: true });
chatContainer.addEventListener('touchend', (e) => {
  if (e.changedTouches[0].clientY - _touchStartY > 30) userScrolledUp = true;
}, { passive: true });

// Reset when user (or code) scrolls back to bottom
chatContainer.addEventListener('scroll', () => {
  if (_chatAutoScrolling) return;
  const atBottom = chatContainer.scrollHeight - chatContainer.scrollTop <= chatContainer.clientHeight + 60;
  if (atBottom) userScrolledUp = false;
}, { passive: true });

function autoScroll() {
  if (userScrolledUp) return;
  _chatAutoScrolling = true;
  chatContainer.scrollTop = chatContainer.scrollHeight;
  requestAnimationFrame(() => { _chatAutoScrolling = false; });
}

// ============================================================
//  CONTEXT USAGE PIE CHART
// ============================================================
const ctxChart = $('ctxChart');
const ctxRing  = $('ctxRing');
const ctxLabel = $('ctxLabel');
const ctxPanel = $('ctxPanel');

const CTX_COLORS = {
  system:   '#00d9ff',  /* cyan  — system prompt */
  history:  '#4dd8a0',  /* teal  — conversation history */
  files:    '#ff7a30',  /* orange — uploaded files */
  rag:      '#fbbf24',  /* amber — search/rag */
  user_turn:'#b794f6',  /* violet — current message */
};

// SVG ring constants — circle r=14 → circumference ≈ 87.96
const RING_R = 14;
const RING_C = 2 * Math.PI * RING_R; // ~87.96

let lastContextUsage = null;

// Show empty ring on load so user always sees the indicator
(function initCtxRing() {
  ctxChart.hidden = false;
  const track = document.createElementNS('http://www.w3.org/2000/svg', 'circle');
  track.setAttribute('class', 'ctx-track');
  track.setAttribute('cx', '18'); track.setAttribute('cy', '18'); track.setAttribute('r', RING_R);
  track.setAttribute('stroke-dasharray', RING_C);
  ctxRing.appendChild(track);
  ctxLabel.textContent = '—';
  ctxLabel.style.color = 'var(--text-muted)';
})();

function updateContextChart(usage) {
  if (!usage || !usage.limit) return;
  ctxChart.hidden = false;
  lastContextUsage = usage;

  const total = usage.num_ctx || usage.limit;
  const segments = [
    { key: 'system',    label: 'System',  val: usage.system    || 0 },
    { key: 'history',   label: 'History', val: usage.history   || 0 },
    { key: 'files',     label: 'Resources', val: usage.files   || 0 },
    { key: 'rag',       label: 'Search',  val: usage.rag       || 0 },
    { key: 'user_turn', label: 'Message', val: usage.user_turn || 0 },
  ];
  const used = segments.reduce((s, x) => s + x.val, 0);
  const avail = Math.max(0, total - used);
  const pct = Math.round((used / total) * 100);

  // Build SVG ring — stacked stroke-dasharray arcs
  ctxRing.innerHTML = '';
  // Track circle (background)
  const track = document.createElementNS('http://www.w3.org/2000/svg', 'circle');
  track.setAttribute('class', 'ctx-track');
  track.setAttribute('cx', '18'); track.setAttribute('cy', '18'); track.setAttribute('r', RING_R);
  track.setAttribute('stroke-dasharray', RING_C);
  ctxRing.appendChild(track);

  let offset = 0;
  for (const seg of segments) {
    if (seg.val <= 0) continue;
    const arcLen = (seg.val / total) * RING_C;
    const c = document.createElementNS('http://www.w3.org/2000/svg', 'circle');
    c.setAttribute('cx', '18'); c.setAttribute('cy', '18'); c.setAttribute('r', RING_R);
    c.setAttribute('stroke', CTX_COLORS[seg.key]);
    // dash: arcLen visible, rest invisible; rotate via dashoffset
    c.setAttribute('stroke-dasharray', `${arcLen} ${RING_C - arcLen}`);
    c.setAttribute('stroke-dashoffset', -offset);
    ctxRing.appendChild(c);
    offset += arcLen;
  }

  // Label (% shown next to ring)
  ctxLabel.textContent = `${pct}%`;
  ctxLabel.style.color = pct > 90 ? 'var(--accent-rose)' : pct > 70 ? 'var(--accent-amber)' : 'var(--text-muted)';

  // Rebuild panel content (if open, update live)
  buildCtxPanel(segments, avail, total, usage);

  // Proactive auto-compact: when context hits 90%, queue compact for after stream ends
  if (pct >= 90 && currentSessionID && !_ctxCompactPending) {
    _ctxCompactPending = true;
    _ctxCompactSessionId = currentSessionID;
  }
}
let _ctxCompactPending = false;
let _ctxCompactSessionId = null;

function buildCtxPanel(segments, avail, total, usage) {
  let html = `<div class="ctx-panel-title">Context Window</div>`;
  for (const seg of segments) {
    if (seg.val <= 0) continue;
    const pct = ((seg.val / total) * 100).toFixed(1);
    html += `<div class="ctx-panel-row">
      <span class="ctx-panel-label">
        <span class="ctx-panel-dot" style="background:${CTX_COLORS[seg.key]}"></span>${seg.label}
      </span>
      <span class="ctx-panel-value">${seg.val} <span style="color:var(--text-muted);font-weight:400">(${pct}%)</span></span>
    </div>`;
  }
  html += `<hr class="ctx-panel-divider">`;
  html += `<div class="ctx-panel-row">
    <span class="ctx-panel-avail">Available</span>
    <span class="ctx-panel-value"><b>${avail}</b> / ${total}</span>
  </div>`;
  if ((usage?.compressed || 0) > 0) {
    html += `<div class="ctx-compressed">⚠ ${usage.compressed} message${usage.compressed === 1 ? '' : 's'} compressed</div>`;
  }
  html += `<div class="ctx-btn-row">
    <button class="ctx-compact-btn" id="ctxCompactBtn">⚡ Compact</button>
    <button class="ctx-forget-btn" id="ctxForgetBtn">🗑 Forget</button>
  </div>`;
  // Debug log section
  html += `<hr class="ctx-panel-divider">`;
  html += `<div class="ctx-panel-title" style="font-size:0.7rem;opacity:0.6;cursor:pointer" id="ctxDebugToggle">Debug Log ▸</div>`;
  html += `<pre id="ctxDebugLog" style="display:none;max-height:120px;overflow-y:auto;font-size:0.65rem;color:var(--text-muted);white-space:pre-wrap;margin:4px 0 0;padding:4px;background:rgba(0,0,0,0.15);border-radius:4px">${_ctxLogEntries.join('\n')}</pre>`;
  ctxPanel.innerHTML = html;

  $('ctxDebugToggle').onclick = (e) => {
    e.stopPropagation();
    const log = document.getElementById('ctxDebugLog');
    const toggle = document.getElementById('ctxDebugToggle');
    if (log.style.display === 'none') {
      log.style.display = 'block';
      toggle.textContent = 'Debug Log ▾';
    } else {
      log.style.display = 'none';
      toggle.textContent = 'Debug Log ▸';
    }
  };

  $('ctxForgetBtn').onclick = async () => {
    if (!currentSessionID) return;
    if (!confirm('Clear context memory? The model will forget this conversation, but messages stay visible in the UI.')) return;
    try {
      await fetch(`/sessions/${currentSessionID}/messages`, { method: 'DELETE' });
      ctxPanel.hidden = true;
      // Add a subtle visual separator — messages stay visible
      const sep = document.createElement('div');
      sep.className = 'ctx-reset-sep';
      sep.textContent = '— context cleared —';
      chatContainer.appendChild(sep);
      chatContainer.scrollTop = chatContainer.scrollHeight;
      // Reset context ring to empty state
      lastContextUsage = null;
      ctxRing.innerHTML = '';
      const _t = document.createElementNS('http://www.w3.org/2000/svg', 'circle');
      _t.setAttribute('class','ctx-track'); _t.setAttribute('cx','18'); _t.setAttribute('cy','18'); _t.setAttribute('r', RING_R); _t.setAttribute('stroke-dasharray', RING_C);
      ctxRing.appendChild(_t);
      ctxLabel.textContent = '—'; ctxLabel.style.color = 'var(--text-muted)';
      delete streamCache[currentSessionID];
      saveCache();
    } catch (e) { console.error('forget context', e); }
  };

  $('ctxCompactBtn').onclick = async () => {
    if (!currentSessionID) return;
    const btn = $('ctxCompactBtn');
    btn.disabled = true;
    btn.textContent = '⏳ Compacting…';
    try {
      const resp = await fetch(`/sessions/${currentSessionID}/compress`, { method: 'POST' });
      if (!resp.ok) throw new Error(await resp.text());
      ctxPanel.hidden = true;
      const sep = document.createElement('div');
      sep.className = 'ctx-reset-sep';
      sep.textContent = '— context compacted —';
      chatContainer.appendChild(sep);
      chatContainer.scrollTop = chatContainer.scrollHeight;
      ctxChart.hidden = true;
      lastContextUsage = null;
      delete streamCache[currentSessionID];
      saveCache();
    } catch (e) {
      console.error('compact context', e);
      btn.disabled = false;
      btn.textContent = '⚡ Compact';
    }
  };
}

// Toggle panel on click
ctxChart.onclick = (e) => {
  e.stopPropagation();
  if (!lastContextUsage) return;
  const hidden = ctxPanel.hidden;
  ctxPanel.hidden = !hidden;
  if (!hidden) return;
  // Position below the ring button
  const rect = ctxChart.getBoundingClientRect();
  ctxPanel.style.top = (rect.bottom + 6) + 'px';
  ctxPanel.style.right = (window.innerWidth - rect.right) + 'px';
  ctxPanel.style.left = 'auto';
  // Rebuild with fresh data
  const total = lastContextUsage.num_ctx || lastContextUsage.limit;
  const segments = [
    { key: 'system',    label: 'System',  val: lastContextUsage.system    || 0 },
    { key: 'history',   label: 'History', val: lastContextUsage.history   || 0 },
    { key: 'files',     label: 'Resources', val: lastContextUsage.files   || 0 },
    { key: 'rag',       label: 'Search',  val: lastContextUsage.rag       || 0 },
    { key: 'user_turn', label: 'Message', val: lastContextUsage.user_turn || 0 },
  ];
  const used = segments.reduce((s, x) => s + x.val, 0);
  const avail = Math.max(0, total - used);
  buildCtxPanel(segments, avail, total, lastContextUsage);
};

// Close panel when clicking outside
document.addEventListener('click', () => { ctxPanel.hidden = true; });

// ============================================================
//  CONTEXT DEBUG LOG — shows what's happening behind the scenes
// ============================================================
const _ctxLogEntries = [];
const CTX_LOG_MAX = 50;

function ctxLog(msg) {
  const ts = new Date().toLocaleTimeString();
  const entry = `[${ts}] ${msg}`;
  _ctxLogEntries.push(entry);
  if (_ctxLogEntries.length > CTX_LOG_MAX) _ctxLogEntries.shift();
  console.log('%c[ctx]', 'color:#7dd3fc', msg);
  // Update panel if open
  const logEl = document.getElementById('ctxDebugLog');
  if (logEl) logEl.textContent = _ctxLogEntries.join('\n');
}

// ── Auto-title: generate a short name from first query ──
const AUTO_TITLED_KEY = 'assistant_auto_titled';
let autoTitled = {};
try { autoTitled = JSON.parse(localStorage.getItem(AUTO_TITLED_KEY) || '{}'); } catch {}

function saveAutoTitled() {
  try { localStorage.setItem(AUTO_TITLED_KEY, JSON.stringify(autoTitled)); } catch {}
}

function autoTitleFromQuery(q) {
  if (!q) return '';
  const stops = new Set(['a','an','the','is','are','was','were','be','been','can','could','would','should','will','have','has','had','do','does','did','i','me','my','we','our','you','your','how','what','when','where','why','who','help','please','make','write','create','get','give','show','tell','explain','just','some','any','this','that','with','for','of','in','on','at','to','from','by','about','into','over','after','before','or','and','but','if','so','very','really','need','want','let','also','use','using','using','its']);
  const words = q.toLowerCase().replace(/[^\w\s]/g, '').split(/\s+/).filter(w => w.length > 2 && !stops.has(w));
  const title = words.slice(0, 5).join(' ');
  if (!title) return q.slice(0, 50);
  return title.charAt(0).toUpperCase() + title.slice(1);
}

async function tryAutoTitle(sessionId, query) {
  if (!sessionId || ghostMode) return;
  // Only if user hasn't manually renamed this session
  if (LS.getTitles()[sessionId]) return;
  // Only once per session
  if (autoTitled[sessionId]) return;
  autoTitled[sessionId] = true;
  saveAutoTitled();
  const t = autoTitleFromQuery(query);
  if (!t) return;
  try {
    await fetch(`/sessions/${sessionId}/title`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ title: t }),
    });
    loadHistory();
  } catch {}
}

// Persist currentSessionID so reload can find a cached/in-flight response
const CURRENT_SID_KEY = 'assistant_current_sid';
function persistCurrentSessionId() {
  try { localStorage.setItem(CURRENT_SID_KEY, currentSessionID || ''); } catch {}
}

// ── Temp session tracking ──
// Temp sessions are per-tab (sessionStorage) so each tab independently tracks
// its own unsaved session without polluting other tabs' state.
const TEMP_SID_KEY = 'assistant_temp_sid';
let tempSessionId = '';
try { tempSessionId = sessionStorage.getItem(TEMP_SID_KEY) || ''; } catch {}

function setTempSession(sid) {
  tempSessionId = sid;
  try { sessionStorage.setItem(TEMP_SID_KEY, sid || ''); } catch {}
  updateSaveBtn();
}

function clearTempSession() {
  const old = tempSessionId;
  tempSessionId = '';
  try { sessionStorage.setItem(TEMP_SID_KEY, ''); } catch {}
  updateSaveBtn();
  return old;
}

// Delete a temp session from the server (fire-and-forget)
function deleteTempIfExists(sid) {
  if (!sid) return;
  fetch(`/sessions/${sid}`, { method: 'DELETE' }).catch(() => {});
}

// Show/hide the "Save Chat" button
function updateSaveBtn() {
  const btn = $('saveTempBtn');
  if (!btn) return;
  const show = !!(tempSessionId && currentSessionID === tempSessionId);
  btn.hidden = !show;
  btn.style.display = show ? '' : 'none';
  // Reset button text when showing (in case it was "Saved" from a previous save)
  if (show) {
    const actionEl = btn.querySelector('.save-temp-action');
    if (actionEl && actionEl.textContent === '✓ Saved') actionEl.textContent = 'Save';
  }
}

// Convert the current temp session into a permanent one.
// Safe to call during streaming — doesn't interrupt the active response.
function saveCurrentSession() {
  if (!tempSessionId || currentSessionID !== tempSessionId) return;
  clearTempSession();
  // Reload sidebar to show the now-permanent session (non-blocking)
  loadHistory();
  const btn = $('saveTempBtn');
  if (btn) {
    const actionEl = btn.querySelector('.save-temp-action');
    if (actionEl) actionEl.textContent = '✓ Saved';
    setTimeout(() => { btn.hidden = true; btn.style.display = 'none'; }, 1200);
  }
}

// Track abandoned (pre-token) queries per session
const ABANDONED_KEY = 'assistant_abandoned_queries';
let abandonedQueries = {};
try { abandonedQueries = JSON.parse(localStorage.getItem(ABANDONED_KEY) || '{}'); } catch {}
function saveAbandoned() {
  try { localStorage.setItem(ABANDONED_KEY, JSON.stringify(abandonedQueries)); } catch {}
}
function markAbandoned(sessionId, query) {
  if (!sessionId || !query) return;
  if (!abandonedQueries[sessionId]) abandonedQueries[sessionId] = [];
  abandonedQueries[sessionId].push({ query, ts: Date.now() });
  saveAbandoned();
}
function isAbandoned(sessionId, query) {
  const arr = abandonedQueries[sessionId];
  return !!arr && arr.some(e => e.query === query);
}
function consumeAbandoned(sessionId, query) {
  const arr = abandonedQueries[sessionId];
  if (!arr) return;
  const idx = arr.findIndex(e => e.query === query);
  if (idx >= 0) arr.splice(idx, 1);
  if (!arr.length) delete abandonedQueries[sessionId];
  saveAbandoned();
}

// Send button toggle
function setSendBtnMode(mode) {
  if (mode === 'stop') {
    sendBtn.classList.add('stop-mode');
    sendBtn.textContent = '■';
    sendBtn.title = 'Stop generating';
    sendBtn.setAttribute('aria-label', 'Stop generating');
    sendBtn.disabled = false;
  } else {
    sendBtn.classList.remove('stop-mode');
    sendBtn.textContent = '➤';
    sendBtn.title = 'Send message';
    sendBtn.setAttribute('aria-label', 'Send message');
  }
}

function stopCurrentStream() {
  // If a followup is pending (stream already ended, just UI waiting), dismiss it
  const followupRow = $('followupRow');
  if (followupRow && !followupRow.hidden) {
    clearFollowupSelector();
    setSendBtnMode('send');
    inFlight = false;
    userInput.focus();
    return;
  }

  const sid = currentSessionID;
  const state = activeStreams.get(sid);
  if (!state) return;

  if (!state.started) {
    handlePreTokenAbandon(sid);
  } else {
    state.wasAbandoned = true;
    state.isDone = true;
    try { state.abortController?.abort(); } catch {}
    // Tell the backend to cancel so it doesn't save a partial response.
    fetch('/chat/cancel', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ session_id: sid }),
    }).catch(() => {});
    const c = state.botDiv.querySelector('.bot-content');
    if (c) c.innerHTML = renderMarkdown(state.accumulated);
    if (state.blobBar) finalizeBlobBar(state.blobBar);
    appendSources(state.botDiv, state.sources);
    if (streamCache[sid]) {
      streamCache[sid].done = true;
      saveCache();
    }
    activeStreams.delete(sid);
  }

  setSendBtnMode('send');
  inFlight = false;
  userInput.focus();
}

// ============================================================
//  MODEL WAKE (reload Ollama with correct context size)
// ============================================================
const modelWakeOverlay = $('modelWakeOverlay');
const modelWakeSub     = $('modelWakeSub');
async function wakeModel(numCtx, label) {
  if (numCtx === currentNumCtx) return;
  modelWakeSub.textContent = label || `Configuring ${numCtx >= 8192 ? '8k' : '4k'} context…`;
  modelWakeOverlay.hidden = false;
  try {
    const r = await fetch('/model/wake', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ num_ctx: numCtx }),
    });
    if (r.ok) {
      currentNumCtx = numCtx;
      if (lastContextUsage) {
        updateContextChart({ ...lastContextUsage, num_ctx: numCtx });
      }
    }
  } catch (e) {
    console.warn('wakeModel failed', e);
  } finally {
    modelWakeOverlay.hidden = true;
  }
}

async function loadWorkspaceModel() {
  modelWakeSub.textContent = 'Loading code model…';
  modelWakeOverlay.hidden = false;
  try {
    await fetch('/model/swap', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ mode: 'workspace' }),
    });
    currentNumCtx = 8192;
  } catch (e) {
    console.warn('loadWorkspaceModel failed', e);
  } finally {
    modelWakeOverlay.hidden = true;
  }
}

// Called after a response when context is at/near capacity — compacts instead of upgrading.
async function autoCompactContext(sessionId) {
  if (!sessionId) return;
  ctxLog('Context near capacity — auto-compacting…');
  try {
    const res = await fetch(`/sessions/${sessionId}/compress`, { method: 'POST' });
    if (res.ok) {
      ctxLog('Context compacted successfully.');
      // Show subtle inline notice
      const note = document.createElement('div');
      note.className = 'ctx-upgrade-banner';
      note.innerHTML = `<span class="ctx-upgrade-icon">⚡</span>
        <span class="ctx-upgrade-text">Context auto-compacted — older messages summarized to stay within 4k window.</span>`;
      chatContainer.appendChild(note);
      chatContainer.scrollTop = chatContainer.scrollHeight;
      setTimeout(() => note.remove(), 5000);
    } else {
      ctxLog('Auto-compact failed: ' + res.statusText);
    }
  } catch (e) {
    ctxLog('Auto-compact error: ' + e.message);
  }
}

// --- Stream Cache ---
const CACHE_KEY = 'assistant_stream_cache';
let streamCache = {};
try { streamCache = JSON.parse(localStorage.getItem(CACHE_KEY) || '{}'); } catch {}

function saveCache() {
  try { localStorage.setItem(CACHE_KEY, JSON.stringify(streamCache)); } catch {}
}

const activeStreams = new Map();

const LS = {
  getHidden:  () => JSON.parse(localStorage.getItem('hidden_sessions') || '[]'),
  setHidden:  (v) => localStorage.setItem('hidden_sessions', JSON.stringify(v)),
  getTitles:  () => JSON.parse(localStorage.getItem('renamed_titles') || '{}'),
  setTitles:  (v) => localStorage.setItem('renamed_titles', JSON.stringify(v)),
  // Folder collapsed state is UI-only — stays in localStorage
  getFolderCollapsed: () => JSON.parse(localStorage.getItem('folder_collapsed') || '{}'),
  setFolderCollapsed: (v) => localStorage.setItem('folder_collapsed', JSON.stringify(v)),
};

window.addEventListener('storage', (e) => {
  if (e.key === 'hidden_sessions' || e.key === 'renamed_titles') loadHistory();
});

// ============================================================
//  SIDEBAR / SESSIONS
// ============================================================
const sidebarBackdrop = $('sidebarBackdrop');

function toggleSidebar() {
  const open = sidebar.classList.toggle('open');
  sidebarBackdrop.classList.toggle('show', open);
}

function closeSidebar() {
  sidebar.classList.remove('open');
  sidebarBackdrop.classList.remove('show');
}

// ── Folder API helpers ──
async function createFolder(sectionKey, container) {
  // Remove any existing inline input
  container.querySelectorAll('.folder-inline-row').forEach(n => n.remove());

  const row = document.createElement('div');
  row.className = 'folder-inline-row';
  const inp = document.createElement('input');
  inp.className = 'folder-name-inp';
  inp.type = 'text';
  inp.placeholder = 'Folder name…';
  inp.maxLength = 50;
  const ok = document.createElement('button');
  ok.className = 'folder-name-act';
  ok.textContent = '✓';
  ok.title = 'Create';
  const cancel = document.createElement('button');
  cancel.className = 'folder-name-act';
  cancel.textContent = '✕';
  cancel.title = 'Cancel';
  row.appendChild(inp);
  row.appendChild(ok);
  row.appendChild(cancel);
  container.insertBefore(row, container.firstChild);
  inp.focus();

  const commit = async () => {
    const name = inp.value.trim();
    row.remove();
    if (!name) return;
    try {
      await fetch('/folders', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name, section: sectionKey })
      });
      loadHistory();
    } catch (e) { console.error('createFolder', e); }
  };
  inp.addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); commit(); } if (e.key === 'Escape') row.remove(); });
  ok.onclick = commit;
  cancel.onclick = () => row.remove();
}

async function renameFolder(folderId, newName) {
  if (!newName || !newName.trim()) return;
  try {
    await fetch(`/folders/${folderId}`, {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: newName.trim() })
    });
    loadHistory();
  } catch (e) { console.error('renameFolder', e); }
}

// Inline rename: replaces nameEl text with an input, calls onCommit(newName) on Enter
function startInlineRename(nameEl, currentName, onCommit) {
  const inp = document.createElement('input');
  inp.className = 'folder-name-inp';
  inp.type = 'text';
  inp.value = currentName || '';
  inp.maxLength = 60;
  inp.style.cssText = 'font-size:inherit;font-weight:inherit;';
  nameEl.replaceWith(inp);
  inp.select();
  inp.focus();

  let done = false;
  const commit = (save) => {
    if (done) return;
    done = true;
    const val = inp.value.trim();
    // Restore original element
    inp.replaceWith(nameEl);
    if (save && val && val !== currentName) {
      nameEl.textContent = val;
      onCommit(val);
    }
  };
  inp.addEventListener('keydown', e => {
    if (e.key === 'Enter') { e.preventDefault(); commit(true); }
    if (e.key === 'Escape') { commit(false); }
  });
  inp.addEventListener('blur', () => setTimeout(() => commit(true), 120));
}

async function deleteFolder(folderId) {
  try {
    await fetch(`/folders/${folderId}`, { method: 'DELETE' });
    loadHistory();
  } catch (e) { console.error('deleteFolder', e); }
}

async function moveToFolder(sessionId, folderId) {
  try {
    if (folderId) {
      await fetch(`/folders/${folderId}/sessions`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ session_id: sessionId })
      });
    } else {
      // Remove from any folder — server removes by session ID regardless of folder
      await fetch(`/folders/none/sessions/${sessionId}`, { method: 'DELETE' });
    }
    loadHistory();
  } catch (e) { console.error('moveToFolder', e); }
}

function toggleFolderCollapse(folderId) {
  const collapsed = LS.getFolderCollapsed();
  collapsed[folderId] = !collapsed[folderId];
  LS.setFolderCollapsed(collapsed);
  loadHistory();
}

function buildFolderRow(sectionKey, folder, folderSessions, isWorkspace, allFolders) {
  const collapsed = LS.getFolderCollapsed()[folder.id] || false;
  const wrap = document.createElement('div');
  wrap.className = 'folder-item';

  const header = document.createElement('div');
  header.className = 'folder-header' + (collapsed ? ' collapsed' : '');

  const arrow = document.createElement('span');
  arrow.className = 'folder-arrow';
  arrow.textContent = collapsed ? '▶' : '▾';

  const nameEl = document.createElement('span');
  nameEl.className = 'folder-name';
  nameEl.textContent = folder.name;

  const count = document.createElement('span');
  count.className = 'folder-count';
  count.textContent = folderSessions.length;

  const menuWrap = document.createElement('div');
  menuWrap.className = 'session-menu-wrap';
  const dots = document.createElement('button');
  dots.className = 'session-dots folder-dots';
  dots.title = 'Folder options';
  dots.innerHTML = '⋯';
  const dropdown = document.createElement('div');
  dropdown.className = 'session-dropdown';
  dropdown.innerHTML = `
    <button class="session-dropdown-opt" data-action="rename">✎ Rename</button>
    <button class="session-dropdown-opt session-dropdown-danger" data-action="delete">✕ Delete folder</button>
  `;
  dots.onclick = (e) => {
    e.stopPropagation();
    document.querySelectorAll('.session-dropdown.open').forEach(d => d.classList.remove('open'));
    const rect = dots.getBoundingClientRect();
    dropdown.style.top = (rect.bottom + 4) + 'px';
    dropdown.style.left = (rect.right - 130) + 'px';
    dropdown.classList.toggle('open');
  };
  dropdown.onclick = (e) => {
    e.stopPropagation();
    const action = e.target.closest('[data-action]')?.dataset.action;
    if (action === 'rename') {
      dropdown.classList.remove('open');
      startInlineRename(nameEl, folder.name, (newName) => renameFolder(folder.id, newName));
    } else if (action === 'delete') {
      deleteFolder(folder.id);
      dropdown.classList.remove('open');
    }
  };
  menuWrap.appendChild(dots);
  menuWrap.appendChild(dropdown);

  const newSessionBtn = document.createElement('button');
  newSessionBtn.className = 'folder-newsession-btn';
  newSessionBtn.title = isWorkspace ? 'New workspace' : 'New chat';
  newSessionBtn.setAttribute('aria-label', isWorkspace ? 'New workspace in folder' : 'New chat in folder');
  newSessionBtn.innerHTML = `<svg width="11" height="11" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M14 2H2a1 1 0 00-1 1v8a1 1 0 001 1h2.5L6 15l1.5-3H14a1 1 0 001-1V3a1 1 0 00-1-1z"/></svg><span class="newchat-plus">+</span>`;
  newSessionBtn.onclick = async (e) => {
    e.stopPropagation();
    pruneEmptySession(currentSessionID);
    try {
      const r = await fetch('/sessions', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: '{}'
      });
      if (!r.ok) return;
      const data = await r.json();
      const sid = data.session_id || data.id;
      if (isWorkspace) {
        await fetch(`/sessions/${sid}/project`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ is_project: true })
        });
      }
      await fetch(`/folders/${folder.id}/sessions`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ session_id: sid })
      });
      // Expand folder so the new session is visible
      const fc = LS.getFolderCollapsed();
      if (fc[folder.id]) { fc[folder.id] = false; LS.setFolderCollapsed(fc); }
      // Navigate to the new session (permanent — clean up any temp)
      const oldTemp = clearTempSession();
      if (oldTemp && oldTemp !== currentSessionID) deleteTempIfExists(oldTemp);
      handlePreTokenAbandon(currentSessionID);
      currentSessionID = sid;
      persistCurrentSessionId();
      clearAttachedFiles();
      setProjectMode(isWorkspace);
      chatContainer.innerHTML = '';
      showLanding(isWorkspace);
      setSendBtnMode('send');
      inFlight = false;
      if (isWorkspace) { loadWorkspaceModel(); } else { wakeModel(4096, 'Preparing new chat…'); }
      loadHistory();
      if (window.innerWidth <= 768) closeSidebar();
      userInput.focus();
    } catch (err) { console.error('folder new session', err); }
  };

  header.appendChild(arrow);
  header.appendChild(nameEl);
  header.appendChild(newSessionBtn);
  header.appendChild(menuWrap);
  header.appendChild(count);
  header.onclick = (e) => {
    if (!e.target.closest('.session-menu-wrap') && !e.target.closest('.folder-newsession-btn')) toggleFolderCollapse(folder.id);
  };
  wrap.appendChild(header);

  if (!collapsed) {
    const body = document.createElement('div');
    body.className = 'folder-body';
    if (folderSessions.length) {
      folderSessions.forEach(s => body.appendChild(buildSessionRow(s, isWorkspace, sectionKey, folder.id, allFolders)));
    } else {
      const empty = document.createElement('div');
      empty.className = 'folder-empty';
      empty.textContent = 'Empty folder';
      body.appendChild(empty);
    }
    wrap.appendChild(body);
  }
  return wrap;
}

function buildSessionRow(s, isWorkspace, sectionKey = null, currentFolderId = null, allFolders = []) {
  const row = document.createElement('div');
  row.className = 'session-item' + (isWorkspace ? ' workspace-item' : '') + (s.id === currentSessionID ? ' active' : '');
  row.dataset.id = s.id;

  const title = document.createElement('div');
  title.className = 'session-title';
  title.textContent = s.title || '(untitled)';
  row.appendChild(title);

  const menuWrap = document.createElement('div');
  menuWrap.className = 'session-menu-wrap';

  const dots = document.createElement('button');
  dots.className = 'session-dots';
  dots.title = 'Options';
  dots.innerHTML = '⋯';
  dots.onclick = (e) => {
    e.stopPropagation();
    document.querySelectorAll('.session-dropdown.open').forEach(d => d.classList.remove('open'));
    // Position fixed dropdown relative to the dots button
    const rect = dots.getBoundingClientRect();
    dropdown.style.top = (rect.bottom + 4) + 'px';
    dropdown.style.left = (rect.right - 120) + 'px'; // align right edge
    dropdown.classList.toggle('open');
  };

  const dropdown = document.createElement('div');
  dropdown.className = 'session-dropdown';

  function rebuildDropdown() {
    dropdown.innerHTML = '';
    const rename = document.createElement('button');
    rename.className = 'session-dropdown-opt';
    rename.innerHTML = '✎ Rename';
    rename.onclick = (e2) => {
      e2.stopPropagation();
      dropdown.classList.remove('open');
      startInlineRename(title, s.title || '', (newName) => {
        const titles = LS.getTitles();
        titles[s.id] = newName;
        LS.setTitles(titles);
        loadHistory();
      });
    };
    dropdown.appendChild(rename);

    if (sectionKey && allFolders.length) {
      const sep = document.createElement('div');
      sep.className = 'dropdown-sep';
      dropdown.appendChild(sep);
      if (currentFolderId) {
        const remove = document.createElement('button');
        remove.className = 'session-dropdown-opt';
        remove.innerHTML = '↩ Remove from folder';
        remove.onclick = (e2) => { e2.stopPropagation(); moveToFolder(s.id, null); dropdown.classList.remove('open'); };
        dropdown.appendChild(remove);
      }
      for (const f of allFolders) {
        if (f.id === currentFolderId) continue;
        const opt = document.createElement('button');
        opt.className = 'session-dropdown-opt';
        opt.innerHTML = `📂 ${escapeHTML(f.name)}`;
        opt.onclick = (e2) => { e2.stopPropagation(); moveToFolder(s.id, f.id); dropdown.classList.remove('open'); };
        dropdown.appendChild(opt);
      }
    }

    const sep2 = document.createElement('div');
    sep2.className = 'dropdown-sep';
    dropdown.appendChild(sep2);
    const del = document.createElement('button');
    del.className = 'session-dropdown-opt session-dropdown-danger';
    del.innerHTML = '✕ Delete';
    del.onclick = (e2) => { e2.stopPropagation(); deleteSession(s.id); dropdown.classList.remove('open'); };
    dropdown.appendChild(del);
  }

  dots.onclick = (e) => {
    e.stopPropagation();
    document.querySelectorAll('.session-dropdown.open').forEach(d => d.classList.remove('open'));
    rebuildDropdown(); // rebuild each time so folder list is current
    const rect = dots.getBoundingClientRect();
    dropdown.style.top = (rect.bottom + 4) + 'px';
    dropdown.style.left = (rect.right - 140) + 'px';
    dropdown.classList.toggle('open');
  };
  dropdown.onclick = (e) => e.stopPropagation();

  menuWrap.appendChild(dots);
  menuWrap.appendChild(dropdown);
  row.appendChild(menuWrap);
  row.onclick = () => switchSession(s.id);
  return row;
}

function renderSessionGroup(container, sessions, isWorkspace, folders) {
  container.innerHTML = '';
  const sectionKey = isWorkspace ? 'workspace' : 'chat';
  // folders for this section only
  const sectionFolders = (folders || []).filter(f => f.section === sectionKey);

  const inFolder = new Set();
  sectionFolders.forEach(f => (f.session_ids || []).forEach(id => inFolder.add(id)));

  // Render folders
  sectionFolders.forEach(folder => {
    const folderSessions = (folder.session_ids || [])
      .map(id => sessions.find(s => s.id === id))
      .filter(Boolean);
    container.appendChild(buildFolderRow(sectionKey, folder, folderSessions, isWorkspace, sectionFolders));
  });

  const ungrouped = sessions.filter(s => !inFolder.has(s.id));

  if (!ungrouped.length && !sectionFolders.length) {
    const empty = document.createElement('div');
    empty.className = 'time-label';
    empty.style.opacity = '0.4';
    empty.textContent = isWorkspace ? 'No workspaces yet' : 'No chats yet';
    container.appendChild(empty);
    return;
  }

  if (ungrouped.length) {
    const today     = new Date(); today.setHours(0,0,0,0);
    const yesterday = new Date(today); yesterday.setDate(yesterday.getDate() - 1);
    const lastWeek  = new Date(today); lastWeek.setDate(lastWeek.getDate() - 7);
    const groups = { 'Today':[], 'Yesterday':[], 'Previous 7 Days':[], 'Older':[] };
    ungrouped.forEach(s => {
      const d = new Date(s.last_active);
      if (d >= today) groups['Today'].push(s);
      else if (d >= yesterday) groups['Yesterday'].push(s);
      else if (d >= lastWeek) groups['Previous 7 Days'].push(s);
      else groups['Older'].push(s);
    });
    for (const [label, items] of Object.entries(groups)) {
      if (!items.length) continue;
      const labelEl = document.createElement('div');
      labelEl.className = 'time-label';
      labelEl.textContent = label;
      container.appendChild(labelEl);
      items.forEach(s => container.appendChild(buildSessionRow(s, isWorkspace, sectionKey, null, sectionFolders)));
    }
  }
}

async function loadHistory() {
  try {
    const [sessionsRes, foldersRes] = await Promise.all([
      fetch('/sessions'),
      fetch('/folders'),
    ]);
    if (!sessionsRes.ok) return;
    const data = await sessionsRes.json();
    const foldersData = foldersRes.ok ? await foldersRes.json() : { folders: [] };
    const folders = foldersData.folders || [];

    let sessions = data.sessions || [];
    const hidden = LS.getHidden();
    const titles = LS.getTitles();

    sessions = sessions.filter(s => !hidden.includes(s.id) && s.id !== tempSessionId);
    sessions.forEach(s => { if (titles[s.id]) s.title = titles[s.id]; });
    sessions.sort((a, b) => new Date(b.last_active) - new Date(a.last_active));

    const workspaces = sessions.filter(s => s.is_project);
    const chats = sessions.filter(s => !s.is_project);

    renderSessionGroup($('workspaceList'), workspaces, true, folders);
    renderSessionGroup($('chatList'), chats, false, folders);

    $('workspaceCount').textContent = workspaces.length;
    $('chatCount').textContent = chats.length;
  } catch (err) { console.error('loadHistory', err); }
}

// Sidebar section collapse toggles
['workspaceSectionToggle', 'chatSectionToggle'].forEach(id => {
  const btn = $(id);
  btn.onclick = () => {
    const expanded = btn.getAttribute('aria-expanded') === 'true';
    btn.setAttribute('aria-expanded', expanded ? 'false' : 'true');
    const body = btn.closest('.sidebar-section').querySelector('.sidebar-section-body');
    if (body) body.classList.toggle('collapsed', expanded);
  };
});

// ── Section 3-dots dropdown menus ──────────────────────────────────
function initSectionDotsMenu(dotsMenuEl) {
  const btn = dotsMenuEl.querySelector('.section-dots-btn');
  const dropdown = dotsMenuEl.querySelector('.section-dots-dropdown');
  if (!btn || !dropdown) return;
  btn.onclick = (e) => {
    e.stopPropagation();
    const isOpen = !dropdown.hidden;
    // Close all other open dropdowns first
    document.querySelectorAll('.section-dots-dropdown').forEach(d => { d.hidden = true; });
    dropdown.hidden = isOpen;
  };
  // Options close the dropdown after action
  dropdown.querySelectorAll('.section-dots-opt').forEach(opt => {
    opt.addEventListener('click', () => { dropdown.hidden = true; }, true);
  });
}
document.querySelectorAll('.section-dots-menu').forEach(initSectionDotsMenu);
// Close any open dropdown on outside click
document.addEventListener('click', () => {
  document.querySelectorAll('.section-dots-dropdown').forEach(d => { d.hidden = true; });
});

// Wire add-folder buttons (now inside dropdowns — IDs unchanged)
$('addWorkspaceFolderBtn').onclick = (e) => {
  e.stopPropagation();
  createFolder('workspace', $('workspaceList'));
};
$('addChatFolderBtn').onclick = (e) => {
  e.stopPropagation();
  createFolder('chat', $('chatList'));
};

// New session buttons in section headers
$('newChatSectionBtn').onclick = (e) => {
  e.stopPropagation();
  startNewSession();
  if (window.innerWidth <= 768) closeSidebar();
};
$('newWorkspaceBtn').onclick = async (e) => {
  e.stopPropagation();
  await startNewSession(true); // creates permanent session + shows workspace landing
  // Set the workspace flag on the session created by startNewSession
  if (currentSessionID) {
    try {
      await fetch(`/sessions/${currentSessionID}/project`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ is_project: true })
      });
      loadHistory();
    } catch (err) { console.error('newWorkspaceSession', err); }
  }
};

function renameSession(id, oldTitle) {
  // Legacy fallback (normally called via inline rename)
  const t = prompt('Rename chat:', oldTitle || '');
  if (!t || t === oldTitle) return;
  const titles = LS.getTitles();
  titles[id] = t;
  LS.setTitles(titles);
  loadHistory();
}

async function deleteSession(id) {
  if (!confirm('Delete this chat permanently?')) return;
  try { await fetch(`/sessions/${id}`, { method: 'DELETE' }); } catch (e) {}
  if (currentSessionID === id) startNewSession();
  loadHistory();
}

// Silently delete a session if it has no messages (user left without sending anything).
function pruneEmptySession(sessionId) {
  if (!sessionId || chatContainer.children.length > 0) return;
  fetch(`/sessions/${sessionId}`, { method: 'DELETE' }).catch(() => {});
}

async function startNewSession(asWorkspace = false) {
  pruneEmptySession(currentSessionID);
  handlePreTokenAbandon(currentSessionID);
  // Clean up previous temp session — await so it's gone before loadHistory
  const oldTemp = clearTempSession();
  if (oldTemp && oldTemp !== currentSessionID) {
    try { await fetch(`/sessions/${oldTemp}`, { method: 'DELETE' }); } catch {}
  }
  currentSessionID = '';
  persistCurrentSessionId();
  clearAttachedFiles();
  setProjectMode(asWorkspace);
  chatContainer.innerHTML = '';
  showLanding(asWorkspace);
  if (window.innerWidth <= 768) closeSidebar();
  setSendBtnMode('send');
  inFlight = false;
  if (asWorkspace) { loadWorkspaceModel(); } else { wakeModel(4096, 'Preparing new chat…'); }
  userInput.focus();

  // Eagerly create session. For plain chats, mark it as temp (hidden from sidebar).
  // Workspace sessions are always permanent.
  try {
    const r = await fetch('/sessions', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{}'
    });
    if (r.ok) {
      const data = await r.json();
      currentSessionID = data.session_id || data.id;
      persistCurrentSessionId();
      if (!asWorkspace) {
        setTempSession(currentSessionID);
      }
    }
  } catch (e) { console.warn('startNewSession: session creation failed', e); }
  await loadHistory();
  updateSaveBtn();
}

async function switchSession(id) {
  if (id === currentSessionID) return;

  // If a stream is active for the session we're leaving, let it finish
  // in the background — don't abort. The backend completes the response
  // and persists it; next time the user opens this session it loads fully.
  const leavingState = activeStreams.get(currentSessionID);
  if (leavingState && !leavingState.isDone) {
    // Close blob overlays and stop dots when leaving
    if (leavingState.blobBar) {
      finalizeBlobBar(leavingState.blobBar);
    }
    leavingState.thinkCollapsed = true;
  } else {
    handlePreTokenAbandon(currentSessionID);
  }

  pruneEmptySession(currentSessionID);
  currentSessionID = id;
  persistCurrentSessionId();
  updateSaveBtn(); // show/hide save button based on whether this is a temp session
  hideLanding();

  chatContainer.innerHTML = '';

  // Check server queue status BEFORE loading history so we know whether
  // to trim the last assistant message (it would be duplicated by the replay).
  let serverHasStream = false;
  if (id && !activeStreams.has(id)) {
    try {
      const qr = await fetch(`/chat/queue/${id}`);
      if (qr.ok) {
        const qs = await qr.json();
        serverHasStream = !!(qs.active || qs.queued);
      }
    } catch {}
  }

  let backendMsgs = [];
  let sessionData = {};
  try {
    const r = await fetch(`/sessions/${id}`);
    if (r.ok) {
      const data = await r.json();
      backendMsgs = data.messages || [];
      sessionData = data.session || {};
      // Update project mode for this session
      const isWorkspaceSession = sessionData.is_project || false;
      setProjectMode(isWorkspaceSession);
      if (isWorkspaceSession) {
        renderProjectFiles(data.project_files || []);
        loadWorkspaceModel();
      } else {
        // Normal chat sessions always use 4k context — compact instead of growing
        const targetCtx = 4096;
        wakeModel(targetCtx);
      }
      // Sync context bar with restored size
      if (lastContextUsage) updateContextChart({ ...lastContextUsage, num_ctx: targetCtx });
    }
  } catch (err) {
    chatContainer.innerHTML = '<div class="message bot-message error-text">Error loading chat history.</div>';
    return;
  }

  const cached = streamCache[id];
  const hasLiveOrCached = !!(cached || activeStreams.has(id) || serverHasStream);

  let msgs = backendMsgs.slice();
  if (hasLiveOrCached) {
    // Strip any trailing assistant messages — they'll be re-streamed live.
    while (msgs.length && msgs[msgs.length - 1].role === 'assistant') msgs.pop();
    // Strip the pending user message only if it matches the local cache's query
    // (the user sent from this tab). Don't strip when serverHasStream — the user
    // message is already persisted and should remain visible in history.
    if (!serverHasStream && cached?.userQuery && msgs.length &&
        msgs[msgs.length - 1].role === 'user' &&
        msgs[msgs.length - 1].content === cached.userQuery) {
      msgs.pop();
    }
  }

  if (abandonedQueries[id]?.length) {
    const filtered = [];
    for (let i = 0; i < msgs.length; i++) {
      const m = msgs[i];
      if (m.role === 'user' && isAbandoned(id, m.content)) {
        if (msgs[i + 1] && msgs[i + 1].role === 'assistant' &&
            !(msgs[i + 1].content || '').trim()) {
          i++;
        }
        continue;
      }
      filtered.push(m);
    }
    msgs = filtered;
  }

  if (!msgs.length && !hasLiveOrCached) {
    showLanding(sessionData.is_project || false);
  } else {
    hideLanding();
    msgs.forEach(m => appendStoredMessage(m));
  }

  if (hasLiveOrCached) {
    if (serverHasStream && !activeStreams.has(id)) {
      // Server stream is authoritative — skip stale local cache, reconnect only.
      // Clear stale cache so renderCachedResponse isn't called on next switch.
      if (cached) { delete streamCache[id]; saveCache(); }
    } else if (activeStreams.has(id)) {
      renderCachedResponse(cached || {
        accumulated: '', actions: [], sources: null, researchSteps: [],
        metaEvents: [], done: false, userQuery: null
      });
      attachLiveUpdater(id);
    } else if (cached) {
      renderCachedResponse(cached);
    }
  }

  chatContainer.scrollTop = chatContainer.scrollHeight;
  loadHistory();
  if (window.innerWidth <= 768) closeSidebar();
  const isLive = activeStreams.has(id) || serverHasStream;
  setSendBtnMode(isLive ? 'stop' : 'send');
  inFlight = isLive;
  userInput.focus();

  // Reconnect to an in-progress server stream (no local activeStream).
  if (serverHasStream && !activeStreams.has(id)) {
    reconnectToServerStream(id);
  }
}

function formatDuration(secs) {
  const s = parseFloat(secs);
  if (isNaN(s)) return '';
  if (s < 60) return `${s.toFixed(1)}s`;
  const m = Math.floor(s / 60);
  const rem = (s % 60).toFixed(0);
  return rem > 0 ? `${m}m ${rem}s` : `${m}m`;
}

function formatMsgTime(date) {
  if (!date) return '';
  const d = new Date(date);
  if (isNaN(d)) return '';
  const h = d.getHours(), m = d.getMinutes();
  const ampm = h >= 12 ? 'PM' : 'AM';
  return `${((h % 12) || 12)}:${String(m).padStart(2, '0')} ${ampm}`;
}

function createMsgMeta(sender, time) {
  const meta = document.createElement('div');
  meta.className = 'msg-meta';
  if (sender) {
    const s = document.createElement('span');
    s.className = 'msg-sender';
    s.textContent = sender;
    meta.appendChild(s);
  }
  const timeStr = formatMsgTime(time);
  if (timeStr) {
    const dot = document.createElement('span');
    dot.className = 'msg-dot';
    dot.textContent = '·';
    meta.appendChild(dot);
    const t = document.createElement('span');
    t.className = 'msg-time';
    t.textContent = timeStr;
    meta.appendChild(t);
  }
  return meta;
}

function appendStoredMessage(m) {
  if (m.role === 'user') {
    const wrapper = document.createElement('div');
    wrapper.className = 'user-msg-wrapper';
    wrapper.appendChild(createMsgMeta('You', m.created_at));
    const div = document.createElement('div');
    div.className = 'message user-message';
    div.appendChild(document.createTextNode(m.content));
    wrapper.appendChild(div);
    chatContainer.appendChild(wrapper);
    return;
  }
  const div = document.createElement('div');
  if (m.role === 'assistant') {
    div.className = 'message bot-message';
    div.appendChild(createMsgMeta('Ed', m.created_at));
    // Restore blob bar from stored meta_events
    const hasBlobs = m.meta_events && m.meta_events !== '[]' && m.meta_events !== 'null';
    if (hasBlobs) {
      const bb = createBlobBar(div);
      if (m.meta_events) {
        let events;
        try { events = JSON.parse(m.meta_events); } catch { events = null; }
        if (Array.isArray(events) && events.length > 0) {
          bb._restoring = true;
          for (const meta of events) handleBlobStage(bb, meta);
          bb._restoring = false;
        }
      }
      finalizeBlobBar(bb);
    }
    const content = document.createElement('div');
    content.className = 'bot-content';
    content.innerHTML = renderMarkdown(m.content);
    div.appendChild(content);
    addTtsButton(div, m.content);
  }
  chatContainer.appendChild(div);
}

function handlePreTokenAbandon(sessionId) {
  if (!sessionId) return;
  const state = activeStreams.get(sessionId);
  if (!state || state.started || state.isDone) return;

  state.wasAbandoned = true;
  markAbandoned(sessionId, state.userQuery);

  if (!userInput.value.trim() && state.userQuery) {
    userInput.value = state.userQuery;
    userInput.style.height = 'auto';
    userInput.style.height = Math.min(userInput.scrollHeight, 200) + 'px';
  }

  state.userDiv?.remove();
  state.botDiv?.remove();

  try { state.abortController?.abort(); } catch {}
  activeStreams.delete(sessionId);
  delete streamCache[sessionId];
  saveCache();

  inFlight = false;
  setSendBtnMode('send');
}

// ============================================================
//  PROJECT MODE
// ============================================================
const resourcesBar      = $('resourcesBar');
const resourcesToggleBtn= $('resourcesToggleBtn');
const resourcesDrawer   = $('resourcesDrawer');
const resourcesBackdrop = $('resourcesBackdrop');
const resourcesCloseBtn = $('resourcesCloseBtn');
const resourcesAddBtn   = $('resourcesAddBtn');
const projectAddFileBtn = $('projectAddFileBtn');
const projectFileInput  = $('projectFileInput');
const projectFileList   = $('projectFileList');
const projectToggleBtn  = $('projectToggleBtn'); // header button still exists
// Legacy alias
const projectBar = resourcesBar;

function openResourcesDrawer() {
  resourcesDrawer.hidden = false;
  resourcesBackdrop.hidden = false;
  requestAnimationFrame(() => {
    resourcesDrawer.classList.add('open');
    resourcesBackdrop.classList.add('show');
  });
}
function closeResourcesDrawer() {
  resourcesDrawer.classList.remove('open');
  resourcesBackdrop.classList.remove('show');
  resourcesDrawer.addEventListener('transitionend', () => {
    resourcesDrawer.hidden = true;
    resourcesBackdrop.hidden = true;
  }, { once: true });
}
resourcesToggleBtn && (resourcesToggleBtn.onclick = openResourcesDrawer);
resourcesCloseBtn && (resourcesCloseBtn.onclick = closeResourcesDrawer);
resourcesBackdrop && (resourcesBackdrop.onclick = closeResourcesDrawer);
resourcesAddBtn && (resourcesAddBtn.onclick = () => projectFileInput.click());

function setProjectMode(on) {
  projectMode = on;
  resourcesBar.hidden = !on;
  projectToggleBtn && projectToggleBtn.classList.toggle('active', on);
  document.body.classList.toggle('workspace-theme', on);
  // Hide think toggle in workspace mode — coder model is always used there
  const toggle = $('modeToggle');
  if (toggle) toggle.hidden = on;
  if (!on) {
    projectFileList.innerHTML = '';
    closeResourcesDrawer();
  }
}

async function toggleProjectMode() {
  // If no session yet, create one so project mode can be enabled immediately
  if (!currentSessionID) {
    try {
      const r = await fetch('/sessions', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
      if (r.ok) {
        const data = await r.json();
        currentSessionID = data.session_id || data.id;
        persistCurrentSessionId();
        hideLanding();
        loadHistory();
      } else {
        alert('Failed to create session');
        return;
      }
    } catch (err) {
      alert('Failed to create session: ' + err.message);
      return;
    }
  }
  const newState = !projectMode;

  // Warn before disabling — all uploaded project files will be permanently removed.
  if (!newState) {
    const ok = confirm('Disabling workspace mode will permanently remove all files uploaded to this session. Continue?');
    if (!ok) return;
  }

  try {
    // Disabling: delete all stored project files first
    if (!newState) {
      const fr = await fetch(`/sessions/${currentSessionID}/files`);
      if (fr.ok) {
        const data = await fr.json();
        const files = data.files || [];
        await Promise.all(
          files.map(f => fetch(`/sessions/${currentSessionID}/files/${f.file_id}`, { method: 'DELETE' }))
        );
      }
    }

    const r = await fetch(`/sessions/${currentSessionID}/project`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ is_project: newState })
    });
    if (!r.ok) throw new Error(await r.text());
    setProjectMode(newState);
    if (newState) {
      // Preload the coder model in the background while files load
      loadWorkspaceModel();
      const fr = await fetch(`/sessions/${currentSessionID}/files`);
      if (fr.ok) {
        const data = await fr.json();
        renderProjectFiles(data.files || []);
      }
    }
    loadHistory();
  } catch (err) {
    console.error('toggleProjectMode', err);
  }
}

function renderProjectFiles(files) {
  projectFileList.innerHTML = '';
  // Update count badge
  const countEl = $('resourcesCount');
  if (countEl) countEl.textContent = files && files.length ? `${files.length} file${files.length !== 1 ? 's' : ''}` : '0 files';

  if (!files || !files.length) return;

  files.forEach(f => {
    const isLink = isLinkName(f.filename);
    const ext = (f.filename.split('.').pop() || '').toLowerCase();
    const icon = isLink ? '🌐' : (['jpg','jpeg','png','gif','webp'].includes(ext) ? '🖼️' : ['pdf'].includes(ext) ? '📑' : ['js','ts','py','go','rs','c','cpp'].includes(ext) ? '⌨️' : '📄');
    const item = document.createElement('div');
    item.className = 'resource-item';
    const wasEdited = _editedFiles.has(f.filename);
    item.innerHTML = `
      <span class="resource-icon">${icon}</span>
      <div class="resource-info">
        <div class="resource-name">${escapeHTML(f.filename)}${wasEdited ? ' <span class="resource-edited-badge">edited</span>' : ''}</div>
        <div class="resource-meta">${isLink ? 'WEB PAGE' : ext.toUpperCase() || 'FILE'}</div>
      </div>
      <span class="resource-open-hint">OPEN →</span>
      <button class="resource-remove" title="Remove" aria-label="Remove">✕</button>
    `;
    // Open file viewer on click (not on remove button)
    item.addEventListener('click', (e) => {
      if (e.target.closest('.resource-remove')) return;
      openFileViewer(f.file_id, f.filename);
    });
    item.querySelector('.resource-remove').onclick = (e) => {
      e.stopPropagation();
      removeProjectFile(f.file_id);
    };
    projectFileList.appendChild(item);
  });
}

async function refreshWorkspaceFiles() {
  if (!currentSessionID) return;
  try {
    const r = await fetch(`/sessions/${currentSessionID}/files`);
    if (r.ok) {
      const data = await r.json();
      renderProjectFiles(data.files || []);
    }
  } catch {}
}

async function addProjectFiles(e) {
  const files = Array.from(e.target.files);
  if (!files.length || !currentSessionID) return;

  for (const f of files) {
    const fd = new FormData();
    fd.append('file', f);
    try {
      const r = await fetch('/upload', { method: 'POST', body: fd });
      if (!r.ok) throw new Error(await r.text());
      const data = await r.json();
      await fetch(`/sessions/${currentSessionID}/files`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ file_id: data.file_id, filename: data.name })
      });
    } catch (err) {
      alert(`Upload failed for ${f.name}: ` + err.message);
    }
  }
  projectFileInput.value = '';
  refreshWorkspaceFiles();
}

async function removeProjectFile(fileId) {
  if (!currentSessionID) return;
  try {
    await fetch(`/sessions/${currentSessionID}/files/${fileId}`, { method: 'DELETE' });
    refreshWorkspaceFiles();
  } catch (err) {
    console.error('removeProjectFile', err);
  }
}

projectToggleBtn && (projectToggleBtn.onclick = toggleProjectMode);
projectAddFileBtn && (projectAddFileBtn.onclick = () => projectFileInput.click());
projectFileInput && (projectFileInput.onchange = addProjectFiles);

// ============================================================
//  FILE VIEWER
// ============================================================
const fileViewerDim    = $('fileViewerDim');
const fileViewerPanel  = $('fileViewerPanel');
const fileViewerName   = $('fileViewerName');
const fileViewerCode   = $('fileViewerCode');
const fileViewerCopyBtn= $('fileViewerCopyBtn');
const fileViewerCloseBtn=$('fileViewerCloseBtn');

function openFileViewer(fileId, filename) {
  fileViewerName.textContent = filename;
  fileViewerCode.textContent = 'Loading…';
  fileViewerPanel.setAttribute('aria-hidden', 'false');
  fileViewerDim.setAttribute('aria-hidden', 'false');
  fetch('/files/raw/' + encodeURIComponent(fileId))
    .then(r => r.ok ? r.text() : Promise.reject(r.statusText))
    .then(text => {
      // Add line numbers
      const lines = text.split('\n');
      fileViewerCode.innerHTML = lines.map((l, i) =>
        `<span class="ln">${i + 1}</span>${escapeHTML(l)}`
      ).join('\n');
    })
    .catch(err => { fileViewerCode.textContent = 'Error loading file: ' + err; });
}
function closeFileViewer() {
  fileViewerPanel.setAttribute('aria-hidden', 'true');
  fileViewerDim.setAttribute('aria-hidden', 'true');
}
fileViewerCloseBtn && (fileViewerCloseBtn.onclick = closeFileViewer);
fileViewerDim && (fileViewerDim.onclick = closeFileViewer);
fileViewerCopyBtn && (fileViewerCopyBtn.onclick = () => {
  navigator.clipboard.writeText(fileViewerCode.textContent).then(() => {
    fileViewerCopyBtn.textContent = 'Copied!';
    setTimeout(() => { fileViewerCopyBtn.textContent = 'Copy'; }, 1500);
  });
});

// ============================================================
//  ACCEPT / REJECT EDIT
// ============================================================
let _pendingEdit = null; // { file_id, filename, old_content, new_content, session_id }
const _editedFiles = new Set(); // filenames that have accepted edits this session

function showEditProposal(args) {
  _pendingEdit = {
    file_id:     args.file_id,
    filename:    args.filename,
    old_content: args.old_content,
    new_content: args.new_content,
    session_id:  currentSessionID,
  };
  $('editProposalTitle').textContent = args.description || 'Proposed Edit';
  $('editProposalFile').textContent = args.filename || args.file_id;

  // Build diff view
  const diffEl = $('editProposalDiff');
  diffEl.innerHTML = '';

  const oldLines = (args.old_content || '').split('\n');
  const newLines = (args.new_content || '').split('\n');

  const removedBlock = document.createElement('div');
  const removedLabel = document.createElement('div');
  removedLabel.className = 'diff-section-label old';
  removedLabel.textContent = '− Before';
  removedBlock.appendChild(removedLabel);
  const removedLines = document.createElement('div');
  removedLines.className = 'diff-block';
  oldLines.forEach((l, i) => {
    const row = document.createElement('div');
    row.className = 'diff-line removed';
    row.innerHTML = `<span class="diff-line-num">${i+1}</span><span class="diff-line-marker">−</span>${escapeHTML(l)}`;
    removedLines.appendChild(row);
  });
  removedBlock.appendChild(removedLines);
  diffEl.appendChild(removedBlock);

  const addedBlock = document.createElement('div');
  const addedLabel = document.createElement('div');
  addedLabel.className = 'diff-section-label new';
  addedLabel.textContent = '+ After';
  addedBlock.appendChild(addedLabel);
  const addedLines = document.createElement('div');
  addedLines.className = 'diff-block';
  newLines.forEach((l, i) => {
    const row = document.createElement('div');
    row.className = 'diff-line added';
    row.innerHTML = `<span class="diff-line-num">${i+1}</span><span class="diff-line-marker">+</span>${escapeHTML(l)}`;
    addedLines.appendChild(row);
  });
  addedBlock.appendChild(addedLines);
  diffEl.appendChild(addedBlock);

  $('editProposalOverlay').hidden = false;
}

$('editAcceptBtn') && ($('editAcceptBtn').onclick = async () => {
  if (!_pendingEdit) return;
  const p = _pendingEdit;
  $('editAcceptBtn').textContent = 'Applying…';
  try {
    const r = await fetch('/files/edit', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(p),
    });
    if (!r.ok) throw new Error(await r.text());
    await r.json();
    $('editProposalOverlay').hidden = true;
    _editedFiles.add(p.filename);
    _pendingEdit = null;
    // Refresh resource list with new file IDs
    refreshWorkspaceFiles();
    // Show toast
    const toast = document.createElement('div');
    toast.className = 'rtn-toast';
    toast.innerHTML = `<div class="toast-title">✓ Edit applied</div><div class="toast-body">${escapeHTML(p.filename)} updated</div>`;
    document.body.appendChild(toast);
    setTimeout(() => toast.remove(), 3000);
  } catch (err) {
    alert('Apply failed: ' + err.message);
  } finally {
    $('editAcceptBtn').textContent = '✓ Accept';
  }
});

$('editRejectBtn') && ($('editRejectBtn').onclick = () => {
  $('editProposalOverlay').hidden = true;
  _pendingEdit = null;
});

// ============================================================
//  GHOST MODE
// ============================================================
function showGhostCover(isEntering) {
  const cover = $('ghostCover');
  const title = $('ghostCoverTitle');
  const sub   = $('ghostCoverSub');
  const btn   = $('ghostCoverDismiss');

  if (isEntering) {
    title.textContent = 'GHOST PROTOCOL';
    sub.textContent   = 'This session will not be saved. It disappears after inactivity.';
    btn.textContent   = 'Start Chatting';
  } else {
    title.textContent = 'SYSTEM RESTORED';
    sub.textContent   = 'Standard logging resumed. Your history is back.';
    btn.textContent   = 'Continue';
  }

  cover.hidden = false;
  btn.onclick = () => {
    cover.hidden = true;
    if (!isEntering) loadHistory();
  };
}

function toggleGhost() {
  ghostMode = !ghostMode;
  document.querySelectorAll('.ghost-btn').forEach(b => b.classList.toggle('active', ghostMode));
  document.body.classList.toggle('ghost-active', ghostMode);
  currentSessionID = '';
  persistCurrentSessionId();

  chatContainer.innerHTML = '';
  $('ghostExitBtn').hidden = !ghostMode;

  showGhostCover(ghostMode);
  if (window.innerWidth <= 768) closeSidebar();
}

// ============================================================
//  SETTINGS PANEL
// ============================================================
const settingsPanel = $('settingsPanel');
const settingsDim   = $('settingsDim');

const targetSourcesInput = $('s_targetSources');
const targetSourcesVal   = $('targetSourcesVal');

// Tracks the last value loaded from server — used to detect dirty state
let _savedTargetSources = null;

function setSettingsDirty(dirty) {
  const btn = $('settingsSaveBtn');
  if (!btn) return;
  btn.disabled = !dirty;
  btn.textContent = dirty ? 'Save' : 'Saved';
}

function updateTargetSourcesVal() {
  if (targetSourcesInput && targetSourcesVal) {
    targetSourcesVal.textContent = targetSourcesInput.value;
  }
  // Enable save button if value differs from what was last loaded
  if (_savedTargetSources !== null) {
    setSettingsDirty(parseInt(targetSourcesInput.value, 10) !== _savedTargetSources);
  }
}
if (targetSourcesInput) {
  targetSourcesInput.addEventListener('input', updateTargetSourcesVal);
}

function openSettings() {
  settingsPanel.classList.add('open');
  settingsDim.classList.add('show');
  $('settingsBtn').classList.add('active');
  loadSettingsIntoPanel();
  loadMemory();
  _syncAutoSpeakToggle();
}

function closeSettings() {
  settingsPanel.classList.remove('open');
  settingsDim.classList.remove('show');
  $('settingsBtn').classList.remove('active');
}

async function loadSettingsIntoPanel() {
  try {
    const r = await fetch('/settings');
    if (!r.ok) return;
    const s = await r.json();
    if (targetSourcesInput && s.target_sources != null) {
      targetSourcesInput.value = String(s.target_sources);
      _savedTargetSources = s.target_sources;
      updateTargetSourcesVal();
      setSettingsDirty(false);
    }
  } catch {}
}

async function saveSettings() {
  const body = {};
  if (targetSourcesInput) {
    body.target_sources = parseInt(targetSourcesInput.value, 10) || 6;
  }
  const saveBtn = $('settingsSaveBtn');
  saveBtn.disabled = true;
  saveBtn.textContent = 'Saving…';
  try {
    const r = await fetch('/settings', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body)
    });
    if (!r.ok) throw new Error(await r.text());
    _savedTargetSources = body.target_sources;
    setSettingsDirty(false);
  } catch (err) {
    alert('Failed to save: ' + err.message);
    setSettingsDirty(true); // re-enable on failure
  }
}

async function resetSettings() {
  if (!confirm('Reset all settings to server defaults?')) return;
  try {
    const r = await fetch('/settings', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ target_sources: 6 })
    });
    if (!r.ok) throw new Error(await r.text());
    await loadSettingsIntoPanel();
  } catch (err) {
    alert('Reset failed: ' + err.message);
  }
}

$('settingsBtn').onclick      = openSettings;
$('settingsCloseBtn').onclick = closeSettings;
settingsDim.onclick           = closeSettings;
$('settingsSaveBtn').onclick  = saveSettings;
$('settingsResetBtn').onclick = resetSettings;

// ── Auto-speak toggle (localStorage, client-side only) ────────────────────
let autoSpeakEnabled = false;
try { autoSpeakEnabled = localStorage.getItem('assistant_auto_speak') === '1'; } catch {}

function _syncAutoSpeakToggle() {
  const btn = $('s_autoSpeak');
  if (!btn) return;
  btn.setAttribute('aria-checked', autoSpeakEnabled ? 'true' : 'false');
}

function _tryAutoSpeak(botDiv, rawText) {
  if (!autoSpeakEnabled) return;
  if (!rawText || rawText.length > 300) return;
  const meta = botDiv.querySelector('.msg-meta');
  const btn = meta && meta.querySelector('.tts-btn');
  if (btn) _speakText(rawText, btn);
}

const _autoSpeakBtn = $('s_autoSpeak');
if (_autoSpeakBtn) {
  _syncAutoSpeakToggle();
  _autoSpeakBtn.addEventListener('click', () => {
    autoSpeakEnabled = !autoSpeakEnabled;
    try { localStorage.setItem('assistant_auto_speak', autoSpeakEnabled ? '1' : '0'); } catch {}
    _syncAutoSpeakToggle();
  });
}

// ============================================================
//  PERSONAL MEMORY
// ============================================================
let _memoryItems = [];
let _savedMemoryItems = [];

function renderMemoryItems() {
  const container = $('memoryItemsContainer');
  container.innerHTML = '';
  _memoryItems.forEach((item, i) => {
    const row = document.createElement('div');
    row.className = 'memory-item-row';
    const inp = document.createElement('input');
    inp.type = 'text';
    inp.className = 'memory-item-input';
    inp.value = item;
    inp.maxLength = 200;
    inp.addEventListener('input', () => {
      _memoryItems[i] = inp.value;
      checkMemoryDirty();
    });
    const del = document.createElement('button');
    del.type = 'button';
    del.className = 'memory-del-btn';
    del.textContent = '✕';
    del.title = 'Remove';
    del.onclick = () => {
      _memoryItems.splice(i, 1);
      renderMemoryItems();
      checkMemoryDirty();
    };
    row.appendChild(inp);
    row.appendChild(del);
    container.appendChild(row);
  });
}

function checkMemoryDirty() {
  const btn = $('memorySaveBtn');
  const dirty = JSON.stringify(_memoryItems) !== JSON.stringify(_savedMemoryItems);
  btn.disabled = !dirty;
  btn.textContent = dirty ? 'Save Memory' : 'Saved';
}

async function loadMemory() {
  try {
    const r = await fetch('/memory');
    if (!r.ok) return;
    const data = await r.json();
    _memoryItems = data.items || [];
    _savedMemoryItems = [..._memoryItems];
    renderMemoryItems();
    checkMemoryDirty();
  } catch {}
}

async function saveMemory() {
  // Filter out empty items
  _memoryItems = _memoryItems.filter(m => m.trim() !== '');
  const btn = $('memorySaveBtn');
  btn.disabled = true;
  btn.textContent = 'Saving…';
  try {
    const r = await fetch('/memory', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ items: _memoryItems })
    });
    if (!r.ok) throw new Error(await r.text());
    _savedMemoryItems = [..._memoryItems];
    renderMemoryItems();
    checkMemoryDirty();
  } catch (err) {
    alert('Failed to save memory: ' + err.message);
    btn.disabled = false;
    btn.textContent = 'Save Memory';
  }
}

$('memoryAddBtn').onclick = () => {
  const inp = $('memoryNewInput');
  const val = inp.value.trim();
  if (!val) return;
  _memoryItems.push(val);
  inp.value = '';
  renderMemoryItems();
  checkMemoryDirty();
};
$('memoryNewInput').addEventListener('keydown', (e) => {
  if (e.key === 'Enter') { e.preventDefault(); $('memoryAddBtn').click(); }
});
$('memorySaveBtn').onclick = saveMemory;

$('deleteAllDataBtn').onclick = async () => {
  if (!confirm('Delete ALL sessions, chat history, and uploaded files?\n\nThis cannot be undone.')) return;
  // Second confirmation — destructive enough to warrant it
  if (!confirm('Are you sure? Everything will be permanently deleted.')) return;
  const btn = $('deleteAllDataBtn');
  btn.disabled = true;
  btn.textContent = 'Deleting…';
  try {
    const r = await fetch('/data', { method: 'DELETE' });
    if (!r.ok) throw new Error(await r.text());
    // Reset local state
    currentSessionID = '';
    persistCurrentSessionId();
    streamCache = {};
    saveCache();
    autoTitled = {};
    saveAutoTitled();
    try {
      localStorage.removeItem('hidden_sessions');
      localStorage.removeItem('renamed_titles');
      localStorage.removeItem('folder_collapsed');
    } catch {}
    btn.disabled = false;
    btn.textContent = 'Delete Everything';
    closeSettings();
    chatContainer.innerHTML = '';
    showLanding(projectMode);
    loadHistory();
    ctxChart.hidden = true;
    lastContextUsage = null;
  } catch (err) {
    alert('Delete failed: ' + err.message);
    btn.disabled = false;
    btn.textContent = 'Delete Everything';
  }
};

// ============================================================
//  MULTI-FILE UPLOAD HANDLER
// ============================================================
async function handleFileSelect(e) {
  const files = Array.from(e.target.files);
  if (!files.length) return;

  for (const f of files) {
    // In workspace mode, upload directly to workspace files — skip chat attachment
    if (projectMode && currentSessionID) {
      const fd = new FormData();
      fd.append('file', f);
      try {
        const r = await fetch('/upload', { method: 'POST', body: fd });
        if (!r.ok) throw new Error(await r.text());
        const data = await r.json();
        await fetch(`/sessions/${currentSessionID}/files`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ file_id: data.file_id, filename: data.name })
        });
        refreshWorkspaceFiles();
      } catch (err) {
        alert(`Upload failed for ${f.name}: ` + err.message);
      }
      continue;
    }

    const fileRef = { id: null, name: f.name, loading: true, timestamp: Date.now() + Math.random() };
    attachedFiles.push(fileRef);
    renderFileChips();

    const fd = new FormData();
    fd.append('file', f);

    try {
      const r = await fetch('/upload', { method: 'POST', body: fd });
      if (!r.ok) throw new Error(await r.text());
      const data = await r.json();

      fileRef.id = data.file_id;
      fileRef.name = data.name;
      fileRef.loading = false;
    } catch (err) {
      alert(`Upload failed for ${f.name}: ` + err.message);
      attachedFiles = attachedFiles.filter(item => item !== fileRef);
    }
    renderFileChips();
  }
  fileInput.value = '';
}

function renderFileChips() {
  fileChipRow.innerHTML = '';
  attachedFiles.forEach(f => {
    const chip = document.createElement('div');
    chip.className = 'file-chip' + (f.isURL ? ' url-chip' : '');
    const icon = f.isURL ? '🌐' : '📎';
    let label;
    if (f.loading) {
      label = f.isURL ? `Fetching ${escapeHTML(f.name)}…` : `Uploading ${escapeHTML(f.name)}…`;
    } else {
      label = escapeHTML(f.name);
    }
    chip.innerHTML = `
      <span>${icon}</span>
      <span class="file-chip-name" title="${escapeHTML(f.url || f.name)}">${label}</span>
      <button class="file-chip-remove" title="Remove" aria-label="Remove attached file">✕</button>
    `;
    chip.querySelector('.file-chip-remove').onclick = () => removeAttachedFile(f);
    fileChipRow.appendChild(chip);
  });
}

// ============================================================
//  URL SCRAPER
// ============================================================
async function scrapeURL() {
  const urlInput = $('urlInput');
  const urlFetchBtn = $('urlFetchBtn');
  const raw = urlInput.value.trim();
  if (!raw) return;

  // normalise: add https:// if no scheme
  const fullURL = /^https?:\/\//i.test(raw) ? raw : 'https://' + raw;

  let hostname = fullURL;
  try { hostname = new URL(fullURL).hostname; } catch {}

  $('urlScrapeBar').hidden = true;
  urlInput.value = '';

  // In workspace mode, scrape directly to workspace files — skip chat attachment
  if (projectMode && currentSessionID) {
    urlFetchBtn.disabled = true;
    try {
      const r = await fetch('/scrape', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ url: fullURL })
      });
      if (!r.ok) throw new Error(await r.text());
      const data = await r.json();
      await fetch(`/sessions/${currentSessionID}/files`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ file_id: data.file_id, filename: hostname })
      });
      refreshWorkspaceFiles();
    } catch (err) {
      alert('Scrape failed: ' + err.message);
    }
    urlFetchBtn.disabled = false;
    return;
  }

  const tmpRef = { id: null, name: hostname, url: fullURL, loading: true, isURL: true, timestamp: Date.now() + Math.random() };
  attachedFiles.push(tmpRef);
  renderFileChips();

  urlFetchBtn.disabled = true;
  try {
    const r = await fetch('/scrape', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url: fullURL })
    });
    if (!r.ok) {
      const msg = await r.text();
      throw new Error(msg);
    }
    const data = await r.json();
    tmpRef.id = data.file_id;
    tmpRef.name = hostname;
    tmpRef.loading = false;
  } catch (err) {
    attachedFiles = attachedFiles.filter(f => f !== tmpRef);
    alert('Scrape failed: ' + err.message);
  }
  urlFetchBtn.disabled = false;
  renderFileChips();
}

function removeAttachedFile(targetFile) {
  attachedFiles = attachedFiles.filter(f => f !== targetFile);
  renderFileChips();
}

function clearAttachedFiles() {
  attachedFiles = [];
  renderFileChips();
}

// ============================================================
//  MARKDOWN RENDERER
// ============================================================
function escapeHTML(s) {
  return s.replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#039;'}[c]));
}

// Matches hostnames saved when a URL is scraped (e.g. "github.com", "docs.python.org")
const _linkNameRe = /^[a-z0-9-]+(\.[a-z0-9-]+)*\.(com|org|net|io|dev|co|ai|app|me|info|edu|gov|xyz|uk|de|fr|in|jp|ru|br|ca|au)$/i;
function isLinkName(name) {
  return name.startsWith('http') || _linkNameRe.test(name);
}

const codeArtifacts = {};
const CODE_CARD_MIN_LINES = 10;

function hashCode(s) {
  let h = 0;
  for (let i = 0; i < s.length; i++) h = ((h << 5) - h + s.charCodeAt(i)) | 0;
  return (h >>> 0).toString(36);
}

const LANG_EXT = {
  javascript:'js', js:'js', typescript:'ts', ts:'ts', jsx:'jsx', tsx:'tsx',
  python:'py', py:'py', html:'html', css:'css', json:'json', bash:'sh',
  shell:'sh', sh:'sh', sql:'sql', java:'java', c:'c', cpp:'cpp', csharp:'cs',
  go:'go', rust:'rs', rs:'rs', ruby:'rb', php:'php', yaml:'yml', yml:'yml',
  toml:'toml', md:'md', markdown:'md', xml:'xml',
};

function buildFileName(lang, idx, explicit) {
  if (explicit) return explicit;
  const ext = LANG_EXT[(lang || '').toLowerCase()] || 'txt';
  return `snippet-${idx}.${ext}`;
}

function parseFenceInfo(info) {
  const raw = (info || '').trim();
  if (!raw) return { lang: '', name: null };
  const parts = raw.split(/[:\s]+/).filter(Boolean);
  const looksLikeFile = (s) => /\.[A-Za-z0-9]{1,8}$/.test(s);
  if (parts.length === 1) {
    const only = parts[0];
    if (looksLikeFile(only)) {
      const ext = only.split('.').pop().toLowerCase();
      const langFromExt = Object.keys(LANG_EXT).find(k => LANG_EXT[k] === ext) || ext;
      return { lang: langFromExt, name: only };
    }
    return { lang: only, name: null };
  }
  const lang = parts[0];
  const nameToken = parts.slice(1).find(looksLikeFile) || null;
  return { lang, name: nameToken };
}

function findFilenameAbove(src, fenceStartIdx) {
  const before = src.slice(Math.max(0, fenceStartIdx - 200), fenceStartIdx);
  const lines = before.split('\n').map(l => l.trim()).filter(Boolean);
  if (!lines.length) return null;
  const last = lines[lines.length - 1];
  const patterns = [
    /`([\w.\-/]+\.[A-Za-z0-9]{1,8})`/,
    /\*\*([\w.\-/]+\.[A-Za-z0-9]{1,8})\*\*/,
    /^#{1,6}\s+.*?([\w.\-/]+\.[A-Za-z0-9]{1,8})/,
    /([\w.\-/]+\.[A-Za-z0-9]{1,8})/,
  ];
  for (const re of patterns) {
    const m = last.match(re);
    if (m) return m[1];
  }
  return null;
}

function renderMarkdown(src) {
  src = src || '';
  const codeBlocks = [];

  src = src.replace(/```([^\n]*)\n?([\s\S]*?)```/g, (_m, info, code, offset) => {
    const { lang, name } = parseFenceInfo(info);
    const explicit = name || findFilenameAbove(src, offset);
    codeBlocks.push({ lang, code, closed: true, explicit });
    return `\u0000CODE${codeBlocks.length - 1}\u0000`;
  });
  src = src.replace(/```([^\n]*)\n?([\s\S]*)$/g, (_m, info, code, offset) => {
    const { lang, name } = parseFenceInfo(info);
    const explicit = name || findFilenameAbove(src, offset);
    codeBlocks.push({ lang, code, closed: false, explicit });
    return `\u0000CODE${codeBlocks.length - 1}\u0000`;
  });

  let html = escapeHTML(src);

  // ── Block-level elements ─────────────────────────────────────
  // Headings
  html = html.replace(/^###\s+(.+)$/gm, '<h3>$1</h3>');
  html = html.replace(/^##\s+(.+)$/gm, '<h2>$1</h2>');
  html = html.replace(/^#\s+(.+)$/gm, '<h1>$1</h1>');

  // Horizontal rules: 3+ dashes/asterisks/underscores on their own line
  html = html.replace(/^[ \t]*[-]{3,}[ \t]*$/gm, '<hr>');
  html = html.replace(/^[ \t]*[*]{3,}[ \t]*$/gm, '<hr>');
  html = html.replace(/^[ \t]*[_]{3,}[ \t]*$/gm, '<hr>');

  // Blockquotes: > prefix (escapeHTML converts > to &gt;)
  html = html.replace(/^&gt;\s?(.*)/gm, '<blockquote>$1</blockquote>');

  // ── Inline formatting ─────────────────────────────────────────
  html = html.replace(/\*\*(.+?)\*\*/g, '<strong>$1</strong>');
  html = html.replace(/\*(.+?)\*/g, '<em>$1</em>');
  html = html.replace(/`([^`\n]+)`/g, '<code>$1</code>');
  // Images: ![alt](url) — must come before link rule
  html = html.replace(/!\[([^\]]*)\]\((https?:\/\/[^\s)]+)\)/g,
    '<img src="$2" alt="$1" class="chat-image" loading="lazy" onerror="this.style.display=\'none\'">');
  html = html.replace(/\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>');

  // ── Lists ─────────────────────────────────────────────────────
  html = html.replace(/(^|\n)((?:[ \t]*[-*]\s+.+(?:\n|$))+)/g, (_m, pre, block) => {
    const items = block.trim().split(/\n/).map(l => `<li>${l.replace(/^[ \t]*[-*]\s+/, '')}</li>`).join('');
    return pre + `<ul>${items}</ul>`;
  });

  // Ensure block elements are surrounded by blank lines so they are not
  // swallowed inside a <p> when paragraph-splitting on \n{2,}
  html = html.replace(/(<h[123][^>]*>)/g, '\n\n$1');
  html = html.replace(/(<\/h[123]>)/g, '$1\n\n');
  html = html.replace(/(<hr>)/g, '\n\n$1\n\n');
  html = html.replace(/(<blockquote>)/g, '\n\n$1');
  html = html.replace(/(<\/blockquote>)/g, '$1\n\n');

  // ── Tables ───────────────────────────────────────────────────
  html = html.replace(/((?:[ \t]*\|.+\|[ \t]*\n?){2,})/g, (tableBlock) => {
    const lines = tableBlock.trim().split('\n').map(l => l.trim()).filter(Boolean);
    if (lines.length < 2) return tableBlock;
    const isSep = (l) => /^\|[-:\s|]+\|$/.test(l);
    if (!isSep(lines[1])) return tableBlock;
    const parseRow = (l) => l.replace(/^\||\|$/g, '').split('|').map(c => c.trim());
    const headers = parseRow(lines[0]);
    const thead = `<thead><tr>${headers.map(h => `<th>${h}</th>`).join('')}</tr></thead>`;
    const tbody = lines.slice(2).map(row => `<tr>${parseRow(row).map(c => `<td>${c}</td>`).join('')}</tr>`).join('');
    return `<table>${thead}<tbody>${tbody}</tbody></table>`;
  });

  // ── Paragraph wrapping ────────────────────────────────────────
  html = html.split(/\n{2,}/).map(block => {
    if (/^\s*<(h\d|ul|ol|pre|hr|blockquote|table|div|\u0000)/.test(block)) return block;
    return `<p>${block.replace(/\n/g, '<br>')}</p>`;
  }).join('\n');

  html = html.replace(/\u0000CODE(\d+)\u0000/g, (_m, i) => {
    const cb = codeBlocks[+i];
    const lineCount = cb.code.split('\n').length;
    if (!cb.closed || lineCount < CODE_CARD_MIN_LINES) {
      return `<pre><code>${escapeHTML(cb.code)}</code></pre>`;
    }
    const id = 'art_' + hashCode(cb.lang + '|' + cb.code);
    const fileName = buildFileName(cb.lang, (+i) + 1, cb.explicit);
    codeArtifacts[id] = { code: cb.code, lang: cb.lang || 'text', name: fileName };
    return `<button type="button" class="code-card" data-artifact-id="${id}" aria-label="Open ${escapeHTML(fileName)} in canvas">
      <span class="code-card-icon">📄</span>
      <span class="code-card-meta">
        <span class="code-card-name">${escapeHTML(fileName)}</span>
        <span class="code-card-sub">${escapeHTML(cb.lang || 'text')} · ${lineCount} lines</span>
      </span>
      <span class="code-card-open">Open ›</span>
    </button>`;
  });

  return html;
}

// ============================================================
//  CODE CANVAS
// ============================================================
const canvasEl       = $('codeCanvas');
const canvasDim      = $('canvasDim');
const canvasCodeEl   = $('canvasCode');
const canvasNameEl   = $('canvasFileName');
const canvasLangEl   = $('canvasFileLang');
const canvasCopyBtn  = $('canvasCopyBtn');
const canvasCloseBtn = $('canvasCloseBtn');
let currentCanvasId = null;

function openCanvas(id) {
  const art = codeArtifacts[id];
  if (!art) return;
  currentCanvasId = id;
  canvasNameEl.textContent = art.name;
  canvasLangEl.textContent = art.lang;
  canvasCodeEl.textContent = art.code;
  canvasEl.classList.add('open');
  canvasDim.classList.add('show');
  canvasEl.setAttribute('aria-hidden', 'false');
  canvasCopyBtn.textContent = 'Copy';
}

function closeCanvas() {
  canvasEl.classList.remove('open');
  canvasDim.classList.remove('show');
  canvasEl.setAttribute('aria-hidden', 'true');
  currentCanvasId = null;
}

async function copyCanvasCode() {
  if (!currentCanvasId) return;
  const art = codeArtifacts[currentCanvasId];
  if (!art) return;
  try {
    await navigator.clipboard.writeText(art.code);
    canvasCopyBtn.textContent = 'Copied ✓';
    setTimeout(() => { canvasCopyBtn.textContent = 'Copy'; }, 1500);
  } catch {
    canvasCopyBtn.textContent = 'Copy failed';
    setTimeout(() => { canvasCopyBtn.textContent = 'Copy'; }, 1500);
  }
}

canvasCloseBtn.onclick = closeCanvas;
canvasDim.onclick      = closeCanvas;
canvasCopyBtn.onclick  = copyCanvasCode;
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && canvasEl.classList.contains('open')) closeCanvas();
});

chatContainer.addEventListener('click', (e) => {
  const card = e.target.closest('.code-card');
  if (!card) return;
  openCanvas(card.dataset.artifactId);
});

// ============================================================
//  FOLLOW-UP SELECTOR (Claude-style option card above input)
// ============================================================
function showFollowupSelector(question, options) {
  clearFollowupSelector();
  const row = $('followupRow');
  row.hidden = false;

  const card = document.createElement('div');
  card.className = 'followup-selector';

  // Header with question + close
  const header = document.createElement('div');
  header.className = 'followup-header';
  header.innerHTML = `<span class="followup-question">${escapeHTML(question)}</span>
    <button type="button" class="followup-close" aria-label="Dismiss">✕</button>`;
  header.querySelector('.followup-close').onclick = clearFollowupSelector;
  card.appendChild(header);

  // Options list
  const list = document.createElement('div');
  list.className = 'followup-options';
  const opts = Array.isArray(options) ? options : [];
  opts.forEach((opt, i) => {
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'followup-option';
    btn.innerHTML = `<span class="followup-num">${i + 1}</span><span class="followup-label">${escapeHTML(String(opt))}</span><span class="followup-arrow">→</span>`;
    btn.onclick = () => {
      clearFollowupSelector();
      sendFollowupReply(String(opt));
    };
    list.appendChild(btn);
  });

  // "Something else" row with skip
  const customRow = document.createElement('div');
  customRow.className = 'followup-custom-row';
  customRow.innerHTML = `<span class="followup-num">✎</span>
    <input type="text" class="followup-custom-input" placeholder="Something else…" autocomplete="off">
    <button type="button" class="followup-skip">Skip</button>`;
  const customInput = customRow.querySelector('.followup-custom-input');
  customInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && customInput.value.trim()) {
      const val = customInput.value.trim();
      clearFollowupSelector();
      sendFollowupReply(val);
    }
  });
  customRow.querySelector('.followup-skip').onclick = clearFollowupSelector;
  list.appendChild(customRow);

  card.appendChild(list);
  row.appendChild(card);
}

function clearFollowupSelector() {
  const row = $('followupRow');
  row.innerHTML = '';
  row.hidden = true;
  // Reset stop button if it was held for followup
  if (sendBtn.classList.contains('stop-mode') && !activeStreams.has(currentSessionID)) {
    setSendBtnMode('send');
  }
}

// Send a followup reply — renders as an inline chip, not a full user bubble
function sendFollowupReply(text) {
  userInput.value = text;
  pendingFollowupReply = true;
  sendMessage();
}
let pendingFollowupReply = false;

// ============================================================
//  STREAM-RESILIENT MESSAGE HANDLER
// ============================================================
async function sendMessage(e) {
  if (e) e.preventDefault();

  if (sendBtn.classList.contains('stop-mode')) {
    stopCurrentStream();
    return;
  }

  if (modeSwitching || inFlight || activeStreams.has(currentSessionID)) return;
  userScrolledUp = false; // reset smart-scroll on new message
  clearFollowupSelector();
  const query = userInput.value.trim();
  if (!query) return;

  // Hide landing page on first message
  hideLanding();
  setStickmanState('alert');

  if (currentSessionID) consumeAbandoned(currentSessionID, query);

  inFlight = true;
  setSendBtnMode('stop');
  userInput.value = '';
  userInput.style.height = 'auto';

  // Capture and reset followup flag
  const isFollowup = pendingFollowupReply;
  pendingFollowupReply = false;

  // User bubble — skip for followups (reply shown inline in think body)
  const userDiv = document.createElement('div');
  if (!isFollowup) {
    userDiv.className = 'user-msg-wrapper';
    userDiv.appendChild(createMsgMeta('You', new Date()));
    const userBubble = document.createElement('div');
    userBubble.className = 'message user-message';
    if (attachedFiles.length) {
      attachedFiles.forEach(f => {
        const line = document.createElement('div');
        line.className = 'attached-file-line';
        line.innerHTML = `📎 <span>${escapeHTML(f.name)}</span>`;
        userBubble.appendChild(line);
      });
    }
    userBubble.appendChild(document.createTextNode(query));
    userDiv.appendChild(userBubble);
  }

  const hasFiles = attachedFiles.length > 0 || projectMode;
  const statusText = hasFiles ? 'Reading files…' : 'Preparing response…';

  // Bot message — reuse last bot div for followup continuations
  let botDiv;
  let reusingBot = false;
  let existingThinkText = '';

  if (isFollowup) {
    const bots = chatContainer.querySelectorAll('.message.bot-message');
    const lastBot = bots.length ? bots[bots.length - 1] : null;
    if (lastBot) {
      botDiv = lastBot;
      reusingBot = true;

      // Remove old bot-content (new response replaces it)
      const oldContent = botDiv.querySelector('.bot-content');
      if (oldContent) oldContent.remove();

      // Insert user's followup choice in the thought blob body
      const existingBarEl = botDiv.querySelector('.blob-bar');
      if (existingBarEl && existingBarEl._blobBarRef) {
        const bb = existingBarEl._blobBarRef;
        const thinkBody = bb.blobs.thought.body;
        if (thinkBody) {
          const sep = document.createElement('div');
          sep.className = 'think-followup-sep';
          sep.innerHTML = `<span class="followup-reply-arrow">↳</span> ${escapeHTML(query)}`;
          thinkBody.appendChild(sep);
          existingThinkText = thinkBody.textContent;
        }
      }
    }
  }

  if (!botDiv) {
    // Normal flow — new user bubble + new bot message
    chatContainer.appendChild(userDiv);
    botDiv = document.createElement('div');
    botDiv.className = 'message bot-message';
    botDiv.appendChild(createMsgMeta('Ed', new Date()));
    const statusWrap = document.createElement('div');
    statusWrap.className = 'status-wrapper';
    statusWrap.innerHTML = `<div class="status-dots"><div class="dot"></div><div class="dot"></div><div class="dot"></div></div><div class="status-text">${escapeHTML(statusText)}</div>`;
    botDiv.appendChild(statusWrap);
    chatContainer.appendChild(botDiv);
  }
  chatContainer.scrollTop = chatContainer.scrollHeight;

  // Stream state tracker
  const abortController = new AbortController();
  const streamState = {
    accumulated: '',
    started: false,
    sources: null,
    actions: [],
    research: null,
    botDiv,
    userDiv,
    isDone: false,
    sessionId: currentSessionID || 'pending_' + Date.now(),
    userQuery: query,
    attachedFilesSnapshot: attachedFiles.map(f => ({ name: f.name, id: f.id })),
    abortController,
    wasAbandoned: false
  };
  streamState._statusEl = botDiv.querySelector('.status-text');

  // If continuing from a followup, reuse the existing blob bar
  if (reusingBot) {
    const existingBarEl = botDiv.querySelector('.blob-bar');
    if (existingBarEl && existingBarEl._blobBarRef) {
      streamState.blobBar = existingBarEl._blobBarRef;
      streamState.thinkText = existingThinkText;
      streamState.thinkCollapsed = false;
      // Re-activate thought blob for continued thinking
      if (streamState.blobBar.blobs.thought.body) {
        streamState.thinkBody = streamState.blobBar.blobs.thought.body;
        streamState.thinkStartTime = Date.now();
        setActiveDots(streamState.blobBar, 'thought', true);
      }
    }
  }
  activeStreams.set(streamState.sessionId, streamState);

  streamCache[streamState.sessionId] = {
    accumulated: '',
    actions: [],
    sources: null,
    researchSteps: [],
    blobData: { thought: [], researched: [], tools: [], files: [] },
    status: statusText,
    timestamp: Date.now(),
    done: false,
    userQuery: query,
    attachedFiles: streamState.attachedFilesSnapshot,
    sessionId: streamState.sessionId
  };
  saveCache();

  const reqBody = {
    session_id: currentSessionID,
    message: query,
    think: getThinkEnabled(),
    incognito: ghostMode,
    file_ids: attachedFiles.map(f => f.id).filter(id => id !== null)
  };

  try {
    const resp = await fetch('/chat/web', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(reqBody),
      signal: abortController.signal,
    });
    if (!resp.ok || !resp.body) throw new Error(`HTTP ${resp.status}`);

    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buf = '';

    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });

      let nl;
      while ((nl = buf.indexOf('\n')) >= 0) {
        const line = buf.slice(0, nl).trim();
        buf = buf.slice(nl + 1);
        if (!line) continue;

        let ev;
        try { ev = JSON.parse(line); } catch { continue; }

        if (ev.type === 'meta' && ev.meta?.session_id) {
          const oldId = streamState.sessionId;
          streamState.sessionId = ev.meta.session_id;
          currentSessionID = ev.meta.session_id;
          persistCurrentSessionId();
          if (oldId !== streamState.sessionId) {
            if (oldId.startsWith('pending_')) {
              streamState.isNewSession = true;
              // Session was created on-demand by backend — mark as temp
              if (!ghostMode && !tempSessionId) setTempSession(ev.meta.session_id);
            }
            streamCache[streamState.sessionId] = streamCache[oldId];
            delete streamCache[oldId];
            activeStreams.delete(oldId);
            activeStreams.set(streamState.sessionId, streamState);
            saveCache();
          }
          if (ghostMode) {
            const h = LS.getHidden();
            if (!h.includes(currentSessionID)) { h.push(currentSessionID); LS.setHidden(h); }
          }
        }

        if (ev.type === 'meta') {
          if (ev.meta?.stage) {
            // Stages that only update the status text, no blob needed
            const statusOnlyStages = new Set(['model_loading', 'model_loaded', 'history_compressing', 'history_compress_done', 'context_usage', 'context_compacting', 'context_compacted']);
            if (statusOnlyStages.has(ev.meta.stage)) {
              handleBlobStage(null, ev.meta, streamState);
            } else {
              if (!streamState.blobBar) {
                streamState.blobBar = createBlobBar(streamState.botDiv);
                // Remove status wrapper when blobs start
                const status = streamState.botDiv.querySelector('.status-wrapper');
                if (status) { status.classList.add('vanishing'); setTimeout(() => status.remove(), 350); }
              }
              handleBlobStage(streamState.blobBar, ev.meta, streamState);
              // Persist meta events for blob restoration on reload
              const cache = streamCache[streamState.sessionId];
              if (cache) {
                if (!cache.metaEvents) cache.metaEvents = [];
                cache.metaEvents.push(ev.meta);
              }
            }
          }
          if (ev.meta?.sources) {
            streamState.sources = ev.meta.sources;
            streamCache[streamState.sessionId].sources = ev.meta.sources;
          }
          if (ev.meta?.context_usage) {
            const cu = ev.meta.context_usage;
            // Sync currentNumCtx with what the server actually used
            if (cu.num_ctx) currentNumCtx = cu.num_ctx;
            updateContextChart(cu);
            const used = (cu.system||0)+(cu.history||0)+(cu.files||0)+(cu.rag||0)+(cu.user_turn||0);
            const total = cu.num_ctx || cu.limit;
            ctxLog(`ctx: ${used}/${total} (${Math.round(used/total*100)}%) — sys:${cu.system} hist:${cu.history} files:${cu.files} rag:${cu.rag} user:${cu.user_turn} avail:${cu.available}${cu.compressed ? ' compressed:'+cu.compressed : ''}`);
            if ((cu.compressed || 0) > 0) {
              streamState.ctxOverloaded = true;
              ctxLog(`⚠ ${cu.compressed} messages were dropped to fit context`);
            }
          }
          saveCache();
        } else if (ev.type === 'think_token') {
          // ── Thinking tokens → Thought blob ──
          if (streamState.sessionId === currentSessionID) {
            setStickmanState('thinking');
            setStickmanThinking(true);
          }
          if (!streamState.blobBar) {
            streamState.blobBar = createBlobBar(streamState.botDiv);
            const status = streamState.botDiv.querySelector('.status-wrapper');
            if (status) { status.classList.add('vanishing'); setTimeout(() => status.remove(), 350); }
          }
          if (!streamState.thinkBody) {
            showBlob(streamState.blobBar, 'thought');
            setActiveDots(streamState.blobBar, 'thought', true);
            openBlob(streamState.blobBar, 'thought');
            streamState.thinkBody = streamState.blobBar.blobs.thought.body;
            streamState.thinkText = '';
            streamState.thinkStartTime = Date.now();
          }
          streamState.thinkText += ev.content || '';
          streamState.blobData = streamState.blobData || { thought: [], researched: [], tools: [], files: [] };
          streamState.blobData.thought = [{ text: streamState.thinkText }];
          streamCache[streamState.sessionId].blobData = streamState.blobData;
          saveCache();
          if (streamState.sessionId === currentSessionID && !streamState._thinkRafPending) {
            streamState._thinkRafPending = requestAnimationFrame(() => {
              streamState._thinkRafPending = null;
              if (streamState.thinkBody) {
                streamState.thinkBody.innerHTML = renderMarkdown(streamState.thinkText);
                blobBodyScroll(streamState.thinkBody);
              }
            });
          }
        } else if (ev.type === 'token') {
          if (streamState.sessionId === currentSessionID) {
            setStickmanState('typing');
            // Thinking is done once regular tokens arrive
            if (streamState.thinkBody) {
              setActiveDots(streamState.blobBar, 'thought', false);
              setStickmanThinking(false);
              // Update pill with elapsed time
              const elapsed = Math.round((Date.now() - (streamState.thinkStartTime || Date.now())) / 1000);
              if (elapsed > 0) {
                const pill = streamState.blobBar.blobs.thought.pill;
                pill.querySelector('.blob-label').textContent = `Thought · ${elapsed}s`;
              }
              // Close thought blob, let response show
              closeAllBlobs(streamState.blobBar.blobs);
            }
          }
          if (!streamState.started) {
            const status = streamState.botDiv.querySelector('.status-wrapper');
            if (status) {
              status.classList.add('vanishing');
              setTimeout(() => status.remove(), 350);
            }
            if (!streamState.botDiv.querySelector('.bot-content')) {
              const c = document.createElement('div');
              c.className = 'bot-content';
              streamState.botDiv.appendChild(c);
            }
            streamState.started = true;
          }
          streamState.accumulated += ev.content || '';
          streamCache[streamState.sessionId].accumulated = streamState.accumulated;
          saveCache();

          if (streamState.sessionId === currentSessionID) {
            // Throttle DOM rebuilds: schedule at most one renderMarkdown per animation frame.
            if (!streamState._rafPending) {
              streamState._rafPending = requestAnimationFrame(() => {
                streamState._rafPending = null;
                const c2 = streamState.botDiv.querySelector('.bot-content');
                if (c2) {
                  c2.innerHTML = renderMarkdown(streamState.accumulated);
                  appendSources(streamState.botDiv, streamState.sources);
                }
                autoScroll();
              });
            }
          }
        } else if (ev.type === 'action') {
          if (ev.name === 'ask_followup') {
            const q = (ev.args && ev.args.question) || '';
            const opts = (ev.args && ev.args.options) || [];
            if (q && streamState.sessionId === currentSessionID) {
              showFollowupSelector(q, opts);
              streamState.hasFollowup = true;
            }
          } else if (ev.name === 'propose_edit') {
            if (streamState.sessionId === currentSessionID) {
              showEditProposal(ev.args || {});
            }
          } else if (ev.name === 'show_notification') {
            triggerNotification(ev.args?.title || 'Assistant', ev.args?.body || '');
          } else {
            streamState.actions.push({ name: ev.name, args: ev.args || {} });
            streamCache[streamState.sessionId].actions = streamState.actions;
            saveCache();
            // Tool actions are now tracked in blob bar — no separate cards
          }
        } else if (ev.type === 'clear_tokens') {
          // Backend recovered tool calls from text — clear the garbage output
          if (streamState._rafPending) {
            cancelAnimationFrame(streamState._rafPending);
            streamState._rafPending = null;
          }
          streamState.accumulated = '';
          streamCache[streamState.sessionId].accumulated = '';
          saveCache();
          if (streamState.sessionId === currentSessionID) {
            const c = streamState.botDiv.querySelector('.bot-content');
            if (c) c.innerHTML = '';
          }
        } else if (ev.type === 'error') {
          const errBox = document.createElement('div');
          errBox.className = 'error-text';
          errBox.textContent = 'Error: ' + (ev.error || 'unknown');
          streamState.botDiv.appendChild(errBox);
        } else if (ev.type === 'done') {
          if (streamState.sessionId === currentSessionID) {
            setStickmanThinking(false);
            setStickmanState('idle');
          }
          if (streamState.blobBar) finalizeBlobBar(streamState.blobBar);
          if (!streamState.started) {
            const status = streamState.botDiv.querySelector('.status-wrapper');
            if (status) status.remove();
          }
          const c = streamState.botDiv.querySelector('.bot-content');
          if (c) c.innerHTML = renderMarkdown(streamState.accumulated);
          appendSources(streamState.botDiv, streamState.sources);
          addTtsButton(streamState.botDiv, streamState.accumulated);
          _tryAutoSpeak(streamState.botDiv, streamState.accumulated);

          streamCache[streamState.sessionId].done = true;
          saveCache();

          // Auto-compact context if compressed OR proactively at 90%
          const needsCompact = streamState.ctxOverloaded || (_ctxCompactPending && _ctxCompactSessionId === streamState.sessionId);
          if (needsCompact && streamState.sessionId === currentSessionID) {
            _ctxCompactPending = false;
            _ctxCompactSessionId = null;
            autoCompactContext(streamState.sessionId);
          }
          // Auto-title on first message of a new session
          if (streamState.isNewSession) {
            tryAutoTitle(streamState.sessionId, streamState.userQuery);
          }
        }

        if (streamState.sessionId === currentSessionID) autoScroll();
      }
    }
  } catch (err) {
    if (err.name === 'AbortError' || streamState.wasAbandoned) {
      delete streamCache[streamState.sessionId];
      saveCache();
    } else if (streamState.sessionId === currentSessionID) {
      streamState.botDiv.innerHTML = `<span class="error-text">Error: ${escapeHTML(err.message)}</span>`;
    }
  } finally {
    if (streamState.sessionId === currentSessionID) {
      setStickmanThinking(false);
      setStickmanState('idle');
    }
    streamState.isDone = true;
    activeStreams.delete(streamState.sessionId);
    setTimeout(() => {
      if (streamCache[streamState.sessionId]?.done) {
        delete streamCache[streamState.sessionId];
        saveCache();
      }
    }, 86400000);

    const isCurrent = streamState.sessionId === currentSessionID;

    if (!streamState.wasAbandoned && isCurrent) {
      clearAttachedFiles();
    }

    // If a followup question is pending, keep stop mode so user can cancel
    if (streamState.hasFollowup && isCurrent) {
      inFlight = false;
      setSendBtnMode('stop');
    } else if (isCurrent) {
      inFlight = false;
      setSendBtnMode('send');
    }
    loadHistory();
  }
}


// ============================================================
//  CACHE RESTORE & LIVE UPDATER
// ============================================================
function renderCachedResponse(cached) {
  if (cached.userQuery) {
    const userDiv = document.createElement('div');
    userDiv.className = 'user-msg-wrapper';
    userDiv.appendChild(createMsgMeta('You', cached.timestamp));
    const userBubble = document.createElement('div');
    userBubble.className = 'message user-message';
    if (cached.attachedFiles?.length) {
      cached.attachedFiles.forEach(f => {
        const line = document.createElement('div');
        line.className = 'attached-file-line';
        line.innerHTML = `📎 <span>${escapeHTML(f.name)}</span>`;
        userBubble.appendChild(line);
      });
    }
    userBubble.appendChild(document.createTextNode(cached.userQuery));
    userDiv.appendChild(userBubble);
    chatContainer.appendChild(userDiv);
  }

  const botDiv = document.createElement('div');
  botDiv.className = 'message bot-message';
  botDiv.appendChild(createMsgMeta('Ed', cached.timestamp));

  const sid = cached.sessionId || currentSessionID;
  const liveState = activeStreams.get(sid);
  if (liveState && !liveState.started && !cached.accumulated) {
    const statusText = cached.status || 'Analyzing & composing response…';
    botDiv.innerHTML = `<div class="status-wrapper"><div class="status-dots"><div class="dot"></div><div class="dot"></div><div class="dot"></div></div><div class="status-text">${escapeHTML(statusText)}</div></div>`;
    liveState.botDiv = botDiv;
    chatContainer.appendChild(botDiv);
    return;
  }

  // Restore blob bar from cache — replay all meta events
  const hasBlobs = cached.metaEvents && cached.metaEvents.length > 0;
  if (hasBlobs) {
    const bb = createBlobBar(botDiv);
    // Replay meta events to rebuild research/tools/files blobs
    if (cached.metaEvents) {
      bb._restoring = true;
      for (const meta of cached.metaEvents) {
        handleBlobStage(bb, meta);
      }
      bb._restoring = false;
    }
    // Finalize if done (stop dots, close panels, update counts)
    if (cached.done) {
      finalizeBlobBar(bb);
    }
    // If stream is still live, wire up for continued updates
    if (liveState && !liveState.isDone) {
      liveState.blobBar = bb;
    }
  }

  const content = document.createElement('div');
  content.className = 'bot-content';
  content.innerHTML = renderMarkdown(cached.accumulated || '');
  botDiv.appendChild(content);

  appendSources(botDiv, cached.sources);
  if (cached.done) addTtsButton(botDiv, cached.accumulated || '');

  if (liveState && !liveState.isDone) {
    liveState.botDiv = botDiv;
  }

  if (!cached.done && !liveState) {
    const note = document.createElement('div');
    note.className = 'muted';
    note.style.cssText = 'margin-top:8px;font-size:0.78rem;';
    note.innerHTML = '⏳ Generation was interrupted. <button onclick="retryCachedSession()" style="background:none;border:none;color:var(--accent-blue);cursor:pointer;text-decoration:underline">Retry</button>';
    botDiv.appendChild(note);
  }

  chatContainer.appendChild(botDiv);
}

function attachLiveUpdater(sessionId) {
  const state = activeStreams.get(sessionId);
  if (!state) return;

  const interval = setInterval(() => {
    if (state.isDone || !activeStreams.has(sessionId)) {
      clearInterval(interval);
      return;
    }
    if (state.sessionId === currentSessionID && state.started) {
      const c = state.botDiv.querySelector('.bot-content');
      if (c) c.innerHTML = renderMarkdown(state.accumulated);
      appendSources(state.botDiv, state.sources);
      autoScroll();
    }
  }, 300);
}

window.retryCachedSession = async () => {
  const cached = streamCache[currentSessionID];
  if (!cached) return;
  const savedQuery = cached.userQuery;
  delete streamCache[currentSessionID];
  saveCache();
  const sid = currentSessionID;
  currentSessionID = '';
  await switchSession(sid);
  if (savedQuery && !userInput.value.trim()) {
    userInput.value = savedQuery;
    userInput.style.height = 'auto';
    userInput.style.height = Math.min(userInput.scrollHeight, 200) + 'px';
    userInput.focus();
  }
};



function appendSources(botDiv, sources) {
  botDiv.querySelectorAll('.sources-block').forEach(n => n.remove());
  if (sources && sources.length) {
    const el = document.createElement('div');
    el.className = 'sources-block';
    const links = sources.map(u => `<a href="${escapeHTML(u)}" target="_blank" rel="noopener">${escapeHTML(hostname(u))}</a>`).join('  ·  ');
    el.innerHTML = '<b>Sources:</b> ' + links;
    botDiv.appendChild(el);
  }
}

// ============================================================
//  BLOB BAR — category pills (THOUGHT, RESEARCHED, TOOLS, FILES)
// ============================================================
const BLOB_TYPES = [
  { key: 'thought',    label: 'Thought',    icon: '💭' },
  { key: 'researched', label: 'Research',   icon: '🔍' },
  { key: 'tools',      label: 'Tools',      icon: '⚡' },
  { key: 'files',      label: 'Resources',  icon: '📎' },
];

function createBlobBar(botDiv) {
  const bar = document.createElement('div');
  bar.className = 'blob-bar';
  // Insert at top of botDiv
  botDiv.insertBefore(bar, botDiv.firstChild);

  const blobs = {};
  for (const t of BLOB_TYPES) {
    // Pill
    const pill = document.createElement('span');
    pill.className = 'blob-pill';
    pill.dataset.blob = t.key;
    pill.innerHTML = `<span class="blob-icon">${t.icon}</span><span class="blob-label">${t.label}</span>`;
    pill.style.display = 'none'; // hidden until content added
    bar.appendChild(pill);

    // Panel (overlay)
    const panel = document.createElement('div');
    panel.className = 'blob-panel';
    panel.dataset.blob = t.key;
    const body = document.createElement('div');
    body.className = 'blob-panel-body';
    // Per-blob scroll intent tracking
    body._userScrolled = false;
    body._autoScrolling = false;
    body.addEventListener('wheel', (e) => {
      if (e.deltaY < 0) body._userScrolled = true;
    }, { passive: true });
    let _bTouchY = 0;
    body.addEventListener('touchstart', (e) => { _bTouchY = e.touches[0].clientY; }, { passive: true });
    body.addEventListener('touchend', (e) => {
      if (e.changedTouches[0].clientY - _bTouchY > 20) body._userScrolled = true;
    }, { passive: true });
    body.addEventListener('scroll', () => {
      if (body._autoScrolling) return;
      const atBottom = body.scrollHeight - body.scrollTop <= body.clientHeight + 40;
      if (atBottom) body._userScrolled = false;
    }, { passive: true });
    panel.appendChild(body);
    bar.appendChild(panel);

    panel.onclick = (e) => e.stopPropagation(); // prevent click-outside from closing
    pill.onclick = (e) => {
      e.stopPropagation();
      const isOpen = panel.classList.contains('open');
      closeAllBlobs(blobs);
      if (!isOpen) {
        panel.classList.add('open');
        pill.classList.add('active');
        body._userScrolled = false; // reset scroll lock when user opens a blob
        if (bar._blobBarRef) bar._blobBarRef._userOpened = true;
      } else {
        if (bar._blobBarRef) bar._blobBarRef._userOpened = false;
      }
    };

    blobs[t.key] = { pill, panel, body, steps: null, stepsByUrl: {}, count: 0, active: false };
  }

  // For research steps inside the researched blob
  const researchSteps = document.createElement('div');
  researchSteps.className = 'research-steps';
  blobs.researched.body.appendChild(researchSteps);
  blobs.researched.steps = researchSteps;

  // For tool steps inside the tools blob
  const toolSteps = document.createElement('div');
  toolSteps.className = 'research-steps';
  blobs.tools.body.appendChild(toolSteps);
  blobs.tools.steps = toolSteps;

  // For file steps inside the files blob
  const fileSteps = document.createElement('div');
  fileSteps.className = 'research-steps';
  blobs.files.body.appendChild(fileSteps);
  blobs.files.steps = fileSteps;

  const blobBar = { bar, blobs, sourceCount: 0 };
  bar._blobBarRef = blobBar; // store ref for followup reuse
  return blobBar;
}

function closeAllBlobs(blobs) {
  for (const key of Object.keys(blobs)) {
    blobs[key].panel.classList.remove('open');
    blobs[key].pill.classList.remove('active');
  }
}

function showBlob(blobBar, key) {
  const b = blobBar.blobs[key];
  if (!b) return;
  b.pill.style.display = '';
}

function setActiveDots(blobBar, key, active) {
  if (blobBar._restoring) return; // skip dots during cache replay
  const b = blobBar.blobs[key];
  if (!b || b.active === active) return;
  b.active = active;
  if (active) {
    const dots = document.createElement('span');
    dots.className = 'blob-dots';
    dots.innerHTML = '<span class="blob-dot"></span><span class="blob-dot"></span><span class="blob-dot"></span>';
    b.pill.appendChild(dots);
  } else {
    const dots = b.pill.querySelector('.blob-dots');
    if (dots) dots.remove();
  }
  setPulse(blobBar, key, active);
}

function blobBodyScroll(body) {
  if (!body || body._userScrolled) return;
  body._autoScrolling = true;
  body.scrollTop = body.scrollHeight;
  requestAnimationFrame(() => { body._autoScrolling = false; });
}

function openBlob(blobBar, key) {
  if (blobBar._restoring) return; // don't auto-open panels during cache replay
  const b = blobBar.blobs[key];
  if (!b) return;
  // Close other blobs, open this one
  for (const [k, ob] of Object.entries(blobBar.blobs)) {
    if (k !== key) { ob.panel.classList.remove('open'); ob.pill.classList.remove('active'); }
  }
  b.panel.classList.add('open');
  b.pill.classList.add('active');
  b.body._userScrolled = false; // reset scroll lock when auto-switching
  blobBar._userOpened = false;  // clear manual flag — stream is driving now
}

function setPulse(blobBar, key, on) {
  if (blobBar._restoring) return;
  const b = blobBar.blobs[key];
  if (!b) return;
  b.pill.classList.toggle('pulse', on);
}

function addBlobStep(blobBar, key, opts) {
  const b = blobBar.blobs[key];
  if (!b || !b.steps) return null;
  showBlob(blobBar, key);
  const step = document.createElement('div');
  step.className = 'research-step ' + (opts.state || 'active');
  step.innerHTML = `<span class="step-icon"></span><span class="step-body"></span>`;
  step.querySelector('.step-body').innerHTML = opts.html;
  b.steps.appendChild(step);
  b.count++;
  if (opts.url) b.stepsByUrl[opts.url] = step;
  return step;
}

function lastActiveStep(blobBar, key) {
  const b = blobBar.blobs[key];
  if (!b || !b.steps) return null;
  const all = b.steps.querySelectorAll('.research-step.active');
  return all.length ? all[all.length - 1] : null;
}

function finalizeBlobBar(blobBar) {
  if (!blobBar) return;
  blobBar._userOpened = false;
  for (const key of Object.keys(blobBar.blobs)) {
    setActiveDots(blobBar, key, false);
    const b = blobBar.blobs[key];
    if (b.steps) {
      b.steps.querySelectorAll('.research-step.active').forEach(s => setStepState(s, 'done'));
    }
    // Update pill label with count
    if (key === 'researched' && blobBar.sourceCount > 0) {
      b.pill.querySelector('.blob-label').textContent = `Research · ${blobBar.sourceCount}`;
    } else if (b.count > 0 && key !== 'thought') {
      b.pill.querySelector('.blob-label').textContent = `${BLOB_TYPES.find(t => t.key === key).label} · ${b.count}`;
    }
  }
  closeAllBlobs(blobBar.blobs);
}

// Update the "Preparing response…" status line while it's still visible.
function updateStatusText(streamState, text) {
  if (streamState._statusEl && !streamState.started) {
    streamState._statusEl.textContent = text;
  }
}

// Route meta stages to the correct blob
// streamState is optional — only needed for status-text-only stages (blobBar may be null)
function handleBlobStage(blobBar, meta, streamState) {
  const stage = meta.stage;
  switch (stage) {
    case 'model_loading':
    case 'model_loaded':
      break;

    // ── Pre-blob status updates ──
    case 'history_compressing':
      updateStatusText(streamState, `Summarizing ${meta.messages || ''} earlier messages…`);
      break;
    case 'history_compress_done':
      updateStatusText(streamState, meta.ok ? 'History compressed. Composing…' : 'Composing response…');
      break;
    case 'exchange_retrieval':
      updateStatusText(streamState, 'Searching past exchanges…');
      ctxLog('RAG: searching past exchanges for relevant context');
      break;
    case 'exchange_retrieved':
      updateStatusText(streamState, meta.matches > 0 ? `Found ${meta.matches} relevant exchange(s). Composing…` : 'Composing response…');
      ctxLog(`RAG: retrieved ${meta.matches || 0} relevant past exchanges`);
      break;
    case 'context_compacting':
      updateStatusText(streamState, `Compacting context (${meta.messages || ''} messages)…`);
      ctxLog(`Server compacting: ${meta.messages || '?'} messages, cursor=${meta.cursor || 0}`);
      if (streamState) streamState._compacting = true;
      break;
    case 'context_compacted':
      updateStatusText(streamState, meta.ok ? 'Context compacted. Composing…' : 'Composing response…');
      ctxLog(`Server compact ${meta.ok ? 'succeeded' : 'failed'}`);
      if (streamState) streamState._compacting = false;
      break;

    // ── Tool calls → TOOLS blob ──
    case 'tool_call': {
      const toolName = meta.tool || 'tool';
      ctxLog(`Tool call: ${toolName}(${meta.args ? JSON.stringify(meta.args).slice(0,80) : ''})`);
      if (toolName === 'web_search') {
        // web_search detailed steps go to RESEARCHED
        showBlob(blobBar, 'researched');
        setActiveDots(blobBar, 'researched', true);
        openBlob(blobBar, 'researched');
      } else {
        showBlob(blobBar, 'tools');
        setActiveDots(blobBar, 'tools', true);
        openBlob(blobBar, 'tools');
        let detail = '';
        if (meta.args && typeof meta.args === 'object') {
          detail = Object.entries(meta.args)
            .filter(([, v]) => v !== '' && v != null)
            .map(([k, v]) => `<span class="step-meta">${escapeHTML(k)}: ${escapeHTML(String(v).slice(0, 120))}</span>`)
            .join(' · ');
        }
        addBlobStep(blobBar, 'tools', {
          state: 'active',
          html: `Calling <b>${escapeHTML(toolName)}</b>…${detail ? '<br>' + detail : ''}`
        });
      }
      break;
    }
    case 'tool_result': {
      const toolName = meta.tool || 'tool';
      if (toolName !== 'web_search') {
        const isErr = (meta.result || '').startsWith('error:');
        let detail = '';
        if (!isErr && meta.result) {
          const preview = meta.result.length > 150 ? meta.result.slice(0, 150) + '…' : meta.result;
          detail = `<br><span class="step-meta">${escapeHTML(preview)}</span>`;
        }
        setStepState(lastActiveStep(blobBar, 'tools'), isErr ? 'failed' : 'done',
          isErr
            ? `<b>${escapeHTML(toolName)}</b> failed`
            : `<b>${escapeHTML(toolName)}</b> done${detail}`);
      }
      break;
    }

    // ── Agent file/link stages → RESOURCES blob ──
    case 'agent_files_start': {
      showBlob(blobBar, 'files');
      setActiveDots(blobBar, 'files', true);
      openBlob(blobBar, 'files');
      const fCount = meta.files || 0;
      const lCount = meta.links || 0;
      const parts = [];
      if (fCount > 0) parts.push(`<b>${fCount}</b> file${fCount === 1 ? '' : 's'}`);
      if (lCount > 0) parts.push(`<b>${lCount}</b> link${lCount === 1 ? '' : 's'}`);
      addBlobStep(blobBar, 'files', { state: 'active', html: `Exploring ${parts.join(' and ')}…` });
      break;
    }
    case 'agent_read_file': {
      const fname = meta.name || meta.file_id || 'file';
      const icon = isLinkName(fname) ? '🌐' : '📄';
      addBlobStep(blobBar, 'files', { state: 'active', html: `${icon} Reading <b>${escapeHTML(fname)}</b>…` });
      break;
    }
    case 'agent_read_done': {
      const fname = meta.name || meta.file_id || 'file';
      const kb = meta.chars ? `${Math.round(meta.chars / 1000)}k chars` : '';
      setStepState(lastActiveStep(blobBar, 'files'), 'done',
        `Read <b>${escapeHTML(fname)}</b>${kb ? ` <span class="step-meta">(${kb})</span>` : ''}`);
      break;
    }

    // ── File reading stages → FILES blob ──
    case 'file_read':
      updateStatusText(streamState, `Reading ${meta.files} file${meta.files === 1 ? '' : 's'}…`);
      showBlob(blobBar, 'files');
      setActiveDots(blobBar, 'files', true);
      openBlob(blobBar, 'files');
      addBlobStep(blobBar, 'files', { state: 'active', html: `Loading <b>${meta.files}</b> file${meta.files === 1 ? '' : 's'}…` });
      break;
    case 'file_chunked':
      setStepState(lastActiveStep(blobBar, 'files'), 'done',
        `Loaded files`);
      addBlobStep(blobBar, 'files', { state: 'done', html: `Split into <b>${meta.chunks}</b> chunk${meta.chunks === 1 ? '' : 's'}` });
      break;
    case 'file_rag_embed':
      addBlobStep(blobBar, 'files', { state: 'active', html: `Embedding <b>${meta.chunks}</b> chunks…` });
      break;
    case 'file_rag_done':
      if (meta.skipped) {
        addBlobStep(blobBar, 'files', { state: 'done', html: `Using all <b>${meta.selected}</b> chunks` });
      } else {
        setStepState(lastActiveStep(blobBar, 'files'), 'done',
          `Selected <b>${meta.selected}</b> of <b>${meta.total}</b> chunks`);
      }
      break;
    case 'file_truncated':
      setStepState(lastActiveStep(blobBar, 'files'), 'done',
        `Read <b>${meta.file_count}</b> file${meta.file_count === 1 ? '' : 's'} <span class="step-meta">(truncated)</span>`);
      break;
    case 'file_ready': {
      const prev = lastActiveStep(blobBar, 'files');
      if (prev) {
        setStepState(prev, 'done',
          `Read <b>${meta.file_count}</b> file${meta.file_count === 1 ? '' : 's'} <span class="step-meta">(${Math.round(meta.chars / 1000)}k chars)</span>`);
      }
      break;
    }

    // ── Web search stages → RESEARCHED blob ──
    case 'search_start':
      updateStatusText(streamState, `Searching: "${meta.query || ''}"…`);
      showBlob(blobBar, 'researched');
      setActiveDots(blobBar, 'researched', true);
      openBlob(blobBar, 'researched');
      addBlobStep(blobBar, 'researched', { state: 'active', html: `Searching <em>"${escapeHTML(meta.query || '')}"</em>` });
      break;
    case 'search_results': {
      setStepState(lastActiveStep(blobBar, 'researched'), 'done');
      const n = meta.count || 0;
      blobBar._fetchedCount = 0; // track successful scrapes
      addBlobStep(blobBar, 'researched', { state: 'done', html: `Found <b>${n}</b> result${n === 1 ? '' : 's'}` });
      break;
    }
    case 'fetch_start': {
      const u = meta.url;
      addBlobStep(blobBar, 'researched', { state: 'active', url: u, html: `Reading <a href="${escapeHTML(u)}" target="_blank" rel="noopener">${escapeHTML(hostname(u))}</a>` });
      break;
    }
    case 'fetch_done': {
      const u = meta.url;
      blobBar._fetchedCount = (blobBar._fetchedCount || 0) + 1;
      blobBar.sourceCount = blobBar._fetchedCount; // actual scraped sites
      const s = blobBar.blobs.researched.stepsByUrl[u];
      setStepState(s, 'done',
        `Read <a href="${escapeHTML(u)}" target="_blank" rel="noopener">${escapeHTML(hostname(u))}</a> <span class="step-meta">(${meta.chars} chars)</span>`);
      break;
    }
    case 'fetch_skipped':
    case 'fetch_fallback': {
      const u = meta.url;
      const s = blobBar.blobs.researched.stepsByUrl[u];
      setStepState(s, 'failed',
        `<a href="${escapeHTML(u)}" target="_blank" rel="noopener">${escapeHTML(hostname(u))}</a> <span class="step-meta">(${escapeHTML(meta.reason || 'skipped')})</span>`);
      break;
    }
    case 'embed_start':
      addBlobStep(blobBar, 'researched', { state: 'active', html: `Analyzing <b>${meta.chunks}</b> chunks…` });
      break;
    case 'embed_done':
      setStepState(lastActiveStep(blobBar, 'researched'), 'done', `Analyzed <b>${meta.kept}</b> chunks`);
      break;
    case 'rank_done':
      addBlobStep(blobBar, 'researched', { state: 'done', html: `Selected top <b>${meta.selected}</b> snippets` });
      break;
    case 'search_failed':
      blobBar.blobs.researched.steps.querySelectorAll('.research-step.active').forEach(s => setStepState(s, 'failed'));
      addBlobStep(blobBar, 'researched', { state: 'failed', html: `Search failed: <span class="step-meta">${escapeHTML(meta.error || 'unknown')}</span>` });
      break;
  }

}

// ---- Action card rendering -------------------------------------------------

function humanizeAction(name) {
  if (!name) return 'Action ran';
  const parts = name.toLowerCase().split(/[_\s-]+/).filter(Boolean);
  if (!parts.length) return 'Action ran';
  const pastTense = {
    add: 'added', create: 'created', send: 'sent', delete: 'deleted',
    remove: 'removed', update: 'updated', set: 'set', get: 'fetched',
    fetch: 'fetched', search: 'searched', schedule: 'scheduled',
    cancel: 'cancelled', book: 'booked', open: 'opened', close: 'closed',
    list: 'listed', find: 'found', read: 'read', write: 'wrote',
    edit: 'edited', save: 'saved', play: 'played', pause: 'paused',
    stop: 'stopped', start: 'started',
  };
  const titleCase = (s) => s.charAt(0).toUpperCase() + s.slice(1);
  const verb = parts[0];
  const rest = parts.slice(1).join(' ');
  if (rest && pastTense[verb] !== undefined) {
    return `${titleCase(rest)} ${pastTense[verb]}`;
  }
  return parts.map(titleCase).join(' ');
}

function actionIcon(name) {
  const n = (name || '').toLowerCase();
  if (n.includes('list')) return '📋';
  if (n.includes('task') || n.includes('todo')) return '✅';
  if (n.includes('email') || n.includes('mail')) return '✉️';
  if (n.includes('calendar') || n.includes('event') || n.includes('meeting')) return '📅';
  if (n.includes('alarm') || n.includes('timer') || n.includes('reminder')) return '⏰';
  if (n.includes('message') || n.includes('chat') || n.includes('text')) return '💬';
  if (n.includes('file') || n.includes('doc')) return '📄';
  if (n.includes('search') || n.includes('find')) return '🔍';
  if (n.includes('delete') || n.includes('remove') || n.includes('cancel')) return '🗑️';
  if (n.includes('call') || n.includes('phone')) return '📞';
  if (n.includes('note')) return '📝';
  return '✓';
}

function formatActionValue(_key, value) {
  if (value === null || value === undefined || value === '') return null;
  if (typeof value === 'boolean') return value ? 'Yes' : 'No';
  if (typeof value === 'string') {
    if (/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}/.test(value)) {
      const d = new Date(value);
      if (!isNaN(d)) {
        const hasTime = !/T00:00:00/.test(value);
        const dateOpts = { month: 'short', day: 'numeric', year: 'numeric' };
        const timeOpts = { hour: 'numeric', minute: '2-digit' };
        return hasTime
          ? `${d.toLocaleDateString(undefined, dateOpts)} · ${d.toLocaleTimeString(undefined, timeOpts)}`
          : d.toLocaleDateString(undefined, dateOpts);
      }
    }
    if (/^\d{4}-\d{2}-\d{2}$/.test(value)) {
      const d = new Date(value);
      if (!isNaN(d)) return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' });
    }
    return value;
  }
  if (Array.isArray(value)) {
    if (!value.length) return null;
    return value.map(v => (typeof v === 'object' ? JSON.stringify(v) : String(v))).join(', ');
  }
  if (typeof value === 'object') return JSON.stringify(value);
  return String(value);
}

function labelKey(k) {
  return k.replace(/[_-]+/g, ' ').replace(/\b\w/g, c => c.toUpperCase());
}

function renderActionCard(a) {
  const card = document.createElement('div');
  card.className = 'action-card';

  const headline = humanizeAction(a.name);
  const icon = actionIcon(a.name);

  const rows = [];
  const argEntries = Object.entries(a.args || {});
  const primaryKeys = ['title', 'name', 'subject', 'summary', 'item', 'list'];
  argEntries.sort((a, b) => {
    const ai = primaryKeys.indexOf(a[0]);
    const bi = primaryKeys.indexOf(b[0]);
    if (ai === -1 && bi === -1) return 0;
    if (ai === -1) return 1;
    if (bi === -1) return -1;
    return ai - bi;
  });

  for (const [k, v] of argEntries) {
    const formatted = formatActionValue(k, v);
    if (formatted === null) continue;
    rows.push({ key: labelKey(k), value: formatted });
  }

  const hasArgs = rows.length > 0;
  const argsJSON = hasArgs ? JSON.stringify(a.args, null, 2) : '';

  card.innerHTML = `
    <div class="action-card-header">
      <span class="action-card-icon">${icon}</span>
      <div class="action-card-titles">
        <div class="action-card-title">${escapeHTML(headline)}</div>
        <div class="action-card-meta">${escapeHTML(a.name || '')}</div>
      </div>
      ${hasArgs ? '<button type="button" class="action-card-toggle" aria-label="Show details">▾</button>' : ''}
    </div>
    ${hasArgs ? `
      <div class="action-card-body">
        <dl class="action-kv">
          ${rows.map(r => `
            <div class="action-kv-row">
              <dt>${escapeHTML(r.key)}</dt>
              <dd>${escapeHTML(r.value)}</dd>
            </div>
          `).join('')}
        </dl>
        <details class="action-raw">
          <summary>Raw JSON</summary>
          <pre>${escapeHTML(argsJSON)}</pre>
        </details>
      </div>
    ` : ''}
  `;

  if (hasArgs) {
    const toggle = card.querySelector('.action-card-toggle');
    const body = card.querySelector('.action-card-body');
    body.style.display = 'none';
    toggle.onclick = () => {
      const shown = body.style.display !== 'none';
      body.style.display = shown ? 'none' : 'block';
      toggle.textContent = shown ? '▾' : '▴';
      toggle.setAttribute('aria-label', shown ? 'Show details' : 'Hide details');
    };
  }

  return card;
}

// ============================================================
//  RESEARCH PANEL
// ============================================================
function hostname(u) {
  try { return new URL(u).hostname.replace(/^www\./, ''); } catch { return u; }
}

function setStepState(step, state, html) {
  if (!step) return;
  step.className = 'research-step ' + state;
  if (html !== undefined) step.querySelector('.step-body').innerHTML = html;
}

// ============================================================
//  EVENTS & PERSISTENCE
// ============================================================
$('newChatBtn').onclick         = () => startNewSession();
$('menuToggle').onclick         = toggleSidebar;
$('sidebarGhostBtn').onclick    = toggleGhost;
$('dockGhostBtn').onclick       = toggleGhost;
$('ghostExitBtn').onclick       = toggleGhost;
sidebarBackdrop.onclick         = closeSidebar;
$('chatForm').onsubmit          = sendMessage;

// ── Stickman ──
const stickmanEl = $('stickman');
function setStickmanState(state) {
  if (stickmanEl) stickmanEl.dataset.state = state;
}
function setStickmanThinking(on) {
  if (stickmanEl) stickmanEl.classList.toggle('sm-thinking', on);
}

// ── Think mode ──
let thinkMode = false;
function getThinkEnabled() { return thinkMode; }

function updateThinkUI() {
  const toggle = $('modeToggle');
  if (toggle) toggle.setAttribute('aria-checked', thinkMode ? 'true' : 'false');
  document.body.classList.toggle('think-mode', thinkMode);
}

async function toggleThinkMode() {
  if (projectMode) return; // disabled in workspace
  thinkMode = !thinkMode;
  updateThinkUI();
  // Show loading overlay while swapping model
  const mode = thinkMode ? 'think' : 'normal';
  modelWakeOverlay.hidden = false;
  try {
    await fetch('/model/swap', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ mode }),
    });
  } catch (e) {
    console.warn('model swap failed', e);
  } finally {
    modelWakeOverlay.hidden = true;
  }
}

const modeToggleEl = $('modeToggle');
if (modeToggleEl) {
  modeToggleEl.addEventListener('click', toggleThinkMode);
  modeToggleEl.addEventListener('keydown', e => {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggleThinkMode(); }
  });
}

// Click outside closes blob panels
document.addEventListener('click', (e) => {
  if (!e.target.closest('.blob-bar')) {
    document.querySelectorAll('.blob-panel.open').forEach(p => p.classList.remove('open'));
    document.querySelectorAll('.blob-pill.active').forEach(p => p.classList.remove('active'));
    // Reset user-opened flag so auto-open can resume
    document.querySelectorAll('.blob-bar').forEach(bar => {
      if (bar._blobBarRef) bar._blobBarRef._userOpened = false;
    });
  }
  // Close session 3-dot menus
  if (!e.target.closest('.session-menu-wrap')) {
    document.querySelectorAll('.session-dropdown.open').forEach(d => d.classList.remove('open'));
  }
});

// Landing page — chip suggestions fill the input
const landingPage          = $('landingPage');
const workspaceLandingPage = $('workspaceLandingPage');

function showLanding(workspaceMode) {
  if (workspaceMode) {
    landingPage.hidden = true;
    workspaceLandingPage.hidden = false;
  } else {
    landingPage.hidden = false;
    workspaceLandingPage.hidden = true;
  }
}
function hideLanding() {
  landingPage.hidden = true;
  workspaceLandingPage.hidden = true;
}
document.querySelectorAll('.landing-chip').forEach(chip => {
  chip.onclick = () => {
    userInput.value = chip.dataset.q;
    userInput.style.height = 'auto';
    userInput.style.height = Math.min(userInput.scrollHeight, 200) + 'px';
    userInput.focus();
  };
});

// Attach menu
$('attachBtn').onclick = (e) => {
  e.stopPropagation();
  const menu = $('attachMenu');
  menu.hidden = !menu.hidden;
};
$('attachLocalBtn').onclick = () => {
  $('attachMenu').hidden = true;
  fileInput.click();
};
$('attachWebBtn').onclick = () => {
  $('attachMenu').hidden = true;
  const bar = $('urlScrapeBar');
  bar.hidden = false;
  $('urlInput').focus();
};
document.addEventListener('click', () => { $('attachMenu').hidden = true; });

// URL scrape bar
fileInput.onchange = handleFileSelect;
$('urlFetchBtn').onclick  = scrapeURL;
$('urlCancelBtn').onclick = () => { $('urlScrapeBar').hidden = true; $('urlInput').value = ''; };
$('urlInput').addEventListener('keydown', (e) => {
  if (e.key === 'Enter') { e.preventDefault(); scrapeURL(); }
  if (e.key === 'Escape') { $('urlScrapeBar').hidden = true; $('urlInput').value = ''; }
});

window.addEventListener('beforeunload', saveCache);
userInput.addEventListener('input', () => {
  userInput.style.height = 'auto';
  userInput.style.height = Math.min(userInput.scrollHeight, 200) + 'px';
});

// On touch devices (Android/iOS) the virtual keyboard's Enter/Return key should
// insert a newline; users send via the send button instead.
const isTouchKeyboard = window.matchMedia('(hover: none) and (pointer: coarse)').matches;
userInput.addEventListener('keydown', (e) => {
  if (!isTouchKeyboard && e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault();
    sendMessage();
  }
});
window.addEventListener('beforeunload', saveCache);

// ============================================================
//  SPEECH TO TEXT  (Web Speech API — free, built into browsers)
// ============================================================
function initSTT() {
  const micBtn = $('micBtn');
  if (!micBtn) return;
  const SpeechRecognition = window.SpeechRecognition || window.webkitSpeechRecognition;
  if (!SpeechRecognition) { micBtn.hidden = true; return; }

  const recognition = new SpeechRecognition();
  recognition.continuous = false;
  recognition.interimResults = true;
  recognition.lang = 'en-US';

  let recording = false;
  let baseText = '';

  recognition.onstart = () => {
    recording = true;
    micBtn.classList.add('recording');
    micBtn.title = 'Stop recording';
    micBtn.setAttribute('aria-label', 'Stop recording');
    baseText = userInput.value;
    if (baseText.length > 0 && !baseText.endsWith(' ')) baseText += ' ';
  };

  recognition.onresult = (evt) => {
    let interim = '', final = '';
    for (let i = evt.resultIndex; i < evt.results.length; i++) {
      evt.results[i].isFinal ? (final += evt.results[i][0].transcript)
                              : (interim += evt.results[i][0].transcript);
    }
    userInput.value = baseText + (final || interim);
    userInput.dispatchEvent(new Event('input'));
  };

  const stopRecording = () => {
    recording = false;
    micBtn.classList.remove('recording');
    micBtn.title = 'Voice input';
    micBtn.setAttribute('aria-label', 'Start voice input');
  };
  recognition.onend = stopRecording;
  recognition.onerror = (e) => { console.warn('STT error:', e.error); stopRecording(); };

  micBtn.addEventListener('click', (e) => {
    e.preventDefault();
    e.stopPropagation();
    if (recording) { recognition.stop(); return; }
    try { recognition.start(); } catch (err) { console.warn('STT start:', err); }
  });
}

// ============================================================
//  TEXT TO SPEECH  (via /tts backend — Piper TTS, free & open)
// ============================================================
let _activeTtsBtn = null;
let _activeTtsAudio = null;

function _detectEmotion(text) {
  const lo = text.toLowerCase();
  const bangs = (text.match(/!/g) || []).length;
  const hasPositive = /\b(great|amazing|excellent|perfect|wonderful|fantastic|awesome|congratulations|brilliant|superb)\b/.test(lo);
  const hasNegative = /\b(sorry|unfortunately|sadly|failed|error|problem|issue|cannot|unable|impossible|apolog)\b/.test(lo);
  const hasQuestion = (text.match(/\?/g) || []).length > 0;
  if (bangs >= 2 || hasPositive) return 'happy';
  if (hasNegative)               return 'sad';
  if (hasQuestion && !bangs)     return 'questioning';
  return 'neutral';
}

function _stripMd(text) {
  return text
    .replace(/```[\s\S]*?```/g, '')
    .replace(/`[^`]+`/g, '')
    .replace(/\*\*(.*?)\*\*/g, '$1')
    .replace(/\*(.*?)\*/g, '$1')
    .replace(/#{1,6}\s+/g, '')
    .replace(/\[([^\]]+)\]\([^)]+\)/g, '$1')
    .replace(/^[-*+]\s+/gm, '')
    .replace(/^\d+\.\s+/gm, '')
    .replace(/\n{2,}/g, '. ')
    .replace(/\n/g, ' ')
    .trim();
}

function _stopTts() {
  if (_activeTtsAudio) {
    _activeTtsAudio.pause();
    _activeTtsAudio.src = '';
    _activeTtsAudio = null;
  }
  if (_activeTtsBtn) {
    _activeTtsBtn.classList.remove('speaking');
    _activeTtsBtn.title = 'Read aloud';
    _activeTtsBtn = null;
  }
}

async function _speakText(rawText, btn) {
  if (_activeTtsBtn === btn) { _stopTts(); return; }
  _stopTts();

  const clean = _stripMd(rawText);
  if (!clean) return;

  btn.classList.add('speaking');
  btn.title = 'Stop reading';
  _activeTtsBtn = btn;

  try {
    const resp = await fetch('/tts', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ text: clean, emotion: _detectEmotion(rawText) }),
    });
    if (!resp.ok) {
      console.warn('TTS error:', await resp.text());
      _stopTts();
      return;
    }
    const blob = await resp.blob();
    const url = URL.createObjectURL(blob);
    const audio = new Audio(url);
    _activeTtsAudio = audio;
    audio.onended = () => { URL.revokeObjectURL(url); _stopTts(); };
    audio.onerror = () => { URL.revokeObjectURL(url); _stopTts(); };
    audio.play();
  } catch (err) {
    console.warn('TTS fetch error:', err);
    _stopTts();
  }
}

function addTtsButton(botDiv, rawText) {
  if (!rawText || rawText.length > 300) return;
  const meta = botDiv.querySelector('.msg-meta');
  if (!meta || meta.querySelector('.tts-btn')) return;

  const btn = document.createElement('button');
  btn.className = 'tts-btn';
  btn.type = 'button';
  btn.title = 'Read aloud';
  btn.setAttribute('aria-label', 'Read aloud');
  btn.innerHTML = `<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polygon points="11 5 6 9 2 9 2 15 6 15 11 19 11 5"/><path d="M15.54 8.46a5 5 0 0 1 0 7.07"/><path d="M19.07 4.93a10 10 0 0 1 0 14.14"/></svg>`;
  btn.addEventListener('click', (e) => { e.stopPropagation(); _speakText(rawText, btn); });
  meta.appendChild(btn);
}

// ============================================================
//  SERVER STREAM RECONNECT
// ============================================================

/**
 * Connects to GET /chat/stream/{sid} and replays the server-side NDJSON
 * stream into the UI, just like sendMessage does for a live POST /chat/web.
 * Called only when switchSession has already confirmed the server has an
 * active stream and there is no local activeStream entry for this session.
 */
async function reconnectToServerStream(sid) {
  if (!sid || activeStreams.has(sid)) return;

  // Build a minimal bot message placeholder.
  const botDiv = document.createElement('div');
  botDiv.className = 'message bot-message';
  botDiv.appendChild(createMsgMeta('Ed', new Date()));
  const statusWrap = document.createElement('div');
  statusWrap.className = 'status-wrapper';
  statusWrap.innerHTML = `<div class="status-dots"><div class="dot"></div><div class="dot"></div><div class="dot"></div></div><div class="status-text">Resuming response…</div>`;
  botDiv.appendChild(statusWrap);

  if (sid === currentSessionID) chatContainer.appendChild(botDiv);

  const abortController = new AbortController();
  const streamState = {
    accumulated: '',
    // Mark started=true immediately: this is a reconnect, not a pre-token
    // pending state. Prevents stopCurrentStream → handlePreTokenAbandon.
    started: true,
    sources: null,
    actions: [],
    botDiv,
    userDiv: null,
    isDone: false,
    sessionId: sid,
    userQuery: null,
    attachedFilesSnapshot: [],
    abortController,
    wasAbandoned: false,
  };
  activeStreams.set(sid, streamState);

  try {
    const resp = await fetch(`/chat/stream/${sid}`, { signal: abortController.signal });
    if (!resp.ok || !resp.body) throw new Error(`HTTP ${resp.status}`);

    // Ensure botDiv is in the DOM if session became current during the fetch.
    if (sid === currentSessionID && !chatContainer.contains(botDiv)) {
      chatContainer.appendChild(botDiv);
    }

    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buf = '';

    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let nl;
      while ((nl = buf.indexOf('\n')) >= 0) {
        const line = buf.slice(0, nl).trim();
        buf = buf.slice(nl + 1);
        if (!line) continue;
        let ev;
        try { ev = JSON.parse(line); } catch { continue; }

        if (ev.type === 'meta') {
          if (ev.meta?.stage) {
            const statusOnlyStages = new Set(['model_loading', 'model_loaded', 'history_compressing', 'history_compress_done', 'context_usage', 'context_compacting', 'context_compacted']);
            if (!statusOnlyStages.has(ev.meta.stage)) {
              if (!streamState.blobBar) {
                streamState.blobBar = createBlobBar(streamState.botDiv);
                const status = streamState.botDiv.querySelector('.status-wrapper');
                if (status) { status.classList.add('vanishing'); setTimeout(() => status.remove(), 350); }
              }
              handleBlobStage(streamState.blobBar, ev.meta, streamState);
            }
          }
          if (ev.meta?.sources) streamState.sources = ev.meta.sources;
          if (ev.meta?.context_usage && sid === currentSessionID) updateContextChart(ev.meta.context_usage);
        } else if (ev.type === 'think_token') {
          if (sid === currentSessionID) { setStickmanState('typing'); setStickmanThinking(true); }
          if (!streamState.blobBar) {
            streamState.blobBar = createBlobBar(streamState.botDiv);
            const status = streamState.botDiv.querySelector('.status-wrapper');
            if (status) { status.classList.add('vanishing'); setTimeout(() => status.remove(), 350); }
          }
          if (!streamState.thinkBody) {
            showBlob(streamState.blobBar, 'thought');
            setActiveDots(streamState.blobBar, 'thought', true);
            openBlob(streamState.blobBar, 'thought');
            streamState.thinkBody = streamState.blobBar.blobs.thought.body;
            streamState.thinkText = '';
            streamState.thinkStartTime = Date.now();
          }
          streamState.thinkText += ev.content || '';
          if (sid === currentSessionID && !streamState._thinkRafPending) {
            streamState._thinkRafPending = requestAnimationFrame(() => {
              streamState._thinkRafPending = null;
              if (streamState.thinkBody) {
                streamState.thinkBody.innerHTML = renderMarkdown(streamState.thinkText);
                blobBodyScroll(streamState.thinkBody);
              }
            });
          }
        } else if (ev.type === 'token') {
          // Thinking done once regular tokens arrive
          if (streamState.thinkBody && sid === currentSessionID) {
            setActiveDots(streamState.blobBar, 'thought', false);
            setStickmanThinking(false);
            const elapsed = Math.round((Date.now() - (streamState.thinkStartTime || Date.now())) / 1000);
            if (elapsed > 0) {
              streamState.blobBar.blobs.thought.pill.querySelector('.blob-label').textContent = `Thought · ${elapsed}s`;
            }
            closeAllBlobs(streamState.blobBar.blobs);
          }
          // Remove status wrapper on first token.
          const status = streamState.botDiv.querySelector('.status-wrapper');
          if (status) { status.classList.add('vanishing'); setTimeout(() => status.remove(), 350); }
          if (!streamState.botDiv.querySelector('.bot-content')) {
            const c = document.createElement('div');
            c.className = 'bot-content';
            streamState.botDiv.appendChild(c);
          }
          streamState.accumulated += ev.content || '';
          if (sid === currentSessionID && !streamState._rafPending) {
            streamState._rafPending = requestAnimationFrame(() => {
              streamState._rafPending = null;
              const c2 = streamState.botDiv.querySelector('.bot-content');
              if (c2) { c2.innerHTML = renderMarkdown(streamState.accumulated); appendSources(streamState.botDiv, streamState.sources); }
              autoScroll();
            });
          }
        } else if (ev.type === 'error') {
          const errBox = document.createElement('div');
          errBox.className = 'error-text';
          errBox.textContent = 'Error: ' + (ev.error || 'unknown');
          streamState.botDiv.appendChild(errBox);
        } else if (ev.type === 'done') {
          if (streamState.blobBar) finalizeBlobBar(streamState.blobBar);
          const status = streamState.botDiv.querySelector('.status-wrapper');
          if (status) status.remove();
          const c = streamState.botDiv.querySelector('.bot-content');
          if (c) c.innerHTML = renderMarkdown(streamState.accumulated);
          appendSources(streamState.botDiv, streamState.sources);
          addTtsButton(streamState.botDiv, streamState.accumulated);
          _tryAutoSpeak(streamState.botDiv, streamState.accumulated);
          if (sid === currentSessionID) {
            setStickmanThinking(false);
            setStickmanState('idle');
          }
        }
        if (sid === currentSessionID) autoScroll();
      }
    }
  } catch (err) {
    if (err.name !== 'AbortError' && sid === currentSessionID) {
      streamState.botDiv.innerHTML = `<span class="error-text">Error reconnecting: ${escapeHTML(err.message)}</span>`;
    }
  } finally {
    streamState.isDone = true;
    activeStreams.delete(sid);
    if (sid === currentSessionID) {
      inFlight = false;
      setSendBtnMode('send');
      setStickmanThinking(false);
      setStickmanState('idle');
      loadHistory();
    }
  }
}

// ============================================================
//  ROUTINES — backend-scheduled, runs even when browser is closed
// ============================================================
let _rtnCache = [];           // local cache of routines from server
let _rtnCustomTimes = [];     // temp state for the form's custom-times list

async function fetchRoutines() {
  try {
    const r = await fetch('/routines');
    if (r.ok) _rtnCache = await r.json();
  } catch {}
  return _rtnCache;
}
async function apiAddRoutine(routine) {
  await fetch('/routines', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(routine) });
  await fetchRoutines();
}
async function apiDeleteRoutine(id) {
  await fetch('/routines/' + id, { method: 'DELETE' });
  await fetchRoutines();
}
async function apiToggleRoutine(id, paused) {
  await fetch('/routines/' + id, { method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ paused }) });
  await fetchRoutines();
}

// ── Notification helper (in-app toast + browser notification) ──
function triggerNotification(title, body) {
  const toast = document.createElement('div');
  toast.className = 'rtn-toast';
  toast.innerHTML = `<div class="toast-title">${escapeHTML(title)}</div><div class="toast-body">${escapeHTML(body)}</div>`;
  document.body.appendChild(toast);
  setTimeout(() => { toast.style.opacity = '0'; toast.style.transition = 'opacity 0.3s'; setTimeout(() => toast.remove(), 350); }, 6000);
  if ('Notification' in window && Notification.permission === 'granted') {
    new Notification(title, { body, icon: 'Icon-192.png' });
  }
}
if ('Notification' in window && Notification.permission === 'default') {
  Notification.requestPermission();
}

// ── Web Push via Service Worker (works when browser/tab is closed) ──
function urlBase64ToUint8Array(base64String) {
  const padding = '='.repeat((4 - base64String.length % 4) % 4);
  const base64 = (base64String + padding).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(base64);
  const arr = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) arr[i] = raw.charCodeAt(i);
  return arr;
}

const PUSH_ENDPOINT_KEY = 'push_registered_endpoint';

async function initPushNotifications() {
  if (!('serviceWorker' in navigator) || !('PushManager' in window)) return;
  try {
    const reg = await navigator.serviceWorker.register('/sw.js');
    await navigator.serviceWorker.ready;

    const resp = await fetch('/push/vapid-key');
    if (!resp.ok) return;
    const { publicKey } = await resp.json();
    const vapidKey = urlBase64ToUint8Array(publicKey);

    // Get existing subscription or create new one.
    let sub = await reg.pushManager.getSubscription();
    if (!sub) {
      sub = await reg.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: vapidKey
      });
    }

    // Only re-register if the endpoint changed (new subscription or server restart).
    const lastEndpoint = localStorage.getItem(PUSH_ENDPOINT_KEY) || '';
    if (sub.endpoint !== lastEndpoint) {
      await fetch('/push/subscribe', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(sub.toJSON())
      });
      localStorage.setItem(PUSH_ENDPOINT_KEY, sub.endpoint);
      console.log('[push] subscription registered');
    }
  } catch (e) {
    console.warn('[push] init failed:', e);
  }
}
initPushNotifications();

// ── Poll backend for routine notifications (model answers) ──
async function pollRoutineNotifications() {
  try {
    const r = await fetch('/routines/notifications');
    if (!r.ok) return;
    const notifs = await r.json();
    for (const n of notifs) {
      triggerNotification(n.title, n.body);
    }
    // Refresh list if notifications came in (status may have changed)
    if (notifs.length > 0) {
      await fetchRoutines();
      renderRoutinesList();
    }
  } catch {}
}
setInterval(pollRoutineNotifications, 15000);

// ── Page open / close ──
async function openRoutinesPage() {
  $('routinesPage').hidden = false;
  await fetchRoutines();
  renderRoutinesList();
  resetRoutineForm();
}
function closeRoutinesPage() {
  $('routinesPage').hidden = true;
}

// ── Render list ──
function routineSchedLabel(r) {
  const DAYS = ['Sun','Mon','Tue','Wed','Thu','Fri','Sat'];
  if (r.freq === 'once') return `Once on ${r.onceDate} at ${r.onceTime}`;
  switch (r.repeat) {
    case 'annually': return `Annually on ${r.annualDate} at ${r.annualTime}`;
    case 'monthly':  return `Monthly on day ${r.monthDay} at ${r.monthlyTime}`;
    case 'weekly':   return `Weekly on ${(r.weekDays||[]).map(d => DAYS[d]).join(', ')} at ${r.weeklyTime}`;
    case 'daily':
      if (r.dailyMode === 'repeat_after') return `Every ${r.repeatHours}h (${r.repeatStart}–${r.repeatEnd})`;
      if (r.dailyMode === 'custom_times') return `Daily at ${(r.customTimes||[]).join(', ')}`;
  }
  return 'Scheduled';
}

function routineCategory(r) {
  if (r.freq === 'once') return 'once';
  return r.repeat || 'daily';
}

const RTN_CAT_ORDER = ['daily', 'weekly', 'monthly', 'annually', 'once'];
const RTN_CAT_LABELS = { daily: 'Daily', weekly: 'Weekly', monthly: 'Monthly', annually: 'Annually', once: 'One-Time' };

function renderRoutinesList() {
  const container = $('routinesList');
  const routines = _rtnCache;
  container.innerHTML = '';
  if (!routines.length) return;

  const groups = {};
  routines.forEach((r) => {
    const cat = routineCategory(r);
    if (!groups[cat]) groups[cat] = [];
    groups[cat].push(r);
  });

  RTN_CAT_ORDER.forEach(cat => {
    const items = groups[cat];
    if (!items || !items.length) return;

    const section = document.createElement('div');
    section.className = 'rtn-acc-section open';
    section.innerHTML = `
      <div class="rtn-acc-header">
        <span class="rtn-acc-arrow">▶</span>
        <span class="rtn-acc-label">${RTN_CAT_LABELS[cat]}</span>
        <span class="rtn-acc-count">${items.length}</span>
      </div>
      <div class="rtn-acc-body"></div>`;

    section.querySelector('.rtn-acc-header').onclick = () => section.classList.toggle('open');

    const body = section.querySelector('.rtn-acc-body');
    items.forEach((r) => {
      const card = document.createElement('div');
      card.className = 'rtn-card';

      let lastTime = 'Never';
      if (r.lastTriggered > 0) {
        const d = new Date(r.lastTriggered);
        lastTime = d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }) + ' at ' + d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
      }
      const status = r.lastStatus || (r.lastTriggered > 0 ? 'ok' : 'never');
      const statusLabel = { ok: 'Completed', failed: 'Failed', skipped: 'Skipped (busy)', never: 'Awaiting first run' }[status] || status;

      card.innerHTML = `
        <div class="rtn-card-top">
          <div class="rtn-card-body">
            <div class="rtn-card-name">${escapeHTML(r.name)}</div>
            <div class="rtn-card-sched">${escapeHTML(routineSchedLabel(r))}</div>
          </div>
          <div class="rtn-card-actions">
            <button class="rtn-card-btn ${r.paused ? 'paused' : ''}" data-act="toggle" data-id="${r.id}" data-paused="${r.paused ? '1' : '0'}" title="${r.paused ? 'Resume' : 'Pause'}">${r.paused ? '▶' : '⏸'}</button>
            <button class="rtn-card-btn danger" data-act="del" data-id="${r.id}" title="Delete">✕</button>
          </div>
        </div>
        ${r.prompt ? `<div class="rtn-card-prompt">${escapeHTML(r.prompt)}</div>` : ''}
        <div class="rtn-card-detail">
          <div class="rtn-detail-row">
            <span class="rtn-detail-label">Last run</span>
            <span class="rtn-detail-val">${escapeHTML(lastTime)}</span>
          </div>
          <div class="rtn-detail-row">
            <span class="rtn-detail-label">Status</span>
            <span class="rtn-status-dot ${status}"></span>
            <span class="rtn-detail-val">${escapeHTML(statusLabel)}</span>
          </div>
        </div>`;
      body.appendChild(card);
    });

    container.appendChild(section);
  });

  container.onclick = async (e) => {
    const btn = e.target.closest('[data-act]');
    if (btn) {
      e.stopPropagation();
      const id = btn.dataset.id;
      if (btn.dataset.act === 'toggle') {
        await apiToggleRoutine(id, btn.dataset.paused === '0');
      } else if (btn.dataset.act === 'del') {
        await apiDeleteRoutine(id);
      }
      renderRoutinesList();
      return;
    }
    const card = e.target.closest('.rtn-card');
    if (card) card.classList.toggle('expanded');
  };
}

// ── Form helpers ──
function wireRadioGroup(containerId, onChange) {
  const el = $(containerId);
  if (!el) return;
  el.onclick = (e) => {
    const btn = e.target.closest('.rtn-radio');
    if (!btn) return;
    el.querySelectorAll('.rtn-radio').forEach(b => b.classList.remove('active'));
    btn.classList.add('active');
    if (onChange) onChange(btn);
  };
}
function activeRadioVal(containerId, attr) {
  const el = $(containerId);
  const btn = el?.querySelector('.rtn-radio.active');
  return btn ? btn.dataset[attr] : '';
}

function updateFormVisibility() {
  const freq = activeRadioVal('rtFreqGroup', 'freq');
  $('rtRecurringBlock').hidden = freq !== 'recurring';
  $('rtOnceBlock').hidden = freq !== 'once';
  if (freq !== 'recurring') return;
  const repeat = activeRadioVal('rtRepeatGroup', 'repeat');
  $('rtDailyBlock').hidden   = repeat !== 'daily';
  $('rtWeeklyBlock').hidden  = repeat !== 'weekly';
  $('rtMonthlyBlock').hidden = repeat !== 'monthly';
  $('rtAnnualBlock').hidden  = repeat !== 'annually';
  if (repeat === 'daily') {
    const dm = activeRadioVal('rtDailyModeGroup', 'dmode');
    $('rtRepeatAfterBlock').hidden = dm !== 'repeat_after';
    $('rtCustomTimesBlock').hidden = dm !== 'custom_times';
  }
}

function renderCustomTimes() {
  const list = $('rtCustomTimesList');
  list.innerHTML = '';
  _rtnCustomTimes.forEach((t, i) => {
    const chip = document.createElement('span');
    chip.className = 'rtn-time-chip';
    chip.innerHTML = `${escapeHTML(t)}<button class="rtn-time-chip-del" data-i="${i}">✕</button>`;
    list.appendChild(chip);
  });
  list.onclick = (e) => {
    const btn = e.target.closest('.rtn-time-chip-del');
    if (!btn) return;
    _rtnCustomTimes.splice(+btn.dataset.i, 1);
    renderCustomTimes();
  };
}

function setupDayPicker() {
  $('rtDayPicker').onclick = (e) => {
    const btn = e.target.closest('.rtn-day-btn');
    if (!btn) return;
    btn.classList.toggle('active');
  };
}

function resetRoutineForm() {
  $('rtName').value = '';
  $('rtPrompt').value = '';
  _rtnCustomTimes = [];
  renderCustomTimes();
  const resetGroup = (id, idx) => { const btns = $(id).querySelectorAll('.rtn-radio'); btns.forEach(b => b.classList.remove('active')); if (btns[idx]) btns[idx].classList.add('active'); };
  resetGroup('rtFreqGroup', 0);
  resetGroup('rtRepeatGroup', 0);
  resetGroup('rtDailyModeGroup', 0);
  $('rtDayPicker').querySelectorAll('.rtn-day-btn').forEach(b => b.classList.remove('active'));
  updateFormVisibility();
}

async function saveNewRoutine() {
  const name = $('rtName').value.trim();
  if (!name) { $('rtName').focus(); return; }
  const prompt = $('rtPrompt').value.trim();
  const freq = activeRadioVal('rtFreqGroup', 'freq');

  const routine = {
    id: Date.now().toString(36) + Math.random().toString(36).slice(2, 6),
    name, prompt, freq, paused: false, lastTriggered: 0,
  };

  if (freq === 'once') {
    routine.onceDate = $('rtOnceDate').value;
    routine.onceTime = $('rtOnceTime').value || '10:00';
    if (!routine.onceDate) { $('rtOnceDate').focus(); return; }
  } else {
    routine.repeat = activeRadioVal('rtRepeatGroup', 'repeat');
    switch (routine.repeat) {
      case 'daily':
        routine.dailyMode = activeRadioVal('rtDailyModeGroup', 'dmode');
        if (routine.dailyMode === 'repeat_after') {
          routine.repeatHours = parseInt($('rtRepeatHours').value) || 2;
          routine.repeatStart = $('rtRepeatStart').value || '08:00';
          routine.repeatEnd   = $('rtRepeatEnd').value || '22:00';
        } else {
          routine.customTimes = [..._rtnCustomTimes];
          if (!routine.customTimes.length) { $('rtCustomTimeInput').focus(); return; }
        }
        break;
      case 'weekly':
        routine.weekDays = [...$('rtDayPicker').querySelectorAll('.rtn-day-btn.active')].map(b => +b.dataset.day);
        routine.weeklyTime = $('rtWeeklyTime').value || '10:00';
        if (!routine.weekDays.length) return;
        break;
      case 'monthly':
        routine.monthDay = parseInt($('rtMonthDay').value) || 1;
        routine.monthlyTime = $('rtMonthlyTime').value || '10:00';
        break;
      case 'annually':
        routine.annualDate = $('rtAnnualDate').value;
        routine.annualTime = $('rtAnnualTime').value || '10:00';
        if (!routine.annualDate) { $('rtAnnualDate').focus(); return; }
        break;
    }
  }

  await apiAddRoutine(routine);
  renderRoutinesList();
  resetRoutineForm();
}

// ── Wire everything up ──
function initRoutines() {
  $('sidebarRoutinesBtn').onclick = () => { closeSidebar(); openRoutinesPage(); };
  $('routinesCloseBtn').onclick = closeRoutinesPage;
  $('routineAddBtn').onclick = saveNewRoutine;

  wireRadioGroup('rtFreqGroup', updateFormVisibility);
  wireRadioGroup('rtRepeatGroup', updateFormVisibility);
  wireRadioGroup('rtDailyModeGroup', updateFormVisibility);
  setupDayPicker();

  $('rtAddTimeBtn').onclick = () => {
    const v = $('rtCustomTimeInput').value;
    if (v && !_rtnCustomTimes.includes(v)) { _rtnCustomTimes.push(v); _rtnCustomTimes.sort(); renderCustomTimes(); }
  };

  updateFormVisibility();
}
initRoutines();

window.addEventListener('DOMContentLoaded', async () => {
  initSTT();

  // Restore temp session from previous page load instead of deleting it.
  // Temp sessions persist across reloads — only deleted when user starts a new chat
  // or explicitly discards.
  if (tempSessionId) {
    currentSessionID = tempSessionId;
    persistCurrentSessionId();
    hideLanding();
    await loadHistory();
    updateSaveBtn();
    // Load the session's messages into the chat view
    await switchSession(tempSessionId);
  } else {
    currentSessionID = '';
    persistCurrentSessionId();
    showLanding(false);
    await loadHistory();
    updateSaveBtn();
  }
  setSendBtnMode('send');
});
