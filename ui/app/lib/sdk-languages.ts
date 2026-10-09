/** One catalogue keeps SDK creation, described intent, and successor editing aligned. */
export const SDK_LANGUAGES = ["typescript", "python", "go"] as const;
export type SDKLanguage = typeof SDK_LANGUAGES[number];

/** Narrows only supported package emitters without guessing from an unknown language. */
export function isSDKLanguage(value: unknown): value is SDKLanguage {
  return typeof value === "string" && (SDK_LANGUAGES as readonly string[]).includes(value);
}

/** Existing configurations without a language retain the historical default. */
export function sdkLanguageOrDefault(value: unknown): SDKLanguage {
  // Valid persisted languages must survive successor creation unchanged.
  return isSDKLanguage(value) ? value : "typescript";
}
