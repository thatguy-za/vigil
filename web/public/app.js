const $ = s => document.querySelector(s);
const view = $('#view');
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
let timers = [];
const later = (fn, ms) => { const t = setInterval(fn, ms); timers.push(t); };
const leaving = []; // run (and cleared) when the page changes
const onLeave = fn => leaving.push(fn);

async function api(path, opts = {}) {
  const r = await fetch('/api' + path, {
    ...opts, headers: opts.body ? { 'Content-Type': 'application/json' } : {},
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  if (r.status === 204) return null;
  const j = await r.json().catch(() => ({}));
  if (r.status === 401 && !path.startsWith('/auth/')) { boot(); throw new Error('Signed out'); }
  if (!r.ok) throw new Error(j.error || r.statusText);
  return j;
}

function toast(msg) {
  const t = Object.assign(document.createElement('div'), { className: 'toast' });
  t.innerHTML = `<span>${esc(msg)}</span><button type="button" class="x" aria-label="Dismiss">×</button>`;
  t.querySelector('.x').onclick = () => t.remove();
  document.body.append(t); setTimeout(() => t.remove(), 5000);
}

// Modal sheet with an X. By default closing goes back to the home page.
function closeSheets() { document.querySelectorAll('.sheet').forEach(e => e.remove()); document.body.classList.remove('noscroll'); }
function sheet(title, html, onClose = () => { location.hash = '#/'; }) {
  closeSheets();
  const el = document.createElement('div'); el.className = 'sheet';
  el.innerHTML = `<div class="panel" role="dialog" aria-label="${esc(title)}"><div class="ph"><h2>${esc(title)}</h2><button type="button" class="x" aria-label="Close">×</button></div><div class="pb">${html}</div></div>`;
  el._close = onClose;
  el.querySelector('.x').onclick = onClose;
  el.onclick = e => { if (e.target === el) onClose(); };
  document.body.append(el); document.body.classList.add('noscroll');
  return el;
}

const drawer = $('#drawer');
const closeDrawer = () => drawer.classList.remove('open');
$('#menu').onclick = () => drawer.classList.add('open');
drawer.querySelector('.scrim').onclick = closeDrawer;
drawer.querySelector('.x').onclick = () => { closeDrawer(); location.hash = '#/'; };
drawer.querySelectorAll('a').forEach(a => a.addEventListener('click', closeDrawer));
document.addEventListener('keydown', e => {
  if (e.key !== 'Escape') return;
  const s = document.querySelector('.sheet');
  s ? s._close() : closeDrawer();
});
$('#logout').onclick = () => { closeDrawer(); signOut(); };

const MODES = { off: 'Off', continuous: '24x7', schedule: 'Schedule', motion: 'Motion' };
const pill = s => `<span class="pill ${s.state}">${{ recording: 'Recording', watching: 'Watching', connecting: 'Connecting', error: 'Error', off: 'Off', idle: 'Idle' }[s.state]}</span>`;
const fmtBytes = b => b > 2 ** 30 ? (b / 2 ** 30).toFixed(1) + ' GB' : b > 2 ** 20 ? (b / 2 ** 20).toFixed(0) + ' MB' : Math.round(b / 1024) + ' KB';
const fmtTime = d => new Date(d).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });

// ---------- Live video ----------
// Plays /live.mp4 through Media Source (H.264 only). If that isn't possible, or the
// stream keeps dropping, it shows a fresh still every 2 s instead.
function livePlayer(vid, img, camId, badge) {
  const MS = window.ManagedMediaSource || window.MediaSource;
  let gen = 0, ctrl = null, snapTimer = null, retry = null;
  const setBadge = live => { if (badge) { badge.className = 'badge live'; badge.textContent = live ? 'LIVE' : 'STILLS'; } };
  const snap = () => {
    if (img.dataset.busy) return;
    img.dataset.busy = 1;
    const n = new Image();
    n.onload = () => { img.src = n.src; delete img.dataset.busy; };
    n.onerror = () => delete img.dataset.busy;
    n.src = `/api/cameras/${camId}/snapshot?t=${Date.now()}`;
  };
  const showStills = () => { vid.hidden = true; img.hidden = false; setBadge(false); snap(); snapTimer ||= setInterval(snap, 2000); };
  const stopStills = () => { clearInterval(snapTimer); snapTimer = null; };

  async function video(myGen) {
    ctrl = new AbortController();
    const resp = await fetch(`/api/cameras/${camId}/live.mp4`, { signal: ctrl.signal });
    if (!resp.ok) {
      const e = new Error((await resp.json().catch(() => ({}))).error || resp.status);
      e.fatal = resp.status === 415;
      throw e;
    }
    const type = `video/mp4; codecs="${resp.headers.get('X-Codec')}"`;
    if (!MS || !MS.isTypeSupported(type)) { const e = new Error('This browser can\'t play the stream'); e.fatal = true; throw e; }
    const ms = new MS();
    vid.disableRemotePlayback = true;
    vid.src = URL.createObjectURL(ms);
    await new Promise(r => ms.addEventListener('sourceopen', r, { once: true }));
    const sb = ms.addSourceBuffer(type);
    sb.mode = 'sequence';
    const queue = [];
    const pump = () => { if (!sb.updating && queue.length) { try { sb.appendBuffer(queue.shift()); } catch { queue.length = 0; } } };
    sb.addEventListener('updateend', () => {
      const b = sb.buffered;
      if (b.length) {
        const end = b.end(b.length - 1);
        if (end - vid.currentTime > 2.5) vid.currentTime = end - 0.4; // stay near the live edge
        if (end - b.start(0) > 20) { try { sb.remove(0, end - 10); return; } catch { } }
      }
      pump();
    });
    const reader = resp.body.getReader();
    let shown = false;
    while (gen === myGen) {
      const { done, value } = await reader.read();
      if (done) break;
      queue.push(value); pump();
      if (!shown) { shown = true; vid.hidden = false; img.hidden = true; stopStills(); setBadge(true); vid.play().catch(() => { }); }
    }
  }
  async function loop(myGen) {
    showStills(); // something to look at while the video connects
    for (let attempt = 0; attempt < 4 && gen === myGen; attempt++) {
      try { await video(myGen); }
      catch (e) { if (e.name === 'AbortError' || gen !== myGen) return; if (e.fatal) break; }
      if (gen !== myGen) return;
      showStills();
      await new Promise(r => retry = setTimeout(r, 2000 + attempt * 2000));
    }
    if (gen === myGen) showStills();
  }
  return {
    start() { gen++; loop(gen); },
    stop() { gen++; ctrl?.abort(); clearTimeout(retry); stopStills(); vid.pause(); vid.removeAttribute('src'); vid.load(); },
  };
}

// ---------- Cameras ----------
async function camerasView() {
  const admin = me.role === 'admin';
  const [cams, found] = await Promise.all([api('/cameras'), admin ? api('/discovery').catch(() => []) : []]);
  const fresh = found.filter(d => d.status === 'new');
  const foundHtml = fresh.length ? `<div class="found card pad" id="homefound"><h2>Found on your network</h2>${fresh.map(d => `
    <div class="fhome" data-d="${esc(d.id)}"><div><b>${esc(d.name)}</b><div class="sub">${esc(d.host)}</div></div>
    <div class="fa"><button class="pri" data-add>Add</button><button data-dis>Dismiss</button></div></div>`).join('')}</div>` : '';
  view.onclick = async e => {
    const row = e.target.closest('.fhome'); if (!row) return;
    if (e.target.dataset.add !== undefined) location.hash = '#/camera?d=' + encodeURIComponent(row.dataset.d);
    if (e.target.dataset.dis !== undefined) {
      try { await api(`/discovery/${row.dataset.d}/dismiss`, { method: 'POST' }); } catch (err) { return toast(err.message); }
      row.remove();
      if (!$('#homefound .fhome')) $('#homefound').remove();
    }
  };
  if (!cams.length) {
    if (!admin) { view.innerHTML = '<div class="empty"><h2>No cameras</h2><p>An admin needs to add one.</p></div>'; return; }
    view.innerHTML = foundHtml + `<div class="empty"><h2>No cameras</h2>
      <p>Add one to start recording.</p>
      <a class="btn pri" href="#/camera">Add camera</a></div>`;
    return;
  }
  view.innerHTML = `<div class="head"><h2>Cameras</h2>${admin ? '<a class="btn" href="#/camera">Add camera</a>' : ''}</div>` + foundHtml +
    `<div class="grid">${cams.map(c => `
    <div class="card cam">
      <div class="thumb"><span>no picture</span><img data-id="${c.id}" alt="">
        <a class="cover" href="#/live/${c.id}" aria-label="Live view of ${esc(c.name)}"></a>
        <div class="tl-top">${pill(c.status)}${admin ? `<a class="tedit" href="#/camera/${c.id}">Edit</a>` : ''}</div>
        <div class="tl-bot"><span>${esc(c.name)}</span><span class="mono">${MODES[c.mode]}${c.transcode ? ', transcoded' : ''}</span></div>
      </div>
      ${c.status.state === 'error' ? `<div class="tile-err err">${esc(c.status.error)}</div>` : ''}
    </div>`).join('')}</div>`;
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

// ---------- Live page ----------
async function liveView(id) {
  const cam = (await api('/cameras')).find(c => c.id == id);
  if (!cam) { view.innerHTML = '<div class="empty">Camera not found.</div>'; return; }
  view.innerHTML = `<div class="head"><h2>${esc(cam.name)}</h2>${pill(cam.status)}</div>
    <div class="player"><img alt=""><video muted autoplay playsinline hidden></video><span class="badge live" id="badge">LIVE</span></div>
    <div class="evnav"><button type="button" id="fs">Fullscreen</button><a class="btn" href="#/clips/${cam.id}">Recordings</a>${me.role === 'admin' ? `<a class="btn" href="#/camera/${cam.id}">Edit</a>` : ''}</div>`;
  const box = view.querySelector('.player'), v = box.querySelector('video');
  const lp = livePlayer(v, box.querySelector('img'), cam.id, $('#badge'));
  lp.start(); onLeave(lp.stop);
  $('#fs').onclick = () => { if (!v.hidden && v.webkitEnterFullscreen) v.webkitEnterFullscreen(); else (box.requestFullscreen || box.webkitRequestFullscreen)?.call(box); };
}

// ---------- Add / edit camera ----------
const DAYS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];
// Tap/drag painting for a grid of cells with data-i; arr holds '0'/'1' per index.
function paintGrid(g, arr) {
  let paint = null;
  const cell = (x, y) => { const el = document.elementFromPoint(x, y); return el && g.contains(el) && el.dataset.i !== undefined ? el : null; };
  const set = el => { if (el && paint !== null) { arr[el.dataset.i] = paint ? '1' : '0'; el.classList.toggle('on', paint); } };
  g.onpointerdown = e => { const el = cell(e.clientX, e.clientY); if (el) { paint = arr[el.dataset.i] !== '1'; set(el); g.setPointerCapture(e.pointerId); } };
  g.onpointermove = e => set(cell(e.clientX, e.clientY));
  g.onpointerup = g.onpointercancel = () => paint = null;
}
const MASK_CELLS = 16 * 9;
const dow = col => (col + 1) % 7; // grid columns start Monday, schedule index starts Sunday

let linkedDiscovery = null; // discovered device the camera being added came from
let foundSel = null;        // device picked from "Found on your network"

// Adding a camera is two steps: 1) the camera's address and login, which Vigil
// uses to find the streams, 2) streams, transcoding and recording details.
// Editing an existing camera shows step 2 only.
async function cameraForm(id, preselect) {
  linkedDiscovery = null; foundSel = null;
  const [sys, existing] = await Promise.all([api('/system'), id ? api('/cameras/' + id) : null]);
  const c = existing || { name: '', url: '', sub_url: '', mode: 'continuous', schedule: '0'.repeat(168), transcode: false, max_height: 720, audio: false, retention_days: 14, motion_mask: '' };
  if (c.schedule.length !== 168) c.schedule = '0'.repeat(168);
  const enc = sys.hw.encoder;
  sheet(id ? 'Edit camera' : 'Add camera', `
  <form id="f" autocomplete="off" novalidate>
    ${id ? '' : `<div class="stepper"><span class="on" id="st1"></span><span id="st2"></span></div>
    <div id="stepname" class="sub" style="margin-bottom:12px"></div>
    <div id="s1">
      <div id="found"><div class="sub">Scanning the network…</div></div>
      <label for="host" style="margin-top:8px">Address <small>IP or hostname</small></label>
      <input id="host" type="text" placeholder="192.168.1.20" autocapitalize="off" autocomplete="off" inputmode="url">
      <div class="two">
        <div><label for="cu">Username</label><input id="cu" type="text" value="admin" autocapitalize="off" autocomplete="off"></div>
        <div><label for="cp">Password</label><input id="cp" type="password" autocomplete="new-password"></div>
      </div>
      <button type="button" class="pri" id="detect" style="width:100%;margin-top:14px">Connect</button>
      <div id="dmsg" style="margin-top:12px"></div>
      <div style="margin-top:12px"><button type="button" class="link" id="manual">Enter stream URLs by hand</button></div>
    </div>`}
    <div id="s2" ${id ? '' : 'hidden'}>
      <label for="name" style="margin-top:0">Name</label>
      <input id="name" type="text" value="${esc(c.name)}" placeholder="Front door">
      <div id="prev"></div>
      <details id="urls" ${id ? 'open' : ''}>
        <summary>Stream URLs</summary>
        <label for="url">Main stream <small>RTSP</small></label>
        <input id="url" type="url" value="${esc(c.url)}" placeholder="rtsp://user:pass@192.168.1.20:554/stream1" inputmode="url" autocapitalize="off">
        <label for="sub">Sub-stream <small>optional, low resolution</small></label>
        <input id="sub" type="url" value="${esc(c.sub_url)}" placeholder="rtsp://user:pass@192.168.1.20:554/stream2" inputmode="url" autocapitalize="off">
        <div class="actions" style="margin-top:12px"><button type="button" id="test">Test connection</button></div>
      </details>
      <label>Recording</label>
      <div class="seg" id="mode">${['off', 'continuous', 'schedule', 'motion'].map(m =>
        `<button type="button" data-m="${m}" class="${c.mode === m ? 'on' : ''}">${MODES[m]}</button>`).join('')}</div>
      <div id="sched" hidden></div>
      <div id="motion" hidden>
        <label for="sens">Sensitivity <small>higher picks up smaller movement</small></label>
        <input id="sens" type="range" min="1" max="10" value="${c.motion_sensitivity || 5}" style="width:100%">
        <label>Ignore zones <small>paint over trees, timestamps, roads</small></label>
        <div class="maskbox"><img id="maskimg" alt=""><div class="mask" id="mask">${Array.from({ length: MASK_CELLS }, (_, i) => `<div data-i="${i}"></div>`).join('')}</div></div>
        <div class="actions" style="margin-top:8px"><button type="button" id="maskclr">Clear</button></div>
        <div class="sub" style="margin-top:8px">Analysis runs on the ${sys.hw.hw_decode ? 'GPU' : 'CPU'}; a sub-stream makes it much cheaper. Clips run from 5 s before to 10 s after the motion.</div>
      </div>
      <div class="sw"><span>Record audio<small>Saved as AAC.</small></span><input id="audio" type="checkbox" ${c.audio ? 'checked' : ''}></div>
      <div class="sw"><span>Transcode video<small>Off: the camera's stream is saved as is, which is cheapest. On: re-encode to H.264 on the ${enc.hardware ? 'GPU' : 'CPU'} (${esc(enc.name)}).</small></span><input id="tc" type="checkbox" ${c.transcode ? 'checked' : ''}></div>
      <div id="tcopt" hidden><label for="mh">Max height</label>
        <select id="mh">${[480, 720, 1080].map(h => `<option ${c.max_height === h ? 'selected' : ''}>${h}</option>`).join('')}</select></div>
      <label for="ret">Keep recordings for <small>(Days. 0 = disabled)</small></label>
      <input id="ret" type="number" min="0" max="3650" value="${c.retention_days}" inputmode="numeric">
      <div id="err" class="err"></div>
      <div class="actions">${id ? '' : '<button type="button" id="back">Back</button>'}<button class="pri" id="save" type="submit">Save</button>${id ? '<button type="button" class="del" id="del">Delete</button>' : ''}</div>
    </div>
  </form>`);

  // ----- step 2 controls -----
  let mode = c.mode, sched = c.schedule.split('');
  const mask = (c.motion_mask?.length === MASK_CELLS ? c.motion_mask : '0'.repeat(MASK_CELLS)).split('');
  const drawMask = () => $('#mask').childNodes.forEach((el, i) => el.classList.toggle('on', mask[i] === '1'));
  drawMask(); paintGrid($('#mask'), mask);
  $('#maskclr').onclick = () => { mask.fill('0'); drawMask(); };
  if (id) $('#maskimg').src = `/api/cameras/${id}/snapshot?t=${Date.now()}`;
  const upd = () => {
    $('#sched').hidden = mode !== 'schedule';
    $('#motion').hidden = mode !== 'motion';
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
    paintGrid($('#grid'), sched);
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
      const blobUrl = URL.createObjectURL(await r.blob());
      $('#maskimg').src = blobUrl;
      $('#prev').innerHTML = `<div class="preview"><img src="${blobUrl}" alt="Camera preview"></div>`;
    } catch (err) { $('#prev').innerHTML = `<div class="err" style="margin-top:8px">No picture: ${esc(err.message)}</div>`; }
    b.disabled = false; b.textContent = 'Test connection';
  };

  // ----- step switching (new cameras only) -----
  let step = id ? 2 : 1, advance = null;
  const NAMES = ['Step 1 of 2: camera login', 'Step 2 of 2: streams and recording'];
  const show = n => {
    step = n;
    if (id) return;
    clearTimeout(advance);
    $('#s1').hidden = n !== 1; $('#s2').hidden = n !== 2;
    $('#st1').classList.toggle('on', true); $('#st2').classList.toggle('on', n === 2);
    $('#stepname').textContent = NAMES[n - 1];
    document.querySelector('.panel').scrollTop = 0;
  };
  const toStep2 = manual => {
    if (!$('#name').value.trim()) $('#name').value = foundSel?.name || ('Camera ' + ($('#host').value.trim() || ''));
    $('#urls').open = manual || !$('#url').value;
    show(2);
    if ($('#url').value.trim() && !$('#prev').innerHTML) $('#test').click();
  };

  if (!id) {
    show(1);
    foundSection(preselect);
    $('#back').onclick = () => show(1);
    $('#manual').onclick = () => toStep2(true);
    $('#detect').onclick = async e => {
      const b = e.target, msg = $('#dmsg'), host = $('#host').value.trim();
      if (!host) { msg.innerHTML = '<div class="err">Enter the camera\'s address.</div>'; $('#host').focus(); return; }
      b.disabled = true; b.textContent = 'Connecting…';
      const steps = [];
      const draw = (state, text) => {
        msg.innerHTML = '<div class="steps">' + steps.map((s, i) => `<div class="step ${i === steps.length - 1 && !state ? 'run' : 'done'}"><i></i><span>${esc(s)}</span></div>`).join('') +
          (state ? `<div class="step ${state}"><i></i><span>${esc(text)}</span></div>` : '<div class="sub" style="margin-top:6px">Can take up to 30 s on a Pi.</div>') + '</div>';
      };
      steps.push('Starting'); draw();
      let final = null, ok = false;
      try {
        const resp = await fetch('/api/cameras/detect', { method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ host, username: $('#cu').value, password: $('#cp').value, discovery_id: foundSel && foundSel.host === host ? foundSel.id : '' }) });
        if (resp.status === 401) { boot(); throw new Error('Signed out'); }
        if (!resp.ok) throw new Error((await resp.json().catch(() => ({}))).error || 'Something went wrong');
        const reader = resp.body.getReader(), dec = new TextDecoder();
        let buf = '';
        for (;;) {
          const { done, value } = await reader.read();
          if (value) buf += dec.decode(value, { stream: true });
          let nl;
          while ((nl = buf.indexOf('\n')) >= 0) {
            const line = buf.slice(0, nl).trim(); buf = buf.slice(nl + 1);
            if (!line) continue;
            const ev = JSON.parse(line);
            if (ev.step) { steps.push(ev.step); draw(); } else final = ev;
          }
          if (done) break;
        }
        if (!final) throw new Error('The connection was interrupted. Try again.');
        if (final.error) throw new Error(final.error);
        const r = final.result;
        $('#url').value = r.url; $('#sub').value = r.sub_url; $('#prev').innerHTML = '';
        linkedDiscovery = foundSel && foundSel.host === host ? foundSel.id : null;
        const d = x => x ? ` (${x})` : '';
        draw('ok', `Main stream${d(r.main_info)}. ${r.sub_url ? 'Sub-stream' + d(r.sub_info) + '.' : 'No sub-stream.'}`);
        ok = true;
      } catch (err) {
        draw('bad', err.message);
        if (/username or password/i.test(err.message)) $('#cp').focus();
      }
      b.disabled = false; b.textContent = 'Connect';
      if (ok) advance = setTimeout(() => toStep2(false), 1200); // let them read the result, then move on
    };
  }

  $('#f').onsubmit = async e => {
    e.preventDefault();
    if (!id && step === 1) { $('#detect').click(); return; } // Enter in step 1 = Connect
    const err = m => { $('#err').textContent = m; $('#save').disabled = false; };
    if (!$('#name').value.trim()) return err('Name is required.');
    if (!$('#url').value.trim()) { $('#urls').open = true; return err('Main stream URL is required.'); }
    $('#save').disabled = true; $('#err').textContent = '';
    const body = { name: $('#name').value, url: $('#url').value, sub_url: $('#sub').value, mode, schedule: sched.join(''),
      transcode: $('#tc').checked, max_height: +$('#mh').value, audio: $('#audio').checked, retention_days: +$('#ret').value || 0, motion_sensitivity: +$('#sens').value, motion_mask: mask.includes('1') ? mask.join('') : '' };
    try { await api(id ? '/cameras/' + id : '/cameras' + (linkedDiscovery ? '?discovery=' + encodeURIComponent(linkedDiscovery) : ''), { method: id ? 'PUT' : 'POST', body }); location.hash = '#/'; }
    catch (e2) { err(e2.message); }
  };
  if (id) $('#del').onclick = () => {
    sheet('Delete camera', `<p>Delete <b>${esc(c.name)}</b>?</p>
      <div class="sw"><span>Also delete its recordings</span><input id="purge" type="checkbox"></div>
      <div class="actions"><button class="pri bad" id="dgo">Delete camera</button></div>`);
    $('#dgo').onclick = async () => {
      await api(`/cameras/${id}?purge=${$('#purge').checked ? 1 : 0}`, { method: 'DELETE' });
      location.hash = '#/';
    };
  };
}

// "Found on your network": ONVIF cameras that are new or were dismissed earlier.
// Picking one fills in its address; the login is entered below it.
async function foundSection(pre) {
  const box = $('#found');
  let items = [];
  const draw = () => {
    if (!items.length) { box.innerHTML = ''; return; }
    box.innerHTML = '<div class="found-h" style="margin-top:0"><b>Found on your network</b></div>' + items.map(d => `
      <div class="fcam ${foundSel?.id === d.id ? 'sel' : ''}">
        <button type="button" class="fpick" data-d="${esc(d.id)}"><span><b>${esc(d.name)}</b> <span class="sub">${esc(d.host)}${d.status === 'dismissed' ? ' · dismissed' : ''}</span></span><span class="sub">${foundSel?.id === d.id ? 'Selected' : 'Use'}</span></button>
      </div>`).join('');
  };
  const pick = id => {
    foundSel = items.find(d => d.id === id) || null;
    if (!foundSel) return;
    $('#host').value = foundSel.host;
    if (!$('#name').value) $('#name').value = foundSel.name;
    draw();
    $('#cp').focus();
  };
  box.onclick = e => { const t = e.target.closest('.fpick'); if (t) pick(t.dataset.d); };
  try { items = await api('/discovery'); } catch { }
  draw();
  if (pre) pick(pre);
}

// ---------- Recordings ----------
const fmtClock = ms => new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });

// Camera selector, then the camera view (live or playback), then a timeline.
async function clipsView(camId, date) {
  const cams = await api('/cameras');
  if (!cams.length) { view.innerHTML = '<div class="empty">No cameras.</div>'; return; }
  const cam = cams.find(c => c.id == camId) || cams[0];
  const r = await api(`/cameras/${cam.id}/recordings` + (date ? `?date=${date}` : ''));
  const evs = (r.date ? await api(`/cameras/${cam.id}/events?date=${r.date}`) : []).map(e => ({ ...e, ms: +new Date(e.start) }));
  const clips = r.clips.map(c => ({ ...c, ms: +new Date(c.start) })).sort((x, y) => x.ms - y.ms);
  const t0 = r.date ? +new Date(r.date + 'T00:00:00') : 0; // local midnight
  const pct = ms => (ms - t0) / 864000; // share of a 24 h day, in %
  const di = r.dates.indexOf(r.date);

  view.innerHTML = `<h2>Recordings</h2>
    <div class="chips">${cams.map(c => `<button class="${c.id === cam.id ? 'on' : ''}" data-c="${c.id}">${esc(c.name)}</button>`).join('')}</div>
    <div class="player"><img id="live" alt=""><video id="lv" muted autoplay playsinline hidden></video><video id="vid" controls playsinline hidden></video><span class="badge live" id="badge">LIVE</span></div>
    ${r.dates.length ? `
    <div class="tlbar"><button type="button" id="dprev" aria-label="Previous day" ${di >= r.dates.length - 1 ? 'disabled' : ''}>‹</button>
      <select id="date">${r.dates.map(d => `<option ${d === r.date ? 'selected' : ''}>${d}</option>`).join('')}</select>
      <button type="button" id="dnext" aria-label="Next day" ${di <= 0 ? 'disabled' : ''}>›</button></div>
    <div class="tl" id="tl">
      ${clips.map(c => `<i class="cv" style="left:${pct(c.ms)}%;width:${Math.max(c.duration / 864, .3)}%"></i>`).join('')}
      ${evs.map(e => `<i class="ev" style="left:${pct(e.ms)}%;width:${Math.max((new Date(e.end) - e.ms) / 864000, .5)}%"></i>`).join('')}
      <i class="playhead" id="head" hidden></i>
    </div>
    <div class="tlx"><span>00:00</span><span>06:00</span><span>12:00</span><span>18:00</span><span>24:00</span></div>
    <div class="sub" style="margin-top:6px"><span class="key cv"></span>recorded <span class="key ev"></span>motion. Tap the timeline to jump.</div>
    <div class="evnav"><button type="button" id="pe" ${evs.length ? '' : 'disabled'}>‹ Prev event</button><button type="button" id="golive">Live</button><button type="button" id="ne" ${evs.length ? '' : 'disabled'}>Next event ›</button></div>`
    : '<div class="empty">No recordings yet.</div>'}`;
  view.querySelector('.chips').onclick = e => e.target.dataset.c && (location.hash = `#/clips/${e.target.dataset.c}`);

  const vid = $('#vid'), lv = $('#lv'), img = $('#live'), badge = $('#badge');
  let cur = null;
  const lp = livePlayer(lv, img, cam.id, badge);
  lp.start(); onLeave(lp.stop);
  if (!r.dates.length) return;

  const head = $('#head'), tl = $('#tl');
  const setHead = ms => { head.hidden = false; head.style.left = pct(ms) + '%'; badge.textContent = fmtClock(ms); };
  const goLive = () => {
    cur = null; vid.pause(); vid.hidden = true; head.hidden = true; lp.start();
  };
  const play = (c, offset = 0) => {
    cur = c; lp.stop(); img.hidden = true; lv.hidden = true; vid.hidden = false; badge.className = 'badge';
    const seek = () => { vid.currentTime = Math.max(0, offset); };
    if (vid.getAttribute('src') !== c.path) { vid.src = c.path; vid.onloadedmetadata = seek; } else seek();
    setHead(c.ms + offset * 1000);
    vid.play().catch(() => { });
  };
  // A whole day is ~250 s per pixel on a phone, so snap a tap to the nearest clip within ~24 px.
  const playAt = ms => {
    const tol = 24 * 86400000 / tl.getBoundingClientRect().width;
    let best = null, bd = Infinity;
    for (const c of clips) {
      const end = c.ms + c.duration * 1000, d = ms < c.ms ? c.ms - ms : ms > end ? ms - end : 0;
      if (d < bd) { best = c; bd = d; }
    }
    if (!best || bd > tol) return toast('No recording near that time');
    play(best, bd === 0 ? (ms - best.ms) / 1000 : 0);
  };
  vid.ontimeupdate = () => cur && setHead(cur.ms + vid.currentTime * 1000);
  vid.onended = () => { // keep going into the next clip when recording was continuous
    const n = clips[clips.indexOf(cur) + 1];
    if (n && n.ms - (cur.ms + cur.duration * 1000) < 15000) play(n);
  };

  const msAt = e => { const b = tl.getBoundingClientRect(); return t0 + Math.min(1, Math.max(0, (e.clientX - b.left) / b.width)) * 86400000; };
  let drag = false;
  tl.onpointerdown = e => { drag = true; tl.setPointerCapture(e.pointerId); setHead(msAt(e)); };
  tl.onpointermove = e => drag && setHead(msAt(e));
  tl.onpointerup = e => { if (drag) { drag = false; playAt(msAt(e)); } };
  tl.onpointercancel = () => { drag = false; };

  const nowMs = () => cur ? cur.ms + vid.currentTime * 1000 : Date.now();
  const jump = ev => {
    const c = clips.find(c => ev.clip === c.path);
    if (!c) return toast('Clip deleted');
    play(c, Math.max(0, (ev.ms - c.ms) / 1000 - 2));
  };
  $('#ne').onclick = () => { const e = evs.find(e => e.ms > nowMs() + 1000); e ? jump(e) : toast('No later events today'); };
  $('#pe').onclick = () => { const e = [...evs].reverse().find(e => e.ms < nowMs() - 3000); e ? jump(e) : toast('No earlier events today'); };
  $('#golive').onclick = goLive;
  $('#date').onchange = e => clipsView(cam.id, e.target.value);
  $('#dprev').onclick = () => clipsView(cam.id, r.dates[di + 1]);
  $('#dnext').onclick = () => clipsView(cam.id, r.dates[di - 1]);
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

// ---------- Sign in / setup ----------
let me = null;

function authView(kind) {
  document.body.classList.add('auth'); closeSheets(); closeDrawer();
  const setup = kind === 'setup';
  view.innerHTML = `<div class="authbox card pad"><h2 style="margin-bottom:4px">${setup ? 'Create the first account' : 'Sign in'}</h2>
    ${setup ? '<div class="sub">It will be an admin. Add other users later from the menu.</div>' : ''}
    <form id="af">
      <label for="au">Username</label><input id="au" type="text" autocapitalize="off" autocomplete="username" required>
      <label for="ap">Password${setup ? ' <small>at least 8 characters</small>' : ''}</label>
      <input id="ap" type="password" autocomplete="${setup ? 'new-password' : 'current-password'}" required ${setup ? 'minlength="8"' : ''}>
      ${setup ? '<label for="ap2">Confirm password</label><input id="ap2" type="password" autocomplete="new-password" required>' : ''}
      <div class="err" id="ae"></div>
      <div class="actions"><button class="pri" id="ago">${setup ? 'Create account' : 'Sign in'}</button></div>
    </form></div>`;
  $('#au').focus();
  $('#af').onsubmit = async e => {
    e.preventDefault();
    if (setup && $('#ap').value !== $('#ap2').value) { $('#ae').textContent = 'Passwords do not match'; return; }
    $('#ago').disabled = true; $('#ae').textContent = '';
    try { await api(setup ? '/auth/setup' : '/auth/login', { method: 'POST', body: { username: $('#au').value.trim(), password: $('#ap').value } }); boot(); }
    catch (err) { $('#ae').textContent = err.message; $('#ago').disabled = false; }
  };
}

async function boot() {
  timers.forEach(clearInterval); timers = [];
  let st;
  try { st = await (await fetch('/api/auth/status')).json(); }
  catch { view.innerHTML = '<div class="empty err">Can\'t reach Vigil</div>'; return; }
  if (st.setup_required) { me = null; return authView('setup'); }
  if (!st.user) { me = null; return authView('login'); }
  me = st.user;
  document.body.classList.remove('auth');
  document.body.classList.toggle('viewer', me.role !== 'admin');
  $('#duser').textContent = `${me.username} · ${me.role}`;
  $('#dver').textContent = 'Vigil ' + st.version;
  route();
}

async function signOut() {
  try { await api('/auth/logout', { method: 'POST' }); } catch { }
  location.hash = '#/';
  boot();
}

// ---------- Users (admin) and account ----------
const ROLE_HELP = { admin: 'Full access.', viewer: 'Live view and recordings only.' };

async function usersView() {
  const us = await api('/users');
  view.innerHTML = `<div class="head"><h2>Users</h2><a class="btn" href="#/users/new">Add user</a></div><div class="users-list">${us.map(u => `<button class="clip card" data-u="${u.id}">
    <span><b>${esc(u.username)}</b>${u.id === me.id ? ' <span class="sub">(you)</span>' : ''}</span>
    <span class="pill ${u.role === 'admin' ? 'recording' : ''}">${u.role === 'admin' ? 'Admin' : 'Viewer'}</span></button>`).join('')}</div>
    `;
  view.onclick = e => { const b = e.target.closest('[data-u]'); if (b) location.hash = '#/users/' + b.dataset.u; };
}

const goUsers = () => location.hash === '#/users' ? route() : (location.hash = '#/users');

function userSheet(u) {
  const isNew = !u;
  sheet(isNew ? 'Add user' : u.username, `<form id="uf" autocomplete="off">
    ${isNew ? '<label for="un" style="margin-top:0">Username</label><input id="un" type="text" autocapitalize="off" required>' : ''}
    <label${isNew ? '' : ' style="margin-top:0"'}>Role</label>
    <div class="seg" id="role">${['viewer', 'admin'].map(r => `<button type="button" data-r="${r}" class="${(u?.role || 'viewer') === r ? 'on' : ''}">${r === 'admin' ? 'Admin' : 'Viewer'}</button>`).join('')}</div>
    <div class="sub" id="rolehelp" style="margin-top:6px"></div>
    <label for="up">${isNew ? 'Password' : 'New password'} <small>${isNew ? 'at least 8 characters' : 'leave blank to keep the current one'}</small></label>
    <input id="up" type="password" autocomplete="new-password" minlength="8" ${isNew ? 'required' : ''}>
    <div class="err" id="ue"></div>
    <div class="actions"><button class="pri" id="usave">Save</button>${!isNew && u.id !== me.id ? '<button type="button" class="del" id="udel">Delete</button>' : ''}</div>
  </form>`);
  let role = u?.role || 'viewer';
  const upd = () => {
    document.querySelectorAll('#role button').forEach(b => b.classList.toggle('on', b.dataset.r === role));
    $('#rolehelp').textContent = ROLE_HELP[role];
  };
  upd();
  $('#role').onclick = e => { if (e.target.dataset.r) { role = e.target.dataset.r; upd(); } };
  $('#uf').onsubmit = async e => {
    e.preventDefault(); $('#usave').disabled = true; $('#ue').textContent = '';
    try {
      if (isNew) await api('/users', { method: 'POST', body: { username: $('#un').value.trim(), password: $('#up').value, role } });
      else await api('/users/' + u.id, { method: 'PUT', body: { role, password: $('#up').value } });
      goUsers();
    } catch (err) { $('#ue').textContent = err.message; $('#usave').disabled = false; }
  };
  if (!isNew && u.id !== me.id) $('#udel').onclick = () => {
    sheet('Delete user', `<p>Delete <b>${esc(u.username)}</b>? They are signed out straight away.</p>
      <div class="err" id="de"></div><div class="actions"><button class="pri bad" id="dgo">Delete user</button></div>`);
    $('#dgo').onclick = async () => {
      try { await api('/users/' + u.id, { method: 'DELETE' }); goUsers(); } catch (err) { $('#de').textContent = err.message; }
    };
  };
}

function accountView() {
  view.innerHTML = `<h2>Account</h2>
    <div class="card pad"><dl><dt>Username</dt><dd>${esc(me.username)}</dd><dt>Role</dt><dd>${me.role === 'admin' ? 'Admin' : 'Viewer'}</dd></dl></div>
    <h2 style="margin-top:20px">Change password</h2>
    <form id="pf" class="card pad" autocomplete="off">
      <label for="pc" style="margin-top:0">Current password</label><input id="pc" type="password" autocomplete="current-password" required>
      <label for="pn">New password <small>at least 8 characters</small></label><input id="pn" type="password" autocomplete="new-password" required minlength="8">
      <label for="pn2">Confirm new password</label><input id="pn2" type="password" autocomplete="new-password" required>
      <div id="pm" class="err"></div>
      <div class="actions"><button class="pri" id="psave">Change password</button></div>
    </form>
    <div class="actions" style="margin-top:20px"><button type="button" class="del" id="so">Sign out</button></div>`;
  $('#so').onclick = signOut;
  $('#pf').onsubmit = async e => {
    e.preventDefault();
    const m = $('#pm'); m.className = 'err'; m.textContent = '';
    if ($('#pn').value !== $('#pn2').value) { m.textContent = 'Passwords do not match'; return; }
    $('#psave').disabled = true;
    try {
      await api('/auth/password', { method: 'POST', body: { current: $('#pc').value, new: $('#pn').value } });
      m.className = 'ok'; m.textContent = 'Password changed. Other sessions were signed out.'; $('#pf').reset();
    } catch (err) { m.textContent = err.message; }
    $('#psave').disabled = false;
  };
}

// ---------- Router ----------
async function route() {
  timers.forEach(clearInterval); timers = []; leaving.splice(0).forEach(f => f()); view.onclick = null; closeSheets(); closeDrawer();
  const [path, query] = location.hash.slice(1).split('?');
  const [, a, b] = path.split('/');
  const q = new URLSearchParams(query || '');
  if (me.role !== 'admin' && ['camera', 'system', 'users'].includes(a)) { location.hash = '#/'; return; }
  document.querySelectorAll('nav a').forEach(n => n.classList.toggle('on',
    n.dataset.t === (a === 'clips' ? 'clips' : a === 'system' ? 'system' : a === 'users' || a === 'account' ? '' : 'cams')));
  try {
    if (a === 'camera') { await camerasView(); await cameraForm(b, q.get('d')); }
    else if (a === 'clips') await clipsView(b);
    else if (a === 'live') await liveView(b);
    else if (a === 'system') await systemView();
    else if (a === 'users') {
      await usersView();
      if (b === 'new') userSheet(null);
      else if (b) { const t = (await api('/users')).find(u => u.id == b); if (t) userSheet(t); }
    }
    else if (a === 'account') accountView();
    else await camerasView();
  } catch (e) { view.innerHTML = `<div class="empty err">${esc(e.message)}</div>`; }
  window.scrollTo(0, 0);
}
addEventListener('hashchange', () => me && route());
boot();
if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(() => { });
