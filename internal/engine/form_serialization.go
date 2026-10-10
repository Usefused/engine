package engine

import (
	"fmt"
	"reflect"
)

// addDeepFormValue preserves nested object/array structure using deterministic bracketed form keys.
func addDeepFormValue(name string, value any, form queryParameters, allowReserved bool, depth int) error {
	// Bound recursion independently of request size, including cyclic values supplied by Go callers.
	if depth > 32 {
		return fmt.Errorf("nested form value exceeds maximum depth")
	}
	raw := reflect.ValueOf(value)
	// Null values carry no form entry, matching absent optional JSON properties.
	if !raw.IsValid() {
		return nil
	}
	switch raw.Kind() {
	case reflect.Map:
		return addDeepFormObject(name, raw, form, allowReserved, depth)
	case reflect.Slice, reflect.Array:
		for index := 0; index < raw.Len(); index++ {
			// Explicit indexes retain object grouping and array order at the provider boundary.
			if err := addDeepFormValue(fmt.Sprintf("%s[%d]", name, index), raw.Index(index).Interface(), form, allowReserved, depth+1); err != nil {
				return err
			}
		}
	default:
		// Union fields can choose a scalar (including an empty-string reset); its bracket path is already complete.
		form.Add(name, fmt.Sprint(value), allowReserved)
	}
	return nil
}

// addDeepFormObject retains field names while rejecting maps that cannot represent JSON objects.
func addDeepFormObject(name string, raw reflect.Value, form queryParameters, allowReserved bool, depth int) error {
	// JSON objects require string keys; other maps have no stable provider field representation.
	if raw.Type().Key().Kind() != reflect.String {
		return fmt.Errorf("form objects require string keys")
	}
	for _, key := range raw.MapKeys() {
		// Each nested field keeps its own key; EncodeForm sorts the final wire representation.
		if err := addDeepFormValue(name+"["+key.String()+"]", raw.MapIndex(key).Interface(), form, allowReserved, depth+1); err != nil {
			return err
		}
	}
	return nil
}
