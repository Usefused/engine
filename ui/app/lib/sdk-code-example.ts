import type { IntegrationObject } from "~/lib/api";

interface CodeExampleSchema {
  type?: string;
  format?: string;
  properties?: Record<string, CodeExampleSchema>;
  required?: string[];
  items?: CodeExampleSchema;
}

// generatedCamelCase mirrors the SDK generator's public member normalization,
// including its valid-identifier fallback for numeric provider names.
export function generatedCamelCase(value: string, prefix: string): string {
  let normalized = value.replace(/[^a-zA-Z0-9]+(.)?/g, (_, character) => character ? character.toUpperCase() : "");
  if (!normalized) return prefix;
  if (/^[0-9]/.test(normalized)) normalized = `${prefix}${normalized}`;
  return normalized.charAt(0).toLowerCase() + normalized.slice(1);
}

// generatedPascalCase produces the generated service property from the same
// identifier normalization used for resource and operation members.
export function generatedPascalCase(value: string, prefix: string): string {
  const camel = generatedCamelCase(value, prefix);
  return camel.charAt(0).toUpperCase() + camel.slice(1);
}

// schemaExampleValue returns a harmless typed placeholder without reading
// examples that may contain provider or customer data.
function schemaExampleValue(schema?: CodeExampleSchema): string {
  if (schema?.items) return "[]";
  if (schema?.properties) return "{}";
  const examples: Record<string, string> = {
    integer: "123",
    number: "123",
    boolean: "true",
    array: "[]",
    object: "{}",
  };
  return examples[schema?.type ?? ""] ?? '"string"';
}

// parameterExampleValue maps the projected primitive parameter type to the
// same safe placeholders used for request-schema fields.
function parameterExampleValue(type: string): string {
  return schemaExampleValue({ type });
}

// generatedMethodPath renders the ordinary single-service path; generated
// packages may add a service disambiguator only when selected names collide.
export function generatedMethodPath(serviceName: string, endpoint: IntegrationObject): string {
  const segments = [
    "sdk",
    generatedPascalCase(serviceName, "resource"),
    endpoint.resource && generatedCamelCase(endpoint.resource, "resource"),
    generatedCamelCase(endpoint.name, "operation"),
  ].filter(Boolean);
  return segments.join(".");
}

interface ExampleOption {
  name: string;
  value: string;
  optional: boolean;
}

// generatedExampleOptions combines declared parameters and required object-body
// fields while bounding the documentation snippet to eight visible entries.
function generatedExampleOptions(endpoint: IntegrationObject, requestSchema?: CodeExampleSchema): { options: ExampleOption[]; omitted: number } {
  const parameterOptions = (endpoint.parameters ?? []).map((parameter) => ({
    name: parameter.name,
    value: parameterExampleValue(parameter.type),
    optional: !parameter.required,
  }));
  const existingNames = new Set(parameterOptions.map((option) => option.name));
  const requiredFields = new Set(requestSchema?.required ?? []);
  const bodyOptions = Object.entries(requestSchema?.properties ?? {})
    .filter(([name]) => requiredFields.has(name) && !existingNames.has(name))
    .map(([name, schema]) => ({ name, value: schemaExampleValue(schema), optional: false }));
  const combined = [...parameterOptions, ...bodyOptions];
  return { options: combined.slice(0, 8), omitted: Math.max(0, combined.length - 8) };
}

// renderExampleOption preserves the exact option key and marks optional inputs
// without changing the generated method signature.
function renderExampleOption(option: ExampleOption): string {
  const suffix = option.optional ? " // optional" : "";
  return `  ${JSON.stringify(option.name)}: ${option.value},${suffix}`;
}

// omittedOptionsLine bounds large provider contracts while stating precisely
// how much generated surface is not shown in the compact example.
function omittedOptionsLine(omitted: number): string | null {
  if (omitted === 0) return null;
  const noun = omitted === 1 ? "option" : "options";
  return `  // ${omitted} more generated ${noun}`;
}

// usesRootPayload identifies the scalar and array request shapes that generated
// clients expose as a separate first argument.
function usesRootPayload(requestSchema?: CodeExampleSchema): boolean {
  if (!requestSchema) return false;
  if (requestSchema.properties) return false;
  return Boolean(requestSchema.type && requestSchema.type !== "object");
}

// hasUnprojectedBody reports a declared request representation whose option
// fields cannot be safely inferred from the bounded UI schema projection.
function hasUnprojectedBody(endpoint: IntegrationObject, requestSchema?: CodeExampleSchema): boolean {
  const declared = Boolean(endpoint.request_content?.representations?.length);
  return declared && !requestSchema?.properties;
}

// renderOptionsObject formats a valid object argument for zero or more exact
// generated option keys.
function renderOptionsObject(lines: string[]): string {
  if (lines.length === 0) return "{}";
  return `{\n${lines.join("\n")}\n}`;
}

// renderRootPayloadCall keeps the positional payload separate from any
// parameter options, matching the generated scalar and array API.
function renderRootPayloadCall(methodPath: string, requestSchema: CodeExampleSchema, optionLines: string[]): string {
  const optionsArgument = optionLines.length > 0 ? `, ${renderOptionsObject(optionLines)}` : "";
  return `const result = await ${methodPath}(${schemaExampleValue(requestSchema)}${optionsArgument});`;
}

// typescriptSDKCallExample shows the exact generated member path and declared
// option keys without embedding credentials, selectors, or source examples.
export function typescriptSDKCallExample(serviceName: string, endpoint: IntegrationObject, requestSchema?: CodeExampleSchema): string {
  const methodPath = generatedMethodPath(serviceName, endpoint);
  const { options, omitted } = generatedExampleOptions(endpoint, requestSchema);
  const optionLines = options.map(renderExampleOption);
  const omittedLine = omittedOptionsLine(omitted);
  if (omittedLine) optionLines.push(omittedLine);

  // Scalar and array roots are generated as a payload argument rather than
  // being merged into the options object used by ordinary object bodies.
  if (usesRootPayload(requestSchema) && requestSchema) {
    return renderRootPayloadCall(methodPath, requestSchema, optionLines);
  }
  if (hasUnprojectedBody(endpoint, requestSchema)) {
    optionLines.push("  // Add request fields from the generated options type.");
  }
  return `const result = await ${methodPath}(${renderOptionsObject(optionLines)});`;
}
