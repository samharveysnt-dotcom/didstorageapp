import React from "react";
import { AbsoluteFill, useCurrentFrame, useVideoConfig } from "remotion";
import {
  AppFrame,
  GradientBg,
  H1,
  H2,
  Mono,
  Page,
  Pill,
  SceneCaption,
  Sidebar,
  TD,
  TR,
  Table,
  Topbar,
  useFadeInUp,
} from "../primitives";
import { theme } from "../theme";

/*
 * Scene 4 — CDR + profit + SIP trace diagram (15s).
 *
 * Beats:
 *   0-50    CDR list page enters. Top row "just landed" — has an
 *           `admin_action: redirect` pill and a profit cell.
 *   50-end  Cut to the SIP-trace SVG diagram for that same CDR. Arrows draw
 *           one-at-a-time across the lifelines. Final state holds for ~3s.
 */

const CDRS = [
  { id: 1, started: "13:42:18", did: "+1 415 555 0188", from: "+44 20 7946 0211", dur: "0m 42s", min: 1, rate: 0.014, cost: 0.008, profit: 0.006, cause: "Normal Call Clearing", admin: "redirect", admin_by: "admin@didstorage" },
  { id: 2, started: "13:42:11", did: "+44 20 7946 0114", from: "+49 30 2576 0833", dur: "1m 11s", min: 2, rate: 0.022, cost: 0.014, profit: 0.016, cause: "Normal Call Clearing" },
  { id: 3, started: "13:41:48", did: "+49 30 2576 0119", from: "+1 213 555 0184", dur: "3m 06s", min: 4, rate: 0.013, cost: 0.0075, profit: 0.022, cause: "Normal Call Clearing" },
  { id: 4, started: "13:41:22", did: "+63 2 8540 0223", from: "+1 415 555 0162", dur: "0m 09s", min: 0, rate: 0.045, cost: 0.032, profit: 0,    cause: "User Busy" },
  { id: 5, started: "13:40:55", did: "+1 800 555 0123", from: "+1 415 555 0177", dur: "2m 33s", min: 3, rate: 0.014, cost: 0.008, profit: 0.018, cause: "Normal Call Clearing" },
];

const TRACE_MSGS = [
  { from: "caller", to: "didstorage", label: "INVITE",       method: "request", t: "+0.000s", code: 0 },
  { from: "didstorage", to: "caller", label: "100 Trying",   method: "reply",   t: "+0.014s", code: 100 },
  { from: "didstorage", to: "customer", label: "INVITE",     method: "request", t: "+0.022s", code: 0 },
  { from: "customer", to: "didstorage", label: "100 Trying", method: "reply",   t: "+0.041s", code: 100 },
  { from: "customer", to: "didstorage", label: "180 Ringing",method: "reply",   t: "+0.318s", code: 180 },
  { from: "didstorage", to: "caller", label: "180 Ringing",  method: "reply",   t: "+0.325s", code: 180 },
  { from: "customer", to: "didstorage", label: "200 OK",     method: "reply",   t: "+2.108s", code: 200 },
  { from: "didstorage", to: "caller", label: "200 OK",       method: "reply",   t: "+2.115s", code: 200 },
  { from: "caller", to: "didstorage", label: "ACK",          method: "request", t: "+2.183s", code: 0 },
  { from: "didstorage", to: "customer", label: "ACK",        method: "request", t: "+2.196s", code: 0 },
  { from: "caller", to: "didstorage", label: "BYE",          method: "request", t: "+44.221s", code: 0 },
  { from: "didstorage", to: "customer", label: "BYE",        method: "request", t: "+44.231s", code: 0 },
  { from: "customer", to: "didstorage", label: "200 OK",     method: "reply",   t: "+44.281s", code: 200 },
  { from: "didstorage", to: "caller", label: "200 OK",       method: "reply",   t: "+44.294s", code: 200 },
];

export const CDRTrace: React.FC = () => {
  const frame = useCurrentFrame();
  const { durationInFrames } = useVideoConfig();

  const showTrace = frame >= 70;
  const frameEnter = useFadeInUp(0, 24);
  const captionAnim = useFadeInUp(8);
  const fadeOut = Math.max(0, Math.min(1, (durationInFrames - frame) / 14));

  return (
    <AbsoluteFill style={{ opacity: fadeOut }}>
      <GradientBg />

      <AbsoluteFill style={frameEnter}>
        {!showTrace ? <CDRListView frame={frame} /> : <TraceView frame={frame} />}
      </AbsoluteFill>

      <AbsoluteFill style={captionAnim}>
        <SceneCaption
          eyebrow={showTrace ? "Compliance" : "Billed at BYE"}
          title={
            showTrace
              ? "Every call, traced. Admin actions stamped."
              : "Profit lands on every CDR. Supplier cost snapshotted."
          }
          sub={
            showTrace
              ? "Sequence diagram from real pcaps. INVITE → 200 → BYE. The admin redirect is overlaid in-time."
              : "Margin computed at call-end. Rate-card swaps don't re-write history."
          }
        />
      </AbsoluteFill>
    </AbsoluteFill>
  );
};

/* ---------- View A: CDR list ---------- */

const CDRListView: React.FC<{ frame: number }> = ({ frame }) => {
  return (
    <AppFrame>
      <Sidebar active="/cdrs" />
      <Page>
        <Topbar />
        <H1>CDRs</H1>
        <H2>Last 24 hours · 312 calls · $4,812.40 billed · $1,948.20 profit</H2>
        <Table
          cols={["Started", "DID", "From", "Duration", "Rate", "Cost", "Profit", "Cause"]}
          colWidths={[110, 220, 220, 110, 90, 100, 110, 250]}
        >
          {CDRS.map((c, i) => {
            const rowEnter = Math.max(0, Math.min(1, (frame - i * 6) / 22));
            const isFeatured = i === 0;
            return (
              <TR
                key={c.id}
                style={{
                  opacity: rowEnter,
                  transform: `translateY(${(1 - rowEnter) * 10}px)`,
                  background: isFeatured
                    ? "rgba(88,101,242,0.06)"
                    : undefined,
                }}
              >
                <TD muted>{c.started}</TD>
                <TD>
                  <Mono>{c.did}</Mono>
                  {c.admin && (
                    <Pill kind="warn" style={{ marginLeft: 6 }}>
                      ↪ {c.admin}
                    </Pill>
                  )}
                </TD>
                <TD muted><Mono>{c.from}</Mono></TD>
                <TD style={{ fontVariantNumeric: "tabular-nums" }}>{c.dur}</TD>
                <TD style={{ fontVariantNumeric: "tabular-nums" }}>
                  ${c.rate.toFixed(4)}
                </TD>
                <TD muted style={{ fontVariantNumeric: "tabular-nums" }}>
                  ${c.cost.toFixed(4)}
                </TD>
                <TD
                  style={{
                    color: c.profit > 0 ? theme.ok : theme.muted,
                    fontVariantNumeric: "tabular-nums",
                  }}
                >
                  {c.profit > 0 ? `+$${c.profit.toFixed(4)}` : "—"}
                </TD>
                <TD muted>{c.cause}</TD>
              </TR>
            );
          })}
        </Table>
      </Page>
    </AppFrame>
  );
};

/* ---------- View B: SIP trace sequence diagram ---------- */

const TraceView: React.FC<{ frame: number }> = ({ frame }) => {
  // Animation begins as soon as we cut into this view (frame 70).
  const traceFrame = frame - 70;

  // Lifelines.
  const lifelines = [
    { id: "caller",     label: "Caller",        sub: "203.0.113.10",       x: 320 },
    { id: "didstorage", label: "DIDStorage",    sub: "45.8.93.244",        x: 760 },
    { id: "customer",   label: "Customer leg",  sub: "10.0.99.4",          x: 1200 },
  ];

  const xOf = (id: string) =>
    lifelines.find((l) => l.id === id)?.x ?? 760;

  // Each message takes 6 frames to slide in across its arrow.
  const messageInterval = 6;
  const messagesVisible = Math.floor(traceFrame / messageInterval);

  // Vertical spacing between messages.
  const topY = 220;
  const dyPer = 36;

  // Admin-action marker — horizontal dashed bar overlay at frame 60+ on this view
  const adminMarkerY = topY + dyPer * 10 - 6; // just before BYE
  const adminVisible = traceFrame >= messageInterval * 11;
  return (
    <AppFrame>
      <Sidebar active="/cdrs" />
      <Page>
        <Topbar />
        <div style={{ display: "flex", alignItems: "center", gap: 12, marginBottom: 4 }}>
          <H1 style={{ marginBottom: 0 }}>SIP trace</H1>
          <Pill kind="warn">admin redirect · admin@didstorage</Pill>
          <Pill kind="active">200 OK · normal clearing</Pill>
        </div>
        <div style={{ color: theme.muted, fontSize: 12.5, marginBottom: 16 }}>
          Call-ID <Mono>8f4a-3b2c-9e11@didstorage</Mono> · 42.281s · billed 1 min · profit{" "}
          <span style={{ color: theme.ok, fontWeight: 600 }}>+$0.006</span>
        </div>

        <div
          style={{
            background: theme.card,
            border: `1px solid ${theme.border}`,
            borderRadius: 8,
            padding: 14,
            position: "relative",
            height: 700,
          }}
        >
          <svg
            viewBox="0 0 1500 700"
            preserveAspectRatio="xMidYMid meet"
            style={{ width: "100%", height: "100%" }}
          >
            {/* Lifeline columns */}
            {lifelines.map((l) => (
              <g key={l.id}>
                <rect
                  x={l.x - 90}
                  y={120}
                  width={180}
                  height={42}
                  rx={6}
                  fill={theme.border}
                  stroke={theme.border}
                />
                <text
                  x={l.x}
                  y={142}
                  textAnchor="middle"
                  fontFamily={theme.font}
                  fontSize={15}
                  fontWeight={600}
                  fill={theme.text}
                >
                  {l.label}
                </text>
                <text
                  x={l.x}
                  y={158}
                  textAnchor="middle"
                  fontFamily={theme.mono}
                  fontSize={11}
                  fill={theme.muted}
                >
                  {l.sub}
                </text>
                <line
                  x1={l.x}
                  y1={170}
                  x2={l.x}
                  y2={680}
                  stroke={theme.border}
                  strokeWidth={2}
                  strokeDasharray="4 4"
                />
              </g>
            ))}

            {/* Admin redirect marker */}
            {adminVisible && (
              <g>
                <line
                  x1={120}
                  y1={adminMarkerY}
                  x2={1400}
                  y2={adminMarkerY}
                  stroke={theme.warn}
                  strokeWidth={2}
                  strokeDasharray="6 6"
                  opacity={0.85}
                />
                <rect
                  x={1280}
                  y={adminMarkerY - 18}
                  width={140}
                  height={22}
                  rx={4}
                  fill="rgba(240,178,50,0.18)"
                  stroke={theme.warn}
                />
                <text
                  x={1350}
                  y={adminMarkerY - 4}
                  textAnchor="middle"
                  fontFamily={theme.font}
                  fontSize={11.5}
                  fontWeight={600}
                  fill={theme.warn}
                >
                  ↪ admin redirect
                </text>
              </g>
            )}

            {/* Messages */}
            {TRACE_MSGS.slice(0, messagesVisible + 1).map((m, i) => {
              const x1 = xOf(m.from);
              const x2 = xOf(m.to);
              const y = topY + i * dyPer;
              const isRequest = m.method === "request";
              const color =
                m.code >= 200 && m.code < 300
                  ? theme.ok
                  : m.code >= 100 && m.code < 200
                    ? theme.accentText
                    : theme.text;
              const draw = Math.max(
                0,
                Math.min(1, (traceFrame - i * messageInterval) / messageInterval),
              );
              const x2Actual = x1 + (x2 - x1) * draw;
              const arrowLeft = x2 > x1;
              return (
                <g key={i} opacity={draw}>
                  <line
                    x1={x1}
                    y1={y}
                    x2={x2Actual}
                    y2={y}
                    stroke={color}
                    strokeWidth={2}
                    strokeDasharray={isRequest ? "0" : "5 4"}
                  />
                  {draw > 0.95 && (
                    <polygon
                      points={
                        arrowLeft
                          ? `${x2 - 8},${y - 5} ${x2},${y} ${x2 - 8},${y + 5}`
                          : `${x2 + 8},${y - 5} ${x2},${y} ${x2 + 8},${y + 5}`
                      }
                      fill={color}
                    />
                  )}
                  <text
                    x={(x1 + x2) / 2}
                    y={y - 8}
                    textAnchor="middle"
                    fontFamily={theme.mono}
                    fontSize={12}
                    fontWeight={600}
                    fill={color}
                    opacity={draw}
                  >
                    {m.label}
                  </text>
                  <text
                    x={arrowLeft ? x1 - 8 : x1 + 8}
                    y={y + 4}
                    textAnchor={arrowLeft ? "end" : "start"}
                    fontFamily={theme.mono}
                    fontSize={10}
                    fill={theme.muted}
                    opacity={draw}
                  >
                    {m.t}
                  </text>
                </g>
              );
            })}
          </svg>
        </div>
      </Page>
    </AppFrame>
  );
};
