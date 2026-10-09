import { useEffect, useRef, useState, type MouseEvent, type PointerEvent } from 'react';
import { Sparkles } from 'lucide-react';

type Position = { x: number; y: number };
type Drag = { pointer: number; start: Position; origin: Position; moved: boolean };

/** Keeps the entire launcher reachable after dragging or resizing the viewport. */
function constrain(position: Position): Position {
  return {
    x: Math.max(12, Math.min(position.x, window.innerWidth - 60)),
    y: Math.max(12, Math.min(position.y, window.innerHeight - 60)),
  };
}

/** Retains the user's launcher position across chat toggles and distinguishes dragging from opening chat. */
export default function FusedAgentLauncher({ visible, onOpen, aboveActions = false }: { visible: boolean; onOpen: () => void; aboveActions?: boolean }) {
  const [position, setPosition] = useState<Position | null>(null);
  const [dragging, setDragging] = useState(false);
  const drag = useRef<Drag | null>(null);
  const suppressClick = useRef(false);

  // Viewport changes must not strand a previously moved launcher offscreen.
  useEffect(() => {
    /** Preserve the default bottom-right anchor until the user deliberately moves it. */
    function resize() {
      setPosition(previous => previous ? constrain(previous) : null);
    }
    window.addEventListener('resize', resize);
    return () => window.removeEventListener('resize', resize);
  }, []);

  /** Capture the pointer so a held drag continues outside the small button, including on touchscreens. */
  function begin(event: PointerEvent<HTMLButtonElement>) {
    // Secondary buttons and additional fingers must retain their normal behavior.
    if (!event.isPrimary || event.button !== 0) return;
    const bounds = event.currentTarget.getBoundingClientRect();
    suppressClick.current = false;
    drag.current = { pointer: event.pointerId, start: { x: event.clientX, y: event.clientY }, origin: { x: bounds.left, y: bounds.top }, moved: false };
    event.currentTarget.setPointerCapture(event.pointerId);
  }

  /** A small movement threshold allows ordinary clicks without accidentally nudging the launcher. */
  function move(event: PointerEvent<HTMLButtonElement>) {
    const active = drag.current;
    // Only the captured pointer owns this gesture.
    if (!active || active.pointer !== event.pointerId) return;
    const dx = event.clientX - active.start.x;
    const dy = event.clientY - active.start.y;
    // Ignore hand jitter until the interaction clearly becomes a drag.
    if (!active.moved && Math.hypot(dx, dy) < 6) return;
    active.moved = true;
    suppressClick.current = true;
    setDragging(true);
    setPosition(constrain({ x: active.origin.x + dx, y: active.origin.y + dy }));
  }

  /** End or cancel the gesture while preserving the flag that suppresses its synthetic click. */
  function finish(event: PointerEvent<HTMLButtonElement>) {
    // Lost capture may follow pointer-up; settling twice must not clear the click guard.
    if (!drag.current || drag.current.pointer !== event.pointerId) return;
    drag.current = null;
    setDragging(false);
  }

  /** Pointer drags never open chat; ordinary clicks and keyboard activation do. */
  function activate(event: MouseEvent<HTMLButtonElement>) {
    // Keyboard clicks have detail zero and must remain usable after a pointer drag.
    if (suppressClick.current && event.detail !== 0) {
      event.preventDefault();
      suppressClick.current = false;
      return;
    }
    suppressClick.current = false;
    onOpen();
  }

  // Keep component state mounted while chat is open so closing it restores the chosen position.
  if (!visible) return null;
  // The untouched launcher clears drawer actions; an explicitly dragged position always takes precedence.
  const anchor = position ? '' : `${aboveActions ? 'bottom-24' : 'bottom-5'} right-5`;
  return <button data-fused-agent type="button" onClick={activate} onPointerDown={begin} onPointerMove={move} onPointerUp={finish} onPointerCancel={finish} onLostPointerCapture={finish}
    aria-label="Ask Fused" title="Ask Fused · drag to move" aria-controls="fused-assistant" aria-expanded={false}
    style={position ? { left: position.x, top: position.y } : undefined}
    className={`fixed z-40 flex h-12 w-12 touch-none select-none items-center justify-center rounded-full border border-slate-300 bg-white text-slate-950 shadow-md transition-colors hover:border-slate-400 hover:bg-slate-50 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-slate-950 ${anchor} ${dragging ? 'cursor-grabbing shadow-lg' : 'cursor-grab'}`}>
    <Sparkles className="pointer-events-none h-5 w-5" strokeWidth={1.6} aria-hidden="true" />
  </button>;
}
