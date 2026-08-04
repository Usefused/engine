import React from "react";

interface LogoProps {
  className?: string;
  logoClassName?: string;
  textClassName?: string;
  size?: "sm" | "md" | "lg" | "xl";
  hideText?: boolean;
  grayscale?: boolean;
}

const LOGO_SIZES: Record<NonNullable<LogoProps["size"]>, { logoSizeClass: string; textSizeClass: string; intrinsicW: number; intrinsicH: number }> = {
  sm: { logoSizeClass: "h-5 md:h-6 w-auto", textSizeClass: "text-sm md:text-base", intrinsicW: 24, intrinsicH: 24 },
  md: { logoSizeClass: "h-6 md:h-7 w-auto", textSizeClass: "text-base md:text-lg", intrinsicW: 28, intrinsicH: 28 },
  lg: { logoSizeClass: "h-7 md:h-10 w-auto", textSizeClass: "text-lg md:text-2xl", intrinsicW: 40, intrinsicH: 40 },
  xl: { logoSizeClass: "h-8 md:h-14 w-auto", textSizeClass: "text-xl md:text-2xl", intrinsicW: 56, intrinsicH: 56 },
};

export function Logo({
  className = "",
  logoClassName = "",
  textClassName = "",
  size = "md",
  hideText = false,
  grayscale = false,
}: LogoProps) {
  const { logoSizeClass, textSizeClass, intrinsicW, intrinsicH } = LOGO_SIZES[size];

  const grayscaleClass = grayscale
    ? "grayscale hover:grayscale-0 transition-all opacity-80 hover:opacity-100"
    : "";

  return (
    <div className={`flex items-center gap-2.5 font-bold text-slate-900 ${className}`}>
      <img
        src="/logo.svg"
        alt="Fused Logo"
        width={intrinsicW}
        height={intrinsicH}
        className={`${logoSizeClass} ${grayscaleClass} ${logoClassName}`}
      />
      {!hideText && (
        <span className={`tracking-tighter uppercase ${textSizeClass} ${textClassName}`}>
          Fused
        </span>
      )}
    </div>
  );
}
