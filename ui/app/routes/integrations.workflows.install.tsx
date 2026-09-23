import { useEffect, useState, type FormEvent } from "react";
import { Link, useLoaderData } from "@remix-run/react";
import { api } from "~/lib/api";
import { listAppBuildSelectors, listAppOwningTeams } from "~/lib/app-builder";
import type {
  AppBuildSelector,
  AppOwningTeam,
} from "~/lib/app-builder-contract";
import {
  installWorkflowApp,
  listWorkflows,
  type WorkflowInstallResult,
} from "~/lib/workflow-api";
import {
  composeWorkflowApp,
  workflowSelectionURL,
  type WorkflowAppConfig,
} from "~/lib/workflow-library";
import {
  WorkflowPageHeader,
  WorkflowRequirements,
} from "~/components/workflows/WorkflowDetails";

interface ExistingApp {
  name: string;
  kind: string;
  delivery_mode: string;
  latest_version_id: string;
  latest_version: string;
}
interface AppSource {
  config: WorkflowAppConfig;
  owner_team: string;
}

/** Loads the complete selected set before the installation form can enable any services. */
export async function clientLoader({ request }: { request: Request }) {
  const ids = new URL(request.url).searchParams.getAll("workflow");
  // Selection identity is explicit and bounded even for manually constructed URLs.
  if (!ids.length || ids.length > 32)
    throw new Response("Select 1–32 workflows first.", { status: 400 });
  return (await listWorkflows("", ids)).items;
}

/** Suggests an additive stable successor without inventing prerelease version semantics. */
function successorVersion(version: string): string {
  const match = /^(\d+)\.(\d+)\.(\d+)$/.exec(version);
  // Unusual version labels require the user to choose the next immutable identity.
  if (!match) return "";
  return `${match[1]}.${Number(match[2]) + 1}.0`;
}

/** Composes selected workflows into one new app or an explicit immutable successor of an existing app. */
export default function InstallWorkflows() {
  const workflows = useLoaderData<typeof clientLoader>();
  const [target, setTarget] = useState("new");
  const [apps, setApps] = useState<ExistingApp[]>([]);
  const [appSearch, setAppSearch] = useState("");
  const [source, setSource] = useState<AppSource | null>(null);
  const [name, setName] = useState("");
  const [version, setVersion] = useState("1.0.0");
  const [kind, setKind] = useState<"sdk" | "mcp">("sdk");
  const [language, setLanguage] = useState("typescript");
  const [bucket, setBucket] = useState("");
  const [buckets, setBuckets] = useState<AppBuildSelector[]>([]);
  const [teams, setTeams] = useState<AppOwningTeam[]>([]);
  const [ownerTeamID, setOwnerTeamID] = useState("");
  const [busy, setBusy] = useState(false);
  const [loadingSource, setLoadingSource] = useState(false);
  const [status, setStatus] = useState("");
  const [error, setError] = useState("");
  const [result, setResult] = useState<WorkflowInstallResult | null>(null);

  // Ownership choices use the existing Engine-authorized app selectors.
  useEffect(() => {
    let active = true;
    listAppOwningTeams()
      .then((page) => {
        // Ignore late responses after this form has unmounted or changed owner.
        if (active) setTeams(page.items);
      })
      .catch((cause) => {
        // Ignore late responses after this form has unmounted or changed owner.
        if (active) setError(String(cause));
      });
    return () => {
      active = false;
    };
  }, []);

  // A changed owner requires a fresh eligible credential selection; stale bucket choices cannot carry across teams.
  useEffect(() => {
    let active = true;
    setBucket("");
    setBuckets([]);
    listAppBuildSelectors(ownerTeamID, "BUCKET")
      .then((page) => {
        // A stale owner response must never repopulate credential choices.
        if (!active) return;
        setBuckets(page.items);
        setBucket(
          page.items.find((item) => item.display_name === "default")
            ?.display_name ??
            page.items[0]?.display_name ??
            ""
        );
      })
      .catch((cause) => {
        // Ignore late responses after this form has unmounted or changed owner.
        if (active) setError(String(cause));
      });
    return () => {
      active = false;
    };
  }, [ownerTeamID]);

  // Existing-app discovery runs only on explicit demand and remains bounded server-side.
  async function searchApps() {
    setError("");
    try {
      const data = await api.mcpGraphql<{
        appFamilies: { items: ExistingApp[] };
      }>(
        `query WorkflowAppChoices($search: String!) {
        appFamilies(search: $search, limit: 20, offset: 0) { items { name kind delivery_mode latest_version_id latest_version } }
      }`,
        { search: appSearch }
      );
      setApps(data.appFamilies.items);
    } catch (cause) {
      setError(String(cause));
    }
  }

  // Source is fetched from Engine with edit authority, keeping private mappings out of Registry.
  async function chooseApp(id: string) {
    setTarget(id);
    setSource(null);
    setError("");
    // A new app starts its own identity and does not inherit the previous target's configuration.
    if (id === "new") {
      setName("");
      setVersion("1.0.0");
      return;
    }
    setLoadingSource(true);
    try {
      const response = await api.appConfig.source<AppSource>(id);
      setSource(response);
      setName(response.config.name);
      setVersion(successorVersion(response.config.version));
      setKind(response.config.kind);
      setLanguage(response.config.language ?? "typescript");
    } catch (cause) {
      setError(String(cause));
    } finally {
      setLoadingSource(false);
    }
  }

  // The preview is recomputed from explicit form state before every install, so stale previews cannot authorize changed inputs.
  function candidate(): WorkflowAppConfig {
    if (target !== "new" && !source)
      throw new Error("Load an existing app before extending it.");
    const base = source
      ? { ...source.config, version }
      : {
          apiVersion: "fused/v1",
          kind,
          name: name.trim(),
          version,
          bucket,
          services: {},
          // Only generated SDKs have a language; MCP requires a useful server description.
          ...(kind === "sdk"
            ? { language }
            : {
                description: workflows
                  .map((workflow) => workflow.template.name)
                  .join("; "),
              }),
        };
    return composeWorkflowApp(base, workflows);
  }

  // One user action authorizes the visible additive service changes and ordinary app plan/apply.
  async function install(event: FormEvent) {
    event.preventDefault();
    setError("");
    setResult(null);
    try {
      const config = candidate();
      // A successor must not overwrite an immutable version, even if the operation set happens to match.
      if (source && version === source.config.version)
        throw new Error("Choose a new app version for this extension.");
      setBusy(true);
      const owner =
        source?.owner_team ??
        teams.find((team) => team.id === ownerTeamID)?.slug ??
        "";
      setResult(await installWorkflowApp(config, workflows, owner, setStatus));
      setStatus(
        "App installed. Complete any provider connections before running workflows."
      );
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setBusy(false);
    }
  }

  let preview = "";
  let conflict = "";
  try {
    preview = JSON.stringify(candidate(), null, 2);
  } catch (cause) {
    // Composition conflicts block the action before any workspace change.
    conflict = cause instanceof Error ? cause.message : String(cause);
  }

  return (
    <main className="mx-auto max-w-5xl p-4 sm:p-8 space-y-6">
      <Link
        className="text-sm text-violet-700"
        to={workflowSelectionURL(
          "/integrations/workflows",
          workflows.map((workflow) => workflow.id)
        )}
      >
        ← Selected workflows
      </Link>
      <WorkflowPageHeader
        title="Add workflows to an app"
        description="Combine the selected workflows in one SDK or MCP. Required service versions will be enabled automatically."
      />
      <WorkflowRequirements workflows={workflows} />
      <form
        onSubmit={install}
        className="rounded-xl border bg-white p-5 space-y-5"
      >
        <fieldset
          disabled={[busy, !!result, loadingSource].some(Boolean)}
          className="space-y-5"
        >
          <legend className="text-lg font-semibold mb-3">App setup</legend>
          <div className="flex flex-wrap gap-2">
            <input
              aria-label="Search existing apps"
              placeholder="Find an existing app"
              value={appSearch}
              onChange={(event) => setAppSearch(event.target.value)}
              className="min-w-0 rounded-lg border px-3 py-2"
            />
            <button
              type="button"
              onClick={searchApps}
              className="rounded-lg border px-4 py-2"
            >
              Search apps
            </button>
          </div>
          <label className="block text-sm font-medium">
            Destination
            <select
              value={target}
              onChange={(event) => void chooseApp(event.target.value)}
              className="mt-1 block w-full rounded-lg border p-2"
            >
              <option value="new">Create a new app</option>
              {apps.map((app) => (
                <option
                  key={app.latest_version_id}
                  value={app.latest_version_id}
                >
                  {app.name} · {app.kind} · {app.latest_version}
                </option>
              ))}
            </select>
          </label>
          <div className="grid gap-4 sm:grid-cols-2">
            <label className="block text-sm font-medium">
              App name
              <input
                required
                value={name}
                disabled={!!source}
                onChange={(event) => setName(event.target.value)}
                className="mt-1 block w-full rounded-lg border p-2"
              />
            </label>
            <label className="block text-sm font-medium">
              {source ? "New app version" : "App version"}
              <input
                required
                pattern="v?[0-9]+\.[0-9]+\.[0-9]+([+-].*)?"
                value={version}
                onChange={(event) => setVersion(event.target.value)}
                className="mt-1 block w-full rounded-lg border p-2"
              />
            </label>
          </div>
          {/* Existing app identity and credentials remain authoritative when adding workflows. */}
          {!source && (
            <NewAppFields
              kind={kind}
              setKind={setKind}
              language={language}
              setLanguage={setLanguage}
              ownerTeamID={ownerTeamID}
              setOwnerTeamID={setOwnerTeamID}
              teams={teams}
              bucket={bucket}
              setBucket={setBucket}
              buckets={buckets}
            />
          )}
          <details>
            <summary className="cursor-pointer text-sm font-medium text-violet-700">
              Review combined configuration
            </summary>
            <pre className="mt-3 max-h-96 overflow-auto rounded-lg bg-slate-50 p-4 text-xs">
              {preview}
            </pre>
          </details>
          {/* A conflicting selection never reaches service activation. */}
          {conflict && (
            <p role="alert" className="text-sm text-red-700">
              {conflict}
            </p>
          )}
          <button
            disabled={!!conflict}
            className="rounded-lg bg-violet-600 px-5 py-2.5 font-medium text-white disabled:opacity-50"
          >
            {source ? "Create app version" : "Create app"} with{" "}
            {workflows.length} workflow{workflows.length === 1 ? "" : "s"}
          </button>
        </fieldset>
        <WorkflowInstallMessages
          loadingSource={loadingSource}
          status={status}
          error={error}
        />
      </form>
      {/* One-time credentials stay in component memory and are never placed in URLs or persistent browser storage. */}
      {result && <WorkflowResult result={result} kind={kind} />}
    </main>
  );
}

/** Keeps one-time execution credentials local while linking to the existing app delivery screens. */
function WorkflowResult({
  result,
  kind,
}: {
  result: WorkflowInstallResult;
  kind: "sdk" | "mcp";
}) {
  return (
    <section className="rounded-xl border border-green-200 bg-green-50 p-5 space-y-3">
      <h2 className="text-lg font-semibold">Workflows installed</h2>
      <Link
        className="font-medium text-violet-700"
        to={
          kind === "mcp"
            ? `/integrations/mcp/${result.app_id}`
            : `/integrations/sdks/${result.app_id}`
        }
      >
        Open app for{" "}
        {kind === "mcp"
          ? "MCP connection instructions"
          : "SDK generation and download"}{" "}
        →
      </Link>
      {result.execution_token && (
        <label className="block text-sm">
          Save this execution token; it is only shown once.
          <input
            readOnly
            type="password"
            value={result.execution_token}
            onFocus={(event) => event.target.select()}
            className="mt-2 block w-full rounded border p-2 font-mono"
          />
          <button
            type="button"
            className="mt-2 underline"
            onClick={() =>
              void navigator.clipboard.writeText(result.execution_token!)
            }
          >
            Copy token
          </button>
        </label>
      )}
      <p>
        <Link to="/integrations/buckets" className="text-sm underline">
          Manage credentials and provider connections
        </Link>
      </p>
    </section>
  );
}

interface NewAppFieldsProps {
  kind: "sdk" | "mcp";
  setKind: (kind: "sdk" | "mcp") => void;
  language: string;
  setLanguage: (language: string) => void;
  ownerTeamID: string;
  setOwnerTeamID: (id: string) => void;
  teams: AppOwningTeam[];
  bucket: string;
  setBucket: (bucket: string) => void;
  buckets: AppBuildSelector[];
}

/** Presents only the choices that establish a new app's immutable delivery and ownership. */
function NewAppFields({
  kind,
  setKind,
  language,
  setLanguage,
  ownerTeamID,
  setOwnerTeamID,
  teams,
  bucket,
  setBucket,
  buckets,
}: NewAppFieldsProps) {
  return (
    <div className="grid gap-4 sm:grid-cols-2">
      <label className="text-sm font-medium">
        App type
        <select
          value={kind}
          onChange={(event) => setKind(event.target.value as "sdk" | "mcp")}
          className="mt-1 block w-full rounded-lg border p-2"
        >
          <option value="sdk">SDK</option>
          <option value="mcp">MCP</option>
        </select>
      </label>
      {kind === "sdk" && (
        <label className="text-sm font-medium">
          Language
          <select
            value={language}
            onChange={(event) => setLanguage(event.target.value)}
            className="mt-1 block w-full rounded-lg border p-2"
          >
            <option value="typescript">TypeScript</option>
            <option value="python">Python</option>
          </select>
        </label>
      )}
      <label className="text-sm font-medium">
        Owner
        <select
          value={ownerTeamID}
          onChange={(event) => setOwnerTeamID(event.target.value)}
          className="mt-1 block w-full rounded-lg border p-2"
        >
          <option value="">Me</option>
          {teams.map((team) => (
            <option value={team.id} key={team.id}>
              {team.name}
            </option>
          ))}
        </select>
      </label>
      <label className="text-sm font-medium">
        Credentials
        <select
          required
          value={bucket}
          onChange={(event) => setBucket(event.target.value)}
          className="mt-1 block w-full rounded-lg border p-2"
        >
          <option value="">Select credentials</option>
          {buckets.map((item) => (
            <option key={item.resource_id} value={item.display_name}>
              {item.display_name}
            </option>
          ))}
        </select>
      </label>
    </div>
  );
}

/** Announces ongoing work and actionable failures independently of the locked form controls. */
function WorkflowInstallMessages({
  loadingSource,
  status,
  error,
}: {
  loadingSource: boolean;
  status: string;
  error: string;
}) {
  return (
    <div className="space-y-2">
      {/* Source reads and installation stages have separate statuses so a failed load cannot look installed. */}
      {loadingSource && <p role="status">Loading app configuration…</p>}
      {status && (
        <p role="status" className="text-sm text-slate-600">
          {status}
        </p>
      )}
      {error && (
        <p role="alert" className="text-sm text-red-700">
          {error}
        </p>
      )}
    </div>
  );
}
