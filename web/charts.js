// 패널 렌더러: SVG 차트와 원자료 표. 분모 30 미만은 흐리게 표시한다.

export const SMALL_N = 30;

export function esc(t) {
  return String(t).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);
}
export function pct(x, d = 1) {
  return Number.isFinite(x) ? `${(x * 100).toFixed(d)}%` : '–';
}
const fmt = (v) => (v === null || v === undefined ? 'NULL' : typeof v === 'number' ? v.toLocaleString('ko-KR') : String(v));
const ratio = (n, d) => (d > 0 ? n / d : NaN);
const cmp = (a, b) => (typeof a === 'number' && typeof b === 'number' ? a - b : String(a) < String(b) ? -1 : String(a) > String(b) ? 1 : 0);

function columnsOf(view) {
  const idx = new Map(view.columns.map((c, i) => [c.name, i]));
  const col = (role) => view.display[role] ?? null;
  const get = (row, role) => row[idx.get(col(role))];
  return { idx, col, get };
}

function rateCell(num, den, cls = '') {
  const small = den < SMALL_N;
  return `<td class="${small ? 'small ' : ''}${cls}">${pct(ratio(num, den))}<span class="frac">${fmt(num)}/${fmt(den)}</span>${small ? '<span class="tag">n&lt;30</span>' : ''}</td>`;
}

function legendHtml(series) {
  if (series.length < 2) return '';
  return `<div class="legend">${series.map((s, i) => `<span><i class="sw${i + 1}"></i>${esc(s)}</span>`).join('')}</div>`;
}

// ── line · bar ──────────────────────────────────────────

function xyData(view) {
  const { col, get } = columnsOf(view);
  const series = [];
  const xs = [];
  const map = new Map();
  for (const r of view.rows) {
    const s = col('series') ? fmt(get(r, 'series')) : '';
    const x = get(r, 'x');
    if (!map.has(s)) { map.set(s, new Map()); series.push(s); }
    map.get(s).set(fmt(x), r);
    if (!xs.some((v) => fmt(v) === fmt(x))) xs.push(x);
  }
  xs.sort(cmp);
  return { series, xs: xs.map(fmt), map, get };
}

function niceMax(v) {
  if (!(v > 0)) return 0.1;
  const steps = [0.05, 0.1, 0.2, 0.25, 0.4, 0.5, 0.6, 0.8, 1];
  return steps.find((s) => s >= v) ?? Math.ceil(v * 10) / 10;
}

function xyChart(view, title) {
  const type = view.display.type;
  const { series, xs, map, get } = xyData(view);
  const W = 720, H = 260, L = 48, R = 16, T = 14, B = 34;
  let maxR = 0;
  for (const s of series) for (const r of map.get(s).values()) maxR = Math.max(maxR, ratio(get(r, 'numerator'), get(r, 'denominator')) || 0);
  const ymax = niceMax(maxR);
  const n = xs.length;
  const slot = (W - L - R) / Math.max(n, 1);
  const x = (i) => L + slot * (i + 0.5);
  const y = (v) => T + (1 - v / ymax) * (H - T - B);
  let s = `<svg class="chart" viewBox="0 0 ${W} ${H}" role="img" aria-label="${esc(title)}">`;
  for (let g = 0; g <= 4; g++) {
    const v = (ymax * g) / 4;
    const yy = y(v).toFixed(1);
    s += `<line class="grid" x1="${L}" x2="${W - R}" y1="${yy}" y2="${yy}"/><text x="${L - 6}" y="${+yy + 3}" text-anchor="end">${Math.round(v * 100)}%</text>`;
  }
  const every = Math.ceil(n / 10);
  xs.forEach((lb, i) => {
    if (i % every === 0 || i === n - 1) s += `<text x="${x(i).toFixed(1)}" y="${H - 12}" text-anchor="middle">${esc(lb)}</text>`;
  });
  if (type === 'bar') {
    const k = series.length;
    const bw = Math.min(24, (slot * 0.72) / k);
    series.forEach((se, j) => {
      xs.forEach((lb, i) => {
        const r = map.get(se).get(lb);
        if (!r) return;
        const num = get(r, 'numerator'), den = get(r, 'denominator'), v = ratio(num, den) || 0;
        const x0 = x(i) - (k * bw) / 2 + j * bw + 1, w = Math.max(bw - 2, 1), y0 = y(v), h = y(0) - y0;
        const rr = Math.min(4, w / 2, h);
        const d = h <= 0 ? '' : `M${x0},${y(0)} V${y0 + rr} Q${x0},${y0} ${x0 + rr},${y0} H${x0 + w - rr} Q${x0 + w},${y0} ${x0 + w},${y0 + rr} V${y(0)} Z`;
        s += `<path d="${d}" class="f${j + 1}" opacity="${den < SMALL_N ? 0.35 : 1}"/>`;
        s += `<rect class="hit" x="${x0}" y="${T}" width="${w}" height="${H - T - B}"><title>${esc(`${lb}${se ? ` · ${se}` : ''}: ${pct(v)} (${fmt(num)}/${fmt(den)})${den < SMALL_N ? ' · n<30' : ''}`)}</title></rect>`;
      });
    });
  } else {
    series.forEach((se, j) => {
      const pts = xs.map((lb, i) => {
        const r = map.get(se).get(lb);
        return r ? { i, lb, num: get(r, 'numerator'), den: get(r, 'denominator') } : null;
      }).filter(Boolean);
      s += `<polyline class="ln s${j + 1}" points="${pts.map((p) => `${x(p.i).toFixed(1)},${y(ratio(p.num, p.den) || 0).toFixed(1)}`).join(' ')}"/>`;
      for (const p of pts) {
        const cy = y(ratio(p.num, p.den) || 0);
        if (n <= 24) s += `<circle class="dot f${j + 1}" cx="${x(p.i).toFixed(1)}" cy="${cy.toFixed(1)}" r="4" opacity="${p.den < SMALL_N ? 0.4 : 1}"/>`;
        s += `<circle class="hit" cx="${x(p.i).toFixed(1)}" cy="${cy.toFixed(1)}" r="10"><title>${esc(`${p.lb}${se ? ` · ${se}` : ''}: ${pct(ratio(p.num, p.den))} (${fmt(p.num)}/${fmt(p.den)})${p.den < SMALL_N ? ' · n<30' : ''}`)}</title></circle>`;
      }
    });
  }
  return `${legendHtml(series)}<div class="chartbox">${s}</svg></div>`;
}

function xyTable(view) {
  const { series, xs, map, get } = xyData(view);
  const extra = view.display.extra ?? [];
  const idx = new Map(view.columns.map((c, i) => [c.name, i]));
  const xName = view.display.x;
  let h = `<div class="scroll"><table><thead><tr><th>${esc(xName)}</th>${series.map((s) => `<th>${esc(s || '비율')}</th>`).join('')}${series.length === 1 ? extra.map((e) => `<th>${esc(e)}</th>`).join('') : ''}</tr></thead><tbody>`;
  for (const lb of xs) {
    h += `<tr><td>${esc(lb)}</td>`;
    for (const se of series) {
      const r = map.get(se).get(lb);
      h += r ? rateCell(get(r, 'numerator'), get(r, 'denominator')) : '<td class="small">–</td>';
    }
    if (series.length === 1) {
      const r = map.get(series[0]).get(lb);
      for (const e of extra) h += `<td class="num">${r ? esc(fmt(r[idx.get(e)])) : '–'}</td>`;
    }
    h += '</tr>';
  }
  return `${h}</tbody></table></div>`;
}

// ── funnel ──────────────────────────────────────────────

function funnelGroups(view) {
  const { col, get } = columnsOf(view);
  const groups = new Map();
  for (const r of view.rows) {
    const g = col('cohort') ? fmt(get(r, 'cohort')) : '';
    if (!groups.has(g)) groups.set(g, []);
    groups.get(g).push(r);
  }
  for (const list of groups.values()) list.sort((a, b) => get(a, 'step_no') - get(b, 'step_no'));
  const keys = [...groups.keys()].sort(cmp);
  return { groups, keys, get, hasCohort: !!col('cohort') };
}

function funnelChart(view, title, cohort) {
  const { groups, get } = funnelGroups(view);
  const rows = groups.get(cohort) ?? [];
  const W = 720, L = 130, R = 190, H = 16 + rows.length * 36;
  const base = rows.length ? get(rows[0], 'reached') : 0;
  let s = `<svg class="chart" viewBox="0 0 ${W} ${H}" role="img" aria-label="${esc(title)}">`;
  rows.forEach((r, i) => {
    const yy = 8 + i * 36, reached = get(r, 'reached'), eligible = get(r, 'eligible'), unknown = get(r, 'unknown');
    const w = base > 0 ? ((W - L - R) * reached) / base : 0;
    const conv = ratio(reached, eligible);
    s += `<text x="0" y="${yy + 16}">${esc(fmt(get(r, 'step_name')))}</text>`;
    s += `<rect class="track" x="${L}" y="${yy}" width="${W - L - R}" height="24" rx="4"/>`;
    s += `<rect class="f1" x="${L}" y="${yy}" width="${Math.max(w, 0).toFixed(1)}" height="24" rx="4" opacity="${eligible < SMALL_N ? 0.4 : 1}"/>`;
    s += `<text x="${W - R + 10}" y="${yy + 16}">${fmt(reached)}${i ? ` · ${pct(conv)}` : ''}${unknown ? ` · 판정 불가 ${fmt(unknown)}` : ''}</text>`;
    s += `<rect class="hit" x="${L}" y="${yy}" width="${W - L - R}" height="24"><title>${esc(`${fmt(get(r, 'step_name'))}: ${fmt(reached)} 도달 · 전환율 ${pct(conv)} (${fmt(reached)}/${fmt(eligible)}) · 처음 대비 ${pct(ratio(reached, base))}`)}</title></rect>`;
  });
  return `<div class="chartbox">${s}</svg></div>`;
}

function funnelTable(view, cohort) {
  const { groups, get } = funnelGroups(view);
  const rows = groups.get(cohort) ?? [];
  const base = rows.length ? get(rows[0], 'reached') : 0;
  let h = '<div class="scroll"><table><thead><tr><th>단계</th><th>도달</th><th>전환율 (도달/분모)</th><th>판정 불가</th><th>처음 대비</th></tr></thead><tbody>';
  for (const r of rows) {
    h += `<tr><td>${esc(`${get(r, 'step_no')}. ${fmt(get(r, 'step_name'))}`)}</td><td class="num">${fmt(get(r, 'reached'))}</td>${rateCell(get(r, 'reached'), get(r, 'eligible'))}<td class="num">${fmt(get(r, 'unknown'))}</td><td>${pct(ratio(get(r, 'reached'), base))}</td></tr>`;
  }
  return `${h}</tbody></table></div>`;
}

// ── cohort (히트맵 표) ───────────────────────────────────

function cohortData(view) {
  const { col, get } = columnsOf(view);
  const series = [];
  for (const r of view.rows) {
    const s = col('series') ? fmt(get(r, 'series')) : '';
    if (!series.includes(s)) series.push(s);
  }
  return { series, get, hasSeries: !!col('series'), hasDeleted: !!col('deleted_n') };
}

function heatStep(v, max) {
  if (!Number.isFinite(v) || max <= 0) return null;
  return Math.min(7, Math.max(1, Math.ceil((v / max) * 7)));
}

function cohortTable(view, series) {
  const { get, hasSeries } = cohortData(view);
  const rows = view.rows.filter((r) => !hasSeries || fmt(get(r, 'series')) === series);
  const cohorts = [...new Set(rows.map((r) => fmt(get(r, 'cohort'))))].sort(cmp);
  const periods = [...new Set(rows.map((r) => get(r, 'period')))].sort((a, b) => a - b);
  const cell = new Map(rows.map((r) => [`${fmt(get(r, 'cohort'))}|${get(r, 'period')}`, r]));
  let max = 0;
  for (const r of rows) if (get(r, 'denominator') >= SMALL_N) max = Math.max(max, ratio(get(r, 'numerator'), get(r, 'denominator')) || 0);
  let h = `<div class="scroll"><table class="heat"><thead><tr><th>${esc(view.display.cohort)}</th>${periods.map((p) => `<th>${esc(`${view.display.period} ${p}`)}</th>`).join('')}</tr></thead><tbody>`;
  for (const c of cohorts) {
    h += `<tr><td>${esc(c)}</td>`;
    for (const p of periods) {
      const r = cell.get(`${c}|${p}`);
      if (!r) { h += '<td class="empty">·</td>'; continue; }
      const num = get(r, 'numerator'), den = get(r, 'denominator'), v = ratio(num, den), small = den < SMALL_N;
      const step = small ? null : heatStep(v, max);
      h += `<td class="cell${small ? ' small' : ''}${step ? ` h${step}` : ''}" title="${esc(`${c} · ${view.display.period} ${p}: ${pct(v)} (${fmt(num)}/${fmt(den)})${small ? ' · n<30, 색 없음' : ''}`)}">${pct(v, 0)}<span class="frac">${fmt(num)}/${fmt(den)}</span></td>`;
    }
    h += '</tr>';
  }
  return `${h}</tbody></table></div>`;
}

// ── number · table ──────────────────────────────────────

function numberCard(view, headline) {
  const { col, get } = columnsOf(view);
  const r = view.rows[0] ?? [];
  if (col('value')) return `<div class="numcard"><span class="big">${esc(fmt(get(r, 'value')))}</span>${col('label') ? `<span class="muted">${esc(fmt(get(r, 'label')))}</span>` : ''}</div>`;
  const num = get(r, 'numerator'), den = get(r, 'denominator');
  return `<div class="numcard"><span class="big${den < SMALL_N ? ' dim' : ''}">${pct(ratio(num, den))}</span><span class="muted">${fmt(num)} / ${fmt(den)}${col('label') ? ` · ${esc(fmt(get(r, 'label')))}` : ''}${den < SMALL_N ? ' <span class="tag">n&lt;30</span>' : ''}</span></div>`;
}

export function plainTable(view, limit = 500) {
  const rows = view.rows.slice(0, limit);
  let h = `<div class="scroll"><table><thead><tr>${view.columns.map((c) => `<th>${esc(c.name)}</th>`).join('')}</tr></thead><tbody>`;
  for (const r of rows) h += `<tr>${r.map((v) => `<td>${esc(fmt(v))}</td>`).join('')}</tr>`;
  h += '</tbody></table></div>';
  if (view.rows.length > limit) h += `<p class="muted note-sm">${view.rows.length}행 중 ${limit}행만 표시</p>`;
  return h;
}

const isTimeLike = (v) => typeof v === 'number' || /^\d{4}-\d{2}(-\d{2})?/.test(String(v));

/** table 패턴 자동 차트: 첫 칸이 x, 나머지 수 칸(6개까지)이 값. 시간 축이면 선, 범주면 가로 막대 */
export function tableChart(view, title) {
  const { columns, rows } = view;
  const key = view.display.key ?? [];
  if (rows.length < 2 || columns.length < 2 || key.length > 1) return null;
  const xs = rows.map((r) => r[0]);
  if (xs.some((v) => v === null) || new Set(xs.map((v) => typeof v)).size > 1) return null;
  const numIdx = columns.map((_, i) => i).slice(1).filter((i) => rows.every((r) => r[i] === null || typeof r[i] === 'number') && rows.some((r) => typeof r[i] === 'number')).slice(0, 6);
  if (!numIdx.length) return null;
  // 한 축에 그리므로 첫 값과 크기가 10배 넘게 다른 칸은 표에만 둔다
  const peak = (i) => Math.max(...rows.map((r) => Math.abs(typeof r[i] === 'number' ? r[i] : 0)));
  const base = peak(numIdx[0]);
  const drawn = numIdx.filter((i) => { const m = peak(i); return base === 0 || m === 0 ? i === numIdx[0] : m / base <= 10 && base / m <= 10; });
  const hidden = numIdx.length - drawn.length;
  const names = [columns[0].name, ...drawn.map((i) => columns[i].name)];
  const sub = rows.map((r) => [r[0], ...drawn.map((i) => r[i])]);
  const note = hidden ? `<p class="muted note-sm">크기가 크게 다른 값 ${hidden}개는 표에만 있습니다.</p>` : '';
  if (xs.every(isTimeLike)) return countLineChart(names, [...sub].sort((a, b) => cmp(a[0], b[0])), title) + note;
  if (rows.length > 30) return null;
  return barRows(names, sub, title) + note;
}

function barRows(names, rows, title) {
  const first = 1;
  const max = Math.max(0, ...rows.map((r) => (typeof r[first] === 'number' ? r[first] : 0)));
  const W = 720, L = 160, R = 90, H = 10 + rows.length * 28;
  let s = `<svg class="chart" viewBox="0 0 ${W} ${H}" role="img" aria-label="${esc(title)}">`;
  rows.forEach((r, i) => {
    const y = 6 + i * 28;
    const v = typeof r[first] === 'number' ? r[first] : 0;
    const w = max > 0 ? ((W - L - R) * Math.max(v, 0)) / max : 0;
    s += `<text x="0" y="${y + 14}">${esc(fmt(r[0]).slice(0, 22))}</text>`;
    s += `<rect class="f1" x="${L}" y="${y}" width="${w.toFixed(1)}" height="18" rx="4"/>`;
    s += `<text x="${(L + w + 6).toFixed(1)}" y="${y + 14}">${esc(fmt(r[first]))}</text>`;
    s += `<rect class="hit" x="${L}" y="${y}" width="${W - L - R}" height="18"><title>${esc(`${fmt(r[0])}: ${names.slice(1).map((n, j) => `${n} ${fmt(r[j + 1])}`).join(' · ')}`)}</title></rect>`;
  });
  const note = names.length > 2 ? `<p class="muted note-sm">막대는 ${esc(names[1])} 기준. 나머지 값은 표에 있습니다.</p>` : '';
  return `<div class="chartbox">${s}</svg></div>${note}`;
}

/** state = 선택한 코호트·series. controls = 선택 상자가 필요하면 선택지 */
export function renderPanel(view, title, state = {}) {
  const type = view.display.type;
  if (type === 'line' || type === 'bar') return { html: xyChart(view, title) + xyTable(view), controls: null };
  if (type === 'funnel') {
    const { keys, hasCohort } = funnelGroups(view);
    const cohort = state.cohort && keys.includes(state.cohort) ? state.cohort : keys.includes('ALL') ? 'ALL' : keys.at(-1);
    return { html: funnelChart(view, title, cohort) + funnelTable(view, cohort), controls: hasCohort ? { name: 'cohort', label: view.display.cohort, options: keys, value: cohort } : null };
  }
  if (type === 'cohort') {
    const { series, hasSeries } = cohortData(view);
    const s = state.series && series.includes(state.series) ? state.series : series[0];
    return { html: cohortTable(view, s), controls: hasSeries ? { name: 'series', label: view.display.series, options: series, value: s } : null };
  }
  if (type === 'number') return { html: numberCard(view) + plainTable(view), controls: null };
  return { html: (tableChart(view, title) ?? '') + plainTable(view), controls: null };
}

/** 대시보드 카드용 작은 차트. 표만 있는 패턴은 null */
export function thumbChart(view, title) {
  const type = view.display.type;
  if (type === 'line' || type === 'bar') return xyChart(view, title).replace(/^<div class="legend">.*?<\/div>/, '');
  if (type === 'funnel') {
    const { keys } = funnelGroups(view);
    return funnelChart(view, title, keys.includes('ALL') ? 'ALL' : keys.at(-1));
  }
  if (type === 'table') return tableChart(view, title)?.replace(/^<div class="legend">.*?<\/div>/, '') ?? null;
  return null;
}

/** 품질 검사용 수 선 그래프: 첫 칸이 x, 나머지 수 칸이 series(6개까지) */
export function countLineChart(columns, rows, title) {
  const names = columns.slice(1, 7);
  const W = 720, H = 220, L = 48, R = 16, T = 14, B = 34;
  let max = 0;
  for (const r of rows) for (let j = 1; j <= names.length; j++) if (typeof r[j] === 'number') max = Math.max(max, r[j]);
  const ymax = max > 0 ? max : 1;
  const n = rows.length;
  const x = (i) => L + ((W - L - R) * (n <= 1 ? 0.5 : i / (n - 1)));
  const y = (v) => T + (1 - v / ymax) * (H - T - B);
  let s = `<svg class="chart" viewBox="0 0 ${W} ${H}" role="img" aria-label="${esc(title)}">`;
  for (let g = 0; g <= 4; g++) {
    const v = (ymax * g) / 4;
    const yy = y(v).toFixed(1);
    s += `<line class="grid" x1="${L}" x2="${W - R}" y1="${yy}" y2="${yy}"/><text x="${L - 6}" y="${+yy + 3}" text-anchor="end">${esc(fmt(Math.round(v)))}</text>`;
  }
  const every = Math.ceil(n / 8);
  rows.forEach((r, i) => {
    if (i % every === 0 || i === n - 1) s += `<text x="${x(i).toFixed(1)}" y="${H - 12}" text-anchor="middle">${esc(fmt(r[0]))}</text>`;
  });
  names.forEach((_, j) => {
    const pts = rows.map((r, i) => (typeof r[j + 1] === 'number' ? `${x(i).toFixed(1)},${y(r[j + 1]).toFixed(1)}` : null)).filter(Boolean);
    s += `<polyline class="ln s${j + 1}" points="${pts.join(' ')}"/>`;
  });
  rows.forEach((r, i) => {
    s += `<rect class="hit" x="${(x(i) - (W - L - R) / Math.max(n, 1) / 2).toFixed(1)}" y="${T}" width="${((W - L - R) / Math.max(n, 1)).toFixed(1)}" height="${H - T - B}"><title>${esc(`${fmt(r[0])}: ${names.map((nm, j) => `${nm} ${fmt(r[j + 1])}`).join(' · ')}`)}</title></rect>`;
  });
  return `${legendHtml(names)}<div class="chartbox">${s}</svg></div>`;
}

export function headlineHtml(h) {
  if (!h) return '';
  const value = h.numerator === null ? fmt(h.value) : pct(h.value);
  return `<div class="dd-headline"><span class="big${h.lowN ? ' dim' : ''}">${esc(value)}</span><span class="muted">${esc(h.label)}${h.numerator !== null ? ` · ${fmt(h.numerator)}/${fmt(h.denominator)}` : ''}${h.lowN ? ' <span class="tag">n&lt;30</span>' : ''}</span></div>`;
}
