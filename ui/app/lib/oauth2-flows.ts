import type { AuthConfig, OAuth2FlowContract, OAuth2FlowName } from "./api";

export const oauth2FlowNames: OAuth2FlowName[] = [
  "authorizationCode",
  "clientCredentials",
  "password",
  "implicit",
  "deviceAuthorization",
];

export function oauth2FlowEntries(auth: AuthConfig): [OAuth2FlowName, OAuth2FlowContract][] {
  return oauth2FlowNames.flatMap((name) => {
    const flow = auth.oauth2_flows?.[name];
    return flow ? [[name, flow]] : [];
  });
}

export function firstAvailableOAuth2Flow(auth: AuthConfig): OAuth2FlowName | null {
  return oauth2FlowNames.find((name) => !auth.oauth2_flows?.[name]) ?? null;
}

export function emptyOAuth2Flow(): OAuth2FlowContract {
  return { scopes: {} };
}

export function updateOAuth2Flow(
  auth: AuthConfig,
  name: OAuth2FlowName,
  flow: OAuth2FlowContract,
): AuthConfig {
  return { ...auth, oauth2_flows: { ...(auth.oauth2_flows ?? {}), [name]: flow } };
}

export function renameOAuth2Flow(
  auth: AuthConfig,
  current: OAuth2FlowName,
  next: OAuth2FlowName,
): AuthConfig {
  if (current === next) return auth;
  const flows = { ...(auth.oauth2_flows ?? {}) };
  const value = flows[current] ?? emptyOAuth2Flow();
  delete flows[current];
  flows[next] = value;
  return { ...auth, oauth2_flows: flows };
}

export function removeOAuth2Flow(auth: AuthConfig, name: OAuth2FlowName): AuthConfig {
  const flows = { ...(auth.oauth2_flows ?? {}) };
  delete flows[name];
  return { ...auth, oauth2_flows: flows };
}

export function oauth2ScopeNames(flow: OAuth2FlowContract): string[] {
  return Object.keys(flow.scopes ?? {}).sort();
}

export function replaceOAuth2Scopes(flow: OAuth2FlowContract, names: string[]): OAuth2FlowContract {
  const scopes: Record<string, string> = {};
  for (const raw of names) {
    const name = raw.trim();
    if (name && scopes[name] === undefined) scopes[name] = flow.scopes?.[name] ?? "";
  }
  return { ...flow, scopes };
}

export function oauth2FlowNeedsAuthorizationURL(name: OAuth2FlowName): boolean {
  return name === "authorizationCode" || name === "implicit";
}

export function oauth2FlowNeedsTokenURL(name: OAuth2FlowName): boolean {
  return name !== "implicit";
}
