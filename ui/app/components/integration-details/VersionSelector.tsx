import { useNavigate, useSearchParams } from "@remix-run/react";
import { ChevronDown } from "lucide-react";

interface VersionSelectorProps {
  currentVersionTag?: string;
  versions: Array<{id: string, name: string, is_public: boolean, status?: string, created_at: string, updated_at: string}>;
}

export function VersionSelector({ currentVersionTag, versions }: VersionSelectorProps) {
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();

  if (!versions || versions.length === 0) return null;

  return (
    <div className="relative inline-block text-left">
      <select
        className="appearance-none bg-slate-50 border border-slate-200 text-slate-700 text-sm rounded-md pl-3 pr-8 py-1 focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-transparent font-medium"
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
            {v.name} {v.status === "public" || v.is_public ? "(Public)" : ""}
          </option>
        ))}
      </select>
      <div className="pointer-events-none absolute inset-y-0 right-0 flex items-center px-2 text-slate-500">
        <ChevronDown size={14} />
      </div>
    </div>
  );
}
