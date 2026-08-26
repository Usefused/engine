package api

import (
	"errors"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
)

var unifiedExecutionStepGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "UnifiedExecutionStep",
	Fields: graphql.Fields{
		"target": &graphql.Field{Type: graphql.String}, "phase": &graphql.Field{Type: graphql.String},
		"status": &graphql.Field{Type: graphql.String}, "error_code": &graphql.Field{Type: graphql.String},
	},
})

// appExecutionReceiptArgs keeps child navigation out of aggregate analytics filters.
func appExecutionReceiptArgs() graphql.FieldConfigArgument {
	args := appExecutionActivityArgs(true)
	args["parent_execution_id"] = &graphql.ArgumentConfig{Type: graphql.String}
	return args
}

// appExecutionReceiptFilter only adds a parent selector after ordinary app authorization.
func appExecutionReceiptFilter(filter store.EngineExecutionFilter, p graphql.ResolveParams) (store.EngineExecutionFilter, error) {
	value := graphQLStringArg(p, "parent_execution_id")
	// An absent parent selects the grouped root history.
	if value == "" {
		return filter, nil
	}
	parent, err := uuid.Parse(value)
	// Nil is not a persisted receipt identity and must not accidentally broaden the query.
	if err != nil || parent == uuid.Nil {
		return filter, errors.New("invalid parent_execution_id")
	}
	filter.ParentExecutionID = parent
	return filter, nil
}

// projectUnifiedReceiptSteps preserves execution order without exposing mapped or raw provider data.
func projectUnifiedReceiptSteps(steps []models.UnifiedExecutionStep) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(steps))
	// The canonical event validator bounds this list to the admitted scheduler graph.
	for _, step := range steps {
		items = append(items, map[string]interface{}{"target": step.Target, "phase": step.Phase, "status": step.Status, "error_code": step.ErrorCode})
	}
	return items
}
