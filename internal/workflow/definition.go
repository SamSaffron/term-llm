// Package workflow executes capability-scoped Lua workflows.
package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
)

const maxWorkflowSourceBytes = 1 << 20

// Metadata is declared by the leading workflow{...} call in a definition.
type Metadata struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Inputs      any      `json:"inputs,omitempty"`
	Phases      []string `json:"phases,omitempty"`
}

// Definition is an exact Lua source file and its parsed metadata.
type Definition struct {
	Metadata
	Path   string `json:"path"`
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
}

// ParseDefinition validates Lua syntax and captures the leading workflow declaration.
// The workflow body is compiled but never executed during validation.
func ParseDefinition(path string, source []byte) (*Definition, error) {
	if len(source) > maxWorkflowSourceBytes {
		return nil, fmt.Errorf("workflow source exceeds %d bytes", maxWorkflowSourceBytes)
	}
	if err := validateWorkflowPrefix(string(source)); err != nil {
		return nil, err
	}

	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	defer L.Close()
	validationCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	L.SetContext(validationCtx)

	chunk, err := L.LoadString(string(source))
	if err != nil {
		return nil, fmt.Errorf("parse Lua: %w", err)
	}
	var metadata *Metadata
	L.SetGlobal("workflow", L.NewFunction(func(L *lua.LState) int {
		table := L.CheckTable(1)
		parsed, err := metadataFromTable(table)
		if err != nil {
			L.RaiseError("invalid workflow metadata: %v", err)
		}
		metadata = &parsed
		// Validation must not execute the workflow body. The leading declaration
		// is captured using Lua's own parser, then execution stops here.
		L.RaiseError("workflow metadata captured")
		return 0
	}))

	L.Push(chunk)
	callErr := L.PCall(0, 0, nil)
	if metadata == nil {
		if callErr != nil {
			return nil, fmt.Errorf("validate workflow declaration: %w", callErr)
		}
		return nil, fmt.Errorf("workflow metadata declaration is required")
	}

	sum := sha256.Sum256(source)
	return &Definition{
		Metadata: *metadata,
		Path:     path,
		Source:   string(source),
		SHA256:   hex.EncodeToString(sum[:]),
	}, nil
}

// ParseDefinitionFile reads and validates one explicit Lua workflow file.
func ParseDefinitionFile(path string) (*Definition, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read workflow: %w", err)
	}
	return ParseDefinition(path, source)
}

func validateWorkflowPrefix(source string) error {
	index, err := skipLuaWhitespaceAndComments(source, 0)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(source[index:], "workflow") {
		return fmt.Errorf("workflow definition must begin with workflow { ... }")
	}
	index += len("workflow")
	if index < len(source) && !isLuaSpace(source[index]) && source[index] != '{' {
		return fmt.Errorf("workflow definition must begin with workflow { ... }")
	}
	index, err = skipLuaWhitespaceAndComments(source, index)
	if err != nil {
		return err
	}
	if index >= len(source) || source[index] != '{' {
		return fmt.Errorf("workflow definition must begin with workflow { ... }")
	}
	return nil
}

func skipLuaWhitespaceAndComments(source string, index int) (int, error) {
	for index < len(source) {
		if isLuaSpace(source[index]) {
			index++
			continue
		}
		if strings.HasPrefix(source[index:], "--[[") {
			end := strings.Index(source[index+4:], "]]")
			if end < 0 {
				return 0, fmt.Errorf("unterminated leading Lua block comment")
			}
			index += 4 + end + 2
			continue
		}
		if strings.HasPrefix(source[index:], "--") {
			end := strings.IndexByte(source[index:], '\n')
			if end < 0 {
				return len(source), nil
			}
			index += end + 1
			continue
		}
		break
	}
	return index, nil
}

func isLuaSpace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

func metadataFromTable(table *lua.LTable) (Metadata, error) {
	nameValue := table.RawGetString("name")
	name, ok := nameValue.(lua.LString)
	if !ok || strings.TrimSpace(string(name)) == "" {
		return Metadata{}, fmt.Errorf("name must be a non-empty string")
	}

	metadata := Metadata{Name: strings.TrimSpace(string(name))}
	if value := table.RawGetString("description"); value != lua.LNil {
		description, ok := value.(lua.LString)
		if !ok {
			return Metadata{}, fmt.Errorf("description must be a string")
		}
		metadata.Description = string(description)
	}
	if value := table.RawGetString("inputs"); value != lua.LNil {
		converted, err := luaValueToGo(value)
		if err != nil {
			return Metadata{}, fmt.Errorf("inputs: %w", err)
		}
		metadata.Inputs = converted
	}
	if value := table.RawGetString("phases"); value != lua.LNil {
		phases, ok := value.(*lua.LTable)
		if !ok {
			return Metadata{}, fmt.Errorf("phases must be a table")
		}
		for i := 1; i <= phases.Len(); i++ {
			phase, ok := phases.RawGetInt(i).(lua.LString)
			if !ok || strings.TrimSpace(string(phase)) == "" {
				return Metadata{}, fmt.Errorf("phases[%d] must be a non-empty string", i)
			}
			metadata.Phases = append(metadata.Phases, string(phase))
		}
	}
	return metadata, nil
}

func luaValueToGo(value lua.LValue) (any, error) {
	switch value := value.(type) {
	case *lua.LNilType:
		return nil, nil
	case lua.LBool:
		return bool(value), nil
	case lua.LString:
		return string(value), nil
	case lua.LNumber:
		return float64(value), nil
	case *lua.LTable:
		length := value.Len()
		array := make([]any, length)
		isArray := length > 0
		seen := 0
		var conversionErr error
		value.ForEach(func(key, item lua.LValue) {
			if conversionErr != nil {
				return
			}
			index, ok := key.(lua.LNumber)
			if !ok || int(index) < 1 || int(index) > length || float64(int(index)) != float64(index) {
				isArray = false
				return
			}
			converted, err := luaValueToGo(item)
			if err != nil {
				conversionErr = err
				return
			}
			array[int(index)-1] = converted
			seen++
		})
		if conversionErr != nil {
			return nil, conversionErr
		}
		if isArray && seen == length {
			return array, nil
		}
		object := make(map[string]any)
		value.ForEach(func(key, item lua.LValue) {
			if conversionErr != nil {
				return
			}
			keyString, ok := key.(lua.LString)
			if !ok {
				conversionErr = fmt.Errorf("table key %s is not a string", key.String())
				return
			}
			converted, err := luaValueToGo(item)
			if err != nil {
				conversionErr = err
				return
			}
			object[string(keyString)] = converted
		})
		if conversionErr != nil {
			return nil, conversionErr
		}
		return object, nil
	default:
		return nil, fmt.Errorf("unsupported Lua value %s", value.Type().String())
	}
}
