import React from "react";
import { AbsoluteFill, useCurrentFrame, useVideoConfig } from "remotion";
import {
  AppFrame,
  Button,
  GradientBg,
  H1,
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
import { Icon } from "../icons";

type Call = {
  id: string;
  started: string;
  ageStartFrame: number;
  did: string;
  didType: string;
  country: string;
  customer: string;
  customerId: string;
  reseller?: string;
  from: string;
  routeKind: string;
  routeTarget: string;
  srcIP: string;
  callId: string;
  redirected?: boolean;
};

const CALLS: Call[] = [
  {
    id: "c1",
    started: "13:42:11",
    ageStartFrame: 0,
    did: "+1 415 555 0188",
    didType: "local",
    country: "US",
    customer: "NimbusTalk",
    customerId: "usr_2f9a",
    reseller: "AtlasContact",
    from: "+44 20 7946 0211",
    routeKind: "sip_uri",
    routeTarget: "sip:nimbus@5.6.7.8",
    srcIP: "203.0.113.10",
    callId: "8f4a-3b2c-9e11",
  },
  {
    id: "c2",
    started: "13:41:48",
    ageStartFrame: 0,
    did: "+44 20 7946 0114",
    didType: "national",
    country: "UK",
    customer: "Helios CC",
    customerId: "usr_44b1",
    from: "+49 30 2576 0833",
    routeKind: "ip",
    routeTarget: "10.40.0.55:5060",
    srcIP: "203.0.113.11",
    callId: "11dd-77ee-bb22",
  },
  {
    id: "c3",
    started: "13:41:32",
    ageStartFrame: 0,
    did: "+49 30 2576 0119",
    didType: "national",
    country: "DE",
    customer: "AtlasContact",
    customerId: "usr_91c8",
    from: "+1 213 555 0184",
    routeKind: "sip_account",
    routeTarget: "atlas-de01",
    srcIP: "198.51.100.40",
    callId: "44aa-12cc-3344",
  },
  {
    id: "c4",
    started: "13:40:55",
    ageStartFrame: 0,
    did: "+63 2 8540 0223",
    didType: "national",
    country: "PH",
    customer: "NimbusTalk",
    customerId: "usr_2f9a",
    from: "+1 415 555 0162",
    routeKind: "ip",
    routeTarget: "172.16.4.21:5060",
    srcIP: "203.0.113.12",
    callId: "9012-cafe-beef",
  },
];

const ageOf = (frame: number, ageStartFrame: number) => {
  const secs = Math.max(0, Math.floor((frame - ageStartFrame) / 30));
  if (secs < 60) return `${secs}s`;
  return `${Math.floor(secs / 60)}m ${secs % 60}s`;
};

/*
 * Scene 3 — Live calls (25s, hero).
 *
 * Beats:
 *   0-40    Page enters. Active-calls table is visible with 3-4 ringing calls.
 *           Pulse dot animates. Age cells tick.
 *   40-90   Highlight halo around the "Redirect" button on the top row.
 *   90-160  Redirect modal opens. Form pre-fills. Halo highlights the "Redirect
 *           call" submit button. Modal closes.
 *  160-230  Top row's DID cell gains "↪ redirect" pill. Route target swaps
 *           from sip:nimbus@5.6.7.8 → sip:fraud-honeypot@10.0.99.4 with a
 *           highlight flash.
 *  230-end  Hold + caption swap.
 */

export const LiveCalls: React.FC = () => {
  const frame = useCurrentFrame();
  const { durationInFrames } = useVideoConfig();

  const frameEnter = useFadeInUp(0, 24);

  const showModal = frame >= 90 && frame < 160;
  const redirectApplied = frame >= 160;

  const fadeOut = Math.max(0, Math.min(1, (durationInFrames - frame) / 14));

  // Modal entry/exit animation.
  const modalT =
    frame < 90
      ? 0
      : frame >= 160
        ? 1 - Math.min(1, (frame - 160) / 10)
        : Math.min(1, (frame - 90) / 12);
  const modalScale = 0.94 + modalT * 0.06;
  const modalOpacity = modalT;

  // Backdrop opacity follows modal but caps lower.
  const backdropOpacity = modalT * 0.55;

  // Highlight: Redirect button on row 1 (frame 40-86)
  // Highlight: "Redirect call" submit (frame 120-150)
  // Highlight: route target cell after swap (frame 160-200)

  const captionAnim = useFadeInUp(8);
  const useSecondCaption = frame >= 200;

  return (
    <AbsoluteFill style={{ opacity: fadeOut }}>
      <GradientBg />

      <AbsoluteFill style={frameEnter}>
        <AppFrame>
          <Sidebar active="/live" />
          <Page>
            <Topbar />
            <div
              style={{
                display: "flex",
                justifyContent: "space-between",
                alignItems: "flex-start",
                marginBottom: 14,
              }}
            >
              <div>
                <div style={{ display: "flex", alignItems: "center", gap: 14 }}>
                  <H1 style={{ marginBottom: 0 }}>
                    Active calls{" "}
                    <span style={{ color: theme.muted, fontSize: 16, fontWeight: 500 }}>
                      ({CALLS.length})
                    </span>
                  </H1>
                </div>
                <div
                  style={{
                    display: "inline-flex",
                    alignItems: "center",
                    gap: 8,
                    color: theme.muted,
                    fontSize: 12,
                    marginTop: 6,
                  }}
                >
                  <span
                    style={{
                      width: 8,
                      height: 8,
                      borderRadius: 4,
                      background: theme.ok,
                      boxShadow: `0 0 0 ${
                        4 + (Math.sin(frame / 8) * 0.5 + 0.5) * 5
                      }px rgba(35,165,90,${
                        0.35 - (Math.sin(frame / 8) * 0.5 + 0.5) * 0.3
                      })`,
                    }}
                  />
                  streaming live · 1s
                </div>
              </div>
              <Button variant="ghost">
                <Icon.External color={theme.text} />
                History (CDRs)
              </Button>
            </div>

            <Table
              cols={[
                "Started",
                "Age",
                "DID",
                "Customer",
                "From",
                "Routed to",
                "Source IP",
                "Actions",
              ]}
              colWidths={[100, 80, 230, 200, 200, 270, 130, 240]}
            >
              {CALLS.map((c, i) => {
                const isFeatured = i === 0;
                const showRedirectPill = isFeatured && redirectApplied;
                const route = isFeatured && redirectApplied
                  ? { kind: "sip_uri", target: "sip:fraud-honeypot@10.0.99.4" }
                  : { kind: c.routeKind, target: c.routeTarget };
                // Pulse glow on the row-1 Redirect button between frame 40-90.
                const inRedirectWindow = isFeatured && frame >= 40 && frame < 90;
                const pulseT = inRedirectWindow ? (frame - 40) / 50 : 0;
                const pulse = (Math.sin((frame - 40) / 3) * 0.5 + 0.5);
                const glow = inRedirectWindow
                  ? (1 - Math.max(0, pulseT - 0.7) / 0.3) * (0.5 + pulse * 0.5)
                  : 0;
                const redirectBtnGlow = glow > 0
                  ? `0 0 0 ${3 + pulse * 4}px rgba(88,101,242,${glow * 0.7}), 0 0 24px rgba(88,101,242,${glow * 0.6})`
                  : undefined;
                return (
                  <TR key={c.id}>
                    <TD muted>{c.started}</TD>
                    <TD style={{ fontVariantNumeric: "tabular-nums" }}>
                      {ageOf(frame, c.ageStartFrame)}
                    </TD>
                    <TD>
                      <Mono>{c.did}</Mono>{" "}
                      <span style={{ color: theme.muted, fontSize: 11 }}>
                        · {c.didType} · {c.country}
                      </span>
                      {showRedirectPill && (
                        <Pill kind="warn" style={{ marginLeft: 8 }}>
                          ↪ redirect
                        </Pill>
                      )}
                    </TD>
                    <TD>
                      <span style={{ color: theme.accentText }}>{c.customer}</span>
                      {c.reseller && (
                        <span style={{ color: theme.muted, fontSize: 11 }}>
                          {" · "}
                          {c.reseller}
                        </span>
                      )}
                    </TD>
                    <TD muted><Mono>{c.from}</Mono></TD>
                    <TD
                      style={{
                        position: "relative",
                        color: theme.muted,
                        background:
                          isFeatured && frame >= 160 && frame < 220
                            ? "rgba(35,165,90,0.10)"
                            : undefined,
                      }}
                    >
                      <span
                        style={{
                          color: theme.muted,
                          fontSize: 11,
                          textTransform: "uppercase",
                          marginRight: 6,
                        }}
                      >
                        {route.kind}
                      </span>
                      <Mono>{route.target}</Mono>
                    </TD>
                    <TD muted><Mono>{c.srcIP}</Mono></TD>
                    <TD>
                      <div style={{ display: "flex", gap: 6 }}>
                        <Button variant="secondary" small>
                          <Icon.Warning size={12} />
                          Warn
                        </Button>
                        <Button
                          variant="secondary"
                          small
                          style={{ boxShadow: redirectBtnGlow, position: "relative" }}
                        >
                          <Icon.ArrowRight size={12} />
                          Redirect
                        </Button>
                        <Button variant="danger" small>
                          <Icon.X size={12} />
                          Hangup
                        </Button>
                      </div>
                    </TD>
                  </TR>
                );
              })}
            </Table>
          </Page>
        </AppFrame>

        {/* The Redirect modal — a separate floating dialog */}
        {(modalT > 0 || frame < 165) && (
          <>
            <div
              style={{
                position: "absolute",
                left: 0,
                top: 0,
                right: 0,
                bottom: 0,
                background: "rgba(0,0,0,0.6)",
                backdropFilter: "blur(2px)",
                opacity: backdropOpacity,
              }}
            />
            <div
              style={{
                position: "absolute",
                left: "50%",
                top: "50%",
                transform: `translate(-50%, -50%) scale(${modalScale})`,
                width: 620,
                background: theme.card,
                color: theme.text,
                border: `1px solid ${theme.border}`,
                borderRadius: 10,
                boxShadow: theme.shadow,
                opacity: modalOpacity,
                font: `14px/1.5 ${theme.font}`,
              }}
            >
              <div
                style={{
                  padding: "1rem 1.2rem",
                  borderBottom: `1px solid ${theme.border}`,
                  display: "flex",
                  alignItems: "center",
                  justifyContent: "space-between",
                }}
              >
                <div style={{ fontWeight: 600, fontSize: 16 }}>
                  Redirect call{" "}
                  <span style={{ color: theme.muted, fontWeight: 400, fontSize: 13 }}>
                    · +1 415 555 0188 · NimbusTalk
                  </span>
                </div>
                <Icon.X color={theme.muted} size={18} />
              </div>
              <div style={{ padding: "1.2rem" }}>
                <p style={{ color: theme.muted, fontSize: 13, marginTop: 0, lineHeight: 1.55 }}>
                  Drops the existing outbound leg and bridges the caller to a new
                  destination. The caller stays connected.
                </p>
                <div style={{ display: "flex", gap: 12, marginTop: 10 }}>
                  <div style={{ width: 200 }}>
                    <div
                      style={{
                        color: theme.muted,
                        fontSize: 11,
                        textTransform: "uppercase",
                        letterSpacing: 0.5,
                        marginBottom: 4,
                      }}
                    >
                      Route kind
                    </div>
                    <div
                      style={{
                        background: theme.border,
                        border: `1px solid ${theme.border}`,
                        borderRadius: 6,
                        padding: "0.5rem 0.7rem",
                        fontSize: 13,
                      }}
                    >
                      sip_uri ▾
                    </div>
                  </div>
                  <div style={{ flex: 1 }}>
                    <div
                      style={{
                        color: theme.muted,
                        fontSize: 11,
                        textTransform: "uppercase",
                        letterSpacing: 0.5,
                        marginBottom: 4,
                      }}
                    >
                      Route target
                    </div>
                    <div
                      style={{
                        background: theme.border,
                        border: `1px solid ${theme.accent}`,
                        borderRadius: 6,
                        padding: "0.5rem 0.7rem",
                        fontSize: 13,
                        fontFamily: theme.mono,
                      }}
                    >
                      {frame >= 105
                        ? "sip:fraud-honeypot@10.0.99.4"
                        : (() => {
                            const target = "sip:fraud-honeypot@10.0.99.4";
                            const reveal = Math.max(
                              0,
                              Math.min(target.length, frame - 92),
                            );
                            return target.slice(0, reveal);
                          })()}
                      <span
                        style={{
                          display: "inline-block",
                          width: 7,
                          height: 14,
                          background: theme.accent,
                          verticalAlign: "middle",
                          marginLeft: 2,
                          opacity:
                            frame < 110 && Math.floor(frame / 8) % 2 === 0
                              ? 1
                              : 0.2,
                        }}
                      />
                    </div>
                  </div>
                </div>
                <div style={{ marginTop: 14 }}>
                  <div
                    style={{
                      color: theme.muted,
                      fontSize: 11,
                      textTransform: "uppercase",
                      letterSpacing: 0.5,
                      marginBottom: 4,
                    }}
                  >
                    Reason — recorded in audit log
                  </div>
                  <div
                    style={{
                      background: theme.border,
                      border: `1px solid ${theme.border}`,
                      borderRadius: 6,
                      padding: "0.5rem 0.7rem",
                      fontSize: 13,
                      minHeight: 44,
                    }}
                  >
                    {frame >= 130
                      ? "suspected fraud — diverting to monitored honeypot"
                      : ""}
                  </div>
                </div>
              </div>
              <div
                style={{
                  padding: "0.8rem 1.2rem",
                  borderTop: `1px solid ${theme.border}`,
                  display: "flex",
                  gap: 8,
                  justifyContent: "flex-end",
                }}
              >
                <Button variant="ghost">Cancel</Button>
                <Button
                  variant="primary"
                  style={{
                    boxShadow:
                      frame >= 138 && frame < 162
                        ? `0 0 0 ${3 + (Math.sin((frame - 138) / 3) * 0.5 + 0.5) * 4}px rgba(154,166,255,0.65), 0 0 28px rgba(88,101,242,0.7)`
                        : undefined,
                  }}
                >
                  Redirect call
                </Button>
              </div>
            </div>
          </>
        )}
      </AbsoluteFill>

      <AbsoluteFill style={captionAnim}>
        <SceneCaption
          eyebrow={useSecondCaption ? "Active control" : "In-flight"}
          title={
            useSecondCaption
              ? "Re-route mid-call. Caller stays connected."
              : "Every active call. Warn, redirect, or hang up in one click."
          }
          sub={
            useSecondCaption
              ? "Every action is audit-logged. The CDR carries the admin action when the call ends."
              : "Live SSE stream. Every INVITE that survives /sipctl/authorize lands here within 1 second."
          }
        />
      </AbsoluteFill>
    </AbsoluteFill>
  );
};
