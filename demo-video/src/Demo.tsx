import React from "react";
import { AbsoluteFill, Sequence } from "remotion";
import { TIMING } from "./theme";
import { Opening } from "./scenes/Opening";
import { Dashboard } from "./scenes/Dashboard";
import { Suppliers } from "./scenes/Suppliers";
import { LiveCalls } from "./scenes/LiveCalls";
import { CDRTrace } from "./scenes/CDRTrace";
import { EndCard } from "./scenes/EndCard";

/*
 * 90-second composition.
 *
 * Each scene's own component fades itself out over the final ~14 frames; the
 * next scene starts at exactly that boundary so the cross-fade lands without
 * a blank frame between them.
 */

const CROSSFADE = 0;

let cursor = 0;
const at = (frames: number) => {
  const start = cursor;
  cursor += frames;
  return { from: start, duration: frames };
};

const OPENING   = at(TIMING.opening);
const DASHBOARD = at(TIMING.dashboard);
const SUPPLIERS = at(TIMING.suppliers);
const LIVE      = at(TIMING.liveCalls);
const CDR       = at(TIMING.cdrTrace);
const END       = at(TIMING.endCard);

export const Demo: React.FC = () => {
  return (
    <AbsoluteFill style={{ background: "#0e0f17" }}>
      <Sequence from={OPENING.from} durationInFrames={OPENING.duration}>
        <Opening />
      </Sequence>
      <Sequence from={DASHBOARD.from} durationInFrames={DASHBOARD.duration}>
        <Dashboard />
      </Sequence>
      <Sequence from={SUPPLIERS.from} durationInFrames={SUPPLIERS.duration}>
        <Suppliers />
      </Sequence>
      <Sequence from={LIVE.from} durationInFrames={LIVE.duration}>
        <LiveCalls />
      </Sequence>
      <Sequence from={CDR.from} durationInFrames={CDR.duration}>
        <CDRTrace />
      </Sequence>
      <Sequence from={END.from} durationInFrames={END.duration}>
        <EndCard />
      </Sequence>
    </AbsoluteFill>
  );
};
