/** @type {import('tailwindcss').Config} */
const c = (v) => `rgb(var(--${v}) / <alpha-value>)`;
export default {
  darkMode: "class",
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        bg: c("bg"),
        surface: c("surface"),
        sunken: c("sunken"),
        line: c("line"),
        "line-strong": c("line-strong"),
        fg: c("fg"),
        muted: c("muted"),
        faint: c("faint"),
        accent: { DEFAULT: c("accent"), fg: c("accent-fg"), soft: c("accent-soft") },
        ok: { DEFAULT: c("ok"), soft: c("ok-soft") },
        warn: { DEFAULT: c("warn"), soft: c("warn-soft") },
        danger: { DEFAULT: c("danger"), soft: c("danger-soft") },
        idle: { DEFAULT: c("idle"), soft: c("idle-soft") },
      },
      fontFamily: {
        sans: ["Onest", "system-ui", "-apple-system", "Segoe UI", "sans-serif"],
        mono: ["JetBrains Mono", "ui-monospace", "SFMono-Regular", "Menlo", "monospace"],
      },
      fontSize: {
        "2xs": ["11px", "16px"],
        xs: ["12px", "16px"],
        sm: ["13px", "20px"],
        base: ["14px", "22px"],
        lg: ["16px", "24px"],
        xl: ["20px", "28px"],
        "2xl": ["26px", "32px"],
      },
      borderRadius: { sm: "4px", DEFAULT: "6px", md: "8px", lg: "10px" },
      boxShadow: {
        pop: "0 1px 2px rgb(0 0 0 / 0.06), 0 8px 24px -6px rgb(var(--shadow) / 0.22)",
        hair: "0 1px 0 rgb(var(--line) / 1)",
      },
      keyframes: {
        "fade-in": { from: { opacity: "0" }, to: { opacity: "1" } },
        "pop-in": { from: { opacity: "0", transform: "translateY(-2px) scale(.98)" }, to: { opacity: "1", transform: "none" } },
        shimmer: { "0%": { backgroundPosition: "-200% 0" }, "100%": { backgroundPosition: "200% 0" } },
        pulse2: { "0%,100%": { opacity: "1" }, "50%": { opacity: ".35" } },
        stripe: { from: { backgroundPosition: "0 0" }, to: { backgroundPosition: "16px 0" } },
      },
      animation: {
        "fade-in": "fade-in 120ms ease-out",
        "pop-in": "pop-in 140ms cubic-bezier(.2,.8,.2,1)",
        shimmer: "shimmer 1.6s linear infinite",
        pulse2: "pulse2 1.4s ease-in-out infinite",
        stripe: "stripe 0.8s linear infinite",
      },
    },
  },
  plugins: [],
};
