package configresolver

import (
	"fmt"
	"os"
	"reflect"
	"strings"
)

const (
	envVarPlaceholderStart = "${"
	envVarPlaceholderEnd   = "}"
)

// ResolveEnvVars walks a configuration object recursively and resolves ${VAR}
// placeholders in string values using the current process environment.
func ResolveEnvVars(target any) error {
	if target == nil {
		return fmt.Errorf("config target must be a non-nil pointer")
	}

	value := reflect.ValueOf(target)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return fmt.Errorf("config target must be a non-nil pointer")
	}

	return resolveConfigValue(value)
}

func resolveConfigValue(value reflect.Value) error {
	if !value.IsValid() {
		return nil
	}

	switch value.Kind() {
	case reflect.Pointer:
		// Recurse into pointer targets when present; nil pointers are left untouched.
		if value.IsNil() {
			return nil
		}
		return resolveConfigValue(value.Elem())
	case reflect.Interface:
		if value.IsNil() {
			return nil
		}

		// Interface values are not directly writable, so resolve a cloned value and
		// replace the interface payload with the resolved copy.
		resolved, err := cloneAndResolveValue(value.Elem())
		if err != nil {
			return err
		}
		if value.CanSet() {
			value.Set(resolved)
		}
		return nil
	case reflect.Struct:
		// Walk nested config structs so placeholder expansion applies consistently
		// across the full config object, not just top-level fields.
		for i := range value.NumField() {
			field := value.Field(i)
			if !field.CanSet() {
				continue
			}
			if err := resolveConfigValue(field); err != nil {
				return err
			}
		}
		return nil
	case reflect.String:
		if !value.CanSet() {
			return nil
		}

		// Only string values are expanded. Other scalar types are intentionally
		// left unchanged to keep resolution predictable.
		resolved, err := expandEnvPlaceholders(value.String())
		if err != nil {
			return err
		}
		value.SetString(resolved)
		return nil
	case reflect.Slice, reflect.Array:
		for i := range value.Len() {
			if err := resolveConfigValue(value.Index(i)); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		if value.IsNil() {
			return nil
		}

		// Map entries are not addressable through reflection, so resolve each value
		// through a writable clone and write it back under the same key.
		for _, key := range value.MapKeys() {
			resolved, err := cloneAndResolveValue(value.MapIndex(key))
			if err != nil {
				return err
			}
			value.SetMapIndex(key, resolved)
		}
		return nil
	default:
		return nil
	}
}

func cloneAndResolveValue(value reflect.Value) (reflect.Value, error) {
	if !value.IsValid() {
		return value, nil
	}

	// Map and interface elements are often non-settable; this gives recursive
	// resolution a writable copy to operate on.
	clone := reflect.New(value.Type()).Elem()
	clone.Set(value)

	if err := resolveConfigValue(clone); err != nil {
		return reflect.Value{}, err
	}

	return clone, nil
}

func expandEnvPlaceholders(input string) (string, error) {
	var builder strings.Builder

	for cursor := 0; cursor < len(input); {
		start := strings.Index(input[cursor:], envVarPlaceholderStart)
		if start == -1 {
			builder.WriteString(input[cursor:])
			return builder.String(), nil
		}

		start += cursor
		builder.WriteString(input[cursor:start])

		// Only the explicit ${VAR} form is supported. We do not inherit broader
		// shell expansion semantics such as bare $VAR or default expressions.
		end := strings.Index(input[start+len(envVarPlaceholderStart):], envVarPlaceholderEnd)
		if end == -1 {
			return "", fmt.Errorf("invalid environment variable placeholder in %q", input)
		}

		end += start + len(envVarPlaceholderStart)
		varName := input[start+len(envVarPlaceholderStart) : end]
		if varName == "" || strings.Contains(varName, envVarPlaceholderStart) {
			return "", fmt.Errorf("invalid environment variable placeholder in %q", input)
		}

		value, ok := os.LookupEnv(varName)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", varName)
		}

		builder.WriteString(value)
		cursor = end + 1
	}

	return builder.String(), nil
}
