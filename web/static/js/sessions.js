// The recorded sessions panel: list, rename, delete, and open for replay.

import * as fmt from './fmt.js';

const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]);

export function initSessions(app) {
  const panel = document.getElementById('sessions');
  const button = document.getElementById('btn-sessions');

  async function load() {
    panel.innerHTML = '<h2>Recorded sessions</h2><div class="empty">Loading…</div>';
    let data;
    try {
      const resp = await fetch('/api/sessions');
      data = await resp.json();
      if (!resp.ok) throw new Error(data.error ?? `HTTP ${resp.status}`);
    } catch (err) {
      panel.innerHTML = `<h2>Recorded sessions</h2><div class="empty warn-text">${esc(err.message)}</div>`;
      return;
    }
    const rows = data.sessions.map((s) => {
      const open = s.end === 0;
      const length = open ? 'recording now' : fmt.dur((s.end - s.start) / 1000);
      return `<div class="session" data-id="${s.id}">
        <div class="name" title="Session name">${esc(s.name || fmt.dateTime(s.start))}</div>
        <div class="meta">${esc(fmt.dateTime(s.start))} · ${esc(length)} · ${s.snapshots} snapshots · ${esc(fmt.bytes(s.bytes))}
          <span class="tag ${s.coverage === 'viewport' ? '' : 'global'}">${esc(s.coverage)}</span></div>
        <div class="actions">
          <button class="btn primary" data-do="replay">Replay</button>
          <button class="btn" data-do="rename">Rename</button>
          <button class="btn" data-do="delete" ${open ? 'disabled title="Still being recorded"' : ''}>Delete</button>
        </div></div>`;
    });
    panel.innerHTML = `<h2><span>Recorded sessions</span><span class="meta">${esc(fmt.bytes(data.dbBytes))} on disk</span></h2>` +
      (rows.join('') || '<div class="empty">Nothing recorded yet. Sessions are recorded while someone is watching.</div>');
  }

  panel.addEventListener('click', async (e) => {
    const action = e.target.dataset?.do;
    const row = e.target.closest('.session');
    if (!action || !row) return;
    const id = row.dataset.id;
    if (action === 'replay') {
      panel.hidden = true;
      app.replay.open(id);
    } else if (action === 'rename') {
      const current = row.querySelector('.name').textContent;
      const name = window.prompt('Session name', current);
      if (name === null) return;
      await fetch(`/api/sessions/${id}`, { method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name }) });
      load();
    } else if (action === 'delete') {
      if (!window.confirm('Delete this session and everything recorded in it?')) return;
      const resp = await fetch(`/api/sessions/${id}`, { method: 'DELETE' });
      if (!resp.ok) window.alert((await resp.json()).error ?? 'Delete failed');
      load();
    }
  });

  button.addEventListener('click', () => {
    panel.hidden = !panel.hidden;
    if (!panel.hidden) load();
  });
  app.sessions = { load };
}
