import React from "react";

type IconProps = {
  size?: number;
  color?: string;
  style?: React.CSSProperties;
};

const baseStroke = (size = 14, color = "currentColor") => ({
  width: size,
  height: size,
  viewBox: "0 0 20 20",
  fill: "none" as const,
  stroke: color,
  strokeWidth: 1.8,
  strokeLinecap: "round" as const,
  strokeLinejoin: "round" as const,
});

const filled = (size = 14, color = "currentColor") => ({
  width: size,
  height: size,
  viewBox: "0 0 20 20",
  fill: color,
});

export const Icon = {
  Phone: ({ size, color, style }: IconProps) => (
    <svg {...baseStroke(size, color)} style={style} aria-hidden>
      <path d="M3 4.5C3 3.7 3.7 3 4.5 3h2.2c.6 0 1.1.4 1.3 1l.8 2.5c.1.5 0 1-.4 1.4L7.5 8.8a11 11 0 005.7 5.7l.9-.9c.4-.4.9-.5 1.4-.4L18 14c.6.2 1 .7 1 1.3v2.2c0 .8-.7 1.5-1.5 1.5C9.5 19 1 10.5 1 2.5 1 1.7 1.7 1 2.5 1H4" />
    </svg>
  ),
  Search: ({ size, color, style }: IconProps) => (
    <svg {...baseStroke(size, color)} style={style} aria-hidden>
      <circle cx="9" cy="9" r="6" />
      <line x1="13.5" y1="13.5" x2="18" y2="18" />
    </svg>
  ),
  Warning: ({ size, color, style }: IconProps) => (
    <svg {...baseStroke(size, color)} style={style} aria-hidden>
      <polygon points="10 3 17 16 3 16" />
      <line x1="10" y1="8" x2="10" y2="12" />
      <circle cx="10" cy="14.2" r=".4" fill={color || "currentColor"} stroke="none" />
    </svg>
  ),
  ArrowRight: ({ size, color, style }: IconProps) => (
    <svg {...baseStroke(size, color)} style={style} aria-hidden>
      <polyline points="6 4 12 10 6 16" />
    </svg>
  ),
  X: ({ size, color, style }: IconProps) => (
    <svg {...baseStroke(size, color)} style={style} aria-hidden>
      <line x1="5" y1="5" x2="15" y2="15" />
      <line x1="15" y1="5" x2="5" y2="15" />
    </svg>
  ),
  Check: ({ size, color, style }: IconProps) => (
    <svg {...baseStroke(size, color)} style={style} aria-hidden>
      <polyline points="4 10 8 14 16 6" />
    </svg>
  ),
  Globe: ({ size, color, style }: IconProps) => (
    <svg {...baseStroke(size, color)} style={style} aria-hidden>
      <circle cx="10" cy="10" r="7.5" />
      <line x1="2.5" y1="10" x2="17.5" y2="10" />
      <path d="M10 2.5a11 11 0 010 15M10 2.5a11 11 0 000 15" />
    </svg>
  ),
  Stack: ({ size, color, style }: IconProps) => (
    <svg {...baseStroke(size, color)} style={style} aria-hidden>
      <rect x="3" y="3" width="14" height="3" rx=".5" />
      <rect x="3" y="8.5" width="14" height="3" rx=".5" />
      <rect x="3" y="14" width="14" height="3" rx=".5" />
    </svg>
  ),
  Dollar: ({ size, color, style }: IconProps) => (
    <svg {...baseStroke(size, color)} style={style} aria-hidden>
      <line x1="10" y1="2.5" x2="10" y2="17.5" />
      <path d="M14 6.5c0-1.4-1.8-2.5-4-2.5S6 5.1 6 6.5 7.8 9 10 9s4 1.1 4 2.5-1.8 2.5-4 2.5-4-1.1-4-2.5" />
    </svg>
  ),
  Bolt: ({ size, color, style }: IconProps) => (
    <svg {...baseStroke(size, color)} style={style} aria-hidden>
      <polygon points="11 2 4 11 9 11 9 18 16 9 11 9 11 2" fill={color || "currentColor"} />
    </svg>
  ),
  Chevron: ({ size, color, style }: IconProps) => (
    <svg {...baseStroke(size, color)} style={style} aria-hidden>
      <polyline points="7 5 13 10 7 15" />
    </svg>
  ),
  Info: ({ size, color, style }: IconProps) => (
    <svg {...baseStroke(size, color)} style={style} aria-hidden>
      <circle cx="10" cy="10" r="7.5" />
      <line x1="10" y1="9" x2="10" y2="14" />
      <circle cx="10" cy="6.5" r=".5" fill={color || "currentColor"} stroke="none" />
    </svg>
  ),
  External: ({ size, color, style }: IconProps) => (
    <svg {...baseStroke(size, color)} style={style} aria-hidden>
      <path d="M11 3h6v6" />
      <line x1="17" y1="3" x2="9" y2="11" />
      <path d="M15 13v3a2 2 0 01-2 2H5a2 2 0 01-2-2V8a2 2 0 012-2h3" />
    </svg>
  ),
  Calendar: ({ size, color, style }: IconProps) => (
    <svg {...baseStroke(size, color)} style={style} aria-hidden>
      <rect x="3" y="4.5" width="14" height="13" rx="1.5" />
      <line x1="3" y1="8" x2="17" y2="8" />
      <line x1="7" y1="2.5" x2="7" y2="6" />
      <line x1="13" y1="2.5" x2="13" y2="6" />
    </svg>
  ),
  Filter: ({ size, color, style }: IconProps) => (
    <svg {...baseStroke(size, color)} style={style} aria-hidden>
      <polygon points="3 4 17 4 12 11 12 16 8 17 8 11 3 4" />
    </svg>
  ),
  PulseDot: ({ size = 8, color = "#23a55a", style }: IconProps) => (
    <svg width={size} height={size} viewBox="0 0 8 8" style={style} aria-hidden>
      <circle cx="4" cy="4" r="3.5" fill={color} />
    </svg>
  ),
  // The icon mark for the wordmark — abstract phone-handset glyph + stacked "storage" bar
  Mark: ({ size = 32, color = "#9aa6ff", style }: IconProps) => (
    <svg width={size} height={size} viewBox="0 0 40 40" style={style} aria-hidden>
      <rect x="2" y="2" width="36" height="36" rx="9" fill={color} opacity={0.16} />
      <rect x="2" y="2" width="36" height="36" rx="9" fill="none" stroke={color} strokeWidth={1.5} />
      <path
        d="M12 14c0-1.1.9-2 2-2h3.5c.9 0 1.7.6 2 1.5l1 3a2 2 0 01-.5 2.1l-1.4 1.4a14 14 0 006.4 6.4l1.4-1.4a2 2 0 012.1-.5l3 1c.9.3 1.5 1.1 1.5 2V31c0 1.1-.9 2-2 2-10.5 0-19-8.5-19-19z"
        fill={color}
      />
    </svg>
  ),
};
