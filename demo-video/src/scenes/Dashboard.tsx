import React from "react";
import { AbsoluteFill, useCurrentFrame, useVideoConfig } from "remotion";
import {
  AppFrame,
  GradientBg,
  H1,
  H2,
  KPICard,
  Page,
  SceneCaption,
  Sidebar,
  TD,
  TR,
  Table,
  Topbar,
  useFadeIn,
  useFadeInUp,
} from "../primitives";
import { theme } from "../theme";

const RECENT_CDRS = [
  { started: "13:41:22", did: "+1 415 555 0188",  from: "+44 20 7946 0211", dur: "2m 14s", min: 3,  charge: 0.036,  cause: "Normal Call Clearing" },
  { started: "13:40:58", did: "+44 20 7946 0114", from: "+49 30 2576 0833", dur: "0m 47s", min: 1,  charge: 0.012,  cause: "Normal Call Clearing" },
  { started: "13:40:33", did: "+49 30 2576 0119", from: "+1 213 555 0184",  dur: "5m 02s", min: 6,  charge: 0.072,  cause: "Normal Call Clearing" },
  { started: "13:39:51", did: "+63 2 8540 0223",  from: "+1 415 555 0162",  dur: "0m 09s", min: 0,  charge: 0,      cause: "User Busy" },
  { started: "13:39:14", did: "+1 800 555 0123",  from: "+1 415 555 0177",  dur: "1m 28s", min: 2,  charge: 0.024,  cause: "Normal Call Clearing" },
];

export const Dashboard: React.FC = () => {
  const frame = useCurrentFrame();
  const { durationInFrames } = useVideoConfig();

  const frameEnter = useFadeInUp(0, 24);
  const captionAnim = useFadeInUp(8);
  const cdrSection = useFadeInUp(160);

  // Fade out the whole scene over the final 14 frames for cross-fade.
  const fadeOut = Math.max(0, Math.min(1, (durationInFrames - frame) / 14));

  return (
    <AbsoluteFill style={{ opacity: fadeOut }}>
      <GradientBg />

      <AbsoluteFill style={frameEnter}>
        <AppFrame>
          <Sidebar active="/" />
          <Page>
            <Topbar />
            <H1>Dashboard</H1>

            {/* KPI grid — each card's number ticks up from 0 with a slight stagger */}
            <div
              style={{
                display: "grid",
                gridTemplateColumns: "repeat(4, 1fr)",
                gap: "1rem",
                marginBottom: "1.5rem",
              }}
            >
              <KPICard
                label="Suppliers"
                value={12}
                sub="184 authorized IPs"
                startFrame={20}
              />
              <KPICard
                label="DIDs"
                value={2480}
                sub="1,902 assigned · 578 available"
                startFrame={26}
              />
              <KPICard
                label="Users"
                value={47}
                sub="customer-level accounts"
                startFrame={32}
              />
              <KPICard
                label="Orders"
                value={1840}
                sub="3 kyc-pending · 1 quarantined"
                startFrame={38}
              />
              <KPICard
                label="Total balance"
                value={36214}
                format={(n) => `$${n.toLocaleString("en-US")}`}
                sub="across all users"
                startFrame={44}
              />
              <KPICard
                label="CDRs (24h)"
                value={312}
                sub="$4,812.40 billed"
                startFrame={50}
              />
              <KPICard
                label="Active calls"
                value={14}
                sub="live in Redis"
                startFrame={56}
              />
              <KPICard
                label="Denied (24h)"
                value={9}
                sub="unauthorized_ip / unknown_did"
                startFrame={62}
              />
            </div>

            {/* Recent CDRs section slides in after the KPI cards have settled */}
            <div style={cdrSection}>
              <H2>Recent CDRs</H2>
              <Table cols={["Started", "DID", "From", "Duration", "Min", "Charge", "Cause"]}>
                {RECENT_CDRS.map((c, i) => {
                  const rowEnter = Math.max(
                    0,
                    Math.min(1, (frame - (172 + i * 6)) / 18),
                  );
                  return (
                    <TR
                      key={i}
                      style={{
                        opacity: rowEnter,
                        transform: `translateY(${(1 - rowEnter) * 10}px)`,
                      }}
                    >
                      <TD muted>{c.started}</TD>
                      <TD mono>{c.did}</TD>
                      <TD mono muted>{c.from}</TD>
                      <TD>{c.dur}</TD>
                      <TD>{c.min}</TD>
                      <TD>${c.charge.toFixed(3)}</TD>
                      <TD muted>{c.cause}</TD>
                    </TR>
                  );
                })}
              </Table>
            </div>
          </Page>
        </AppFrame>
      </AbsoluteFill>

      <AbsoluteFill style={captionAnim}>
        <SceneCaption
          eyebrow="Overview"
          title="One pane for the whole inventory."
          sub="Suppliers, DIDs, customers, balance, CDRs, denials — all current as of right now."
        />
      </AbsoluteFill>
    </AbsoluteFill>
  );
};
