import { useNavigate, useSearchParams } from "@remix-run/react";
import { ChevronDown } from "lucide-react";

interface VersionSelectorProps {
  currentVersionTag?: string;
  versions: Array<{id: string, name: string, is_public: boolean, status?: string, created_at: string, updated_at: string}>;
}

function versionStateLabel(version: VersionSelectorProps["versions"][number]) {
  if (version.status === "deprecated") return "Deprecated";
  if (version.status === "draft") return "Draft";
  if (version.status === "public") return "Public";
  return version.is_public ? "Public" : "Draft";
}

export function VersionSelector({ currentVersionTag, versions }: VersionSelectorProps) {
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();

  if (!versions || versions.length === 0) return null;

  return (
    <div className="relative min-w-0 w-full text-left sm:w-auto">
      <select
        className="block w-full min-w-0 appearance-none truncate rounded-md border border-slate-200 bg-slate-50 py-1 pl-3 pr-8 text-sm font-medium text-slate-700 focus:border-transparent focus:outline-none focus:ring-2 focus:ring-blue-500 sm:max-w-[26rem]"
        value={currentVersionTag || ""}
        onChange={(e) => {
          const newVersion = e.target.value;
          const params = new URLSearchParams(searchParams);
          if (newVersion === "") {
            params.delete("version");
          } else {
            params.set("version", newVersion);
          }
          navigate(`?${params.toString()}`);
        }}
      >
        <option value="" disabled hidden>Select service version</option>
        {versions.map((v) => (
          <option key={v.id} value={v.name}>
            {v.name} ({versionStateLabel(v)})
          </option>
        ))}
      </select>
      <div className="pointer-events-none absolute inset-y-0 right-0 flex items-center px-2 text-slate-500">
        <ChevronDown size={14} />
      </div>
    </div>
  );
}
