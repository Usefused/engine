// Package mcpskill renders the Agent Skill that a Fused MCP server serves as an MCP resource.
//
// Facts (operation IDs, services, versions, the calling convention) always come from the
// immutable MCP version. An optional AI-written Prose block may replace only the purpose
// paragraph and the "when to use" bullets; it can never add operations, URLs or code.
package mcpskill

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

const (
	// ResourceURI is the fixed MCP resource identity for the server's own skill.
	ResourceURI = "fused://skills/server"
	// SchemaVersion identifies the Snapshot shape persisted in a plan's resolved payload.
	SchemaVersion = 1
	// nameLimit is the Agent Skills name ceiling shared by every supported agent host.
	nameLimit = 64
	// descriptionLimit keeps frontmatter within the Agent Skills description ceiling.
	descriptionLimit = 1024
	// OperationLimit bounds the rendered inventory; search_docs remains authoritative beyond it.
	OperationLimit = 500
	// nameSuffix distinguishes MCP skills from generated SDK skills.
	nameSuffix = "-mcp"
	// purposeLimit bounds the AI-written purpose paragraph.
	purposeLimit = 600
	// whenLimit bounds each AI-written "when to use" bullet.
	whenLimit = 240
	// whenCountLimit bounds the number of AI-written "when to use" bullets.
	whenCountLimit = 6
)

// Group is the exact callable scope granted through one selected service.
type Group struct {
	Label      string   `json:"label"`
	Version    string   `json:"version,omitempty"`
	Operations []string `json:"operations,omitempty"`
	Imported   []string `json:"imported,omitempty"`
}

// Prose is the only AI-editable part of a skill: plain sentences, no structure.
type Prose struct {
	Purpose   string   `json:"purpose,omitempty"`
	WhenToUse []string `json:"when_to_use,omitempty"`
}

// Snapshot is computed once at plan time and pinned to the MCP version through the applied plan.
type Snapshot struct {
	SchemaVersion int      `json:"schema_version"`
	Services      []Group  `json:"services,omitempty"`
	Other         []string `json:"other,omitempty"`
	Prose         *Prose   `json:"prose,omitempty"`
}

// Input is everything a rendered skill may state about one MCP version.
type Input struct {
	Name        string
	Version     string
	Description string
	// Snapshot is nil for versions planned before skills existed; the skill then omits the inventory.
	Snapshot *Snapshot
	// Allow filters operations by the caller's token; nil means every operation is visible.
	Allow func(operationID string) bool
}

// Operation is one catalogue row used to build a Snapshot.
type Operation struct {
	OperationID string
	// ServiceKey groups rows by service; empty means no public service provenance (for example Unified).
	ServiceKey string
	Imported   bool
}

// Service labels one ServiceKey with its public name and pinned contract version.
type Service struct {
	Key     string
	Label   string
	Version string
}

// BuildSnapshot groups exact catalogue rows by service in a deterministic order.
func BuildSnapshot(operations []Operation, services []Service) *Snapshot {
	labels := make(map[string]Service, len(services))
	for _, service := range services {
		labels[service.Key] = service
	}
	groups := map[string]*Group{}
	snapshot := &Snapshot{SchemaVersion: SchemaVersion}
	for _, operation := range operations {
		id := Text(operation.OperationID)
		// Blank identities cannot be invoked and are never advertised.
		if id == "" {
			continue
		}
		// Rows without service provenance are still callable, so they are listed on their own.
		if operation.ServiceKey == "" {
			snapshot.Other = append(snapshot.Other, id)
			continue
		}
		group, exists := groups[operation.ServiceKey]
		// The first row for a service establishes its label, falling back to the stable key.
		if !exists {
			service := labels[operation.ServiceKey]
			label := Text(service.Label)
			// A missing label must not drop operations; the key still identifies the service.
			if label == "" {
				label = operation.ServiceKey
			}
			group = &Group{Label: label, Version: Text(service.Version)}
			groups[operation.ServiceKey] = group
		}
		// Imported native capabilities are invoked the same way but labelled by origin.
		if operation.Imported {
			group.Imported = append(group.Imported, id)
		} else {
			group.Operations = append(group.Operations, id)
		}
	}
	for _, group := range groups {
		sort.Strings(group.Operations)
		sort.Strings(group.Imported)
		snapshot.Services = append(snapshot.Services, *group)
	}
	sort.Slice(snapshot.Services, func(left, right int) bool { return snapshot.Services[left].Label < snapshot.Services[right].Label })
	sort.Strings(snapshot.Other)
	return snapshot
}

// Render owns every heading and instruction; authored and AI text is interpolated only as plain text.
func Render(input Input) (string, string) {
	name := Name(input.Name)
	snapshot := visibleSnapshot(input.Snapshot, input.Allow)
	prose := admittedProse(snapshot)
	purpose := Text(input.Description)
	// Validated AI prose replaces only the purpose paragraph; the authored description is the fallback.
	if prose != nil && prose.Purpose != "" {
		purpose = prose.Purpose
	}
	var b strings.Builder
	fmt.Fprintf(&b, "---\nname: %s\ndescription: %s\n---\n\n", name, yamlString(description(input, purpose, snapshot)))
	fmt.Fprintf(&b, "# %s MCP server\n\n", Text(input.Name))
	// Every runnable MCP version carries authored metadata, but older rows must still render.
	if purpose != "" {
		b.WriteString(purpose + "\n\n")
	}
	fmt.Fprintf(&b, "Version `%s`. This skill is served by the server itself as `%s`.\n\n", code(input.Version), ResourceURI)
	renderUsage(&b, snapshot, prose)
	renderCalling(&b)
	renderOperations(&b, snapshot)
	return name, b.String()
}

// description tells an agent host when to load the skill.
func description(input Input, purpose string, snapshot *Snapshot) string {
	text := ""
	// The purpose is the strongest trigger signal; omit it rather than inventing one.
	if purpose != "" {
		text = strings.TrimRight(purpose, ".") + ". "
	}
	text += fmt.Sprintf("Use the %s Fused MCP server (version %s) and its search_docs and execute tools", Text(input.Name), Text(input.Version))
	labels := serviceLabels(snapshot)
	// Naming services lets hosts match requests that mention a provider directly.
	if len(labels) > 0 {
		text += " for " + strings.Join(labels, ", ")
	}
	return truncate(text+".", descriptionLimit)
}

// renderUsage states when the agent should prefer this server's tools.
func renderUsage(b *strings.Builder, snapshot *Snapshot, prose *Prose) {
	b.WriteString("## When to use\n\n")
	// AI bullets, when admitted, replace only the generic opening guidance.
	if prose != nil && len(prose.WhenToUse) > 0 {
		for _, item := range prose.WhenToUse {
			fmt.Fprintf(b, "- %s\n", item)
		}
	} else {
		b.WriteString("- The request matches the purpose above, or needs one of the operations listed below.\n")
		labels := serviceLabels(snapshot)
		// Only name services when the version has service-backed operations.
		if len(labels) > 0 {
			fmt.Fprintf(b, "- Prefer these tools over ad hoc HTTP calls for %s: Fused already holds the credentials and policies.\n", strings.Join(labels, ", "))
		}
	}
	b.WriteString("- Do not use it for operations that are not listed; this server's scope is fixed for this version.\n\n")
}

// renderCalling restates the two-tool convention so the agent does not guess operation IDs.
func renderCalling(b *strings.Builder) {
	b.WriteString("## How to call it\n\n")
	b.WriteString("1. Read the server instructions and the `fused://guides/workflows` guide if you have not already.\n")
	b.WriteString("2. Call `search_docs` with one concise intent per query (for example `create issue`), or with an exact `operationId` from the list below. Reuse results marked `execution_ready`.\n")
	b.WriteString("3. Call `execute` with `operationId` and flat `params` for a single call, or one `script` that awaits `call()` for dependent multi-step work.\n")
	b.WriteString("4. Never invent operation IDs, credentials, or auth selectors. Fused supplies authentication. After an unknown outcome, reconcile with reads and never replay an uncertain write.\n\n")
}

// renderOperations lists the visible callable scope, grouped by the service that grants it.
func renderOperations(b *strings.Builder, snapshot *Snapshot) {
	b.WriteString("## Operations\n\n")
	// Versions planned before skills existed have no pinned inventory.
	if snapshot == nil {
		b.WriteString("Browse the operations available to you with `search_docs` (omit the query to list them).\n")
		return
	}
	total := len(snapshot.Other)
	for _, group := range snapshot.Services {
		total += len(group.Operations) + len(group.Imported)
	}
	// A token or version with no visible operations still gets a valid, honest skill.
	if total == 0 {
		b.WriteString("No operations are available to this token.\n")
		return
	}
	remaining := OperationLimit
	for _, group := range snapshot.Services {
		heading := group.Label
		// Version pins disambiguate services enabled at more than one contract version.
		if group.Version != "" {
			heading += " (" + group.Version + ")"
		}
		fmt.Fprintf(b, "### %s\n\n", heading)
		remaining = writeList(b, group.Operations, "", remaining)
		remaining = writeList(b, group.Imported, " (imported MCP capability)", remaining)
		b.WriteString("\n")
	}
	// Operations without service provenance (such as Unified operations) belong to the scope too.
	if len(snapshot.Other) > 0 {
		b.WriteString("### Other operations\n\n")
		remaining = writeList(b, snapshot.Other, "", remaining)
		b.WriteString("\n")
	}
	// Large catalogues stay complete through search_docs rather than an unbounded document.
	if total > OperationLimit {
		fmt.Fprintf(b, "%d more operations are not listed here. Browse them with `search_docs`.\n", total-OperationLimit)
	}
}

// writeList writes up to remaining operation IDs and returns the unused allowance.
func writeList(b *strings.Builder, operations []string, suffix string, remaining int) int {
	for _, operation := range operations {
		// The shared allowance caps the whole document, not each service independently.
		if remaining <= 0 {
			return 0
		}
		fmt.Fprintf(b, "- `%s`%s\n", code(operation), suffix)
		remaining--
	}
	return remaining
}

// visibleSnapshot removes operations the caller's token cannot use, dropping emptied groups.
func visibleSnapshot(snapshot *Snapshot, allow func(string) bool) *Snapshot {
	// No snapshot or no filter means the stored scope is shown unchanged.
	if snapshot == nil || allow == nil {
		return snapshot
	}
	filtered := &Snapshot{SchemaVersion: snapshot.SchemaVersion, Prose: snapshot.Prose, Other: filter(snapshot.Other, allow)}
	for _, group := range snapshot.Services {
		group.Operations, group.Imported = filter(group.Operations, allow), filter(group.Imported, allow)
		// A service whose every operation is denied is not mentioned to this token at all.
		if len(group.Operations)+len(group.Imported) > 0 {
			filtered.Services = append(filtered.Services, group)
		}
	}
	return filtered
}

// filter keeps only allowed operation IDs.
func filter(operations []string, allow func(string) bool) []string {
	var kept []string
	for _, operation := range operations {
		// Restricted tokens must not discover operations they cannot call.
		if allow(operation) {
			kept = append(kept, operation)
		}
	}
	return kept
}

// admittedProse re-validates stored AI prose so a bad payload degrades to the template.
func admittedProse(snapshot *Snapshot) *Prose {
	// Template-only skills have no prose to admit.
	if snapshot == nil || snapshot.Prose == nil {
		return nil
	}
	prose, err := ValidateProse(*snapshot.Prose)
	// Invalid prose never reaches the document; the deterministic text is used instead.
	if err != nil {
		return nil
	}
	return &prose
}

// ValidateProse admits only short plain sentences; it is applied when prose is produced and again when rendered.
func ValidateProse(prose Prose) (Prose, error) {
	result := Prose{Purpose: Text(prose.Purpose)}
	// The purpose is optional but, when present, must be safe and bounded.
	if result.Purpose != "" && (!safeProse(result.Purpose) || len(result.Purpose) > purposeLimit) {
		return Prose{}, errors.New("invalid skill purpose")
	}
	// Too many bullets would bury the fixed guidance and the operation list.
	if len(prose.WhenToUse) > whenCountLimit {
		return Prose{}, errors.New("too many when-to-use items")
	}
	for _, item := range prose.WhenToUse {
		item = Text(item)
		// Each bullet must be one safe, bounded plain sentence.
		if item == "" || !safeProse(item) || len(item) > whenLimit {
			return Prose{}, errors.New("invalid when-to-use item")
		}
		result.WhenToUse = append(result.WhenToUse, item)
	}
	return result, nil
}

// safeProse rejects Markdown structure, code, links and template syntax.
func safeProse(value string) bool {
	return !strings.ContainsAny(value, "`<>{}[]#|*_\\") && !strings.Contains(value, "://") && !strings.HasPrefix(value, "-")
}

// serviceLabels lists visible service names in render order.
func serviceLabels(snapshot *Snapshot) []string {
	// Without a snapshot there is no pinned service inventory to name.
	if snapshot == nil {
		return nil
	}
	labels := make([]string, 0, len(snapshot.Services))
	for _, group := range snapshot.Services {
		labels = append(labels, group.Label)
	}
	return labels
}

// Name maps a display name onto the Agent Skills grammar with an -mcp suffix.
func Name(display string) string {
	var b strings.Builder
	hyphen := false
	for _, character := range strings.ToLower(display) {
		// Lowercase ASCII letters and digits are already skill-safe.
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
			b.WriteRune(character)
			hyphen = false
			continue
		}
		// Every other run of characters becomes one separator, never leading or repeated.
		if b.Len() > 0 && !hyphen {
			b.WriteByte('-')
			hyphen = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	// An empty display name still needs a stable skill identity.
	if slug == "" {
		slug = "fused"
	}
	// Names that already end with the suffix must not become "x-mcp-mcp".
	slug = strings.TrimSuffix(slug, nameSuffix)
	// Truncation keeps room for the suffix and cannot leave a trailing separator.
	if len(slug) > nameLimit-len(nameSuffix) {
		slug = strings.TrimRight(slug[:nameLimit-len(nameSuffix)], "-")
	}
	return slug + nameSuffix
}

// Text flattens text into one printable line so it cannot add Markdown structure.
func Text(value string) string {
	fields := strings.FieldsFunc(value, func(character rune) bool {
		return unicode.IsSpace(character) || unicode.IsControl(character)
	})
	return strings.Join(fields, " ")
}

// code keeps a value inside one inline code span.
func code(value string) string {
	return strings.ReplaceAll(Text(value), "`", "'")
}

// yamlString emits a double-quoted scalar; JSON string syntax is valid YAML.
func yamlString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// truncate bounds text by bytes without splitting a UTF-8 character.
func truncate(value string, limit int) string {
	// Short text is returned unchanged.
	if len(value) <= limit {
		return value
	}
	cut := limit - len("...")
	// Walk back to a rune boundary so the result remains valid UTF-8.
	for cut > 0 && (value[cut]&0xC0) == 0x80 {
		cut--
	}
	return strings.TrimSpace(value[:cut]) + "..."
}
