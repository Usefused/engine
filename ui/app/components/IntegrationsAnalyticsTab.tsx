interface IntegrationsAnalyticsTabProps {
  analytics: any;
}

export default function IntegrationsAnalyticsTab({ analytics }: IntegrationsAnalyticsTabProps) {
  if (!analytics) return null;

  return (
    <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-6 mb-8">
      <div className="bg-white p-6 rounded-xl border border-slate-200 shadow-sm flex items-center gap-4">
        <div className="w-12 h-12 bg-blue-100 text-blue-600 rounded-full flex items-center justify-center font-bold text-xl">
          S
        </div>
        <div>
          <p className="text-sm font-medium text-slate-500">Total Services</p>
          <p className="text-2xl font-bold text-slate-900">{analytics.total_services}</p>
        </div>
      </div>

      <div className="bg-white p-6 rounded-xl border border-slate-200 shadow-sm flex items-center gap-4">
        <div className="w-12 h-12 bg-indigo-100 text-indigo-600 rounded-full flex items-center justify-center font-bold text-xl">
          E
        </div>
        <div>
          <p className="text-sm font-medium text-slate-500">Total Endpoints</p>
          <p className="text-2xl font-bold text-slate-900">{analytics.total_endpoints}</p>
        </div>
      </div>
      <div className="bg-white p-6 rounded-xl border border-slate-200 shadow-sm flex items-center gap-4">
        <div className="w-12 h-12 bg-amber-100 text-amber-600 rounded-full flex items-center justify-center font-bold text-xl">
          D
        </div>
        <div>
          <p className="text-sm font-medium text-slate-500">Pending Drift Events</p>
          <p className="text-2xl font-bold text-slate-900">{analytics.total_drift_events}</p>
        </div>
      </div>
      <div className="bg-white p-6 rounded-xl border border-slate-200 shadow-sm flex items-center gap-4">
        <div className="w-12 h-12 bg-purple-100 text-purple-600 rounded-full flex items-center justify-center font-bold text-xl">
          U
        </div>
        <div>
          <p className="text-sm font-medium text-slate-500">Consumer Upgrades</p>
          <p className="text-2xl font-bold text-slate-900">{analytics.total_sdk_upgrades || 0}</p>
        </div>
      </div>
      <div className="bg-white p-6 rounded-xl border border-slate-200 shadow-sm flex items-center gap-4">
        <div className="w-12 h-12 bg-rose-100 text-rose-600 rounded-full flex items-center justify-center font-bold text-xl">
          L
        </div>
        <div>
          <p className="text-sm font-medium text-slate-500">Legacy SDKs (Outdated)</p>
          <p className="text-2xl font-bold text-slate-900">{analytics.total_outdated_sdks || 0}</p>
        </div>
      </div>
    </div>
  );
}
