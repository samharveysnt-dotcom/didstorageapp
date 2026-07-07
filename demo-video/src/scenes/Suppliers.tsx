import React from "react";
import { AbsoluteFill, useCurrentFrame, useVideoConfig } from "remotion";
import {
  AppFrame,
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

const TABS = ["Details", "IP whitelisting", "Rate cards", "DIDs"];

const IP_BLOCK_PASTE = [
  "203.0.113.10",
  "203.0.113.11",
  "203.0.113.12",
  "198.51.100.40",
  "198.51.100.41/30",
  "192.0.2.55",
].join("\n");

type Rate = {
  country: string;
  flag: string;
  cc: string;
  type: string;
  rate: number;
  cost: number;
  profitPerMin: number;
};

const RATE_CARDS: Rate[] = [
  { country: "United States",   flag: "🇺🇸", cc: "+1",  type: "toll-free", rate: 0.014, cost: 0.0080, profitPerMin: 0.006 },
  { country: "United States",   flag: "🇺🇸", cc: "+1",  type: "local",     rate: 0.012, cost: 0.0065, profitPerMin: 0.0055 },
  { country: "United Kingdom",  flag: "🇬🇧", cc: "+44", type: "mobile",    rate: 0.022, cost: 0.0140, profitPerMin: 0.008 },
  { country: "United Kingdom",  flag: "🇬🇧", cc: "+44", type: "national",  rate: 0.011, cost: 0.0060, profitPerMin: 0.005 },
  { country: "Germany",         flag: "🇩🇪", cc: "+49", type: "national",  rate: 0.013, cost: 0.0075, profitPerMin: 0.0055 },
  { country: "Philippines",     flag: "🇵🇭", cc: "+63", type: "mobile",    rate: 0.045, cost: 0.0320, profitPerMin: 0.013 },
];

export const Suppliers: React.FC = () => {
  const frame = useCurrentFrame();
  const { durationInFrames } = useVideoConfig();

  const frameEnter = useFadeInUp(0, 24);

  // Scene beats:
  //   0-30:   page enters
  //   30-110: halo on "IP whitelisting" tab → textarea fills with pasted block
  //   110-200: halo on rate-cards section → one row expands → profit preview pops out
  //  200-end: hold + caption
  const ipTabActive = frame >= 60;
  const ratesExpanded = frame >= 150;
  const profitOverlay = frame >= 200;

  const fadeOut = Math.max(0, Math.min(1, (durationInFrames - frame) / 14));

  // The pasted-IP text reveals one line at a time between frame 70 and 110.
  const ipLines = IP_BLOCK_PASTE.split("\n");
  const ipReveal = Math.max(0, Math.min(1, (frame - 70) / 40));
  const visibleIPCount = Math.floor(ipReveal * ipLines.length);
  const ipShown = ipLines.slice(0, visibleIPCount).join("\n");

  // The profit preview pops in with a small scale-up.
  const profitT = Math.max(0, Math.min(1, (frame - 200) / 14));
  const profitScale = 0.92 + profitT * 0.08;

  // Caption animates twice — first ("Bring suppliers...") then swaps for second
  // ("Per-country rate cards...") around frame 160 to match what's on screen.
  const captionAnim = useFadeInUp(8);
  const useSecondCaption = frame >= 150;

  return (
    <AbsoluteFill style={{ opacity: fadeOut }}>
      <GradientBg />

      <AbsoluteFill style={frameEnter}>
        <AppFrame>
          <Sidebar active="/suppliers" />
          <Page>
            <Topbar />
            <div style={{ display: "flex", alignItems: "center", gap: 14, marginBottom: 4 }}>
              <H1 style={{ marginBottom: 0 }}>Globe Telecom</H1>
              <Pill kind="active">active</Pill>
              <span style={{ color: theme.muted, fontSize: 13 }}>
                supplier · 48 DIDs · 6 authorized IP groups
              </span>
            </div>
            <div style={{ color: theme.muted, fontSize: 13, marginBottom: 18 }}>
              ID <Mono>sup_8q3f1c</Mono> · created 2026-03-14 · contact{" "}
              <Mono>noc@globetelecom.example</Mono>
            </div>

            {/* Tabs */}
            <div
              style={{
                display: "flex",
                borderBottom: `1px solid ${theme.border}`,
                marginBottom: 18,
                position: "relative",
              }}
            >
              {TABS.map((t, i) => {
                let isActive = false;
                if (i === 0 && frame < 60) isActive = true;
                else if (i === 1 && ipTabActive && !ratesExpanded) isActive = true;
                else if (i === 2 && ratesExpanded) isActive = true;
                return (
                  <div
                    key={t}
                    style={{
                      padding: "0.6rem 1.1rem",
                      color: isActive ? theme.accentText : theme.muted,
                      fontSize: 13.5,
                      fontWeight: 500,
                      borderBottom: isActive
                        ? `2px solid ${theme.accent}`
                        : "2px solid transparent",
                      marginBottom: -1,
                    }}
                  >
                    {t}
                  </div>
                );
              })}
            </div>

            {/* Panel contents: switches between IP whitelisting and rate cards */}
            {!ratesExpanded && (
              <div>
                <div style={{ display: "flex", gap: 14, marginBottom: 14, alignItems: "center" }}>
                  <div
                    style={{
                      color: theme.muted,
                      fontSize: 12,
                      textTransform: "uppercase",
                      letterSpacing: 0.5,
                    }}
                  >
                    IP groups
                  </div>
                  <div style={{ color: theme.text, fontSize: 13.5 }}>
                    <Mono>noc-core</Mono>{" "}
                    <span style={{ color: theme.muted }}>· /sipctl/authorize will accept INVITEs from any IP in this group</span>
                  </div>
                </div>
                <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 14 }}>
                  {/* Paste-IPs textarea */}
                  <div>
                    <div
                      style={{
                        color: theme.muted,
                        fontSize: 11,
                        textTransform: "uppercase",
                        letterSpacing: 0.5,
                        marginBottom: 6,
                      }}
                    >
                      Paste IPs / CIDRs (one per line)
                    </div>
                    <pre
                      style={{
                        background: theme.border,
                        border: `1px solid ${theme.border}`,
                        borderRadius: 6,
                        padding: "0.7rem 0.9rem",
                        margin: 0,
                        minHeight: 180,
                        color: theme.text,
                        fontFamily: theme.mono,
                        fontSize: 13,
                        lineHeight: 1.55,
                        whiteSpace: "pre-wrap",
                      }}
                    >
                      {ipShown}
                      {ipReveal < 1 && (
                        <span
                          style={{
                            display: "inline-block",
                            width: 8,
                            height: 14,
                            background: theme.accent,
                            verticalAlign: "middle",
                            marginLeft: 2,
                            opacity:
                              Math.floor(frame / 8) % 2 === 0 ? 1 : 0.2,
                          }}
                        />
                      )}
                    </pre>
                  </div>
                  {/* Authorized IPs list */}
                  <div>
                    <div
                      style={{
                        color: theme.muted,
                        fontSize: 11,
                        textTransform: "uppercase",
                        letterSpacing: 0.5,
                        marginBottom: 6,
                      }}
                    >
                      Authorized · {visibleIPCount}
                    </div>
                    <div
                      style={{
                        background: theme.card,
                        border: `1px solid ${theme.border}`,
                        borderRadius: 6,
                        padding: "0.5rem 0.6rem",
                        minHeight: 180,
                      }}
                    >
                      {ipLines.slice(0, visibleIPCount).map((ip, i) => (
                        <div
                          key={ip}
                          style={{
                            display: "flex",
                            alignItems: "center",
                            gap: 8,
                            padding: "4px 6px",
                            fontFamily: theme.mono,
                            fontSize: 12.5,
                            color: theme.text,
                            opacity: Math.max(
                              0,
                              Math.min(1, (frame - (75 + i * 6)) / 10),
                            ),
                          }}
                        >
                          <span style={{ color: theme.ok, fontSize: 13 }}>●</span>
                          {ip}
                          <span style={{ color: theme.muted, marginLeft: "auto" }}>
                            accepted
                          </span>
                        </div>
                      ))}
                    </div>
                  </div>
                </div>
              </div>
            )}

            {ratesExpanded && (
              <div style={{ position: "relative" }}>
                <div
                  style={{
                    color: theme.muted,
                    fontSize: 11,
                    textTransform: "uppercase",
                    letterSpacing: 0.5,
                    marginBottom: 6,
                  }}
                >
                  Rate cards · keyed by (country, did_type)
                </div>
                <Table
                  cols={["Country", "Type", "Rate / min", "Supplier cost", "Profit / min", ""]}
                  colWidths={[260, 130, 140, 160, 160, 80]}
                >
                  {RATE_CARDS.map((r, i) => {
                    const rowEnter = Math.max(
                      0,
                      Math.min(1, (frame - (160 + i * 4)) / 14),
                    );
                    const isFeature = i === 0;
                    return (
                      <TR
                        key={i}
                        style={{
                          opacity: rowEnter,
                          background: isFeature
                            ? "rgba(88,101,242,0.05)"
                            : undefined,
                        }}
                      >
                        <TD>
                          <span style={{ marginRight: 8, fontSize: 16 }}>{r.flag}</span>
                          {r.country}{" "}
                          <span style={{ color: theme.muted, fontSize: 11 }}>{r.cc}</span>
                        </TD>
                        <TD>
                          <Pill kind={r.type === "toll-free" ? "warn" : "available"}>
                            {r.type}
                          </Pill>
                        </TD>
                        <TD style={{ fontVariantNumeric: "tabular-nums" }}>
                          ${r.rate.toFixed(4)}
                        </TD>
                        <TD muted style={{ fontVariantNumeric: "tabular-nums" }}>
                          ${r.cost.toFixed(4)}
                        </TD>
                        <TD style={{ color: theme.ok, fontVariantNumeric: "tabular-nums" }}>
                          +${r.profitPerMin.toFixed(4)}
                        </TD>
                        <TD>
                          {isFeature && profitOverlay && (
                            <span
                              style={{
                                color: theme.accentText,
                                fontSize: 11,
                                fontWeight: 500,
                              }}
                            >
                              preview ▾
                            </span>
                          )}
                        </TD>
                      </TR>
                    );
                  })}
                </Table>

                {/* Profit-per-duration preview overlay — pops out of the first row */}
                {profitOverlay && (
                  <div
                    style={{
                      position: "absolute",
                      left: 12,
                      top: 88,
                      width: 580,
                      background: theme.bg,
                      border: `1px solid ${theme.accent}`,
                      borderRadius: 10,
                      padding: 16,
                      boxShadow: theme.shadow,
                      transform: `scale(${profitScale})`,
                      transformOrigin: "top left",
                      opacity: profitT,
                    }}
                  >
                    <div
                      style={{
                        color: theme.muted,
                        fontSize: 11,
                        textTransform: "uppercase",
                        letterSpacing: 0.5,
                        marginBottom: 8,
                      }}
                    >
                      US · toll-free · per-call profit preview
                    </div>
                    <div style={{ display: "flex", gap: 22 }}>
                      {[30, 60, 120, 210].map((sec) => {
                        const mins = Math.ceil(sec / 60);
                        const profit = mins * 0.006;
                        return (
                          <div key={sec} style={{ textAlign: "left" }}>
                            <div style={{ color: theme.muted, fontSize: 11 }}>{sec}s call</div>
                            <div
                              style={{
                                color: theme.ok,
                                fontWeight: 600,
                                fontSize: 22,
                                fontVariantNumeric: "tabular-nums",
                              }}
                            >
                              +${profit.toFixed(3)}
                            </div>
                            <div style={{ color: theme.muted, fontSize: 11 }}>{mins} bm</div>
                          </div>
                        );
                      })}
                    </div>
                    <div
                      style={{
                        marginTop: 12,
                        paddingTop: 12,
                        borderTop: `1px solid ${theme.border}`,
                        color: theme.muted,
                        fontSize: 12,
                      }}
                    >
                      Month-1 estimate · 50 DIDs × 4 ch × 28% busy
                      ={" "}
                      <span style={{ color: theme.ok, fontWeight: 600 }}>
                        +$1,448
                      </span>
                    </div>
                  </div>
                )}
              </div>
            )}
          </Page>
        </AppFrame>
      </AbsoluteFill>

      <AbsoluteFill style={captionAnim}>
        <SceneCaption
          eyebrow={useSecondCaption ? "Margin" : "Multi-tenant inbound"}
          title={
            useSecondCaption
              ? "Per-country rate cards. Profit-per-call, in advance."
              : "Suppliers bring trunks. You set the IP ACL."
          }
          sub={
            useSecondCaption
              ? "Rate cards key by (supplier, country, did_type). Cost snapshotted on every CDR."
              : "Paste a block of IPs. INVITEs land on /sipctl/authorize and decide in milliseconds."
          }
        />
      </AbsoluteFill>
    </AbsoluteFill>
  );
};
