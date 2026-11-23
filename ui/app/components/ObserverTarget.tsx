import { useEffect, useRef } from "react";
import { Loader2 } from "lucide-react";

interface ObserverTargetProps {
  onIntersect: () => void;
  disabled: boolean;
}

/** Triggers pagination callbacks when the sentinel enters the viewport margin. */
export function ObserverTarget({ onIntersect, disabled }: ObserverTargetProps) {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    // Skip attaching an observer while pagination is disabled or before mount.
    if (disabled || !ref.current) return;
    const observer = new IntersectionObserver(
      ([entry]) => {
        // Request the next page only when the sentinel becomes visible.
        if (entry.isIntersecting) {
          onIntersect();
        }
      },
      { rootMargin: "100px" }
    );
    observer.observe(ref.current);
    return () => observer.disconnect();
  }, [onIntersect, disabled]);

  return (
    <div ref={ref} className="h-4 w-full flex justify-center items-center py-2">
      {!disabled && <Loader2 className="w-4 h-4 animate-spin text-slate-400" />}
    </div>
  );
}
