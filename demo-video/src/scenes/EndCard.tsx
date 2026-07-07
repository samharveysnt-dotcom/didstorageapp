import React from "react";
import { AbsoluteFill, spring, useCurrentFrame, useVideoConfig } from "remotion";
import { GradientBg, Wordmark } from "../primitives";
import { theme } from "../theme";

/*
 * Scene 5 — End card (5s).
 *   Wordmark + icon mark fade in. Tagline animates up underneath. Final fade.
 */

export const EndCard: React.FC = () => {
  const frame = useCurrentFrame();
  const { fps, durationInFrames } = useVideoConfig();

  // Wordmark spring-up from frame 0.
  const wordSpring = spring({
    frame: frame - 0,
    fps,
    config: { damping: 200, stiffness: 80, mass: 0.7 },
  });

  // Tagline fades in later.
  const tagT = Math.max(0, Math.min(1, (frame - 32) / 22));

  // Hold + fade out over the last 18 frames.
  const fadeOut = Math.max(0, Math.min(1, (durationInFrames - frame) / 18));

  return (
    <AbsoluteFill style={{ opacity: fadeOut }}>
      <GradientBg />

      {/* Soft accent glow at center for atmospherics */}
      <AbsoluteFill
        style={{
          background:
            "radial-gradient(45% 40% at 50% 50%, rgba(88,101,242,0.22), transparent 70%)",
          opacity: wordSpring,
        }}
      />

      <AbsoluteFill
        style={{
          alignItems: "center",
          justifyContent: "center",
        }}
      >
        <div
          style={{
            opacity: wordSpring,
            transform: `translateY(${(1 - wordSpring) * 20}px) scale(${
              0.95 + wordSpring * 0.05
            })`,
          }}
        >
          <Wordmark size={120} />
        </div>

        <div
          style={{
            marginTop: 56,
            color: theme.muted,
            fontSize: 30,
            fontWeight: 400,
            letterSpacing: 0.3,
            fontFamily: theme.font,
            opacity: tagT,
            transform: `translateY(${(1 - tagT) * 10}px)`,
          }}
        >
          Inbound DIDs. Operational from day one.
        </div>
      </AbsoluteFill>
    </AbsoluteFill>
  );
};
