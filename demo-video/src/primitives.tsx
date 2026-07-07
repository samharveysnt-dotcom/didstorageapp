import React from "react";
import {
  AbsoluteFill,
  interpolate,
  spring,
  useCurrentFrame,
  useVideoConfig,
} from "remotion";
import { theme } from "./theme";
import { Icon } from "./icons";

/* ---------------------------------------------------------------------------
 * GradientBg — soft blurple→navy radial behind every scene.
 * ------------------------------------------------------------------------ */

export const GradientBg: React.FC<{ vignette?: boolean }> = ({
  vignette = true,
}) => {
  return (
    <AbsoluteFill
      style={{
        background:
          "radial-gradient(120% 90% at 50% 0%, #2a2f4a 0%, #1a1c2a 55%, #0e0f17 100%)",
      }}
    >
      {vignette && (
        <AbsoluteFill
          style={{
            background:
              "radial-gradient(80% 60% at 50% 50%, rgba(88,101,242,0.08), transparent 70%)",
          }}
        />
      )}
    </AbsoluteFill>
  );
};

/* ---------------------------------------------------------------------------
 * AppFrame — a centered floating "browser window" with the DIDStorage UI inside.
 * Used by every UI-replicating scene so the chrome stays consistent.
 * ------------------------------------------------------------------------ */

export const AppFrame: React.FC<{
  children: React.ReactNode;
  width?: number;
  height?: number;
}> = ({ children, width = 1680, height = 920 }) => {
  return (
    <div
      style={{
        position: "absolute",
        left: "50%",
        top: "50%",
        transform: "translate(-50%, -50%)",
        width,
        height,
        background: theme.bg,
        borderRadius: 14,
        overflow: "hidden",
        boxShadow:
          "0 30px 80px rgba(0,0,0,0.55), 0 4px 14px rgba(0,0,0,0.35), 0 0 0 1px rgba(255,255,255,0.04)",
        color: theme.text,
        font: `14px/1.5 ${theme.font}`,
        display: "flex",
      }}
    >
      {children}
    </div>
  );
};

/* ---------------------------------------------------------------------------
 * Sidebar — mirrors the Go template sidebar nav.
 * ------------------------------------------------------------------------ */

type NavSection = { group: string; items: Array<{ label: string; href: string }> };

const NAV_SECTIONS: NavSection[] = [
  { group: "Overview", items: [{ label: "Dashboard", href: "/" }] },
  {
    group: "Inventory",
    items: [
      { label: "Suppliers", href: "/suppliers" },
      { label: "DIDs", href: "/dids" },
    ],
  },
  {
    group: "Customers",
    items: [
      { label: "Users", href: "/users" },
      { label: "Orders", href: "/orders" },
      { label: "Resellers", href: "/resellers" },
    ],
  },
  { group: "Live", items: [{ label: "Active calls", href: "/live" }] },
  {
    group: "History",
    items: [
      { label: "CDRs", href: "/cdrs" },
      { label: "Denied calls", href: "/denied-calls" },
    ],
  },
  {
    group: "Settings",
    items: [
      { label: "Site & company", href: "/settings" },
      { label: "Hangup causes", href: "/cause-codes" },
    ],
  },
];

export const Sidebar: React.FC<{ active: string }> = ({ active }) => {
  return (
    <aside
      style={{
        width: 240,
        flexShrink: 0,
        background: theme.sidebar,
        borderRight: `1px solid ${theme.border}`,
        padding: "1.2rem 0",
        display: "flex",
        flexDirection: "column",
      }}
    >
      <div
        style={{
          padding: "0 1.5rem 1.2rem",
          color: theme.accentText,
          fontWeight: 700,
          letterSpacing: 1,
          fontSize: 15,
          borderBottom: `1px solid ${theme.border}`,
          marginBottom: "0.7rem",
        }}
      >
        DIDStorage
      </div>
      <nav style={{ display: "flex", flexDirection: "column", flex: 1 }}>
        {NAV_SECTIONS.map((sec) => (
          <React.Fragment key={sec.group}>
            <span
              style={{
                color: theme.muted,
                fontSize: 10,
                textTransform: "uppercase",
                letterSpacing: 1,
                padding: "0.8rem 1.5rem 0.3rem",
                fontWeight: 600,
                opacity: 0.7,
              }}
            >
              {sec.group}
            </span>
            {sec.items.map((it) => {
              const isActive = it.href === active;
              return (
                <div
                  key={it.href}
                  style={{
                    color: isActive ? theme.accentText : theme.muted,
                    padding: isActive
                      ? "0.55rem 1.5rem 0.55rem calc(1.5rem - 3px)"
                      : "0.55rem 1.5rem",
                    fontWeight: 500,
                    fontSize: 13.5,
                    display: "flex",
                    alignItems: "center",
                    gap: "0.6rem",
                    background: isActive ? theme.sidebarActive : "transparent",
                    borderLeft: isActive ? `3px solid ${theme.accent}` : "none",
                  }}
                >
                  {it.label}
                </div>
              );
            })}
          </React.Fragment>
        ))}
      </nav>
      <div
        style={{
          padding: "1rem 1.5rem",
          borderTop: `1px solid ${theme.border}`,
          color: theme.muted,
          fontSize: 12,
        }}
      >
        admin · log out
      </div>
    </aside>
  );
};

/* ---------------------------------------------------------------------------
 * Topbar — sticky search row above page content.
 * ------------------------------------------------------------------------ */

export const Topbar: React.FC<{ placeholder?: string }> = ({
  placeholder = "Search users / DIDs / orders / call-IDs…",
}) => {
  return (
    <div
      style={{
        display: "flex",
        alignItems: "center",
        gap: "1rem",
        marginBottom: "1.5rem",
      }}
    >
      <div
        style={{
          flex: 1,
          maxWidth: 560,
          position: "relative",
        }}
      >
        <div
          style={{
            width: "100%",
            background: theme.card,
            border: `1px solid ${theme.border}`,
            borderRadius: 6,
            padding: "0.55rem 0.9rem",
            color: theme.muted,
            display: "flex",
            alignItems: "center",
            gap: 8,
            fontSize: 13.5,
          }}
        >
          <Icon.Search size={14} color={theme.muted} />
          {placeholder}
        </div>
      </div>
      <div style={{ marginLeft: "auto", color: theme.muted, fontSize: 13 }}>
        admin
      </div>
    </div>
  );
};

/* ---------------------------------------------------------------------------
 * Page — the right-of-sidebar main column. Inset padding matches the Go layout.
 * ------------------------------------------------------------------------ */

export const Page: React.FC<{ children: React.ReactNode }> = ({ children }) => (
  <main
    style={{
      flex: 1,
      minWidth: 0,
      padding: "1.5rem 2rem",
      overflow: "hidden",
    }}
  >
    {children}
  </main>
);

/* ---------------------------------------------------------------------------
 * H1 / H2 — page headings.
 * ------------------------------------------------------------------------ */

export const H1: React.FC<{ children: React.ReactNode; style?: React.CSSProperties }> = ({
  children,
  style,
}) => (
  <h1
    style={{
      margin: "0 0 1rem",
      fontSize: "1.5rem",
      fontWeight: 600,
      color: theme.text,
      ...style,
    }}
  >
    {children}
  </h1>
);

export const H2: React.FC<{ children: React.ReactNode; style?: React.CSSProperties }> = ({
  children,
  style,
}) => (
  <h2
    style={{
      fontSize: "1.05rem",
      margin: "1.4rem 0 0.6rem",
      color: theme.muted,
      fontWeight: 500,
      textTransform: "uppercase",
      letterSpacing: "0.5px",
      ...style,
    }}
  >
    {children}
  </h2>
);

/* ---------------------------------------------------------------------------
 * KPICard — animated number card. `from→to` ticks over `tickDurationFrames`.
 * ------------------------------------------------------------------------ */

export const KPICard: React.FC<{
  label: string;
  value: number;
  format?: (n: number) => string;
  sub?: string;
  startFrame?: number;
  tickFrames?: number;
}> = ({ label, value, format, sub, startFrame = 0, tickFrames = 35 }) => {
  const frame = useCurrentFrame();
  const t = Math.max(0, Math.min(1, (frame - startFrame) / tickFrames));
  // Ease-out cubic — quick rise then settle
  const eased = 1 - Math.pow(1 - t, 3);
  const shown = Math.round(value * eased);
  const text = format ? format(shown) : shown.toLocaleString("en-US");
  return (
    <div
      style={{
        background: theme.card,
        border: `1px solid ${theme.border}`,
        borderRadius: 8,
        padding: "1rem 1.2rem",
      }}
    >
      <div
        style={{
          color: theme.muted,
          fontSize: 11,
          textTransform: "uppercase",
          letterSpacing: "0.5px",
        }}
      >
        {label}
      </div>
      <div
        style={{
          fontSize: "1.7rem",
          fontWeight: 600,
          marginTop: "0.3rem",
          fontVariantNumeric: "tabular-nums",
          color: theme.text,
        }}
      >
        {text}
      </div>
      {sub && (
        <div style={{ color: theme.muted, fontSize: 12, marginTop: "0.2rem" }}>
          {sub}
        </div>
      )}
    </div>
  );
};

/* ---------------------------------------------------------------------------
 * Pill — colored status pill matching Go template's `.pill-*` classes.
 * ------------------------------------------------------------------------ */

export const Pill: React.FC<{
  kind:
    | "active"
    | "suspended"
    | "cancelled"
    | "available"
    | "assigned"
    | "warn"
    | "accent";
  children: React.ReactNode;
  style?: React.CSSProperties;
}> = ({ kind, children, style }) => {
  const palette: Record<
    string,
    { bg: string; fg: string }
  > = {
    active: { bg: "rgba(35,165,90,.18)", fg: theme.ok },
    suspended: { bg: "rgba(218,55,60,.18)", fg: theme.err },
    cancelled: { bg: "rgba(148,155,164,.16)", fg: theme.muted },
    available: { bg: "rgba(88,101,242,.18)", fg: theme.accentText },
    assigned: { bg: "rgba(240,178,50,.18)", fg: theme.warn },
    warn: { bg: "rgba(240,178,50,.18)", fg: theme.warn },
    accent: { bg: "rgba(88,101,242,.18)", fg: theme.accentText },
  };
  const c = palette[kind];
  return (
    <span
      style={{
        display: "inline-block",
        padding: "0.1rem 0.5rem",
        borderRadius: 10,
        fontSize: 11,
        fontWeight: 500,
        background: c.bg,
        color: c.fg,
        ...style,
      }}
    >
      {children}
    </span>
  );
};

/* ---------------------------------------------------------------------------
 * Button — matches the Go template button variants.
 * ------------------------------------------------------------------------ */

export const Button: React.FC<{
  variant?: "primary" | "ghost" | "danger" | "warn" | "secondary";
  children: React.ReactNode;
  small?: boolean;
  style?: React.CSSProperties;
}> = ({ variant = "primary", small, children, style }) => {
  const v: React.CSSProperties = (() => {
    switch (variant) {
      case "ghost":
        return {
          background: "transparent",
          border: `1px solid ${theme.border}`,
          color: theme.text,
        };
      case "danger":
        return { background: theme.err, color: "#fff", border: "none" };
      case "warn":
        return { background: theme.warn, color: "#1e1f22", border: "none" };
      case "secondary":
        return {
          background: theme.sidebarActive,
          color: theme.text,
          border: "none",
        };
      default:
        return { background: theme.accent, color: "#fff", border: "none" };
    }
  })();
  return (
    <button
      style={{
        font: `inherit`,
        borderRadius: 6,
        cursor: "default",
        fontWeight: 500,
        padding: small ? "0.22rem 0.6rem" : "0.55rem 1rem",
        fontSize: small ? 11.5 : 13.5,
        display: "inline-flex",
        alignItems: "center",
        gap: "0.3rem",
        lineHeight: 1.25,
        ...v,
        ...style,
      }}
    >
      {children}
    </button>
  );
};

/* ---------------------------------------------------------------------------
 * Table — mirrors the Go template table styling.
 * ------------------------------------------------------------------------ */

export const Table: React.FC<{
  cols: string[];
  children: React.ReactNode;
  style?: React.CSSProperties;
  colWidths?: (string | number)[];
}> = ({ cols, children, style, colWidths }) => (
  <table
    style={{
      width: "100%",
      borderCollapse: "collapse",
      background: theme.card,
      border: `1px solid ${theme.border}`,
      borderRadius: 8,
      overflow: "hidden",
      tableLayout: colWidths ? "fixed" : "auto",
      ...style,
    }}
  >
    {colWidths && (
      <colgroup>
        {colWidths.map((w, i) => (
          <col key={i} style={{ width: typeof w === "number" ? `${w}px` : w }} />
        ))}
      </colgroup>
    )}
    <thead>
      <tr>
        {cols.map((c) => (
          <th
            key={c}
            style={{
              textAlign: "left",
              padding: "0.55rem 0.85rem",
              borderBottom: `1px solid ${theme.border}`,
              fontSize: 11,
              color: theme.muted,
              fontWeight: 500,
              textTransform: "uppercase",
              letterSpacing: "0.5px",
              background: theme.border,
              whiteSpace: "nowrap",
              height: 38,
            }}
          >
            {c}
          </th>
        ))}
      </tr>
    </thead>
    <tbody>{children}</tbody>
  </table>
);

export const TR: React.FC<{
  children: React.ReactNode;
  style?: React.CSSProperties;
  highlighted?: boolean;
}> = ({ children, style, highlighted }) => (
  <tr
    style={{
      background: highlighted ? "rgba(154,166,255,0.10)" : "transparent",
      ...style,
    }}
  >
    {children}
  </tr>
);

export const TD: React.FC<{
  children?: React.ReactNode;
  muted?: boolean;
  mono?: boolean;
  style?: React.CSSProperties;
  colSpan?: number;
}> = ({ children, muted, mono, style, colSpan }) => (
  <td
    colSpan={colSpan}
    style={{
      textAlign: "left",
      padding: "0.55rem 0.85rem",
      borderBottom: `1px solid ${theme.border}`,
      fontSize: 13,
      color: muted ? theme.muted : theme.text,
      verticalAlign: "middle",
      whiteSpace: "nowrap",
      overflow: "hidden",
      textOverflow: "ellipsis",
      height: 38,
      fontFamily: mono ? theme.mono : undefined,
      ...style,
    }}
  >
    {children}
  </td>
);

/* ---------------------------------------------------------------------------
 * Mono — inline `<code>` rendering.
 * ------------------------------------------------------------------------ */

export const Mono: React.FC<{
  children: React.ReactNode;
  style?: React.CSSProperties;
}> = ({ children, style }) => (
  <code
    style={{
      fontFamily: theme.mono,
      background: theme.border,
      padding: "0 0.25rem",
      borderRadius: 3,
      fontSize: 12,
      color: theme.text,
      ...style,
    }}
  >
    {children}
  </code>
);

/* ---------------------------------------------------------------------------
 * HighlightHalo — soft pulsing blurple ring around an element. Use to draw
 * the eye toward the next "click" target without showing a cursor.
 * ------------------------------------------------------------------------ */

export const HighlightHalo: React.FC<{
  left: number;
  top: number;
  width: number;
  height: number;
  startFrame: number;
  durationFrames?: number;
  color?: string;
  borderRadius?: number;
}> = ({
  left,
  top,
  width,
  height,
  startFrame,
  durationFrames = 60,
  color = theme.accent,
  borderRadius = 8,
}) => {
  const frame = useCurrentFrame();
  const t = (frame - startFrame) / durationFrames;
  if (t < 0 || t > 1.2) return null;
  // 2 pulses across the duration
  const pulse = Math.sin(t * Math.PI * 2) * 0.5 + 0.5;
  const ringScale = 1 + pulse * 0.08;
  const ringOpacity = (1 - t) * 0.95;
  const glow = pulse * 0.7 + 0.3;
  return (
    <div
      style={{
        position: "absolute",
        left,
        top,
        width,
        height,
        pointerEvents: "none",
        zIndex: 1000,
      }}
    >
      <div
        style={{
          position: "absolute",
          inset: 0,
          borderRadius,
          boxShadow: `0 0 0 ${3 + pulse * 4}px ${color}`,
          opacity: ringOpacity,
          transform: `scale(${ringScale})`,
          transformOrigin: "center",
        }}
      />
      <div
        style={{
          position: "absolute",
          inset: -16,
          borderRadius: borderRadius + 16,
          background: `radial-gradient(closest-side, ${color}55, transparent 70%)`,
          opacity: glow * ringOpacity,
          filter: "blur(6px)",
        }}
      />
    </div>
  );
};

/* ---------------------------------------------------------------------------
 * fadeInUp / scaleIn helpers — spring-driven enters used by scene-level mounts.
 * ------------------------------------------------------------------------ */

export const useFadeInUp = (startFrame: number, distance = 24) => {
  const frame = useCurrentFrame();
  const { fps } = useVideoConfig();
  const s = spring({
    frame: frame - startFrame,
    fps,
    config: { damping: 200, stiffness: 120, mass: 0.7 },
  });
  return {
    opacity: s,
    transform: `translateY(${(1 - s) * distance}px)`,
  };
};

export const useFadeIn = (startFrame: number, durationFrames = 12) => {
  const frame = useCurrentFrame();
  const t = (frame - startFrame) / durationFrames;
  return { opacity: Math.max(0, Math.min(1, t)) };
};

/* ---------------------------------------------------------------------------
 * Wordmark — DIDStorage logo (icon mark + text).
 * ------------------------------------------------------------------------ */

export const Wordmark: React.FC<{
  size?: number;
  color?: string;
  withTagline?: boolean;
  tagline?: string;
}> = ({
  size = 96,
  color = theme.accentText,
  withTagline,
  tagline = "Inbound DIDs. Operational from day one.",
}) => {
  return (
    <div style={{ display: "flex", flexDirection: "column", alignItems: "center" }}>
      <div style={{ display: "flex", alignItems: "center", gap: size * 0.22 }}>
        <Icon.Mark size={size * 0.95} color={color} />
        <div
          style={{
            fontFamily: theme.font,
            fontWeight: 700,
            fontSize: size,
            letterSpacing: -size * 0.025,
            color: "#fff",
            lineHeight: 1,
          }}
        >
          DIDStorage
        </div>
      </div>
      {withTagline && (
        <div
          style={{
            marginTop: size * 0.4,
            color: theme.muted,
            fontSize: size * 0.22,
            fontWeight: 400,
            letterSpacing: 0.5,
            fontFamily: theme.font,
          }}
        >
          {tagline}
        </div>
      )}
    </div>
  );
};

/* ---------------------------------------------------------------------------
 * SceneCaption — bottom-of-frame caption that names what the scene is showing.
 * ------------------------------------------------------------------------ */

export const SceneCaption: React.FC<{
  eyebrow?: string;
  title: string;
  sub?: string;
  startFrame?: number;
  style?: React.CSSProperties;
}> = ({ eyebrow, title, sub, startFrame = 0, style }) => {
  const anim = useFadeInUp(startFrame);
  return (
    <div
      style={{
        position: "absolute",
        left: 80,
        bottom: 70,
        maxWidth: 720,
        ...anim,
        ...style,
      }}
    >
      {eyebrow && (
        <div
          style={{
            color: theme.accentText,
            fontSize: 14,
            fontWeight: 600,
            letterSpacing: 2,
            textTransform: "uppercase",
            marginBottom: 10,
            fontFamily: theme.font,
          }}
        >
          {eyebrow}
        </div>
      )}
      <div
        style={{
          color: "#fff",
          fontSize: 46,
          fontWeight: 700,
          letterSpacing: -1,
          lineHeight: 1.05,
          fontFamily: theme.font,
        }}
      >
        {title}
      </div>
      {sub && (
        <div
          style={{
            color: theme.muted,
            fontSize: 22,
            marginTop: 14,
            lineHeight: 1.35,
            maxWidth: 680,
            fontFamily: theme.font,
          }}
        >
          {sub}
        </div>
      )}
    </div>
  );
};

/* ---------------------------------------------------------------------------
 * Number-tick helper: linear interp clamped to [0,1] for KPI cards / counters.
 * ------------------------------------------------------------------------ */

export const tickedNumber = (
  frame: number,
  start: number,
  durationFrames: number,
  value: number,
) => {
  const t = Math.max(0, Math.min(1, (frame - start) / durationFrames));
  const eased = 1 - Math.pow(1 - t, 3);
  return value * eased;
};

export { interpolate };
