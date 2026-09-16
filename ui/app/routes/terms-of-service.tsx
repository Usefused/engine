import { useEffect } from "react";

const TERMS_OF_SERVICE_URL = "https://usefused.com/legal/terms-of-service";

// TermsOfServiceRedirect preserves old Engine bookmarks while making the homepage terms canonical.
export default function TermsOfServiceRedirect() {
  useEffect(() => {
    // Replacing history prevents the compatibility URL from trapping users in a back-button loop.
    window.location.replace(TERMS_OF_SERVICE_URL);
  }, []);

  return (
    <main className="flex min-h-screen items-center justify-center bg-slate-50 px-6 text-center">
      <p className="text-sm text-slate-600">Opening the Fused <a className="font-semibold text-violet-700 hover:underline" href={TERMS_OF_SERVICE_URL}>Terms of Service</a>…</p>
    </main>
  );
}
