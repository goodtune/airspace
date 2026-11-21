// Formatting helpers used by the Datastar expressions in index.html.

export function int(n) {
  return Number.isFinite(n) ? Math.round(n).toLocaleString('en') : '–';
}

export function num(n) {
  if (!Number.isFinite(n)) return '–';
  return String(Math.round(n * 100) / 100);
}

export function dur(seconds) {
  if (!(seconds >= 0)) return 'unknown';
  if (seconds < 90) return `${Math.round(seconds)} s`;
  const m = Math.round(seconds / 60);
  if (m < 90) return `${m} min`;
  return `${Math.floor(m / 60)} h ${String(m % 60).padStart(2, '0')} min`;
}

export function clock(ms) {
  if (!ms) return '–';
  const d = new Date(ms);
  const t = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  const sameDay = d.toDateString() === new Date().toDateString();
  return sameDay ? t : `${t} ${d.toLocaleDateString([], { weekday: 'short' })}`;
}

export function dateTime(ms) {
  return new Date(ms).toLocaleString([], { dateStyle: 'medium', timeStyle: 'medium' });
}

export function bytes(n) {
  if (n >= 1e9) return `${(n / 1e9).toFixed(2)} GB`;
  if (n >= 1e6) return `${(n / 1e6).toFixed(1)} MB`;
  return `${Math.max(1, Math.round(n / 1e3))} kB`;
}

export function age(fetched, now) {
  if (!fetched) return 'no data yet';
  return `${Math.max(0, Math.round((now - fetched) / 1000))} s`;
}

// Data is stale when it is much older than the poll interval.
export function stale(fetched, interval, now, state) {
  if (!fetched) return false;
  if (state !== 'polling') return false;
  return now - fetched > Math.max(3 * interval, interval + 20) * 1000;
}

export function stateLabel(state, fetched, interval, now) {
  if (state === 'paused') return 'PAUSED';
  if (state === 'stopped') return 'STOPPED';
  return stale(fetched, interval, now, state) ? 'STALE DATA' : 'LIVE';
}

export function scope(global, cost, area) {
  if (global) return 'whole world';
  if (!area) return '–';
  const boxes = area.boxes === 1 ? '1 box' : `${area.boxes} boxes`;
  return `${boxes}, ${Math.round(area.sqdeg)} sq°`;
}

export function bounds(min, max) {
  return max > 0 ? `allowed ${num(min)} to ${num(max)} s` : `minimum ${num(min)} s`;
}
