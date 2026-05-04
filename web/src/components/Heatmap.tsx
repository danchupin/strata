import { useEffect, useMemo, useRef, useState } from 'react';

// Heatmap renders a `time x label` grid via raw `<canvas>`. Custom over
// @nivo/heatmap to fit the 500 KiB gz bundle budget. Log-scale cell color.

export interface HeatmapPoint { ts: string; value: number }
export interface HeatmapRow { label: string; values: HeatmapPoint[] }
export interface HeatmapClick {
  label: string; ts: number; value: number;
  cellStartTs: number; cellEndTs: number;
}
interface HeatmapProps {
  rows: HeatmapRow[];
  rowHeight?: number;
  onCellClick?: (c: HeatmapClick) => void;
  formatTime?: (epochMs: number) => string;
}

const LABEL_W = 180;
const AXIS_H = 24;
const PAD = 8;

function color(value: number, maxLog: number): string {
  if (!Number.isFinite(value) || value <= 0) return 'hsl(220 14% 96% / 0)';
  const t = maxLog > 0 ? Math.min(1, Math.log1p(value) / maxLog) : 0;
  return `hsl(${(220 - t * 220).toFixed(0)} 80% ${(92 - t * 44).toFixed(0)}%)`;
}

function uniqueTimestamps(rows: HeatmapRow[]): number[] {
  const set = new Set<number>();
  for (const r of rows) for (const p of r.values) {
    const t = Date.parse(p.ts);
    if (Number.isFinite(t)) set.add(t);
  }
  return Array.from(set).sort((a, b) => a - b);
}

export function Heatmap({
  rows,
  rowHeight = 22,
  onCellClick,
  formatTime,
}: HeatmapProps) {
  const wrapRef = useRef<HTMLDivElement | null>(null);
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const [width, setWidth] = useState(800);
  const [hover, setHover] = useState<HeatmapClick | null>(null);

  const xs = useMemo(() => uniqueTimestamps(rows), [rows]);
  const matrix = useMemo(() => {
    const idx = new Map<number, number>();
    xs.forEach((t, i) => idx.set(t, i));
    return rows.map((r) => {
      const cells = new Array<number>(xs.length).fill(NaN);
      for (const p of r.values) {
        const i = idx.get(Date.parse(p.ts));
        if (i != null) cells[i] = p.value;
      }
      return { label: r.label, cells };
    });
  }, [rows, xs]);
  const maxLog = useMemo(() => {
    let m = 0;
    for (const r of matrix)
      for (const v of r.cells)
        if (Number.isFinite(v) && v > 0) {
          const l = Math.log1p(v);
          if (l > m) m = l;
        }
    return m;
  }, [matrix]);

  useEffect(() => {
    if (!wrapRef.current) return;
    const obs = new ResizeObserver((es) =>
      setWidth(Math.max(320, Math.floor(es[0]?.contentRect.width ?? 800))),
    );
    obs.observe(wrapRef.current);
    return () => obs.disconnect();
  }, []);

  const totalH = matrix.length * rowHeight + AXIS_H + PAD * 2;
  const gridW = Math.max(0, width - LABEL_W - PAD * 2);
  const cellW = xs.length > 0 ? gridW / xs.length : 0;

  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    const dpr = window.devicePixelRatio || 1;
    canvas.width = Math.floor(width * dpr);
    canvas.height = Math.floor(totalH * dpr);
    canvas.style.width = `${width}px`;
    canvas.style.height = `${totalH}px`;
    const ctx = canvas.getContext('2d');
    if (!ctx) return;
    ctx.scale(dpr, dpr);
    ctx.clearRect(0, 0, width, totalH);

    ctx.fillStyle = 'rgba(120,120,140,0.95)';
    ctx.font = '12px ui-monospace, SFMono-Regular, Menlo, monospace';
    ctx.textBaseline = 'middle';
    matrix.forEach((row, ri) => {
      let label = row.label;
      const max = LABEL_W - 8;
      if (ctx.measureText(label).width > max) {
        // crude single-pass trim: chop until it fits, then add ellipsis.
        const ratio = max / ctx.measureText(label).width;
        label = label.slice(0, Math.max(3, Math.floor(label.length * ratio) - 1)) + '…';
      }
      ctx.fillText(label, PAD, PAD + ri * rowHeight + rowHeight / 2);
    });

    matrix.forEach((row, ri) => {
      for (let ci = 0; ci < row.cells.length; ci++) {
        ctx.fillStyle = color(row.cells[ci], maxLog);
        ctx.fillRect(
          PAD + LABEL_W + ci * cellW,
          PAD + ri * rowHeight,
          Math.max(1, cellW - 1),
          rowHeight - 1,
        );
      }
    });

    if (xs.length > 0) {
      ctx.fillStyle = 'rgba(100,100,120,0.8)';
      ctx.font = '11px ui-sans-serif, system-ui, sans-serif';
      const fmt = formatTime ?? ((t: number) => new Date(t).toLocaleTimeString());
      const ticks = Array.from(new Set([0, Math.floor(xs.length / 2), xs.length - 1]));
      for (const i of ticks) {
        ctx.textAlign = i === 0 ? 'left' : i === xs.length - 1 ? 'right' : 'center';
        ctx.fillText(
          fmt(xs[i]),
          PAD + LABEL_W + i * cellW + cellW / 2,
          PAD + matrix.length * rowHeight + 14,
        );
      }
      ctx.textAlign = 'left';
    }
  }, [matrix, xs, width, totalH, cellW, rowHeight, maxLog, formatTime]);

  function pickCell(ev: React.MouseEvent<HTMLCanvasElement>): HeatmapClick | null {
    const rect = ev.currentTarget.getBoundingClientRect();
    const px = ev.clientX - rect.left;
    const py = ev.clientY - rect.top;
    if (px < PAD + LABEL_W || py < PAD || cellW <= 0) return null;
    if (py >= PAD + matrix.length * rowHeight) return null;
    const ci = Math.floor((px - PAD - LABEL_W) / cellW);
    const ri = Math.floor((py - PAD) / rowHeight);
    if (ci < 0 || ci >= xs.length || ri < 0 || ri >= matrix.length) return null;
    const ts = xs[ci];
    const next = ci + 1 < xs.length ? xs[ci + 1] : ts + (xs[1] - xs[0] || 60_000);
    return { label: matrix[ri].label, ts, value: matrix[ri].cells[ci], cellStartTs: ts, cellEndTs: next };
  }

  if (rows.length === 0 || xs.length === 0) {
    return (
      <div ref={wrapRef} className="flex h-56 items-center justify-center rounded-md border border-dashed border-border/60 bg-muted/30 text-sm text-muted-foreground">
        No data — try a different range or wait for the next poll.
      </div>
    );
  }

  const onClick = (e: React.MouseEvent<HTMLCanvasElement>) => {
    const c = pickCell(e);
    if (c && onCellClick) onCellClick(c);
  };

  return (
    <div ref={wrapRef} className="relative w-full">
      <canvas
        ref={canvasRef}
        role="img"
        aria-label="Hot buckets heatmap"
        onClick={onClick}
        onMouseMove={(e) => setHover(pickCell(e))}
        onMouseLeave={() => setHover(null)}
        className="cursor-pointer select-none"
      />
      {hover && (
        <div
          className="pointer-events-none absolute z-10 -translate-y-2 rounded-md border bg-background px-2 py-1 text-xs shadow"
          style={{ left: PAD + LABEL_W, top: PAD }}
        >
          <div className="font-mono">{hover.label}</div>
          <div className="text-muted-foreground">
            {new Date(hover.ts).toLocaleString()} · <span className="tabular-nums">{Number.isFinite(hover.value) ? hover.value.toFixed(2) : '—'}</span> req/s
          </div>
        </div>
      )}
    </div>
  );
}
