import { type ReactNode } from "react";
import { Link } from "@remix-run/react";
import { Box, CheckCircle2, PlugZap, XCircle } from "lucide-react";
import {
  type ActivatedService,
  type BucketSDKSummary,
  type BucketServiceSummary,
} from "~/lib/api";
import { BucketPagination } from "~/components/buckets/BucketPagination";
import { BucketServiceSelect } from "~/components/buckets/BucketServiceSelect";

type BucketOverviewProps = {
  sdks: BucketSDKSummary[];
  sdkTotal: number;
  services: BucketServiceSummary[];
  workspaceServices: ActivatedService[];
  serviceTotal: number;
  sdkPage: number;
  servicePage: number;
  serviceSearch: string;
  pageSize: number;
  onSDKPageChange: (page: number) => void;
  onServicePageChange: (page: number) => void;
  onServiceSearchChange: (search: string) => void;
};

export function BucketOverview({
  sdks,
  sdkTotal,
  services,
  workspaceServices,
  serviceTotal,
  sdkPage,
  servicePage,
  serviceSearch,
  pageSize,
  onSDKPageChange,
  onServicePageChange,
  onServiceSearchChange,
}: BucketOverviewProps) {
  return (
    <section className="grid gap-4 border-b border-slate-100 px-6 py-4 lg:grid-cols-2">
      <OverviewList
        title="Linked Artefacts"
        total={sdkTotal}
        empty="No SDKs linked to this bucket."
        page={sdkPage}
        pageSize={pageSize}
        onPageChange={onSDKPageChange}
      >
        {sdks.map((sdk) => (
          <SDKRow key={sdk.id} sdk={sdk} />
        ))}
      </OverviewList>
      <OverviewList
        title="Linked Services"
        total={serviceTotal}
        empty={serviceEmptyText(serviceSearch)}
        page={servicePage}
        pageSize={pageSize}
        onPageChange={onServicePageChange}
        actions={
          <BucketServiceSelect
            id="bucket-linked-service-search"
            label="Service"
            placeholder="All linked services"
            options={bucketServiceOptions(services)}
            search={serviceSearch}
            className="w-64"
            hideLabel
            allowAll
            allLabel="All linked services"
            onSearchChange={onServiceSearchChange}
          />
        }
      >
        {services.map((service) => (
          <ServiceRow
            key={service.service_id}
            service={service}
            workspaceServices={workspaceServices}
          />
        ))}
      </OverviewList>
    </section>
  );
}

function OverviewList({
  title,
  total,
  empty,
  page,
  pageSize,
  onPageChange,
  actions,
  children,
}: {
  title: string;
  total: number;
  empty: string;
  page: number;
  pageSize: number;
  onPageChange: (page: number) => void;
  actions?: ReactNode;
  children: ReactNode;
}) {
  return (
    <div className="overflow-hidden rounded-lg border border-slate-200">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-slate-100 px-4 py-3">
        <h3 className="text-sm font-semibold text-slate-900">{title}</h3>
        <div className="flex items-center gap-3">
          {actions}
          <span className="text-xs text-slate-500">{total}</span>
        </div>
      </div>
      <div className="divide-y divide-slate-100">
        {total === 0 ? (
          <p className="px-4 py-5 text-sm text-slate-400">{empty}</p>
        ) : (
          children
        )}
      </div>
      <BucketPagination
        total={total}
        page={page}
        pageSize={pageSize}
        onPageChange={onPageChange}
      />
    </div>
  );
}

function SDKRow({ sdk }: { sdk: BucketSDKSummary }) {
  const label = sdk.name || sdk.id.slice(0, 8);
  const Icon = sdk.active ? CheckCircle2 : XCircle;
  return (
    <Link
      to={`/integrations/sdks/${encodeURIComponent(sdk.id)}`}
      className="flex items-center justify-between gap-3 px-4 py-3 transition-colors hover:bg-slate-50"
    >
      <div className="flex min-w-0 items-center gap-3">
        <Box className="h-4 w-4 shrink-0 text-slate-400" />
        <div className="min-w-0">
          <p className="truncate text-sm font-medium text-slate-900">{label}</p>
          <p className="truncate text-xs text-slate-500">
            {sdk.kind || "sdk"} · {sdk.id.slice(0, 8)}
          </p>
        </div>
      </div>
      <span
        className={`inline-flex items-center gap-1 text-xs ${
          sdk.active ? "text-emerald-600" : "text-slate-400"
        }`}
      >
        <Icon className="h-3.5 w-3.5" />
        {sdk.active ? "Active" : "Inactive"}
      </span>
    </Link>
  );
}

function ServiceRow({
  service,
  workspaceServices,
}: {
  service: BucketServiceSummary;
  workspaceServices: ActivatedService[];
}) {
  const label = service.service_name || service.service_id.slice(0, 8);
  return (
    <Link
      to={serviceDetailHref(service, workspaceServices)}
      className="grid gap-3 px-4 py-3 transition-colors hover:bg-slate-50 sm:grid-cols-[minmax(0,1fr)_auto] sm:items-center"
    >
      <div className="flex min-w-0 items-center gap-3">
        <PlugZap className="h-4 w-4 shrink-0 text-slate-400" />
        <div className="min-w-0">
          <p className="truncate text-sm font-medium text-slate-900">{label}</p>
          <p className="truncate text-xs text-slate-500">
            {service.service_id.slice(0, 8)}
          </p>
        </div>
      </div>
      <div className="grid grid-cols-2 gap-2 text-center text-xs sm:grid-cols-4">
        <ServiceMetric label="Secrets" value={service.secret_count} strong />
        <ServiceMetric label="Env" value={service.value_count} />
        <ServiceMetric label="OAuth" value={service.connect_config_count} />
        <ServiceMetric label="Users" value={service.connected_user_count} />
      </div>
    </Link>
  );
}

function ServiceMetric({
  label,
  value,
  strong,
}: {
  label: string;
  value: number;
  strong?: boolean;
}) {
  return (
    <span
      className={`rounded-md border px-2 py-1 ${
        strong
          ? "border-blue-100 bg-blue-50 text-blue-700"
          : "border-slate-100 bg-slate-50 text-slate-600"
      }`}
    >
      <span className="font-semibold">{value}</span> {label}
    </span>
  );
}

function serviceEmptyText(search: string): string {
  return search.trim()
    ? "No linked services match this search."
    : "No linked services have secrets in this bucket yet.";
}

function bucketServiceOptions(services: BucketServiceSummary[]) {
  return services.map((service) => ({
    id: service.service_id,
    label: serviceLabel(service),
  }));
}

function serviceLabel(service: BucketServiceSummary): string {
  return service.service_name
    ? `${service.service_name} · ${service.service_id.slice(0, 8)}`
    : service.service_id.slice(0, 8);
}

function serviceDetailHref(
  service: BucketServiceSummary,
  workspaceServices: ActivatedService[]
): string {
  const workspaceService = workspaceServices.find(
    (item) => item.service_id === service.service_id
  );
  const slug = workspaceService?.service_slug || service.service_id;
  if (slug.startsWith("@")) {
    const [provider, serviceSlug] = slug.slice(1).split("/");
    if (provider && serviceSlug)
      return `/integrations/${encodeURIComponent(
        provider
      )}/${encodeURIComponent(serviceSlug)}`;
  }
  return `/integrations/${encodeURIComponent(slug)}`;
}
