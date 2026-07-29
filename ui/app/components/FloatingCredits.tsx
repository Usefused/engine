import { useState, useEffect, useRef } from "react";
import { api, Account, BASE } from "~/lib/api";
import { Wallet, GripVertical } from "lucide-react";
import { fetchEventSource } from "@microsoft/fetch-event-source";
import { getApiKey } from "~/lib/session";

export function FloatingCredits() {
  const [account, setAccount] = useState<Account | null>(null);
  const [position, setPosition] = useState({ x: 0, y: 0 });
  const [isDragging, setIsDragging] = useState(false);
  const widgetRef = useRef<HTMLDivElement>(null);

  // Fetch account details on mount and subscribe to live balance updates
  useEffect(() => {
    let active = true;
    const token = getApiKey() || "";
    if (!token) {
      return;
    }
    const ctrl = new AbortController();
    let retryDelay = 2000;
    const maxRetryDelay = 60000;
    let retryCount = 0;
    
    const fetchBalance = () => {
      api.getAccount()
        .then((acc) => {
          if (active) setAccount(acc);
        })
        .catch((err) => {
          console.error("Failed to fetch account balance:", err);
        });
    };

    fetchBalance();

    // Listen for real-time balance deductions
    fetchEventSource(`${BASE}/account/balance/stream`, {
      headers: {
        "X-API-Key": token,
      },
      signal: ctrl.signal,
      async onopen(response) {
        if (!response.ok) {
          throw new Error(`Failed to connect to balance stream: ${response.status}`);
        }
        retryDelay = 2000; // reset delay on successful connection
        retryCount = 0; // reset retry count
      },
      onmessage(ev) {
        if (ev.event === "deduction") {
          try {
            const data = JSON.parse(ev.data);
            if (typeof data.deduction === "number" && active) {
              setAccount((prev) => {
                if (!prev) return null;
                return {
                  ...prev,
                  credit_balance: (prev.credit_balance ?? 0) - data.deduction,
                };
              });
            }
          } catch (e) {
            console.error("Failed to parse deduction event:", e);
          }
        }
      },
      onerror(err) {
        if (ctrl.signal.aborted) {
          throw err; // Stop fetchEventSource from retrying
        }
        if (retryCount >= 3) {
          console.error("Balance stream connection lost. Max retries exceeded. Stopping.");
          throw err; // Stop fetchEventSource from retrying further
        }
        console.warn(`Balance stream connection lost. Retrying in ${retryDelay}ms (Attempt ${retryCount + 1}/3)...`, err);
        const delay = retryDelay;
        retryDelay = Math.min(retryDelay * 1.5, maxRetryDelay);
        retryCount++;
        return delay; // fetchEventSource uses this return value as the retry interval
      }
    });

    return () => {
      active = false;
      ctrl.abort();
    };
  }, []);

  const handleMouseDown = (e: React.MouseEvent<HTMLDivElement>) => {
    // Only drag with left click
    if (e.button !== 0) return;
    
    // Prevent default selection text behavior
    e.preventDefault();
    setIsDragging(true);

    const startX = e.clientX;
    const startY = e.clientY;
    const startPosX = position.x;
    const startPosY = position.y;

    const handleMouseMove = (moveEvent: MouseEvent) => {
      const dx = moveEvent.clientX - startX;
      const dy = moveEvent.clientY - startY;
      setPosition({
        x: startPosX + dx,
        y: startPosY + dy,
      });
    };

    const handleMouseUp = () => {
      setIsDragging(false);
      document.removeEventListener("mousemove", handleMouseMove);
      document.removeEventListener("mouseup", handleMouseUp);
    };

    document.addEventListener("mousemove", handleMouseMove);
    document.addEventListener("mouseup", handleMouseUp);
  };

  const handleTouchStart = (e: React.TouchEvent<HTMLDivElement>) => {
    setIsDragging(true);

    const touch = e.touches[0];
    const startX = touch.clientX;
    const startY = touch.clientY;
    const startPosX = position.x;
    const startPosY = position.y;

    const handleTouchMove = (moveEvent: TouchEvent) => {
      const touchMove = moveEvent.touches[0];
      const dx = touchMove.clientX - startX;
      const dy = touchMove.clientY - startY;
      setPosition({
        x: startPosX + dx,
        y: startPosY + dy,
      });
    };

    const handleTouchEnd = () => {
      setIsDragging(false);
      document.removeEventListener("touchmove", handleTouchMove);
      document.removeEventListener("touchend", handleTouchEnd);
    };

    document.addEventListener("touchmove", handleTouchMove, { passive: false });
    document.addEventListener("touchend", handleTouchEnd);
  };

  if (!account) return null;

  return (
    <div
      ref={widgetRef}
      style={{
        transform: `translate(${position.x}px, ${position.y}px)`,
        touchAction: "none",
      }}
      className={`fixed top-12 right-4 z-20 flex items-center gap-1.5 px-2.5 py-1.5 rounded-lg select-none
        border border-slate-200/50 backdrop-blur-md transition-shadow duration-200 text-xs
        ${isDragging 
          ? "cursor-grabbing bg-white/95 shadow-lg border-blue-200 scale-102" 
          : "cursor-grab bg-white/75 shadow-sm hover:shadow hover:bg-white/85"
        }`}
      onMouseDown={handleMouseDown}
      onTouchStart={handleTouchStart}
    >
      {/* Drag Indicator handle */}
      <div className="text-slate-400 hover:text-slate-500">
        <GripVertical className="w-3.5 h-3.5" />
      </div>

      {/* Credit Icon */}
      <Wallet className="w-3.5 h-3.5 text-blue-600 shrink-0" />

      {/* Credit Info */}
      <span className="font-extrabold text-slate-700 tabular-nums">
        ${(account.credit_balance ?? 0).toFixed(2)}
      </span>
    </div>
  );
}
