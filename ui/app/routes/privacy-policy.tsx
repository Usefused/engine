import { useEffect } from "react";

const PRIVACY_POLICY_URL = "https://usefused.com/legal/privacy-policy";

// PrivacyPolicyRedirect preserves old Engine bookmarks while making the homepage policy canonical.
export default function PrivacyPolicyRedirect() {
  useEffect(() => {
    // Replacing history prevents the compatibility URL from trapping users in a back-button loop.
    window.location.replace(PRIVACY_POLICY_URL);
  }, []);

  return (
    <main className="flex min-h-screen items-center justify-center bg-slate-50 px-6 text-center">
      <p className="text-sm text-slate-600">Opening the Fused <a className="font-semibold text-violet-700 hover:underline" href={PRIVACY_POLICY_URL}>Privacy Policy</a>…</p>
    </main>
  );
}
