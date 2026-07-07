import React from "react";
import { AbsoluteFill, useCurrentFrame, useVideoConfig, spring } from "remotion";
import { GradientBg, useFadeInUp } from "../primitives";
import { theme } from "../theme";

/*
 * Scene 0 — Opening hook (5s).
 *
 *   The "question to the viewer" text is intentionally a placeholder that you
 *   swap by editing OPENING_QUESTION below. Two lines so the type-on animation
 *   has natural rhythm.
 */

export const OPENING_QUESTION_LINE_1 = "OPENING LINE TBD —";
export const OPENING_QUESTION_LINE_2 = "edit src/scenes/Opening.tsx";

export const Opening: React.FC = () => {
  const frame = useCurrentFrame();
  const { fps, durationInFrames } = useVideoConfig();

  // Line 1 enters at frame 8, line 2 at frame 28, both spring-up.
  const eyebrow = useFadeInUp(0, 18);
  const line1 = useFadeInUp(8, 32);
  const line2 = useFadeInUp(28, 32);

  // The accent underline draws after line 1.
  const underline = spring({
    frame: frame - 38,
    fps,
    config: { damping: 200, stiffness: 80 },
  });

  // Fade out the whole scene over the final 12 frames so the cross-fade reads.
  const fadeOut = Math.max(
    0,
    Math.min(1, (durationInFrames - frame) / 12),
  );

  return (
    <AbsoluteFill style={{ opacity: fadeOut }}>
      <GradientBg />
      {/* Subtle background gridlines for tech-y atmosphere */}
      <AbsoluteFill
        style={{
          opacity: 0.06,
          backgroundImage:
            "linear-gradient(rgba(154,166,255,0.5) 1px, transparent 1px), linear-gradient(90deg, rgba(154,166,255,0.5) 1px, transparent 1px)",
          backgroundSize: "80px 80px",
          maskImage:
            "radial-gradient(60% 50% at 50% 50%, #000 0%, transparent 100%)",
          WebkitMaskImage:
            "radial-gradient(60% 50% at 50% 50%, #000 0%, transparent 100%)",
        }}
      />
      <AbsoluteFill
        style={{
          alignItems: "center",
          justifyContent: "center",
          textAlign: "center",
          padding: 120,
        }}
      >
        <div
          style={{
            color: theme.accentText,
            fontFamily: theme.font,
            fontSize: 22,
            fontWeight: 600,
            letterSpacing: 6,
            textTransform: "uppercase",
            marginBottom: 60,
            ...eyebrow,
            opacity: eyebrow.opacity * 0.85,
          }}
        >
          DIDStorage · demo
        </div>
        <div
          style={{
            color: "#fff",
            fontFamily: theme.font,
            fontSize: 84,
            fontWeight: 700,
            letterSpacing: -2,
            lineHeight: 1.05,
            maxWidth: 1500,
            ...line1,
          }}
        >
          {OPENING_QUESTION_LINE_1}
        </div>
        <div
          style={{
            color: "#fff",
            fontFamily: theme.font,
            fontSize: 84,
            fontWeight: 700,
            letterSpacing: -2,
            lineHeight: 1.05,
            maxWidth: 1500,
            marginTop: 14,
            ...line2,
          }}
        >
          {OPENING_QUESTION_LINE_2}
        </div>
        <div
          style={{
            width: 200 * underline,
            height: 5,
            background: theme.accent,
            borderRadius: 3,
            marginTop: 56,
            boxShadow: `0 0 24px ${theme.accent}aa`,
          }}
        />
      </AbsoluteFill>
    </AbsoluteFill>
  );
};
