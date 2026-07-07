export const theme = {
  bg: "#313338",
  card: "#2b2d31",
  text: "#dbdee1",
  muted: "#949ba4",
  accent: "#5865f2",
  accentText: "#9aa6ff",
  ok: "#23a55a",
  warn: "#f0b232",
  err: "#da373c",
  border: "#1e1f22",
  sidebar: "#1e1f22",
  sidebarActive: "#404249",
  rowHover: "#383a40",
  tooltip: "#111214",
  shadow: "0 4px 16px rgba(0,0,0,.5)",
  font:
    'Inter, ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif',
  mono: 'ui-monospace, SFMono-Regular, Menlo, "Cascadia Code", monospace',
} as const;

export const VIDEO = {
  width: 1920,
  height: 1080,
  fps: 30,
} as const;

export const TIMING = {
  opening: 5 * 30,
  dashboard: 20 * 30,
  suppliers: 20 * 30,
  liveCalls: 25 * 30,
  cdrTrace: 15 * 30,
  endCard: 5 * 30,
} as const;

export const TOTAL_FRAMES =
  TIMING.opening +
  TIMING.dashboard +
  TIMING.suppliers +
  TIMING.liveCalls +
  TIMING.cdrTrace +
  TIMING.endCard;
