package unified

import (
	"sort"
)

// canonicalDependencies validates one binding's direct edges and returns a
// stable order for immutable hashing and deterministic scheduling.
func canonicalDependencies(target string, values, targets []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	allowed := make(map[string]struct{}, len(targets))
	for _, candidate := range targets {
		allowed[candidate] = struct{}{}
	}
	dependencies := append([]string(nil), values...)
	sort.Strings(dependencies)
	for index, dependency := range dependencies {
		// self-dependencies can never become ready and would otherwise turn
		// a configuration error into a runtime stall.
		if dependency == target {
			return nil, definitionError("binding cannot depend on itself", nil)
		}
		// every edge must resolve inside the immutable operation rather than
		// becoming a hidden provider call discovered at runtime.
		if _, ok := allowed[dependency]; !ok {
			return nil, definitionError("binding dependency target is not defined", nil)
		}
		// duplicate edges add no ordering authority and make rollback trigger
		// reporting ambiguous, so canonical state rejects them.
		if index > 0 && dependencies[index-1] == dependency {
			return nil, definitionError("binding dependencies must be unique", nil)
		}
	}
	return dependencies, nil
}

// validateBindingGraph proves every persisted operation is an acyclic DAG and
// that rollback operations stay within their binding's service version.
func validateBindingGraph(bindings []BindingDefinition) error {
	targets := make([]string, len(bindings))
	for index, binding := range bindings {
		targets[index] = binding.PublicTarget
	}
	indegree := make(map[string]int, len(bindings))
	consumers := make(map[string][]string, len(bindings))
	for _, binding := range bindings {
		dependencies, err := canonicalDependencies(binding.PublicTarget, binding.DependsOn, targets)
		if err != nil {
			return err
		}
		if err := validateRollbackScope(binding); err != nil {
			return err
		}
		indegree[binding.PublicTarget] = len(dependencies)
		for _, dependency := range dependencies {
			consumers[dependency] = append(consumers[dependency], binding.PublicTarget)
		}
	}
	return validateAcyclicDependencies(indegree, consumers)
}

// validateRollbackScope prevents a binding rollback from silently changing
// service or version while still allowing a distinct compensation endpoint.
func validateRollbackScope(binding BindingDefinition) error {
	if binding.Rollback == nil {
		return nil
	}
	// selectors, connected auth, environment, and bucket routing are owned
	// by the binding service; crossing that boundary needs a separate binding.
	if binding.Rollback.ServiceID != binding.ServiceID || binding.Rollback.ServiceVersionID != binding.ServiceVersionID {
		return definitionError("rollback must use the binding service version", nil)
	}
	return nil
}

// validateAcyclicDependencies consumes topological levels without executing
// them; any nodes left behind are part of a dependency cycle.
func validateAcyclicDependencies(indegree map[string]int, consumers map[string][]string) error {
	ready := make([]string, 0, len(indegree))
	for target, count := range indegree {
		if count == 0 {
			ready = append(ready, target)
		}
	}
	visited := 0
	for len(ready) > 0 {
		target := ready[len(ready)-1]
		ready = ready[:len(ready)-1]
		visited++
		for _, consumer := range consumers[target] {
			indegree[consumer]--
			if indegree[consumer] == 0 {
				ready = append(ready, consumer)
			}
		}
	}
	// a valid DAG must make every binding ready exactly once.
	if visited != len(indegree) {
		return definitionError("binding dependency graph contains a cycle", nil)
	}
	return nil
}
