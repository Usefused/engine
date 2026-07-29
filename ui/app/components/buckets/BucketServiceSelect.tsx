import { useEffect, useRef, useState } from "react";
import { Check, ChevronDown, Search, X } from "lucide-react";

export type ServiceSelectOption = {
  id: string;
  label: string;
};

type BucketServiceSelectProps = {
  id: string;
  label: string;
  placeholder: string;
  options: ServiceSelectOption[];
  search: string;
  selectedServiceId?: string;
  className?: string;
  buttonClassName?: string;
  allowAll?: boolean;
  allLabel?: string;
  hideLabel?: boolean;
  searchPlaceholder?: string;
  emptyLabel?: string;
  preserveSelectionOnSearch?: boolean;
  onSearchChange: (search: string) => void;
  onSelectedServiceChange?: (serviceId: string) => void;
};

const defaultSelectProps = {
  className: "",
  buttonClassName: "",
  allowAll: false,
  allLabel: "All services",
  hideLabel: false,
  searchPlaceholder: "Search services",
  emptyLabel: "No services found.",
  preserveSelectionOnSearch: false,
};

export function BucketServiceSelect(props: BucketServiceSelectProps) {
  // Centralized defaults keep every bucket selector visually consistent while
  // allowing the connect-auth instance to use bucket-specific copy.
  const {
    id,
    label,
    placeholder,
    options,
    search,
    selectedServiceId,
    className,
    buttonClassName,
    allowAll,
    allLabel,
    hideLabel,
    searchPlaceholder,
    emptyLabel,
    preserveSelectionOnSearch,
    onSearchChange,
    onSelectedServiceChange,
  } = { ...defaultSelectProps, ...props };
  const [open, setOpen] = useState(false);
  const [selectedOption, setSelectedOption] = useState<ServiceSelectOption>();
  const containerRef = useSelectDismissal(open, setOpen);
  const state = useServiceSelectState(
    options,
    selectedServiceId,
    selectedOption,
    search,
    placeholder
  );
  return (
    <div ref={containerRef} className={`relative ${className}`}>
      <span
        className={
          hideLabel
            ? "sr-only"
            : "mb-1 block text-xs font-medium text-slate-500"
        }
      >
        {label}
      </span>
      <button
        type="button"
        onClick={() => setOpen((next) => !next)}
        className={`flex h-[38px] w-full items-center justify-between gap-2 rounded-lg border border-slate-200 bg-white px-3 text-left text-sm text-slate-800 shadow-sm outline-none hover:border-slate-300 focus:border-blue-500 focus:ring-2 focus:ring-blue-500/20 ${buttonClassName}`}
        aria-expanded={open}
        aria-controls={id}
      >
        <span
          className={`truncate ${
            state.hasValue ? "text-slate-900" : "text-slate-500"
          }`}
        >
          {state.displayLabel}
        </span>
        <ChevronDown className="h-4 w-4 shrink-0 text-slate-400" />
      </button>
      {open && (
        <ServiceSelectMenu
          id={id}
          search={search}
          selectedServiceId={selectedServiceId}
          options={state.visibleOptions}
          allowAll={allowAll}
          allLabel={allLabel}
          searchPlaceholder={searchPlaceholder}
          emptyLabel={emptyLabel}
          preserveSelectionOnSearch={preserveSelectionOnSearch}
          setOpen={setOpen}
          setSelectedOption={setSelectedOption}
          onSearchChange={onSearchChange}
          onSelectedServiceChange={onSelectedServiceChange}
        />
      )}
    </div>
  );
}

function useSelectDismissal(
  open: boolean,
  setOpen: (open: boolean) => void
) {
  const containerRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    // The listener exists only while expanded so closed selectors add no
    // document-level interaction work and each instance owns its boundary.
    const handlePointerDown = (event: PointerEvent) => {
      if (!containerRef.current?.contains(event.target as Node)) setOpen(false);
    };
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      setOpen(false);
      containerRef.current?.querySelector<HTMLButtonElement>("button")?.focus();
    };
    document.addEventListener("pointerdown", handlePointerDown);
    document.addEventListener("keydown", handleKeyDown);
    return () => {
      document.removeEventListener("pointerdown", handlePointerDown);
      document.removeEventListener("keydown", handleKeyDown);
    };
  }, [open, setOpen]);
  return containerRef;
}

function ServiceSelectMenu({
  id,
  search,
  selectedServiceId,
  options,
  allowAll,
  allLabel,
  searchPlaceholder,
  emptyLabel,
  preserveSelectionOnSearch,
  setOpen,
  setSelectedOption,
  onSearchChange,
  onSelectedServiceChange,
}: {
  id: string;
  search: string;
  selectedServiceId?: string;
  options: ServiceSelectOption[];
  allowAll: boolean;
  allLabel: string;
  searchPlaceholder: string;
  emptyLabel: string;
  preserveSelectionOnSearch: boolean;
  setOpen: (open: boolean) => void;
  setSelectedOption: (option?: ServiceSelectOption) => void;
  onSearchChange: (search: string) => void;
  onSelectedServiceChange?: (serviceId: string) => void;
}) {
  return (
    <div
      id={id}
      className="absolute right-0 z-[70] mt-2 w-full min-w-[260px] overflow-hidden rounded-lg border border-slate-200 bg-white shadow-xl"
    >
      <ServiceSearchInput
        search={search}
        onSearchChange={onSearchChange}
        onSelectedServiceChange={onSelectedServiceChange}
        placeholder={searchPlaceholder}
        preserveSelection={preserveSelectionOnSearch}
      />
      <div className="max-h-56 overflow-y-auto p-1">
        {allowAll && (
          <ServiceOptionRow
            label={allLabel}
            selected={!selectedServiceId}
            onSelect={() =>
              selectService(
                "",
                "",
                setOpen,
                setSelectedOption,
                onSearchChange,
                onSelectedServiceChange
              )
            }
          />
        )}
        <ServiceOptions
          options={options}
          emptyLabel={emptyLabel}
          selectedServiceId={selectedServiceId}
          setOpen={setOpen}
          setSelectedOption={setSelectedOption}
          onSearchChange={onSearchChange}
          onSelectedServiceChange={onSelectedServiceChange}
        />
      </div>
    </div>
  );
}

function ServiceSearchInput({
  search,
  onSearchChange,
  onSelectedServiceChange,
  placeholder,
  preserveSelection,
}: {
  search: string;
  onSearchChange: (search: string) => void;
  onSelectedServiceChange?: (serviceId: string) => void;
  placeholder: string;
  preserveSelection: boolean;
}) {
  return (
    <div className="border-b border-slate-100 p-2">
      <div className="relative">
        <Search className="pointer-events-none absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-slate-400" />
        <input
          autoFocus
          value={search}
          onChange={(event) =>
            handleSearchInput(
              event.target.value,
              onSearchChange,
              preserveSelection ? undefined : onSelectedServiceChange
            )
          }
          placeholder={placeholder}
          className="h-9 w-full rounded-md border border-slate-200 bg-white pl-8 pr-8 text-sm text-slate-800 outline-none placeholder:text-slate-400 focus:border-blue-400 focus:ring-2 focus:ring-blue-100"
        />
        {search && (
          <button
            type="button"
            onClick={() =>
              clearSelection(
                onSearchChange,
                preserveSelection ? undefined : onSelectedServiceChange
              )
            }
            className="absolute right-2 top-1/2 -translate-y-1/2 rounded p-1 text-slate-400 hover:bg-slate-100 hover:text-slate-600"
            aria-label="Clear service search"
            title="Clear"
          >
            <X className="h-3.5 w-3.5" />
          </button>
        )}
      </div>
    </div>
  );
}

function ServiceOptions({
  options,
  emptyLabel,
  selectedServiceId,
  setOpen,
  setSelectedOption,
  onSearchChange,
  onSelectedServiceChange,
}: {
  options: ServiceSelectOption[];
  emptyLabel: string;
  selectedServiceId?: string;
  setOpen: (open: boolean) => void;
  setSelectedOption: (option?: ServiceSelectOption) => void;
  onSearchChange: (search: string) => void;
  onSelectedServiceChange?: (serviceId: string) => void;
}) {
  if (options.length === 0)
    return (
      <p className="px-3 py-6 text-center text-sm text-slate-400">
        {emptyLabel}
      </p>
    );
  return (
    <>
      {options.map((option) => (
        <ServiceOptionRow
          key={option.id}
          label={option.label}
          selected={option.id === selectedServiceId}
          onSelect={() =>
            selectService(
              option.id,
              option.label,
              setOpen,
              setSelectedOption,
              onSearchChange,
              onSelectedServiceChange
            )
          }
        />
      ))}
    </>
  );
}

function useServiceSelectState(
  options: ServiceSelectOption[],
  selectedServiceId: string | undefined,
  selectedOption: ServiceSelectOption | undefined,
  search: string,
  placeholder: string
) {
  const serviceOptions = withSelectedOption(
    options,
    selectedServiceId,
    selectedOption
  );
  const selected = serviceOptions.find(
    (option) => option.id === selectedServiceId
  );
  const visibleOptions = filterOptions(serviceOptions, search);
  return {
    visibleOptions,
    displayLabel: selected?.label || search.trim() || placeholder,
    hasValue: Boolean(selected || search.trim()),
  };
}

function ServiceOptionRow({
  label,
  selected,
  onSelect,
}: {
  label: string;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onSelect}
      className="flex w-full items-center justify-between gap-3 rounded-md px-3 py-2 text-left text-sm text-slate-700 hover:bg-slate-50"
    >
      <span className="truncate">{label}</span>
      {selected && <Check className="h-4 w-4 shrink-0 text-blue-600" />}
    </button>
  );
}

function handleSearchInput(
  input: string,
  onSearchChange: (search: string) => void,
  onSelectedServiceChange?: (serviceId: string) => void
) {
  onSearchChange(input);
  if (onSelectedServiceChange) onSelectedServiceChange("");
}

function selectService(
  serviceId: string,
  label: string,
  setOpen: (open: boolean) => void,
  setSelectedOption: (option?: ServiceSelectOption) => void,
  onSearchChange: (search: string) => void,
  onSelectedServiceChange?: (serviceId: string) => void
) {
  onSelectedServiceChange?.(serviceId);
  setSelectedOption(serviceId ? { id: serviceId, label } : undefined);
  // Selection and search are separate state: retaining the selected label as
  // a query made reopening the menu hide every other service.
  onSearchChange("");
  setOpen(false);
}

function clearSelection(
  onSearchChange: (search: string) => void,
  onSelectedServiceChange?: (serviceId: string) => void
) {
  onSearchChange("");
  onSelectedServiceChange?.("");
}

function filterOptions(options: ServiceSelectOption[], search: string) {
  const needle = search.trim().toLowerCase();
  if (!needle) return options;
  return options.filter(
    (option) =>
      option.label.toLowerCase().includes(needle) ||
      option.id.toLowerCase().includes(needle)
  );
}

function withSelectedOption(
  options: ServiceSelectOption[],
  selectedServiceId?: string,
  selectedOption?: ServiceSelectOption
) {
  if (
    selectedServiceId &&
    !options.some((option) => option.id === selectedServiceId)
  ) {
    return [
      {
        id: selectedServiceId,
        label:
          selectedOption?.id === selectedServiceId
            ? selectedOption.label
            : selectedServiceId.slice(0, 8),
      },
      ...options,
    ];
  }
  return options;
}
