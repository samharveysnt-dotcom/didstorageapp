import React from "react";
import { Composition } from "remotion";
import { Demo } from "./Demo";
import { TOTAL_FRAMES, VIDEO } from "./theme";
import { Opening } from "./scenes/Opening";
import { Dashboard } from "./scenes/Dashboard";
import { Suppliers } from "./scenes/Suppliers";
import { LiveCalls } from "./scenes/LiveCalls";
import { CDRTrace } from "./scenes/CDRTrace";
import { EndCard } from "./scenes/EndCard";

export const Root: React.FC = () => {
  return (
    <>
      {/* Main 90s deliverable */}
      <Composition
        id="Demo"
        component={Demo}
        durationInFrames={TOTAL_FRAMES}
        fps={VIDEO.fps}
        width={VIDEO.width}
        height={VIDEO.height}
      />

      {/* Per-scene compositions — useful for iterating on one scene at a time
          in Remotion Studio without scrubbing through the full 90s. */}
      <Composition
        id="Scene0-Opening"
        component={Opening}
        durationInFrames={150}
        fps={VIDEO.fps}
        width={VIDEO.width}
        height={VIDEO.height}
      />
      <Composition
        id="Scene1-Dashboard"
        component={Dashboard}
        durationInFrames={600}
        fps={VIDEO.fps}
        width={VIDEO.width}
        height={VIDEO.height}
      />
      <Composition
        id="Scene2-Suppliers"
        component={Suppliers}
        durationInFrames={600}
        fps={VIDEO.fps}
        width={VIDEO.width}
        height={VIDEO.height}
      />
      <Composition
        id="Scene3-LiveCalls"
        component={LiveCalls}
        durationInFrames={750}
        fps={VIDEO.fps}
        width={VIDEO.width}
        height={VIDEO.height}
      />
      <Composition
        id="Scene4-CDRTrace"
        component={CDRTrace}
        durationInFrames={450}
        fps={VIDEO.fps}
        width={VIDEO.width}
        height={VIDEO.height}
      />
      <Composition
        id="Scene5-EndCard"
        component={EndCard}
        durationInFrames={150}
        fps={VIDEO.fps}
        width={VIDEO.width}
        height={VIDEO.height}
      />
    </>
  );
};
