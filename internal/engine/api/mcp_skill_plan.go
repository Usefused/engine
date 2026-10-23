package api

import (
	"context"
	"log/slog"
	"strings"

	"github.com/Usefused/engine/internal/engine/mcpskill"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
)

// planMCPSkill computes the served skill's exact scope and, only when the config opts in with
// fused-skill-polish, asks the Registry LLM to polish its prose. It never fails planning: a catalogue
// or model problem leaves a template-only skill, because the MCP server is fully usable without one.
func planMCPSkill(ctx context.Context, s store.Store, name, description string, selections []models.SDKSelection, resolved []sdkResolvedService, polish bool) *mcpskill.Snapshot {
	catalogue, err := loadMCPAppOperationCatalogue(ctx, s, store.AppCatalogItem{Name: name, Selections: selections})
	// The pinned inventory must be complete; on failure the served skill points agents at search_docs instead.
	if err != nil {
		slog.InfoContext(ctx, "MCP skill inventory unavailable at plan time", "reason", err.Error())
		return nil
	}
	snapshot := mcpskill.BuildSnapshot(mcpSkillOperations(catalogue.Operations), mcpSkillServices(resolved))
	// Server metadata leaves the Engine only with explicit, version-pinned consent.
	if polish {
		snapshot.Prose = mcpskill.Polish(ctx, mcpskill.NewPolishRequest(name, description, snapshot))
	}
	return snapshot
}

// mcpSkillOperations maps the exact-version catalogue onto skill rows, keeping only public provenance.
func mcpSkillOperations(operations []mcpAppOperation) []mcpskill.Operation {
	rows := make([]mcpskill.Operation, 0, len(operations))
	for _, operation := range operations {
		imported := strings.HasPrefix(operation.Kind, "mcp_")
		key := ""
		// Only physical and imported rows carry service provenance; others are listed on their own.
		if operation.Kind == appOperationKindPhysical || imported {
			key = operation.ServiceID.String()
		}
		rows = append(rows, mcpskill.Operation{OperationID: operation.OperationID, ServiceKey: key, Imported: imported})
	}
	return rows
}

// mcpSkillServices labels services by their authored config name and pinned version.
func mcpSkillServices(resolved []sdkResolvedService) []mcpskill.Service {
	services := make([]mcpskill.Service, 0, len(resolved))
	for _, service := range resolved {
		services = append(services, mcpskill.Service{Key: service.ServiceID.String(), Label: service.ServiceName, Version: service.Version})
	}
	return services
}
