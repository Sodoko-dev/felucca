/* ============================================================
   HEARTH CONSOLE — app.js  (v2)
   Pure vanilla JS SPA. No build step, no dependencies.
   Polls /api/v1/* every 3s; falls back to mock data gracefully.
   ============================================================ */

'use strict';

/* --- Constants -------------------------------------------- */
const POLL_INTERVAL_MS = 3000;
const API_BASE = '/api/v1';

const STATE_COLORS = {
  running:  '#22c55e',
  creating: '#fbbf24',
  paused:   '#60a5fa',
  sleeping: '#818cf8',   /* v2: indigo/violet — cool, distinct from green running */
  stopped:  '#555560',
  error:    '#f87171',
};

/* --- Bearer token ----------------------------------------- */
/* Read ?token= on boot, store in localStorage, strip from URL */
(function initToken() {
  const params = new URLSearchParams(window.location.search);
  const t = params.get('token');
  if (t) {
    localStorage.setItem('hearth_token', t);
    params.delete('token');
    const newSearch = params.toString();
    const newUrl = window.location.pathname + (newSearch ? '?' + newSearch : '') + window.location.hash;
    history.replaceState(null, '', newUrl);
  }
})();

function getToken() {
  return localStorage.getItem('hearth_token') || null;
}

function setToken(t) {
  if (t) localStorage.setItem('hearth_token', t);
  else localStorage.removeItem('hearth_token');
}

function authHeaders() {
  const t = getToken();
  const h = { 'Content-Type': 'application/json' };
  if (t) h['Authorization'] = `Bearer ${t}`;
  return h;
}

/* Show/hide the 401 auth banner */
function show401Banner() {
  let banner = document.getElementById('auth-banner');
  if (banner) return; // already shown
  banner = document.createElement('div');
  banner.id = 'auth-banner';
  banner.className = 'auth-banner';
  banner.innerHTML = `
    <span class="auth-banner-msg">401 Unauthorized — bearer token required</span>
    <input class="auth-banner-input" id="auth-token-input" type="password"
           placeholder="Paste token here…" autocomplete="off" spellcheck="false"/>
    <button class="auth-banner-btn" id="auth-token-apply">Apply</button>
    <button class="auth-banner-dismiss" id="auth-banner-dismiss">${SVG.close}</button>
  `;
  document.body.insertBefore(banner, document.getElementById('app'));
  document.getElementById('auth-token-apply').addEventListener('click', () => {
    const val = document.getElementById('auth-token-input').value.trim();
    if (!val) return;
    setToken(val);
    banner.remove();
    poll(); // retry immediately
  });
  document.getElementById('auth-banner-dismiss').addEventListener('click', () => banner.remove());
}

/* --- Mock data -------------------------------------------- */
const MOCK_NODES = [
  {
    id: 'nd-01f2a3b4',
    hostname: 'kata-lab-0',
    addr: '192.168.105.2',
    cpus: 8,
    mem_total_mib: 16384,
    mem_free_mib: 9200,
    vm_count: 5,
    status: 'ready',
    last_heartbeat: new Date(Date.now() - 4000).toISOString(),
  },
  {
    id: 'nd-02c4d5e6',
    hostname: 'kata-lab-1',
    addr: '192.168.105.3',
    cpus: 8,
    mem_total_mib: 16384,
    mem_free_mib: 14100,
    vm_count: 2,
    status: 'ready',
    last_heartbeat: new Date(Date.now() - 1500).toISOString(),
  },
  {
    id: 'nd-03f7a8b9',
    hostname: 'bare-metal-0',
    addr: '10.0.0.5',
    cpus: 32,
    mem_total_mib: 131072,
    mem_free_mib: 98000,
    vm_count: 12,
    status: 'ready',
    last_heartbeat: new Date(Date.now() - 800).toISOString(),
  },
  {
    id: 'nd-04e1c2d3',
    hostname: 'kata-lab-2',
    addr: '192.168.105.4',
    cpus: 4,
    mem_total_mib: 8192,
    mem_free_mib: 0,
    vm_count: 0,
    status: 'down',
    last_heartbeat: new Date(Date.now() - 95000).toISOString(),
  },
];

const MOCK_SANDBOXES = [
  {
    id: 'sb-a1b2c3d4',
    name: 'inference-worker-0',
    namespace: 'prod',
    node_id: 'nd-01f2a3b4',
    state: 'running',
    vcpus: 2,
    mem_mib: 2048,
    ip: '10.231.0.10',
    created_at: new Date(Date.now() - 7200000).toISOString(),
    parent_id: null,
  },
  {
    id: 'sb-b2c3d4e5',
    name: 'inference-worker-1',
    namespace: 'prod',
    node_id: 'nd-01f2a3b4',
    state: 'running',
    vcpus: 2,
    mem_mib: 2048,
    ip: '10.231.0.11',
    created_at: new Date(Date.now() - 6800000).toISOString(),
    parent_id: 'sb-a1b2c3d4',
  },
  {
    id: 'sb-c3d4e5f6',
    name: 'inference-worker-2',
    namespace: 'prod',
    node_id: 'nd-02c4d5e6',
    state: 'sleeping',          /* v2: sleeping state demo */
    vcpus: 2,
    mem_mib: 2048,
    ip: '10.231.0.12',          /* IP retained from pre-sleep networking */
    created_at: new Date(Date.now() - 6400000).toISOString(),
    parent_id: 'sb-a1b2c3d4',
  },
  {
    id: 'sb-d4e5f6a7',
    name: 'fine-tune-job-llm',
    namespace: 'experiments',
    node_id: 'nd-03f7a8b9',
    state: 'running',
    vcpus: 4,
    mem_mib: 8192,
    ip: '10.231.0.20',
    created_at: new Date(Date.now() - 3600000).toISOString(),
    parent_id: null,
  },
  {
    id: 'sb-e5f6a7b8',
    name: 'fine-tune-branch-a',
    namespace: 'experiments',
    node_id: 'nd-03f7a8b9',
    state: 'running',
    vcpus: 4,
    mem_mib: 8192,
    ip: '10.231.0.21',
    created_at: new Date(Date.now() - 1800000).toISOString(),
    parent_id: 'sb-d4e5f6a7',
  },
  {
    id: 'sb-f6a7b8c9',
    name: 'fine-tune-branch-b',
    namespace: 'experiments',
    node_id: 'nd-03f7a8b9',
    state: 'paused',
    vcpus: 4,
    mem_mib: 8192,
    ip: '10.231.0.22',
    created_at: new Date(Date.now() - 1500000).toISOString(),
    parent_id: 'sb-d4e5f6a7',
  },
  {
    id: 'sb-a7b8c9d0',
    name: 'code-exec-agent-01',
    namespace: 'agents',
    node_id: 'nd-01f2a3b4',
    state: 'creating',
    vcpus: 1,
    mem_mib: 512,
    ip: null,
    created_at: new Date(Date.now() - 12000).toISOString(),
    parent_id: null,
  },
  {
    id: 'sb-b8c9d0e1',
    name: 'code-exec-agent-02',
    namespace: 'agents',
    node_id: 'nd-02c4d5e6',
    state: 'stopped',
    vcpus: 1,
    mem_mib: 512,
    ip: null,
    created_at: new Date(Date.now() - 86400000).toISOString(),
    parent_id: null,
  },
  {
    id: 'sb-c9d0e1f2',
    name: 'data-pipeline-etl',
    namespace: 'data',
    node_id: 'nd-03f7a8b9',
    state: 'error',
    vcpus: 2,
    mem_mib: 4096,
    ip: null,
    created_at: new Date(Date.now() - 43200000).toISOString(),
    parent_id: null,
  },
  {
    id: 'sb-d0e1f2a3',
    name: 'staging-api-server',
    namespace: 'staging',
    node_id: 'nd-02c4d5e6',
    state: 'running',
    vcpus: 2,
    mem_mib: 1024,
    ip: '10.231.0.30',
    created_at: new Date(Date.now() - 172800000).toISOString(),
    parent_id: null,
  },
  {
    id: 'sb-e1f2a3b4',
    name: 'ml-checkpoint-snap',
    namespace: 'experiments',
    node_id: 'nd-03f7a8b9',
    state: 'sleeping',          /* v2: second sleeping demo — child of fine-tune-job-llm */
    vcpus: 4,
    mem_mib: 8192,
    ip: '10.231.0.23',
    created_at: new Date(Date.now() - 900000).toISOString(),
    parent_id: 'sb-d4e5f6a7',
  },
];

/* --- State ------------------------------------------------ */
const state = {
  view: 'fleet',
  nodes: [],
  sandboxes: [],
  isMock: false,
  lastPoll: null,
  pollError: null,
  nsFilter: '__all__',
  sbSearch: '',
  sbStateFilter: '__all__',
  sortCol: 'name',
  sortDir: 1,
  treeNs: '__all__',
  loading: true,
};

/* Namespace color index (deterministic hash) */
const nsColorCache = {};
let nsColorIdx = 0;
function getNsBadgeClass(ns) {
  if (nsColorCache[ns] === undefined) {
    nsColorCache[ns] = nsColorIdx % 8;
    nsColorIdx++;
  }
  return `ns-badge-${nsColorCache[ns]}`;
}

/* --- API / fetch with mock fallback ----------------------- */
async function apiFetch(path) {
  const headers = {};
  const t = getToken();
  if (t) headers['Authorization'] = `Bearer ${t}`;
  try {
    const res = await fetch(`${API_BASE}${path}`, {
      headers,
      signal: AbortSignal.timeout(2500),
    });
    if (res.status === 401) { show401Banner(); return { data: null, mock: true }; }
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const data = await res.json();
    state.pollError = null;
    return { data, mock: false };
  } catch (err) {
    return { data: null, mock: true, err };
  }
}

async function apiPost(path, body) {
  if (state.isMock) return { mock: true };
  try {
    const headers = authHeaders();
    const res = await fetch(`${API_BASE}${path}`, {
      method: 'POST',
      headers,
      body: body ? JSON.stringify(body) : undefined,
      signal: AbortSignal.timeout(5000),
    });
    if (res.status === 401) { show401Banner(); return { error: '401 Unauthorized' }; }
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const text = await res.text();
    return { data: text ? JSON.parse(text) : null, mock: false };
  } catch (err) {
    return { error: err.message, mock: false };
  }
}

async function apiDelete(path) {
  if (state.isMock) return { mock: true };
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = `Bearer ${t}`;
    const res = await fetch(`${API_BASE}${path}`, {
      method: 'DELETE',
      headers,
      signal: AbortSignal.timeout(5000),
    });
    if (res.status === 401) { show401Banner(); return { error: '401 Unauthorized' }; }
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    return { ok: true };
  } catch (err) {
    return { error: err.message };
  }
}

/* --- Poll ------------------------------------------------- */
async function poll() {
  const [nodesRes, sbRes] = await Promise.all([
    apiFetch('/nodes'),
    apiFetch('/sandboxes'),
  ]);

  let changed = false;

  if (nodesRes.mock || sbRes.mock) {
    if (!state.isMock) {
      state.isMock = true;
      state.nodes = MOCK_NODES;
      state.sandboxes = MOCK_SANDBOXES;
      changed = true;
      simulateMockActivity();
    }
  } else {
    state.isMock = false;
    const newNodes = nodesRes.data.nodes || [];
    const newSbs   = sbRes.data.sandboxes || [];
    // Heartbeats/memory churn on every agent heartbeat; a full re-render for
    // those replays the card-enter animations (visible flicker). Only
    // re-render on structural changes; volatile fields are patched in place.
    const stripVolatile = ns => ns.map(({ last_heartbeat, mem_free_mib, ...rest }) => rest);
    changed = JSON.stringify(stripVolatile(newNodes)) !== JSON.stringify(stripVolatile(state.nodes)) ||
              JSON.stringify(newSbs) !== JSON.stringify(state.sandboxes);
    state.nodes = newNodes;
    state.sandboxes = newSbs;
  }

  state.lastPoll = Date.now();
  state.loading = false;

  if (changed) {
    render();
  } else {
    updateLiveIndicator();
    updateHeaderStats();
    updateNodeCardsInPlace();
    updateHeartbeats();
  }
}

/* Patch volatile node data (heartbeat age, memory) into the existing DOM
   without a full re-render, so polling never replays entry animations. */
function updateNodeCardsInPlace() {
  state.nodes.forEach(node => {
    const card = document.querySelector(`.node-card[data-node-id="${CSS.escape(node.id)}"]`);
    if (!card) return;
    const memUsed = node.mem_total_mib - node.mem_free_mib;
    const memPct  = node.mem_total_mib > 0 ? (memUsed / node.mem_total_mib) * 100 : 0;
    const memBar  = card.querySelector('.metric-bar-fill.mem');
    if (memBar) {
      memBar.style.width = `${memPct.toFixed(1)}%`;
      memBar.classList.toggle('high', memPct > 85);
    }
    const memText = card.querySelectorAll('.metric-value')[1];
    if (memText) memText.textContent = `${formatBytes(memUsed)} / ${formatBytes(node.mem_total_mib)}`;
    const hbEl = card.querySelector('.node-heartbeat');
    if (hbEl) {
      const hb = formatHeartbeat(node.last_heartbeat);
      hbEl.setAttribute('data-heartbeat', node.last_heartbeat);
      hbEl.className = `node-heartbeat ${hb.cls}`;
      hbEl.textContent = hb.text;
    }
  });
}

/* Simulate live mock activity */
function simulateMockActivity() {
  setInterval(() => {
    if (!state.isMock) return;
    state.nodes.forEach(n => {
      if (n.status === 'ready') {
        n.last_heartbeat = new Date().toISOString();
        n.mem_free_mib = Math.max(512, n.mem_free_mib + (Math.random() - 0.5) * 512 | 0);
      }
    });
    // Occasionally flip creating → running
    state.sandboxes.forEach(sb => {
      if (sb.state === 'creating' && Math.random() > 0.85) {
        sb.state = 'running';
        sb.ip = `10.100.${Math.floor(Math.random()*10)}.${Math.floor(Math.random()*200)+10}`;
      }
    });
    updateHeartbeats();
    if (state.view === 'sandboxes') renderSandboxTableInPlace();
    updateHeaderStats();
  }, 3000);
}

/* --- Namespace helpers ------------------------------------ */
function getNamespaces() {
  const ns = new Set(state.sandboxes.map(s => s.namespace));
  return [...ns].sort();
}

/* --- Time helpers ----------------------------------------- */
/* API sends unix seconds (number); mock data uses ISO strings. */
function toMs(t) {
  if (typeof t === 'string' && /^\d+(\.\d+)?$/.test(t)) t = Number(t); // data-* attrs
  if (typeof t === 'number') return t < 1e12 ? t * 1000 : t;
  return new Date(t).getTime();
}

function formatAge(iso) {
  const delta = (Date.now() - toMs(iso)) / 1000;
  if (delta < 60)       return `${delta|0}s`;
  if (delta < 3600)     return `${(delta/60)|0}m`;
  if (delta < 86400)    return `${(delta/3600)|0}h`;
  return `${(delta/86400)|0}d`;
}

function formatHeartbeat(iso) {
  const delta = (Date.now() - toMs(iso)) / 1000;
  if (delta < 10)  return { text: `${delta|0}s ago`, cls: 'heartbeat-fresh' };
  if (delta < 60)  return { text: `${delta|0}s ago`, cls: 'heartbeat-fresh' };
  if (delta < 300) return { text: `${(delta/60)|0}m ago`, cls: 'heartbeat-stale' };
  return { text: `${(delta/60)|0}m ago`, cls: 'heartbeat-dead' };
}

function formatBytes(mib) {
  if (mib >= 1024) return `${(mib/1024).toFixed(1)} GiB`;
  return `${mib} MiB`;
}

/* --- SVG icon helpers ------------------------------------- */
const SVG = {
  fleet:   `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><rect x="1" y="1" width="6" height="6" rx="1"/><rect x="9" y="1" width="6" height="6" rx="1"/><rect x="1" y="9" width="6" height="6" rx="1"/><rect x="9" y="9" width="6" height="6" rx="1"/></svg>`,
  sandbox: `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><rect x="1" y="3" width="14" height="10" rx="1.5"/><path d="M5 3V2M11 3V2"/><path d="M4 8h8M4 10.5h5"/></svg>`,
  tree:    `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="8" cy="3" r="1.5"/><circle cx="3" cy="13" r="1.5"/><circle cx="13" cy="13" r="1.5"/><path d="M8 4.5v3M8 7.5L3 11.5M8 7.5L13 11.5"/><circle cx="8" cy="9" r="1" fill="currentColor" stroke="none"/></svg>`,
  plus:    `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="2"><path d="M8 3v10M3 8h10"/></svg>`,
  stop:    `<svg viewBox="0 0 16 16" fill="currentColor"><rect x="3" y="3" width="10" height="10" rx="1.5"/></svg>`,
  start:   `<svg viewBox="0 0 16 16" fill="currentColor"><path d="M4 3l9 5-9 5V3z"/></svg>`,
  pause:   `<svg viewBox="0 0 16 16" fill="currentColor"><rect x="3" y="3" width="3.5" height="10" rx="1"/><rect x="9.5" y="3" width="3.5" height="10" rx="1"/></svg>`,
  resume:  `<svg viewBox="0 0 16 16" fill="currentColor"><path d="M4 3l9 5-9 5V3z"/></svg>`,
  fork:    `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="4" cy="4" r="2"/><circle cx="12" cy="4" r="2"/><circle cx="8" cy="13" r="2"/><path d="M4 6v2.5a1 1 0 001 1h6a1 1 0 001-1V6M8 11v-1.5"/></svg>`,
  trash:   `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M2 4h12M5 4V2.5A.5.5 0 015.5 2h5a.5.5 0 01.5.5V4M6 7v5M10 7v5M3 4l1 9.5A.5.5 0 004.5 14h7a.5.5 0 00.5-.5L13 4"/></svg>`,
  terminal:`<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><rect x="1.5" y="2.5" width="13" height="11" rx="1.5"/><path d="M4 6l3 3-3 3M8.5 12h3.5"/></svg>`,
  refresh: `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M13.5 8A5.5 5.5 0 112.5 5.5"/><path d="M1 2l1.5 3.5L6 4"/></svg>`,
  close:   `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 3l10 10M13 3L3 13"/></svg>`,
  /* v2 actions */
  sleep:   `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M11.5 3A6 6 0 014 10.5a6 6 0 006-7.5z"/><path d="M6 2.5h3M7 4.5h2M6 6.5h3" stroke-width="1" opacity="0.7"/></svg>`,
  wake:    `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="8" cy="8" r="3"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3.22 3.22l1.42 1.42M11.36 11.36l1.42 1.42M3.22 12.78l1.42-1.42M11.36 4.64l1.42-1.42"/></svg>`,
};

/* --- Toast system ----------------------------------------- */
function toast(msg, type = 'info', duration = 3500) {
  const container = document.getElementById('toast-container');
  const el = document.createElement('div');
  el.className = `toast ${type}`;
  el.innerHTML = `<div class="toast-dot"></div><span>${msg}</span>`;
  container.appendChild(el);
  setTimeout(() => {
    el.classList.add('dismissing');
    setTimeout(() => el.remove(), 250);
  }, duration);
}

/* --- Router ----------------------------------------------- */
function navigate(view) {
  state.view = view;
  document.querySelectorAll('.nav-btn').forEach(b => {
    b.classList.toggle('active', b.dataset.view === view);
  });
  render();
}

/* --- Header ----------------------------------------------- */
function renderHeader() {
  const nsList = getNamespaces();
  const nsOpts = ['__all__', ...nsList].map(ns =>
    `<option value="${ns}" ${state.nsFilter === ns ? 'selected' : ''}>${ns === '__all__' ? 'All namespaces' : ns}</option>`
  ).join('');

  return `
    <div class="header-brand">
      <svg class="brand-logo" viewBox="0 0 28 28" fill="none" xmlns="http://www.w3.org/2000/svg">
        <!-- Hearth/flame motif -->
        <rect x="4" y="20" width="20" height="4" rx="1.5" fill="#f97316" opacity="0.9"/>
        <rect x="7" y="18" width="14" height="2.5" rx="1" fill="#f97316" opacity="0.7"/>
        <!-- Flame shape -->
        <path d="M14 4
                 C14 4 18 7 18 11
                 C18 13.5 16.5 14.5 16.5 14.5
                 C16.5 14.5 17 12 15.5 10.5
                 C15.5 10.5 16 13 14 14.5
                 C12 13 12.5 10.5 12.5 10.5
                 C11 12 11.5 14.5 11.5 14.5
                 C11.5 14.5 10 13.5 10 11
                 C10 7 14 4 14 4Z"
              fill="#f97316"/>
        <path d="M14 9
                 C14 9 15.5 11 15.5 12.5
                 C15.5 13.8 14.8 14.5 14 14.5
                 C13.2 14.5 12.5 13.8 12.5 12.5
                 C12.5 11 14 9 14 9Z"
              fill="#fde68a" opacity="0.9"/>
        <!-- Base grate lines -->
        <line x1="8" y1="20" x2="8" y2="18" stroke="#f97316" stroke-width="1" opacity="0.5"/>
        <line x1="12" y1="20" x2="12" y2="18" stroke="#f97316" stroke-width="1" opacity="0.5"/>
        <line x1="16" y1="20" x2="16" y2="18" stroke="#f97316" stroke-width="1" opacity="0.5"/>
        <line x1="20" y1="20" x2="20" y2="18" stroke="#f97316" stroke-width="1" opacity="0.5"/>
      </svg>
      <span class="brand-name">HEARTH</span>
    </div>

    <nav class="header-nav">
      <button class="nav-btn ${state.view === 'fleet' ? 'active' : ''}" data-view="fleet">
        <span class="nav-btn-icon">${SVG.fleet}</span>Fleet
      </button>
      <button class="nav-btn ${state.view === 'sandboxes' ? 'active' : ''}" data-view="sandboxes">
        <span class="nav-btn-icon">${SVG.sandbox}</span>Sandboxes
      </button>
      <button class="nav-btn ${state.view === 'tree' ? 'active' : ''}" data-view="tree">
        <span class="nav-btn-icon">${SVG.tree}</span>Fork Tree
      </button>
    </nav>

    <div class="header-spacer"></div>

    <div class="ns-filter-wrap">
      <span class="ns-filter-label">NS</span>
      <select class="ns-filter-select" id="ns-filter">
        ${nsOpts}
      </select>
    </div>

    <div id="mock-badge" class="mock-badge ${state.isMock ? '' : 'hidden'}">
      Mock data
    </div>

    <div class="header-stats">
      <div class="stat-chip">
        <span class="stat-chip-label">Nodes</span>
        <div class="stat-sep"></div>
        <span class="stat-chip-value" id="stat-nodes-ready">0</span>
        <span class="stat-chip-label">/ ${state.nodes.length} ready</span>
      </div>
      <div class="stat-sep"></div>
      <div class="stat-chip">
        <span class="stat-chip-label">Sandboxes</span>
        <div class="stat-sep"></div>
        <span class="stat-chip-value" id="stat-sb-total">0</span>
      </div>
      <div class="stat-sep"></div>
      <div class="stat-chip">
        <span class="stat-chip-label">Running</span>
        <div class="stat-sep"></div>
        <span class="stat-chip-value highlight" id="stat-sb-running">0</span>
      </div>
      <div class="stat-sep"></div>
      <div class="stat-chip">
        <span class="stat-chip-label">Sleeping</span>
        <div class="stat-sep"></div>
        <span class="stat-chip-value sleeping" id="stat-sb-sleeping">0</span>
      </div>
    </div>

    <div class="live-indicator">
      <div class="live-dot" id="live-dot"></div>
      <span id="live-text">LIVE</span>
    </div>
  `;
}

function updateHeaderStats() {
  const ready   = state.nodes.filter(n => n.status === 'ready').length;
  const filtered = getFilteredSandboxes();
  const running  = filtered.filter(s => s.state === 'running').length;
  const sleeping = filtered.filter(s => s.state === 'sleeping').length;

  const el = id => document.getElementById(id);
  if (el('stat-nodes-ready')) el('stat-nodes-ready').textContent = ready;
  if (el('stat-sb-total'))    el('stat-sb-total').textContent    = filtered.length;
  if (el('stat-sb-running'))  el('stat-sb-running').textContent  = running;
  if (el('stat-sb-sleeping')) el('stat-sb-sleeping').textContent = sleeping;

  const mockBadge = el('mock-badge');
  if (mockBadge) mockBadge.classList.toggle('hidden', !state.isMock);

  updateNsFilter();
}

function updateNsFilter() {
  const sel = document.getElementById('ns-filter');
  if (!sel) return;
  const nsList = getNamespaces();
  const current = sel.value;
  const opts = ['__all__', ...nsList].map(ns =>
    `<option value="${ns}" ${(state.nsFilter === ns) ? 'selected' : ''}>${ns === '__all__' ? 'All namespaces' : ns}</option>`
  ).join('');
  sel.innerHTML = opts;
  if (current && [...sel.options].find(o => o.value === current)) {
    sel.value = current;
  }
}

function updateLiveIndicator() {
  const dot = document.getElementById('live-dot');
  const text = document.getElementById('live-text');
  if (!dot || !text) return;
  const age = state.lastPoll ? (Date.now() - state.lastPoll) / 1000 : 999;
  const stale = age > 10;
  dot.classList.toggle('stale', stale);
  text.textContent = stale ? 'STALE' : 'LIVE';
}

function updateHeartbeats() {
  document.querySelectorAll('[data-heartbeat]').forEach(el => {
    const iso = el.dataset.heartbeat;
    const { text, cls } = formatHeartbeat(iso);
    el.textContent = text;
    el.className = `node-heartbeat ${cls}`;
  });
}

/* --- Fleet view ------------------------------------------- */
function renderFleet() {
  const nodes = state.nsFilter === '__all__'
    ? state.nodes
    : state.nodes.filter(n => {
        const nodeIds = state.sandboxes
          .filter(s => s.namespace === state.nsFilter)
          .map(s => s.node_id);
        return nodeIds.includes(n.id);
      });

  if (state.loading) {
    return `
      <div class="view-enter">
        <div class="page-header">
          <div class="page-title-group">
            <div class="page-eyebrow">Infrastructure</div>
            <div class="page-title">Fleet</div>
          </div>
        </div>
        <div class="fleet-grid">
          ${[1,2,3].map(() => `
            <div class="node-card">
              <div style="display:flex;gap:10px;margin-bottom:14px">
                <div class="skeleton" style="width:120px;height:16px"></div>
              </div>
              <div class="skeleton" style="height:4px;margin-bottom:8px"></div>
              <div class="skeleton" style="height:4px;margin-bottom:8px"></div>
            </div>
          `).join('')}
        </div>
      </div>`;
  }

  const readyCount = state.nodes.filter(n => n.status === 'ready').length;

  return `
    <div class="view-enter">
      <div class="page-header">
        <div class="page-title-group">
          <div class="page-eyebrow">Infrastructure</div>
          <div class="page-title">Fleet</div>
          <div class="page-subtitle">${state.nodes.length} nodes registered &mdash; ${readyCount} ready</div>
        </div>
      </div>

      ${nodes.length === 0 ? `
        <div class="empty-state">
          <div class="empty-state-icon">⬡</div>
          <div class="empty-state-title">No nodes registered</div>
          <div class="empty-state-desc">Node agents will appear here once they connect and heartbeat to the control plane.</div>
        </div>
      ` : `
        <div class="section">
          <div class="section-header">
            <div class="section-title">
              Nodes
              <span class="section-count">${nodes.length}</span>
            </div>
          </div>
          <div class="fleet-grid">
            ${nodes.map(renderNodeCard).join('')}
          </div>
        </div>
      `}
    </div>
  `;
}

function renderNodeCard(node) {
  const memUsed = node.mem_total_mib - node.mem_free_mib;
  const memPct  = node.mem_total_mib > 0 ? (memUsed / node.mem_total_mib) * 100 : 0;
  // Estimate CPU from vm_count (heuristic for demo)
  const cpuPct  = node.status === 'down' ? 0 : Math.min(95, (node.vm_count / (node.cpus / 2)) * 100);
  const cpuHigh = cpuPct > 80;
  const memHigh = memPct > 85;
  const hb = formatHeartbeat(node.last_heartbeat);

  return `
    <div class="node-card status-${node.status} card-enter" data-node-id="${escAttr(node.id)}">
      <div class="node-card-header">
        <div class="node-hostname-group">
          <div class="node-hostname">${escHtml(node.hostname)}</div>
          <div class="node-addr">${escHtml(node.addr)}</div>
        </div>
        <div class="node-status-badge ${node.status}">
          <div class="status-dot ${node.status}"></div>
          ${node.status.toUpperCase()}
        </div>
      </div>

      <div class="node-metrics">
        <div class="metric-row">
          <div class="metric-label-row">
            <span class="metric-label">CPU</span>
            <span class="metric-value">${node.cpus} cores &mdash; ${cpuPct.toFixed(0)}% est.</span>
          </div>
          <div class="metric-bar-track">
            <div class="metric-bar-fill cpu ${cpuHigh ? 'high' : ''}" style="width:${cpuPct.toFixed(1)}%"></div>
          </div>
        </div>

        <div class="metric-row">
          <div class="metric-label-row">
            <span class="metric-label">Memory</span>
            <span class="metric-value">${formatBytes(memUsed)} / ${formatBytes(node.mem_total_mib)}</span>
          </div>
          <div class="metric-bar-track">
            <div class="metric-bar-fill mem ${memHigh ? 'high' : ''}" style="width:${memPct.toFixed(1)}%"></div>
          </div>
        </div>
      </div>

      <div class="node-footer">
        <div class="node-vm-count">
          <span class="vm-count-num">${node.vm_count}</span>
          <span>active VM${node.vm_count !== 1 ? 's' : ''}</span>
        </div>
        <div class="node-heartbeat ${hb.cls}" data-heartbeat="${node.last_heartbeat}">
          ${hb.text}
        </div>
      </div>
    </div>
  `;
}

/* --- Sandboxes view --------------------------------------- */
function getFilteredSandboxes() {
  let sbs = state.sandboxes;
  if (state.nsFilter !== '__all__') {
    sbs = sbs.filter(s => s.namespace === state.nsFilter);
  }
  if (state.sbStateFilter !== '__all__') {
    sbs = sbs.filter(s => s.state === state.sbStateFilter);
  }
  if (state.sbSearch) {
    const q = state.sbSearch.toLowerCase();
    sbs = sbs.filter(s =>
      s.name.toLowerCase().includes(q) ||
      s.id.toLowerCase().includes(q) ||
      s.namespace.toLowerCase().includes(q) ||
      (s.ip && s.ip.includes(q))
    );
  }
  // Sort
  sbs = [...sbs].sort((a, b) => {
    let av = a[state.sortCol] ?? '';
    let bv = b[state.sortCol] ?? '';
    if (typeof av === 'string') av = av.toLowerCase();
    if (typeof bv === 'string') bv = bv.toLowerCase();
    if (av < bv) return -state.sortDir;
    if (av > bv) return state.sortDir;
    return 0;
  });
  return sbs;
}

function renderSandboxes() {
  return `
    <div class="view-enter">
      <div class="page-header">
        <div class="page-title-group">
          <div class="page-eyebrow">Compute</div>
          <div class="page-title">Sandboxes</div>
          <div class="page-subtitle">${state.sandboxes.length} total across all namespaces</div>
        </div>
        <div class="page-actions">
          <button class="btn btn-primary" id="btn-new-sandbox">
            <span class="btn-icon">${SVG.plus}</span>
            New Sandbox
          </button>
        </div>
      </div>

      <div class="sandbox-toolbar">
        <input
          class="search-input"
          type="text"
          placeholder="Search by name, id, ip..."
          id="sb-search"
          value="${escAttr(state.sbSearch)}"
        />
        <select class="filter-select" id="sb-state-filter">
          <option value="__all__"  ${state.sbStateFilter === '__all__'  ? 'selected' : ''}>All states</option>
          <option value="running"  ${state.sbStateFilter === 'running'  ? 'selected' : ''}>Running</option>
          <option value="sleeping" ${state.sbStateFilter === 'sleeping' ? 'selected' : ''}>Sleeping</option>
          <option value="creating" ${state.sbStateFilter === 'creating' ? 'selected' : ''}>Creating</option>
          <option value="paused"   ${state.sbStateFilter === 'paused'   ? 'selected' : ''}>Paused</option>
          <option value="stopped"  ${state.sbStateFilter === 'stopped'  ? 'selected' : ''}>Stopped</option>
          <option value="error"    ${state.sbStateFilter === 'error'    ? 'selected' : ''}>Error</option>
        </select>
        <button class="btn btn-secondary btn-sm" id="btn-refresh-sbs" title="Refresh">
          <span class="btn-icon">${SVG.refresh}</span>Refresh
        </button>
      </div>

      <div id="sandbox-table-container">
        ${renderSandboxTable()}
      </div>
    </div>
  `;
}

function renderSandboxTable() {
  const sbs = getFilteredSandboxes();

  if (state.loading) {
    return `
      <div class="sandbox-table-wrap">
        <table class="sandbox-table">
          <thead><tr>
            <th>Name</th><th>Namespace</th><th>State</th><th>Node</th>
            <th>vCPUs</th><th>Memory</th><th>IP</th><th>Age</th><th>Actions</th>
          </tr></thead>
          <tbody>
            ${[1,2,3].map(() => `
              <tr class="loading-row">
                <td><div class="skeleton" style="width:140px"></div></td>
                <td><div class="skeleton" style="width:70px"></div></td>
                <td><div class="skeleton" style="width:65px"></div></td>
                <td><div class="skeleton" style="width:90px"></div></td>
                <td><div class="skeleton" style="width:30px"></div></td>
                <td><div class="skeleton" style="width:60px"></div></td>
                <td><div class="skeleton" style="width:90px"></div></td>
                <td><div class="skeleton" style="width:40px"></div></td>
                <td><div class="skeleton" style="width:90px"></div></td>
              </tr>
            `).join('')}
          </tbody>
        </table>
      </div>`;
  }

  function th(col, label) {
    const sorted = state.sortCol === col;
    const dir = sorted ? (state.sortDir === 1 ? ' ↑' : ' ↓') : '';
    return `<th class="${sorted ? 'sorted' : ''}" data-sort="${col}">${label}${dir}</th>`;
  }

  const tableHtml = `
    <div class="sandbox-table-wrap">
      <table class="sandbox-table">
        <thead>
          <tr>
            ${th('name', 'Name')}
            ${th('namespace', 'Namespace')}
            ${th('state', 'State')}
            ${th('node_id', 'Node')}
            ${th('vcpus', 'vCPUs')}
            ${th('mem_mib', 'Memory')}
            ${th('ip', 'IP')}
            ${th('created_at', 'Age')}
            <th>Actions</th>
          </tr>
        </thead>
        <tbody>
          ${sbs.length === 0 ? `
            <tr><td colspan="9">
              <div class="empty-state">
                <div class="empty-state-icon">□</div>
                <div class="empty-state-title">No sandboxes found</div>
                <div class="empty-state-desc">Try adjusting your filters, or create a new sandbox.</div>
              </div>
            </td></tr>
          ` : sbs.map(sb => renderSandboxRow(sb)).join('')}
        </tbody>
      </table>
    </div>
  `;

  return tableHtml;
}

function renderSandboxRow(sb) {
  const node = state.nodes.find(n => n.id === sb.node_id);
  const nodeLabel = node ? node.hostname : sb.node_id.slice(0, 12);
  const hasFork = sb.parent_id !== null;

  const canStart  = sb.state === 'stopped' || sb.state === 'paused';
  const canStop   = sb.state === 'running' || sb.state === 'creating';
  const canPause  = sb.state === 'running';
  const canResume = sb.state === 'paused';
  /* v2: sleep available when running or paused; wake when sleeping */
  const canSleep  = sb.state === 'running' || sb.state === 'paused';
  const canWake   = sb.state === 'sleeping';
  /* v2: fork available when running, paused, or sleeping */
  const canFork   = sb.state === 'running' || sb.state === 'paused' || sb.state === 'sleeping';
  const canDelete = sb.state !== 'creating';

  /* v2: IP — monospace when present, em-dash when null */
  const ipDisplay = sb.ip
    ? `<span class="ip-cell mono">${escHtml(sb.ip)}</span>`
    : `<span class="ip-cell empty">&mdash;</span>`;

  /* v2: sleeping state gets a moon glyph in the badge */
  const stateGlyph = sb.state === 'sleeping' ? ' &#x25D4;' : '';

  return `
    <tr data-sb-id="${sb.id}">
      <td>
        <div class="sb-name">
          <div class="sb-name-text">
            ${escHtml(sb.name)}
            ${hasFork ? `<span class="fork-badge" title="Forked from ${sb.parent_id}">${SVG.fork} fork</span>` : ''}
          </div>
          <div class="sb-id">${sb.id}</div>
        </div>
      </td>
      <td><span class="${getNsBadgeClass(sb.namespace)} ns-badge">${escHtml(sb.namespace)}</span></td>
      <td>
        <div class="state-badge ${sb.state}">
          <div class="state-dot ${sb.state}"></div>
          ${sb.state}${stateGlyph}
        </div>
      </td>
      <td><span class="resource-cell">${escHtml(nodeLabel)}</span></td>
      <td><span class="resource-cell">${sb.vcpus}<span class="unit"> vCPU</span></span></td>
      <td><span class="resource-cell">${formatBytes(sb.mem_mib)}</span></td>
      <td>${ipDisplay}</td>
      <td><span class="age-cell">${formatAge(sb.created_at)}</span></td>
      <td>
        <div class="actions-cell">
          ${canStart  ? `<button class="action-btn success" title="Start"  data-action="start"  data-id="${sb.id}">${SVG.start}</button>`  : ''}
          ${canStop   ? `<button class="action-btn danger"  title="Stop"   data-action="stop"   data-id="${sb.id}">${SVG.stop}</button>`   : ''}
          ${canPause  ? `<button class="action-btn"         title="Pause"  data-action="pause"  data-id="${sb.id}">${SVG.pause}</button>`  : ''}
          ${canResume ? `<button class="action-btn success" title="Resume" data-action="resume" data-id="${sb.id}">${SVG.resume}</button>` : ''}
          ${canSleep  ? `<button class="action-btn sleep"   title="Sleep"  data-action="sleep"  data-id="${sb.id}">${SVG.sleep}</button>`  : ''}
          ${canWake   ? `<button class="action-btn wake"    title="Wake"   data-action="wake"   data-id="${sb.id}">${SVG.wake}</button>`   : ''}
          ${canFork   ? `<button class="action-btn accent"  title="Fork"   data-action="fork"   data-id="${sb.id}">${SVG.fork}</button>`   : ''}
          <button class="action-btn" title="Terminal (coming soon)" disabled>${SVG.terminal}</button>
          ${canDelete ? `<button class="action-btn danger"  title="Delete" data-action="delete" data-id="${sb.id}">${SVG.trash}</button>`  : ''}
        </div>
      </td>
    </tr>
  `;
}

/* --- Fork Tree view --------------------------------------- */
function renderTree() {
  const nsList = getNamespaces();
  const nsOpts = ['__all__', ...nsList].map(ns =>
    `<option value="${ns}" ${state.treeNs === ns ? 'selected' : ''}>${ns === '__all__' ? 'All namespaces' : ns}</option>`
  ).join('');

  return `
    <div class="view-enter">
      <div class="page-header">
        <div class="page-title-group">
          <div class="page-eyebrow">Topology</div>
          <div class="page-title">Fork Tree</div>
          <div class="page-subtitle">Parent-child sandbox relationships by namespace</div>
        </div>
        <div class="page-actions">
          <select class="filter-select" id="tree-ns-filter">
            ${nsOpts}
          </select>
        </div>
      </div>

      <div id="fork-tree-content">
        ${renderTreeContent()}
      </div>
    </div>
  `;
}

function renderTreeContent() {
  let sbs = state.sandboxes;
  if (state.treeNs !== '__all__') {
    sbs = sbs.filter(s => s.namespace === state.treeNs);
  }

  // Group by namespace
  const byNs = {};
  sbs.forEach(sb => {
    if (!byNs[sb.namespace]) byNs[sb.namespace] = [];
    byNs[sb.namespace].push(sb);
  });

  if (Object.keys(byNs).length === 0) {
    return `
      <div class="empty-state">
        <div class="empty-state-icon">⌥</div>
        <div class="empty-state-title">No fork relationships</div>
        <div class="empty-state-desc">Fork a running sandbox to see the tree here.</div>
      </div>`;
  }

  const nsKeys = Object.keys(byNs).sort();
  const svgs = nsKeys.map(ns => renderNsTree(ns, byNs[ns])).join('');

  return `
    <div class="section">
      ${svgs}
    </div>
  `;
}

function renderNsTree(ns, sandboxes) {
  // Build adjacency
  const byId = {};
  sandboxes.forEach(sb => { byId[sb.id] = sb; });
  const children = {};
  const roots = [];
  sandboxes.forEach(sb => {
    if (!sb.parent_id || !byId[sb.parent_id]) {
      roots.push(sb);
    } else {
      if (!children[sb.parent_id]) children[sb.parent_id] = [];
      children[sb.parent_id].push(sb);
    }
  });

  // Lay out tree: BFS with positions
  const NODE_W = 160;
  const NODE_H = 52;
  const H_GAP  = 30;
  const V_GAP  = 44;

  const positions = {};
  let maxX = 0, maxY = 0;

  function layoutSubtree(node, depth, xOffset) {
    const kids = children[node.id] || [];
    if (kids.length === 0) {
      positions[node.id] = { x: xOffset, y: depth * (NODE_H + V_GAP) };
      maxX = Math.max(maxX, xOffset + NODE_W);
      maxY = Math.max(maxY, depth * (NODE_H + V_GAP) + NODE_H);
      return NODE_W + H_GAP;
    }
    let totalWidth = 0;
    let childX = xOffset;
    kids.forEach(kid => {
      const w = layoutSubtree(kid, depth + 1, childX);
      totalWidth += w;
      childX += w;
    });
    const cx = xOffset + (totalWidth - H_GAP) / 2 - NODE_W / 2;
    positions[node.id] = { x: cx, y: depth * (NODE_H + V_GAP) };
    maxX = Math.max(maxX, cx + NODE_W);
    maxY = Math.max(maxY, depth * (NODE_H + V_GAP) + NODE_H);
    return totalWidth;
  }

  let xCursor = 20;
  roots.forEach(root => {
    const w = layoutSubtree(root, 0, xCursor);
    xCursor += w + H_GAP;
  });

  const PAD = 20;
  const svgW = Math.max(maxX + PAD * 2, 400);
  const svgH = maxY + PAD * 2;

  // Draw edges
  const edges = [];
  sandboxes.forEach(sb => {
    if (sb.parent_id && positions[sb.parent_id] && positions[sb.id]) {
      const p = positions[sb.parent_id];
      const c = positions[sb.id];
      const px = p.x + PAD + NODE_W / 2;
      const py = p.y + PAD + NODE_H;
      const cx2 = c.x + PAD + NODE_W / 2;
      const cy2 = c.y + PAD;
      const midy = (py + cy2) / 2;
      edges.push(`<path class="tree-edge" d="M${px},${py} C${px},${midy} ${cx2},${midy} ${cx2},${cy2}"/>`);
    }
  });

  // Draw nodes
  const nodes = sandboxes.map(sb => {
    if (!positions[sb.id]) return '';
    const pos = positions[sb.id];
    const x = pos.x + PAD;
    const y = pos.y + PAD;
    const isRoot = !sb.parent_id || !byId[sb.parent_id];
    /* v2: use state class for rect styling; root gets special amber border */
    const stateClass = isRoot ? 'root' : sb.state;
    const stateColor = STATE_COLORS[sb.state] || '#555560';
    const shortName = sb.name.length > 18 ? sb.name.slice(0, 16) + '…' : sb.name;
    const shortId   = sb.id.slice(-8);

    return `
      <g class="tree-node-group" data-sb-id="${sb.id}"
         transform="translate(${x}, ${y})"
         onclick="window._treeNodeClick('${sb.id}')">
        <rect class="tree-node-rect ${stateClass}" width="${NODE_W}" height="${NODE_H}"/>
        <!-- state color accent -->
        <rect x="0" y="0" width="3" height="${NODE_H}" rx="2.5" ry="2.5" fill="${stateColor}" opacity="0.7"/>
        <text class="tree-node-name" x="12" y="18">${escHtml(shortName)}</text>
        <text class="tree-node-state" x="12" y="30" fill="${stateColor}">${sb.state}</text>
        <text class="tree-node-ns"    x="12" y="42">${shortId}</text>
      </g>
    `;
  }).join('');

  return `
    <div class="section">
      <div class="section-header">
        <div class="section-title">
          <span class="${getNsBadgeClass(ns)} ns-badge">${escHtml(ns)}</span>
          <span class="section-count">${sandboxes.length}</span>
        </div>
      </div>
      <div class="fork-tree-wrap">
        <div class="fork-tree-svg-wrap">
          <svg
            width="${svgW}"
            height="${svgH}"
            viewBox="0 0 ${svgW} ${svgH}"
            xmlns="http://www.w3.org/2000/svg"
            style="display:block;min-width:${svgW}px"
          >
            <!-- Grid lines -->
            <defs>
              <pattern id="grid-${ns}" width="40" height="40" patternUnits="userSpaceOnUse">
                <path d="M 40 0 L 0 0 0 40" fill="none" stroke="rgba(255,255,255,0.025)" stroke-width="1"/>
              </pattern>
            </defs>
            <rect width="${svgW}" height="${svgH}" fill="url(#grid-${ns})"/>
            ${edges.join('\n')}
            ${nodes}
          </svg>
        </div>
      </div>
    </div>
  `;
}

/* --- Modals ----------------------------------------------- */
let activeModal = null;

function openModal(id) {
  const overlay = document.getElementById(id);
  if (overlay) {
    overlay.classList.add('open');
    activeModal = id;
  }
}

function closeModal(id) {
  const overlay = document.getElementById(id);
  if (overlay) {
    overlay.classList.remove('open');
    if (activeModal === id) activeModal = null;
  }
}

function renderNewSandboxModal() {
  return `
    <div class="modal-overlay" id="modal-new-sandbox">
      <div class="modal">
        <div class="modal-header">
          <span class="modal-title">New Sandbox</span>
          <button class="modal-close" onclick="closeModal('modal-new-sandbox')">${SVG.close}</button>
        </div>
        <div class="modal-body">
          <div class="form-group">
            <label class="form-label">Name <span class="form-hint">lowercase, alphanumeric + hyphens</span></label>
            <input class="form-input" id="new-sb-name" type="text" placeholder="my-sandbox" autocomplete="off" spellcheck="false"/>
          </div>
          <div class="form-group">
            <label class="form-label">Namespace</label>
            <input class="form-input" id="new-sb-ns" type="text" placeholder="default" value="${state.nsFilter !== '__all__' ? escAttr(state.nsFilter) : 'default'}" autocomplete="off"/>
          </div>
          <div class="form-group">
            <label class="form-label">vCPUs</label>
            <div class="slider-wrap">
              <input class="form-slider" type="range" min="1" max="4" value="1" step="1" id="new-sb-vcpus"
                     oninput="document.getElementById('new-sb-vcpus-val').textContent=this.value"/>
              <span class="slider-value" id="new-sb-vcpus-val">1</span>
            </div>
          </div>
          <div class="form-group">
            <label class="form-label">Memory</label>
            <div class="mem-options" id="mem-opts">
              ${[128, 256, 512, 1024, 2048].map((m, i) => `
                <div class="mem-option ${i === 2 ? 'selected' : ''}" data-mem="${m}" onclick="selectMem(${m})">
                  ${m >= 1024 ? m/1024 + ' GiB' : m + ' MiB'}
                </div>
              `).join('')}
            </div>
          </div>
          <div class="terminal-placeholder">
            <div class="terminal-placeholder-icon">${SVG.terminal}</div>
            <div class="terminal-coming-soon">Terminal — Coming soon</div>
            <div style="font-size:11px;color:var(--text-muted)">exec &amp; interactive terminal will be available in v2</div>
          </div>
        </div>
        <div class="modal-footer">
          <button class="btn btn-secondary" onclick="closeModal('modal-new-sandbox')">Cancel</button>
          <button class="btn btn-primary" id="btn-create-sandbox">
            ${SVG.plus} Create
          </button>
        </div>
      </div>
    </div>
  `;
}

function renderConfirmModal() {
  return `
    <div class="modal-overlay" id="modal-confirm">
      <div class="modal">
        <div class="modal-header">
          <span class="modal-title" id="confirm-title">Confirm action</span>
          <button class="modal-close" onclick="closeModal('modal-confirm')">${SVG.close}</button>
        </div>
        <div class="modal-body">
          <p class="confirm-text" id="confirm-body">Are you sure?</p>
        </div>
        <div class="modal-footer">
          <button class="btn btn-secondary" onclick="closeModal('modal-confirm')">Cancel</button>
          <button class="btn btn-danger" id="btn-confirm-ok">Confirm</button>
        </div>
      </div>
    </div>
  `;
}

function renderForkModal() {
  return `
    <div class="modal-overlay" id="modal-fork">
      <div class="modal">
        <div class="modal-header">
          <span class="modal-title">Fork Sandbox</span>
          <button class="modal-close" onclick="closeModal('modal-fork')">${SVG.close}</button>
        </div>
        <div class="modal-body">
          <div class="form-group">
            <label class="form-label">New Sandbox Name</label>
            <input class="form-input" id="fork-sb-name" type="text" placeholder="my-fork" autocomplete="off" spellcheck="false"/>
          </div>
          <div id="fork-parent-info" style="font-size:11px;color:var(--text-muted);padding:8px 0">
            Fork creates a copy-on-write clone inheriting parent memory state.
          </div>
        </div>
        <div class="modal-footer">
          <button class="btn btn-secondary" onclick="closeModal('modal-fork')">Cancel</button>
          <button class="btn btn-primary" id="btn-fork-ok">
            ${SVG.fork} Fork
          </button>
        </div>
      </div>
    </div>
  `;
}

window.selectMem = function(m) {
  document.querySelectorAll('.mem-option').forEach(el => {
    el.classList.toggle('selected', parseInt(el.dataset.mem) === m);
  });
};

/* --- Actions ---------------------------------------------- */
async function sandboxAction(action, sbId) {
  const sb = state.sandboxes.find(s => s.id === sbId);
  if (!sb) return;

  if (action === 'delete') {
    showConfirm(
      'Delete Sandbox',
      `Permanently delete <span class="confirm-target">${escHtml(sb.name)}</span>? This action cannot be undone.`,
      async () => {
        if (state.isMock) {
          state.sandboxes = state.sandboxes.filter(s => s.id !== sbId);
          toast(`Deleted ${sb.name}`, 'success');
          renderSandboxTableInPlace();
          return;
        }
        const res = await apiDelete(`/sandboxes/${sbId}`);
        if (res.error) { toast(`Error: ${res.error}`, 'error'); return; }
        toast(`Deleted ${sb.name}`, 'success');
        await poll();
      }
    );
    return;
  }

  if (action === 'fork') {
    openForkModal(sbId);
    return;
  }

  /* v2: sleep action */
  if (action === 'sleep') {
    showConfirm(
      'Sleep Sandbox',
      `Snapshot and suspend <span class="confirm-target">${escHtml(sb.name)}</span>? Memory will be freed; wake resumes from snapshot.`,
      async () => {
        const prevState = sb.state;
        sb.state = 'sleeping';
        renderSandboxTableInPlace();
        if (state.isMock) {
          toast(`${sb.name} is now sleeping`, 'success');
          return;
        }
        const res = await apiPost(`/sandboxes/${sbId}/sleep`);
        if (res.error) {
          sb.state = prevState;
          toast(`Sleep failed: ${res.error}`, 'error');
          renderSandboxTableInPlace();
        } else {
          toast(`${sb.name} is now sleeping`, 'success');
        }
      }
    );
    return;
  }

  /* v2: wake action — shows wake_ms from response */
  if (action === 'wake') {
    const prevState = sb.state;
    sb.state = 'running';
    renderSandboxTableInPlace();
    if (state.isMock) {
      const fakeMs = Math.floor(Math.random() * 120) + 20;
      toast(`${sb.name} woke in ${fakeMs} ms`, 'success');
      return;
    }
    const res = await apiPost(`/sandboxes/${sbId}/wake`);
    if (res.error) {
      sb.state = prevState;
      toast(`Wake failed: ${res.error}`, 'error');
      renderSandboxTableInPlace();
    } else {
      const ms = res.data && res.data.wake_ms != null ? res.data.wake_ms : null;
      toast(ms != null ? `${sb.name} woke in ${ms} ms` : `${sb.name} is running`, 'success');
    }
    return;
  }

  // Optimistic update for remaining actions
  const prevState = sb.state;
  if (action === 'stop')   { sb.state = 'stopped'; sb.ip = null; }
  if (action === 'start')  { sb.state = 'running'; }
  if (action === 'pause')  { sb.state = 'paused'; }
  if (action === 'resume') { sb.state = 'running'; }
  renderSandboxTableInPlace();

  if (state.isMock) {
    toast(`${action.charAt(0).toUpperCase() + action.slice(1)}ed ${sb.name}`, 'success');
    return;
  }

  const res = await apiPost(`/sandboxes/${sbId}/${action}`);
  if (res.error) {
    sb.state = prevState; // revert
    toast(`Error: ${res.error}`, 'error');
    renderSandboxTableInPlace();
  } else {
    toast(`${action.charAt(0).toUpperCase() + action.slice(1)}ed ${sb.name}`, 'success');
  }
}

function renderSandboxTableInPlace() {
  const container = document.getElementById('sandbox-table-container');
  if (container) container.innerHTML = renderSandboxTable();
  attachSandboxTableListeners();
}

function showConfirm(title, body, onOk) {
  document.getElementById('confirm-title').textContent = title;
  document.getElementById('confirm-body').innerHTML = body;
  const okBtn = document.getElementById('btn-confirm-ok');
  const newOk = okBtn.cloneNode(true);
  newOk.addEventListener('click', () => {
    closeModal('modal-confirm');
    onOk();
  });
  okBtn.replaceWith(newOk);
  openModal('modal-confirm');
}

function openForkModal(parentId) {
  const parent = state.sandboxes.find(s => s.id === parentId);
  if (!parent) return;
  const nameEl = document.getElementById('fork-sb-name');
  if (nameEl) nameEl.value = `${parent.name}-fork`;
  const info = document.getElementById('fork-parent-info');
  if (info) {
    const sleepNote = parent.state === 'sleeping'
      ? ' Parent is sleeping — fork restores from snapshot.'
      : ' Fork creates a copy-on-write clone inheriting parent memory state.';
    info.textContent = `Parent: ${parent.name} (${parentId}).${sleepNote}`;
  }
  const okBtn = document.getElementById('btn-fork-ok');
  const newOk = okBtn.cloneNode(true);
  newOk.addEventListener('click', async () => {
    const name = document.getElementById('fork-sb-name').value.trim();
    if (!name) { toast('Name is required', 'error'); return; }
    closeModal('modal-fork');
    if (state.isMock) {
      const newSb = {
        ...parent,
        id: 'sb-' + Math.random().toString(16).slice(2, 10),
        name,
        state: 'creating',
        ip: null,
        parent_id: parentId,
        created_at: new Date().toISOString(),
      };
      state.sandboxes.push(newSb);
      toast(`Forked → ${name}`, 'success');
      renderSandboxTableInPlace();
      return;
    }
    /* v2: POST /sandboxes/{id}/fork returns 201 with child JSON including parent_id */
    const res = await apiPost(`/sandboxes/${parentId}/fork`, { name });
    if (res.error) { toast(`Fork failed: ${res.error}`, 'error'); return; }
    const childName = (res.data && res.data.name) ? res.data.name : name;
    toast(`Forked → ${childName}`, 'success');
    await poll();
  });
  okBtn.replaceWith(newOk);
  openModal('modal-fork');
}

/* --- Create sandbox --------------------------------------- */
async function createSandbox() {
  const name     = document.getElementById('new-sb-name').value.trim();
  const ns       = document.getElementById('new-sb-ns').value.trim() || 'default';
  const vcpus    = parseInt(document.getElementById('new-sb-vcpus').value, 10);
  const memEl    = document.querySelector('.mem-option.selected');
  const mem_mib  = memEl ? parseInt(memEl.dataset.mem, 10) : 512;

  if (!name) { toast('Name is required', 'error'); return; }

  closeModal('modal-new-sandbox');

  if (state.isMock) {
    const newSb = {
      id: 'sb-' + Math.random().toString(16).slice(2, 10),
      name, namespace: ns, node_id: state.nodes[0]?.id || 'nd-unknown',
      state: 'creating', vcpus, mem_mib, ip: null,
      created_at: new Date().toISOString(), parent_id: null,
    };
    state.sandboxes.push(newSb);
    toast(`Creating ${name}…`, 'info');
    render();
    return;
  }

  const res = await apiPost('/sandboxes', { name, namespace: ns, vcpus, mem_mib });
  if (res.error) { toast(`Create failed: ${res.error}`, 'error'); return; }
  toast(`Creating ${name}…`, 'info');
  await poll();
}

/* --- Event listeners -------------------------------------- */
let _globalListenersAttached = false;

function attachListeners() {
  // Nav buttons (re-attach each render — they're recreated in the header)
  document.querySelectorAll('.nav-btn').forEach(btn => {
    btn.addEventListener('click', () => navigate(btn.dataset.view));
  });

  // Namespace filter (recreated in header each render)
  document.getElementById('ns-filter')?.addEventListener('change', e => {
    state.nsFilter = e.target.value;
    updateHeaderStats();
    if (state.view === 'sandboxes') renderSandboxTableInPlace();
    else if (state.view === 'tree') {
      const treeContent = document.getElementById('fork-tree-content');
      if (treeContent) treeContent.innerHTML = renderTreeContent();
      attachTreeViewListeners();
    }
    else if (state.view === 'fleet') {
      const content = document.getElementById('content');
      if (content) content.innerHTML = renderFleet();
    }
  });

  // Sandbox view specifics
  attachSandboxViewListeners();

  // Tree view
  attachTreeViewListeners();

  // Modal close on overlay click (modals recreated each render)
  document.querySelectorAll('.modal-overlay').forEach(overlay => {
    overlay.addEventListener('click', e => {
      if (e.target === overlay) closeModal(overlay.id);
    });
  });

  // Keyboard: Escape — attach only once to avoid stacking handlers
  if (!_globalListenersAttached) {
    document.addEventListener('keydown', e => {
      if (e.key === 'Escape' && activeModal) closeModal(activeModal);
    });
    _globalListenersAttached = true;
  }
}

function attachSandboxViewListeners() {
  document.getElementById('btn-new-sandbox')?.addEventListener('click', () => {
    openModal('modal-new-sandbox');
  });

  document.getElementById('btn-refresh-sbs')?.addEventListener('click', async () => {
    await poll();
  });

  document.getElementById('sb-search')?.addEventListener('input', e => {
    state.sbSearch = e.target.value;
    renderSandboxTableInPlace();
  });

  document.getElementById('sb-state-filter')?.addEventListener('change', e => {
    state.sbStateFilter = e.target.value;
    renderSandboxTableInPlace();
  });

  document.getElementById('btn-create-sandbox')?.addEventListener('click', createSandbox);

  attachSandboxTableListeners();
}

function attachSandboxTableListeners() {
  // Sort headers
  document.querySelectorAll('.sandbox-table th[data-sort]').forEach(th => {
    th.addEventListener('click', () => {
      const col = th.dataset.sort;
      if (state.sortCol === col) state.sortDir *= -1;
      else { state.sortCol = col; state.sortDir = 1; }
      renderSandboxTableInPlace();
    });
  });

  // Action buttons
  document.querySelectorAll('[data-action]').forEach(btn => {
    btn.addEventListener('click', e => {
      e.stopPropagation();
      const action = btn.dataset.action;
      const id     = btn.dataset.id;
      sandboxAction(action, id);
    });
  });
}

function attachTreeViewListeners() {
  document.getElementById('tree-ns-filter')?.addEventListener('change', e => {
    state.treeNs = e.target.value;
    const treeContent = document.getElementById('fork-tree-content');
    if (treeContent) treeContent.innerHTML = renderTreeContent();
  });
}

// Tree node click handler (global to work with SVG onclick)
window._treeNodeClick = function(sbId) {
  const sb = state.sandboxes.find(s => s.id === sbId);
  if (!sb) return;
  toast(`${sb.name} — ${sb.state} (${sb.id})`, 'info', 2500);
};

/* --- Main render ------------------------------------------ */
function render() {
  const header = document.getElementById('header');
  if (header) header.innerHTML = renderHeader();

  const content = document.getElementById('content');
  if (!content) return;

  if (state.view === 'fleet')     content.innerHTML = renderFleet();
  else if (state.view === 'sandboxes') content.innerHTML = renderSandboxes();
  else if (state.view === 'tree') content.innerHTML = renderTree();

  // Ensure modals are always present in DOM
  let modalsWrap = document.getElementById('modals');
  if (!modalsWrap) {
    modalsWrap = document.createElement('div');
    modalsWrap.id = 'modals';
    document.body.appendChild(modalsWrap);
  }
  modalsWrap.innerHTML =
    renderNewSandboxModal() +
    renderConfirmModal() +
    renderForkModal();

  attachListeners();
  updateHeaderStats();
  updateLiveIndicator();
}

/* --- Escape helpers --------------------------------------- */
function escHtml(str) {
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}
function escAttr(str) { return escHtml(str); }

/* --- Boot ------------------------------------------------- */
document.addEventListener('DOMContentLoaded', () => {
  // Initial skeleton render
  render();

  // First poll
  poll().then(() => {
    // Start polling loop
    setInterval(() => {
      poll();
    }, POLL_INTERVAL_MS);
  });
});
