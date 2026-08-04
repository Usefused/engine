import React, { createContext, useContext, useState, useEffect, useRef } from "react";
import { Info, CheckCircle, AlertTriangle, XCircle, HelpCircle, X } from "lucide-react";

// Types
export type ToastType = 'info' | 'success' | 'warning' | 'error' | 'confirm' | 'prompt';

export interface ToastItem {
  id: string;
  type: ToastType;
  message: string;
  duration?: number;
  confirmLabel?: string;
  cancelLabel?: string;
  checkboxLabel?: string;
  placeholder?: string;
  defaultValue?: string;
  resolve?: (value: unknown) => void;
  exiting?: boolean;
}

export interface ConfirmOptions {
  confirmLabel?: string;
  cancelLabel?: string;
  checkboxLabel?: string;
}

export interface PromptOptions {
  placeholder?: string;
  defaultValue?: string;
}

export interface ToastContextType {
  info: (message: string, duration?: number) => void;
  success: (message: string, duration?: number) => void;
  warning: (message: string, duration?: number) => void;
  error: (message: string, duration?: number) => void;
  confirm: {
    (message: string, options?: Omit<ConfirmOptions, 'checkboxLabel'>): Promise<boolean>;
    (message: string, options: ConfirmOptions & { checkboxLabel: string }): Promise<{ confirmed: boolean; permissionGranted: boolean }>;
  };
  prompt: (message: string, options?: PromptOptions) => Promise<string | null>;
}

const ToastContext = createContext<ToastContextType | undefined>(undefined);

export function useToast() {
  const context = useContext(ToastContext);
  if (!context) {
    throw new Error("useToast must be used within a ToastProvider");
  }
  return context;
}

function cancelDismissValue(toast: ToastItem): unknown {
  if (toast.type === "prompt") return null;
  if (toast.checkboxLabel) return { confirmed: false, permissionGranted: false };
  return false;
}

interface InteractiveToastCardProps {
  toast: ToastItem;
  inputValue: string;
  setInputValue: (v: string) => void;
  checkboxValue: boolean;
  setCheckboxValue: (v: boolean) => void;
  iconColors: string;
  Icon: React.ComponentType<{ className?: string }>;
  confirmBtnClass: string;
  handleDismiss: (val: unknown) => void;
}

function InteractiveToastCard({
  toast,
  inputValue,
  setInputValue,
  checkboxValue,
  setCheckboxValue,
  iconColors,
  Icon,
  confirmBtnClass,
  handleDismiss,
}: InteractiveToastCardProps) {
  const handleConfirm = () => {
    if (toast.type === "prompt") {
      handleDismiss(inputValue);
      return;
    }
    handleDismiss(toast.checkboxLabel ? { confirmed: true, permissionGranted: checkboxValue } : true);
  };

  return (
    <>
      {/* Blue accent top bar */}
      <div className="h-[3px] w-full bg-blue-500" />

      {/* Card body */}
      <div className="relative px-5 pt-5 pb-5 flex flex-col items-center text-center gap-3">
        {/* X close — top-right */}
        <button
          data-track="close_toast"
          type="button"
          onClick={(e) => {
            e.stopPropagation();
            handleDismiss(cancelDismissValue(toast));
          }}
          className="absolute top-3 right-3 text-slate-400 hover:text-slate-600 dark:hover:text-slate-300 transition-colors p-1.5 rounded-lg hover:bg-slate-100 dark:hover:bg-slate-800 cursor-pointer"
          aria-label="Close notification"
        >
          <X className="w-3.5 h-3.5" />
        </button>

        {/* Icon badge */}
        <div className={`p-3 rounded-2xl ${iconColors}`}>
          <Icon className="w-6 h-6" />
        </div>

        {/* Message */}
        <p className="text-[13px] font-medium text-gray-700 dark:text-gray-200 leading-relaxed max-w-[240px]">
          {toast.message}
        </p>

        {/* Prompt input */}
        {toast.type === "prompt" && (
          <input
            type="text"
            value={inputValue}
            onChange={(e) => setInputValue(e.target.value)}
            placeholder={toast.placeholder}
            className="w-full text-sm px-3 py-2.5 border border-gray-200 dark:border-slate-600 rounded-xl focus:border-blue-500 focus:ring-2 focus:ring-blue-500/20 outline-none bg-white dark:bg-slate-700 text-gray-900 dark:text-gray-100 placeholder:text-gray-400"
            onKeyDown={(e) => {
              if (e.key === "Enter") handleDismiss(inputValue);
            }}
            autoFocus
          />
        )}

        {/* Permission checkbox — card style */}
        {toast.type === "confirm" && toast.checkboxLabel && (
          <label
            htmlFor={`chk-${toast.id}`}
            className="flex items-center gap-2.5 w-full bg-slate-50 dark:bg-slate-800/60 border border-slate-200 dark:border-slate-700 rounded-xl px-3 py-2.5 cursor-pointer group text-left"
          >
            <input
              type="checkbox"
              id={`chk-${toast.id}`}
              checked={checkboxValue}
              onChange={(e) => setCheckboxValue(e.target.checked)}
              className="w-4 h-4 text-indigo-600 border-slate-300 dark:border-slate-600 rounded focus:ring-indigo-500 cursor-pointer shrink-0"
            />
            <span className="text-xs text-slate-600 dark:text-slate-300 select-none group-hover:text-slate-800 dark:group-hover:text-slate-100 transition-colors">
              {toast.checkboxLabel}
            </span>
          </label>
        )}
      </div>

      {/* Divider */}
      <div className="h-px bg-gray-200 dark:bg-slate-700" />

      {/* iOS-style full-width footer buttons */}
      <div className="flex">
        <button
          data-track="cancel_toast_action"
          type="button"
          onClick={(e) => {
            e.stopPropagation();
            handleDismiss(cancelDismissValue(toast));
          }}
          className="flex-1 py-3 text-sm font-medium text-gray-500 dark:text-gray-400 hover:bg-gray-200 dark:hover:bg-slate-700 transition-colors cursor-pointer border-r border-gray-200 dark:border-slate-700"
        >
          {toast.cancelLabel || "Cancel"}
        </button>
        <button
          data-track="confirm_toast_action"
          type="button"
          onClick={(e) => {
            e.stopPropagation();
            handleConfirm();
          }}
          className={`flex-1 py-3 text-sm transition-colors cursor-pointer ${confirmBtnClass}`}
        >
          {toast.confirmLabel || (toast.type === "prompt" ? "Submit" : "Confirm")}
        </button>
      </div>
    </>
  );
}

interface CompactToastCardProps {
  toast: ToastItem;
  iconColors: string;
  Icon: React.ComponentType<{ className?: string }>;
  handleDismiss: (val: unknown) => void;
}

function CompactToastCard({ toast, iconColors, Icon, handleDismiss }: CompactToastCardProps) {
  return (
    <div className="p-4 flex gap-3 items-center">
      <div className={`p-2 rounded-lg shrink-0 ${iconColors}`}>
        <Icon className="w-5 h-5" />
      </div>
      <p className="flex-1 min-w-0 text-sm font-semibold text-slate-800 dark:text-slate-200 whitespace-pre-wrap break-words">
        {toast.message}
      </p>
      <button
        data-track="dismiss_toast"
        type="button"
        onClick={(e) => {
          e.stopPropagation();
          handleDismiss(false);
        }}
        className="text-slate-400 hover:text-slate-600 dark:hover:text-slate-300 transition-colors p-1 rounded-lg hover:bg-slate-100 dark:hover:bg-slate-800/80 shrink-0 cursor-pointer"
        aria-label="Close notification"
      >
        <X className="w-4 h-4" />
      </button>
    </div>
  );
}

function ToastItemComponent({ toast }: { toast: ToastItem }) {
  const [isVisible, setIsVisible] = useState(false);
  const [inputValue, setInputValue] = useState(toast.defaultValue || "");
  const [checkboxValue, setCheckboxValue] = useState(false);
  const timerRef = useRef<NodeJS.Timeout | null>(null);

  useEffect(() => {
    const timeout = setTimeout(() => setIsVisible(true), 10);
    return () => clearTimeout(timeout);
  }, []);

  const handleDismiss = (val: unknown) => {
    if (toast.resolve) {
      toast.resolve(val);
    }
  };

  const startTimer = () => {
    if (toast.duration && toast.duration > 0) {
      timerRef.current = setTimeout(() => {
        handleDismiss(null);
      }, toast.duration);
    }
  };

  const clearTimer = () => {
    if (timerRef.current) {
      clearTimeout(timerRef.current);
      timerRef.current = null;
    }
  };

  useEffect(() => {
    startTimer();
    return () => clearTimer();
  }, [toast.duration]);

  const isShowing = isVisible && !toast.exiting;
  const isInteractive = toast.type === "confirm" || toast.type === "prompt";

  const Icon = {
    info: Info,
    success: CheckCircle,
    warning: AlertTriangle,
    error: XCircle,
    confirm: HelpCircle,
    prompt: HelpCircle,
  }[toast.type];

  const iconColors = {
    info: "text-blue-500 bg-blue-50 dark:bg-blue-950/40 border border-blue-100 dark:border-blue-900/50",
    success: "text-emerald-500 bg-emerald-50 dark:bg-emerald-950/40 border border-emerald-100 dark:border-emerald-900/50",
    warning: "text-amber-500 bg-amber-50 dark:bg-amber-950/40 border border-amber-100 dark:border-amber-900/50",
    error: "text-red-500 bg-red-50 dark:bg-red-950/40 border border-red-100 dark:border-red-900/50",
    confirm: "text-blue-500 bg-blue-50 dark:bg-blue-950/40 border border-blue-100 dark:border-blue-900/50",
    prompt:  "text-blue-500 bg-blue-50 dark:bg-blue-950/40 border border-blue-100 dark:border-blue-900/50",
  }[toast.type];

  const confirmBtnClass = "text-blue-600 dark:text-blue-400 hover:bg-gray-200 dark:hover:bg-slate-700 font-semibold";

  return (
    <div
      onMouseEnter={clearTimer}
      onMouseLeave={startTimer}
      className={`w-full backdrop-blur-md shadow-2xl overflow-hidden flex flex-col transition-all duration-300 ease-out transform ${
        isShowing
          ? "opacity-100 translate-y-0 scale-100 pointer-events-auto"
          : "opacity-0 translate-y-4 scale-95 pointer-events-none"
      } ${
        isInteractive
          ? "rounded-2xl bg-gray-100 dark:bg-slate-800 border border-gray-200 dark:border-slate-700"
          : "rounded-xl bg-white/95 dark:bg-slate-900/95 border border-slate-200 dark:border-slate-800/80"
      }`}
    >
      {isInteractive ? (
        <InteractiveToastCard
          toast={toast}
          inputValue={inputValue}
          setInputValue={setInputValue}
          checkboxValue={checkboxValue}
          setCheckboxValue={setCheckboxValue}
          iconColors={iconColors}
          Icon={Icon}
          confirmBtnClass={confirmBtnClass}
          handleDismiss={handleDismiss}
        />
      ) : (
        <CompactToastCard toast={toast} iconColors={iconColors} Icon={Icon} handleDismiss={handleDismiss} />
      )}
    </div>
  );
}

export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [toasts, setToasts] = useState<ToastItem[]>([]);

  const dismissToast = (id: string) => {
    setToasts((prev) =>
      prev.map((t) => (t.id === id ? { ...t, exiting: true } : t))
    );
    setTimeout(() => {
      setToasts((prev) => prev.filter((t) => t.id !== id));
    }, 300);
  };

  const showBasicToast = (type: ToastType, message: string, duration = 4000) => {
    const id = Math.random().toString(36).substring(2, 9);
    const newToast: ToastItem = {
      id,
      type,
      message,
      duration,
      resolve: () => dismissToast(id),
    };
    setToasts((prev) => [...prev, newToast]);
  };

  const info = (message: string, duration?: number) => showBasicToast('info', message, duration);
  const success = (message: string, duration?: number) => showBasicToast('success', message, duration);
  const warning = (message: string, duration?: number) => showBasicToast('warning', message, duration);
  const error = (message: string, duration?: number) => showBasicToast('error', message, duration);

  const confirm = ((message: string, options?: ConfirmOptions): Promise<unknown> => {
    return new Promise((resolve) => {
      const id = Math.random().toString(36).substring(2, 9);
      const newToast: ToastItem = {
        id,
        type: 'confirm',
        message,
        duration: 0,
        confirmLabel: options?.confirmLabel,
        cancelLabel: options?.cancelLabel,
        checkboxLabel: options?.checkboxLabel,
        resolve: (val) => {
          dismissToast(id);
          resolve(val);
        }
      };
      setToasts((prev) => [...prev, newToast]);
    });
  }) as ToastContextType['confirm'];

  const prompt = (message: string, options?: PromptOptions): Promise<string | null> => {
    return new Promise((resolve) => {
      const id = Math.random().toString(36).substring(2, 9);
      const newToast: ToastItem = {
        id,
        type: 'prompt',
        message,
        duration: 0,
        placeholder: options?.placeholder,
        defaultValue: options?.defaultValue,
        resolve: (val) => {
          dismissToast(id);
          resolve(val as string | null);
        }
      };
      setToasts((prev) => [...prev, newToast]);
    });
  };

  return (
    <ToastContext.Provider value={{ info, success, warning, error, confirm, prompt }}>
      {children}
      {/* Toast stacking container */}
      <div className="fixed bottom-4 left-1/2 -translate-x-1/2 md:left-auto md:right-4 md:translate-x-0 z-[9999] flex flex-col-reverse gap-3 max-w-sm w-[calc(100%-2rem)] md:w-80 pointer-events-none">
        {toasts.map((toast) => (
          <ToastItemComponent key={toast.id} toast={toast} />
        ))}
      </div>
    </ToastContext.Provider>
  );
}
