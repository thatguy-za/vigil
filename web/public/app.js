const $ = s => document.querySelector(s);
const view = $('#view');
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
let timers = [];
const later = (fn, ms) => { const t = setInterval(fn, ms); timers.push(t); };

async function api(path, opts = {}) {
  const r = await fetch('/api' + path, {
    ...opts, headers: opts.body ? { 'Content-Type': 'application/json' } : {},
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  if (r.status === 204) return null;
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.error || r.statusText);
  return j;
}

function toast(msg) {
  const t = Object.assign(document.createElement('div'), { className: 'toast', textContent: msg });
  document.body.append(t); setTimeout(() => t.remove(), 3500);
}

const MODES = { off: 'Off', continuous: '24x7', schedule: 'Schedule', motion: 'Motion' };
const pill = s => `<span class="pill ${s.state}">${{ recording: 'Recording', connecting: 'Connecting', error: 'Error', off: 'Off', idle: 'Idle' }[s.state]}</span>`;
const fmtBytes = b => b > 2 ** 30 ? (b / 2 ** 30).toFixed(1) + ' GB' : (b / 2 ** 20).toFixed(0) + ' MB';
const fmtTime = d => new Date(d).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });

// ---------- Cameras ----------
async function camerasView() {
  const cams = await api('/cameras');
  if (!cams.length) {
    const sys = await api('/system');
    view.innerHTML = `<div class="empty"><h2>No cameras yet</h2>
      <p>Add an RTSP camera to start recording.</p>
      <p class="sub">${esc(sys.hw.recommendation.summary)}</p>
      <a class="btn pri" href="#/camera">Add your first camera</a></div>`;
    return;
  }
  view.innerHTML = `<div class="grid">${cams.map(c => `
    <a class="card cam" href="#/camera/${c.id}">
      <div class="thumb"><span>Loading preview…</span><img data-id="${c.id}" alt=""></div>
      <div class="row"><div><b>${esc(c.name)}</b><div class="sub">${MODES[c.mode]}${c.transcode ? ' · transcoded' : ''}</div></div>${pill(c.status)}</div>
      ${c.status.state === 'error' ? `<div class="row err">${esc(c.status.error)}</div>` : ''}
    </a>`).join('')}</div>
    <a class="btn pri fab" href="#/camera" aria-label="Add camera">+</a>`;
  const imgs = [...view.querySelectorAll('img')];
  const refresh = () => imgs.forEach(img => {
    if (img.dataset.busy) return;
    img.dataset.busy = 1;
    const n = new Image();
    n.onload = () => { img.src = n.src; delete img.dataset.busy; };
    n.onerror = () => delete img.dataset.busy;
    n.src = `/api/cameras/${img.dataset.id}/snapshot?t=${Date.now()}`;
  });
  refresh(); later(refresh, 2000);
  later(() => { if (location.hash === '#/' || location.hash === '') updateStatus(); }, 5000);
}

async function updateStatus() {
  try {
    const cams = await api('/cameras');
    document.querySelectorAll('.cam').forEach((el, i) => {
      if (cams[i]) el.querySelector('.pill').outerHTML = pill(cams[i].status);
    });
  } catch { }
}

// ---------- Add / edit camera ----------
const DAYS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];
const dow = col => (col + 1) % 7; // grid columns start Monday, schedule index starts Sunday

async function cameraForm(id) {
  const [sys, existing] = await Promise.all([api('/system'), id ? api('/cameras/' + id) : null]);
  const c = existing || { name: '', url: '', sub_url: '', mode: 'continuous', schedule: '0'.repeat(168), transcode: false, max_height: 720, audio: false, retention_days: 0 };
  if (c.schedule.length !== 168) c.schedule = '0'.repeat(168);
  const enc = sys.hw.encoder;
  view.innerHTML = `<h2>${id ? 'Edit camera' : 'Add camera'}</h2>
  <form id="f" class="card pad" autocomplete="off">
    <label for="name" style="margin-top:0">Name</label>
    <input id="name" type="text" value="${esc(c.name)}" placeholder="Front door" required>
    <label for="url">Stream URL <small>RTSP</small></label>
    <input id="url" type="url" value="${esc(c.url)}" placeholder="rtsp://user:pass@192.168.1.20:554/stream1" required inputmode="url" autocapitalize="off">
    <div class="actions" style="margin-top:10px"><button type="button" id="test">Test connection</button></div>
    <div id="prev"></div>
    <label for="sub">Sub-stream URL <small>optional, low resolution; used for live preview</small></label>
    <input id="sub" type="url" value="${esc(c.sub_url)}" placeholder="rtsp://user:pass@192.168.1.20:554/stream2" inputmode="url" autocapitalize="off">
    <label>Record</label>
    <div class="seg" id="mode">${['off', 'continuous', 'schedule', 'motion'].map(m =>
      `<button type="button" data-m="${m}" class="${c.mode === m ? 'on' : ''}" ${m === 'motion' ? 'disabled title="Coming soon"' : ''}>${MODES[m]}${m === 'motion' ? ' (soon)' : ''}</button>`).join('')}</div>
    <div id="sched" hidden></div>
    <div class="sw"><span>Record audio<small>Converted to AAC</small></span><input id="audio" type="checkbox" ${c.audio ? 'checked' : ''}></div>
    <div class="sw"><span>Transcode video<small>${enc.hardware ? `GPU encoder (${esc(enc.name)}) available` : `CPU only (${esc(enc.name)}): heavy on this board`}. Leave off unless playback fails.</small></span><input id="tc" type="checkbox" ${c.transcode ? 'checked' : ''}></div>
    <div id="tcopt" hidden><label for="mh">Max height</label>
      <select id="mh">${[480, 720, 1080].map(h => `<option ${c.max_height === h ? 'selected' : ''}>${h}</option>`).join('')}</select></div>
    <label for="ret">Keep recordings for <small>days, 0 = default (14)</small></label>
    <input id="ret" type="number" min="0" max="3650" value="${c.retention_days}" inputmode="numeric">
    <div id="err" class="err"></div>
    <div class="actions"><button class="pri" id="save">Save</button>${id ? '<button type="button" class="del" id="del">Delete</button>' : ''}</div>
  </form>`;

  let mode = c.mode, sched = c.schedule.split('');
  const upd = () => {
    $('#sched').hidden = mode !== 'schedule';
    $('#tcopt').hidden = !$('#tc').checked;
    document.querySelectorAll('#mode button').forEach(b => b.classList.toggle('on', b.dataset.m === mode));
  };
  $('#mode').onclick = e => { const m = e.target.dataset?.m; if (m && !e.target.disabled) { mode = m; upd(); } };
  $('#tc').onchange = upd;

  // schedule painter: rows = hours, columns = Mon..Sun
  const sc = $('#sched');
  const drawSched = () => {
    sc.innerHTML = `<div class="sub" style="margin-top:12px">Tap or drag to choose the hours to record.</div>
    <div class="chips" style="margin-top:8px">
      <button type="button" data-p="all">All day</button><button type="button" data-p="night">Nights</button>
      <button type="button" data-p="work">Work hours</button><button type="button" data-p="none">Clear</button></div>
    <div class="sched" id="grid"><div class="h"></div>${DAYS.map(d => `<div class="h">${d}</div>`).join('')}
    ${Array.from({ length: 24 }, (_, h) => `<div class="h">${String(h).padStart(2, '0')}</div>` +
      DAYS.map((_, col) => `<div data-i="${dow(col) * 24 + h}" class="${sched[dow(col) * 24 + h] === '1' ? 'on' : ''}"></div>`).join('')).join('')}</div>`;
    let paint = null;
    const cell = (x, y) => { const el = document.elementFromPoint(x, y); return el?.dataset?.i ? el : null; };
    const set = el => { if (el && paint !== null) { sched[el.dataset.i] = paint ? '1' : '0'; el.classList.toggle('on', paint); } };
    const g = $('#grid');
    g.onpointerdown = e => { const el = cell(e.clientX, e.clientY); if (el) { paint = sched[el.dataset.i] !== '1'; set(el); g.setPointerCapture(e.pointerId); } };
    g.onpointermove = e => set(cell(e.clientX, e.clientY));
    g.onpointerup = g.onpointercancel = () => paint = null;
    sc.querySelector('.chips').onclick = e => {
      const p = e.target.dataset?.p; if (!p) return;
      sched = sched.map((_, i) => { const h = i % 24; return p === 'all' ? '1' : p === 'night' ? (h >= 18 || h < 6 ? '1' : '0') : p === 'work' ? (h >= 8 && h < 18 ? '1' : '0') : '0'; });
      drawSched();
    };
  };
  drawSched(); upd();

  $('#test').onclick = async e => {
    const b = e.target, url = $('#url').value.trim();
    b.disabled = true; b.textContent = 'Connecting…'; $('#prev').innerHTML = '';
    try {
      const r = await fetch('/api/cameras/test', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ url }) });
      if (!r.ok) throw new Error((await r.json()).error || 'failed');
      $('#prev').innerHTML = `<div class="preview"><img src="${URL.createObjectURL(await r.blob())}" alt="Camera preview"></div><div class="sub">Connected.</div>`;
    } catch (err) { $('#prev').innerHTML = `<div class="err" style="margin-top:8px">${esc(err.message)}</div>`; }
    b.disabled = false; b.textContent = 'Test connection';
  };

  $('#f').onsubmit = async e => {
    e.preventDefault();
    $('#save').disabled = true; $('#err').textContent = '';
    const body = { name: $('#name').value, url: $('#url').value, sub_url: $('#sub').value, mode, schedule: sched.join(''),
      transcode: $('#tc').checked, max_height: +$('#mh').value, audio: $('#audio').checked, retention_days: +$('#ret').value || 0 };
    try { await api(id ? '/cameras/' + id : '/cameras', { method: id ? 'PUT' : 'POST', body }); location.hash = '#/'; }
    catch (err) { $('#err').textContent = err.message; $('#save').disabled = false; }
  };
  if (id) $('#del').onclick = async () => {
    if (!confirm('Delete this camera?')) return;
    const purge = confirm('Also delete its recordings?\n\nOK = delete recordings, Cancel = keep the files.');
    await api(`/cameras/${id}?purge=${purge ? 1 : 0}`, { method: 'DELETE' });
    location.hash = '#/';
  };
}

// ---------- Recordings ----------
async function clipsView(camId, date) {
  const cams = await api('/cameras');
  if (!cams.length) { view.innerHTML = '<div class="empty">Add a camera first.</div>'; return; }
  const cam = cams.find(c => c.id == camId) || cams[0];
  const r = await api(`/cameras/${cam.id}/recordings` + (date ? `?date=${date}` : ''));
  view.innerHTML = `<h2>Recordings</h2>
    <div class="chips">${cams.map(c => `<button class="${c.id === cam.id ? 'on' : ''}" data-c="${c.id}">${esc(c.name)}</button>`).join('')}</div>
    ${r.dates.length ? `<select id="date">${r.dates.map(d => `<option ${d === r.date ? 'selected' : ''}>${d}</option>`).join('')}</select>
    <div id="player" style="margin-top:12px"></div>
    <div>${r.clips.map((c, i) => `<button class="clip card" data-i="${i}"><span><b>${fmtTime(c.start)}</b> <span class="sub">5 min segment${c.live ? ' · recording now' : ''}</span></span><span class="sub">${fmtBytes(c.bytes)}</span></button>`).join('')}</div>`
      : '<div class="empty">No recordings yet.</div>'}`;
  view.querySelector('.chips').onclick = e => e.target.dataset.c && (location.hash = `#/clips/${e.target.dataset.c}`);
  if (r.dates.length) {
    $('#date').onchange = e => clipsView(cam.id, e.target.value);
    view.onclick = e => {
      const b = e.target.closest('.clip'); if (!b) return;
      $('#player').innerHTML = `<video controls autoplay playsinline src="${esc(r.clips[b.dataset.i].path)}"></video>`;
      $('#player').scrollIntoView({ behavior: 'smooth', block: 'center' });
    };
  }
}

// ---------- System ----------
async function systemView() {
  const s = await api('/system'), h = s.hw, d = s.disk, used = d.total_bytes - d.free_bytes;
  view.innerHTML = `<h2>System</h2>
  <div class="card pad"><dl>
    <dt>Device</dt><dd>${esc(h.model)}</dd><dt>Memory</dt><dd>${h.mem_mb} MB</dd><dt>CPU cores</dt><dd>${h.cores}</dd>
    <dt>Video encoder</dt><dd>${esc(h.encoder.name)} ${h.encoder.hardware ? '(GPU)' : '(CPU)'}</dd>
    <dt>Vigil memory</dt><dd>${s.vigil_mem_mb} MB</dd><dt>Version</dt><dd>${esc(s.version)}</dd></dl></div>
  <h2 style="margin-top:20px">Storage</h2>
  <div class="card pad"><div class="bar"><i style="width:${d.total_bytes ? used / d.total_bytes * 100 : 0}%"></i></div>
    <div class="sub">${fmtBytes(used)} used of ${fmtBytes(d.total_bytes)} · ${fmtBytes(d.free_bytes)} free</div></div>
  <h2 style="margin-top:20px">Recommended setup</h2>
  <div class="card pad"><b>${esc(h.recommendation.summary)}</b><ul>${h.recommendation.tips.map(t => `<li>${esc(t)}</li>`).join('')}</ul></div>`;
}

// ---------- Router ----------
async function route() {
  timers.forEach(clearInterval); timers = []; view.onclick = null;
  const [, a, b, c] = location.hash.split('/');
  document.querySelectorAll('nav a').forEach(n => n.classList.toggle('on',
    n.dataset.t === (a === 'clips' ? 'clips' : a === 'system' ? 'system' : 'cams')));
  try {
    if (a === 'camera') await cameraForm(b);
    else if (a === 'clips') await clipsView(b);
    else if (a === 'system') await systemView();
    else await camerasView();
  } catch (e) { view.innerHTML = `<div class="empty err">${esc(e.message)}</div>`; }
  window.scrollTo(0, 0);
}
addEventListener('hashchange', route);
route();
if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(() => { });
