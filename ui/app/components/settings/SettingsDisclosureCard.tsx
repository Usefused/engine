import { useState, type ReactNode } from "react";

interface SettingsDisclosureCardProps {
  id: string;
  title: string;
  description: ReactNode;
  children: ReactNode;
}

// SettingsDisclosureCard gives independent settings areas one keyboard-native disclosure without unmounting their state.
export function SettingsDisclosureCard({
  id,
  title,
  description,
  children,
}: SettingsDisclosureCardProps) {
  const [expanded, setExpanded] = useState(true);
  const contentID = `${id}-content`;
  const descriptionID = `${id}-description`;

  // handleToggle preserves the mounted content and changes only its disclosure state.
  function handleToggle() {
    // Functional state avoids stale toggles during rapid keyboard or pointer activation.
    setExpanded((current) => !current);
  }

  return (
    <section className="overflow-hidden rounded-xl border border-slate-200 bg-white shadow-sm">
      <div className="p-6">
        <h2>
          <button
            type="button"
            className="group flex w-full items-center justify-between gap-4 text-left focus:outline-none focus-visible:ring-2 focus-visible:ring-blue-500 focus-visible:ring-offset-2"
            aria-expanded={expanded}
            aria-controls={contentID}
            aria-describedby={descriptionID}
            onClick={handleToggle}
          >
            <span className="text-lg font-semibold text-slate-900">{title}</span>
            <svg
              aria-hidden="true"
              viewBox="0 0 20 20"
              fill="none"
              className="h-5 w-5 shrink-0 text-slate-500 transition-transform group-aria-expanded:rotate-180"
            >
              <path
                d="m5 7.5 5 5 5-5"
                stroke="currentColor"
                strokeWidth="1.75"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
            </svg>
          </button>
        </h2>
        <p id={descriptionID} className="mt-1 text-sm text-slate-500">
          {description}
        </p>
        <div id={contentID} hidden={!expanded} className="mt-6">
          {children}
        </div>
      </div>
    </section>
  );
}
