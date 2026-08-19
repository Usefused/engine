package api

import "errors"

type unifiedGraphBinding struct {
	target       string
	dependencies []string
}

// validateUnifiedDocGraph proves the public binding graph is closed and
// acyclic before compilation resolves any provider operations.
func validateUnifiedDocGraph(bindings []unifiedGraphBinding) error {
	known := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		known[binding.target] = struct{}{}
	}
	indegree := make(map[string]int, len(bindings))
	consumers := make(map[string][]string, len(bindings))
	for _, binding := range bindings {
		dependencies, err := validateUnifiedDocDependencies(binding, known)
		if err != nil {
			return err
		}
		indegree[binding.target] = len(dependencies)
		for _, dependency := range dependencies {
			consumers[dependency] = append(consumers[dependency], binding.target)
		}
	}
	return validateUnifiedDocAcyclic(indegree, consumers)
}

// validateUnifiedDocDependencies rejects edges that cannot name one exact
// binding in the same operation.
func validateUnifiedDocDependencies(binding unifiedGraphBinding, known map[string]struct{}) ([]string, error) {
	seen := make(map[string]struct{}, len(binding.dependencies))
	for _, dependency := range binding.dependencies {
		// a self edge can never be scheduled and is always a config defect.
		if dependency == binding.target {
			return nil, errors.New("binding cannot depend on itself")
		}
		// dependencies never auto-select hidden provider calls.
		if _, ok := known[dependency]; !ok {
			return nil, errors.New("dependency target is not bound")
		}
		// duplicate edges carry no extra meaning and complicate rollback
		// trigger attribution.
		if _, ok := seen[dependency]; ok {
			return nil, errors.New("dependency targets must be unique")
		}
		seen[dependency] = struct{}{}
	}
	return binding.dependencies, nil
}

// validateUnifiedDocAcyclic consumes every ready node; any remainder proves a
// cycle without relying on map declaration order.
func validateUnifiedDocAcyclic(indegree map[string]int, consumers map[string][]string) error {
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
	// every valid binding must become ready once in a DAG.
	if visited != len(indegree) {
		return errors.New("dependency cycle detected")
	}
	return nil
}
