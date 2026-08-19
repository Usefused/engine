import type { ConnectBranding, ConnectBrandingInput } from "./api";

export type ConnectBrandingField = keyof ConnectBrandingInput;
export type ConnectBrandingErrors = Partial<Record<ConnectBrandingField, string>>;
// The default follows the Engine UI's canonical --brand-violet token.
export const DEFAULT_CONNECT_BRANDING_PRIMARY_COLOR = "#2563eb";

export interface ConnectBrandingConfirmationSummary {
  displayNameChanged: boolean;
  primaryColorChanged: boolean;
  logoChanged: boolean;
  logoPresent: boolean;
  supportURLChanged: boolean;
  supportURLPresent: boolean;
  privacyURLChanged: boolean;
  privacyURLPresent: boolean;
}

// emptyConnectBrandingInput keeps the editable form controlled while the Engine setting loads.
export function emptyConnectBrandingInput(): ConnectBrandingInput {
  return {
    display_name: "",
    logo_url: "",
    primary_color: DEFAULT_CONNECT_BRANDING_PRIMARY_COLOR,
    support_url: "",
    privacy_url: "",
  };
}

// connectBrandingInput strips response-only fields before an update reaches the Engine.
export function connectBrandingInput(
  branding: ConnectBranding,
): ConnectBrandingInput {
  return {
    display_name: branding.display_name,
    logo_url: branding.logo_url,
    primary_color: branding.primary_color,
    support_url: branding.support_url,
    privacy_url: branding.privacy_url,
  };
}

// normalizeConnectBrandingInput prevents accidental whitespace from becoming part of rendered links or labels.
export function normalizeConnectBrandingInput(
  branding: ConnectBrandingInput,
): ConnectBrandingInput {
  return {
    display_name: branding.display_name.trim(),
    logo_url: branding.logo_url.trim(),
    primary_color: branding.primary_color.trim(),
    support_url: branding.support_url.trim(),
    privacy_url: branding.privacy_url.trim(),
  };
}

// connectBrandingConfirmationSummary exposes change and presence facts without returning customer-entered URLs or names.
export function connectBrandingConfirmationSummary(
  current: ConnectBrandingInput,
  next: ConnectBrandingInput,
): ConnectBrandingConfirmationSummary {
  return {
    displayNameChanged: current.display_name !== next.display_name,
    primaryColorChanged: current.primary_color !== next.primary_color,
    logoChanged: current.logo_url !== next.logo_url,
    logoPresent: Boolean(next.logo_url),
    supportURLChanged: current.support_url !== next.support_url,
    supportURLPresent: Boolean(next.support_url),
    privacyURLChanged: current.privacy_url !== next.privacy_url,
    privacyURLPresent: Boolean(next.privacy_url),
  };
}

// validateConnectBrandingInput mirrors browser constraints so save failures are actionable before a request is made.
export function validateConnectBrandingInput(
  branding: ConnectBrandingInput,
): ConnectBrandingErrors {
  const errors: ConnectBrandingErrors = {};
  const displayName = branding.display_name.trim();
  // A visible name is required because every hosted page identifies the requesting app.
  if (!displayName) {
    errors.display_name = "Enter the app name shown on connection pages.";
  }
  // The browser mirrors the Engine's rune and control-character bounds to avoid a rejected save round trip.
  if (Array.from(displayName).length > 100 || containsControl(displayName)) {
    errors.display_name = "Use 1 to 100 visible characters without control characters.";
  }
  // Only a complete hexadecimal colour can be inserted into the preview's inline style.
  if (!/^#[0-9a-fA-F]{6}$/.test(branding.primary_color.trim())) {
    errors.primary_color = "Choose a six-digit hexadecimal colour.";
  }
  addOptionalHTTPSURLValidation(errors, "logo_url", branding.logo_url);
  addOptionalHTTPSURLValidation(errors, "support_url", branding.support_url);
  addOptionalHTTPSURLValidation(errors, "privacy_url", branding.privacy_url);
  return errors;
}

// safeLogoPreviewURL permits the browser to request only a logo URL accepted by the settings contract.
export function safeLogoPreviewURL(value: string): string | null {
  const errors: ConnectBrandingErrors = {};
  addOptionalHTTPSURLValidation(errors, "logo_url", value);
  // Invalid and empty values both use the same local preview fallback without starting a request.
  if (errors.logo_url || !value.trim()) return null;
  return value.trim();
}

// connectBrandingPreviewName keeps an empty in-progress name legible without mutating the saved value.
export function connectBrandingPreviewName(value: string): string {
  // Incomplete edits retain a meaningful preview until a valid name is entered.
  if (!value.trim()) return "Your app";
  return value.trim();
}

// safePrimaryColour limits inline preview styles to validated hexadecimal colours.
export function safePrimaryColour(value: string): string {
  // Incomplete colour edits must never create an invalid inline style.
  if (!/^#[0-9a-fA-F]{6}$/.test(value)) {
    return DEFAULT_CONNECT_BRANDING_PRIMARY_COLOR;
  }
  return value;
}

// addOptionalHTTPSURLValidation shares the same optional HTTPS rule across every externally rendered link.
function addOptionalHTTPSURLValidation(
  errors: ConnectBrandingErrors,
  field: "logo_url" | "support_url" | "privacy_url",
  value: string,
): void {
  const candidate = value.trim();
  // Optional blank links deliberately select the Engine's built-in fallback behavior.
  if (!candidate) return;
  const validationError = httpsURLValidationError(candidate);
  // A single validator keeps persisted links and the live logo preview on the same safety boundary.
  if (validationError) errors[field] = validationError;
}

// httpsURLValidationError mirrors the Engine's bounded, hierarchical HTTPS URL contract.
function httpsURLValidationError(candidate: string): string | null {
  // The Engine limits bytes and control characters before copying any origin into CSP.
  if (utf8ByteLength(candidate) > 2048 || containsControl(candidate)) {
    return "Use at most 2048 bytes without control characters.";
  }
  // Requiring a hierarchical HTTPS prefix rejects opaque URLs while matching Go's case-insensitive scheme parsing.
  if (!/^https:\/\//iu.test(candidate)) {
    return "Use an absolute HTTPS URL without embedded credentials.";
  }
  // Raw authorities must remain ASCII and unescaped because Go rejects encoded host bytes before host validation.
  if (!hasSafeRawAuthority(candidate)) {
    return "Use an unescaped ASCII hostname or its punycode form.";
  }
  try {
    const parsed = new URL(candidate);
    return parsedHTTPSURLValidationError(parsed);
  } catch {
    // URL parsing catches malformed authorities and invalid or out-of-range ports.
    return "Enter a valid absolute HTTPS URL.";
  }
}

// parsedHTTPSURLValidationError checks normalized URL components that cannot be inspected reliably as text.
function parsedHTTPSURLValidationError(parsed: URL): string | null {
  // Credentials and missing authorities are never safe CSP image/link sources.
  if (parsed.protocol !== "https:" || parsed.username || parsed.password || !parsed.hostname) {
    return "Use an absolute HTTPS URL without embedded credentials.";
  }
  // DNS labels, literal IP hosts, and explicit ports follow the Engine's bounded grammar.
  if (!validConnectBrandingHost(parsed.hostname) || !validConnectBrandingPort(parsed.port)) {
    return "Use a valid HTTPS hostname and port.";
  }
  return null;
}

// utf8ByteLength matches Go's byte-length limit for non-ASCII URLs.
function utf8ByteLength(value: string): number {
  return new TextEncoder().encode(value).length;
}

// containsControl rejects invisible delimiters before they reach HTML or CSP construction.
function containsControl(value: string): boolean {
  for (const character of value) {
    const code = character.codePointAt(0) ?? 0;
    // Unicode control characters occupy the C0 and C1 ranges used by Go's unicode.IsControl.
    if (code <= 0x1f || (code >= 0x7f && code <= 0x9f)) return true;
  }
  return false;
}

// hasSafeRawAuthority inspects only the host/userinfo segment so Unicode and escapes remain valid in URL paths.
function hasSafeRawAuthority(value: string): boolean {
  const authority = value.slice("https://".length).split(/[/?#]/u, 1)[0];
  // An empty raw authority must not be repaired into a hostname by browser URL normalization.
  if (!authority) return false;
  // Encoded bytes and backslashes are rejected before URL can silently decode or reinterpret authority boundaries.
  if (authority.includes("%") || authority.includes("\\")) return false;
  return Array.from(authority).every(isASCIICharacter);
}

// isASCIICharacter prevents an implicit browser punycode conversion from diverging from Engine validation.
function isASCIICharacter(character: string): boolean {
  return character.charCodeAt(0) <= 0x7f;
}

// validConnectBrandingHost accepts bracketed IP literals and bounded ASCII DNS names.
function validConnectBrandingHost(hostname: string): boolean {
  // URL retains brackets around IPv6 hosts, making their grammar distinct from DNS labels.
  if (hostname.startsWith("[") && hostname.endsWith("]")) {
    return validIPv6Literal(hostname.slice(1, -1));
  }
  // DNS length is bounded before labels are inspected individually.
  if (!hostname || hostname.length > 253) return false;
  return hostname.split(".").every(validDNSLabel);
}

// validConnectBrandingPort accepts omitted ports and the explicit TCP range supported by Engine.
function validConnectBrandingPort(port: string): boolean {
  // Omitted and normalized default ports are valid without further parsing.
  if (!port) return true;
  // Numeric bounds reject port zero while URL parsing already rejects values over 65535.
  if (!/^\d+$/u.test(port)) return false;
  const value = Number(port);
  return value > 0 && value <= 65535;
}

// validIPv6Literal relies on URL parsing for structure and rejects characters outside IP notation.
function validIPv6Literal(value: string): boolean {
  // A colon distinguishes IPv6 from an arbitrary bracketed token.
  if (!value.includes(":")) return false;
  return /^[0-9a-fA-F:.]+$/.test(value);
}

// validDNSLabel mirrors the Engine's ASCII label and boundary restrictions.
function validDNSLabel(label: string): boolean {
  // Empty, oversized, or hyphen-bounded labels cannot form a CSP host source.
  if (!label || label.length > 63 || label.startsWith("-") || label.endsWith("-")) {
    return false;
  }
  return /^[A-Za-z0-9-]+$/.test(label);
}
