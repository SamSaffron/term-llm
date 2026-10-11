package chat

import (
	"fmt"
	"strconv"
	"strings"
)

// Terminal title formats use a small, text/template-compatible syntax:
//
//	{{title}} {{agent}} ...            documented placeholders
//	{{env.NAME}} {{env "NAME"}}        environment lookups
//	{{.Title}} {{.Env.NAME}}           field access on the format data
//	{{env "X" | default "host"}}       pipelines; default may also be called
//	{{default "host" (env "X")}}       directly, with parenthesised arguments
//	{{- ... -}}                        whitespace trimming
//
// It is evaluated by hand instead of with text/template because template
// execution uses reflect.Value.MethodByName, which disables the linker's
// dead-method elimination for the whole binary (see internal/htmlesc).

type titleFormatProgram []titleFormatNode

// titleFormatNode is either literal text (pipeline == nil) or an action.
type titleFormatNode struct {
	text     string
	pipeline []titleFormatCommand
}

// titleFormatCommand is a function call (fn != "") or a single operand.
type titleFormatCommand struct {
	fn   string
	args []titleFormatOperand
}

type titleFormatOperand struct {
	kind  titleFormatOperandKind
	value string
	sub   []titleFormatCommand // for parenthesised pipelines
}

type titleFormatOperandKind int

const (
	titleOperandString titleFormatOperandKind = iota
	titleOperandField
	titleOperandEnv
	titleOperandPipeline
)

// titleTrimSpace is the whitespace text/template recognises around trim markers.
const titleTrimSpace = " \t\r\n"

func parseTitleFormat(format string) (titleFormatProgram, error) {
	var prog titleFormatProgram
	rest := format
	for rest != "" {
		start := strings.Index(rest, "{{")
		if start < 0 {
			prog = append(prog, titleFormatNode{text: rest})
			break
		}
		text := rest[:start]
		rest = rest[start+2:]
		if len(rest) >= 2 && rest[0] == '-' && strings.IndexByte(titleTrimSpace, rest[1]) >= 0 {
			text = strings.TrimRight(text, titleTrimSpace)
			rest = rest[1:]
		}
		if text != "" {
			prog = append(prog, titleFormatNode{text: text})
		}
		end := indexActionEnd(rest)
		if end < 0 {
			return nil, fmt.Errorf("unclosed action")
		}
		action := rest[:end]
		rest = rest[end+2:]
		if n := len(action); n >= 2 && action[n-1] == '-' && strings.IndexByte(titleTrimSpace, action[n-2]) >= 0 {
			action = action[:n-1]
			rest = strings.TrimLeft(rest, titleTrimSpace)
		}
		pipeline, err := parseTitlePipeline(action)
		if err != nil {
			return nil, err
		}
		prog = append(prog, titleFormatNode{pipeline: pipeline})
	}
	return prog, nil
}

// indexActionEnd finds the closing "}}" of an action, skipping quoted strings.
func indexActionEnd(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '`':
			j := skipTitleString(s, i)
			if j < 0 {
				return -1
			}
			i = j - 1
		case '}':
			if i+1 < len(s) && s[i+1] == '}' {
				return i
			}
		}
	}
	return -1
}

// skipTitleString returns the index just past the string literal at s[i].
func skipTitleString(s string, i int) int {
	quote := s[i]
	for j := i + 1; j < len(s); j++ {
		switch {
		case quote == '"' && s[j] == '\\':
			j++
		case s[j] == quote:
			return j + 1
		}
	}
	return -1
}

func tokenizeTitleAction(action string) ([]string, error) {
	var tokens []string
	for i := 0; i < len(action); {
		c := action[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '|' || c == '(' || c == ')':
			tokens = append(tokens, string(c))
			i++
		case c == '"' || c == '`':
			j := skipTitleString(action, i)
			if j < 0 {
				return nil, fmt.Errorf("unterminated string in %q", action)
			}
			tokens = append(tokens, action[i:j])
			i = j
		default:
			j := i
			for j < len(action) && !strings.ContainsRune(" \t\n\r|()\"`", rune(action[j])) {
				j++
			}
			tokens = append(tokens, action[i:j])
			i = j
		}
	}
	return tokens, nil
}

func parseTitlePipeline(action string) ([]titleFormatCommand, error) {
	tokens, err := tokenizeTitleAction(action)
	if err != nil {
		return nil, err
	}
	pipeline, rest, err := parseTitlePipelineTokens(tokens)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("unexpected %q in %q", rest[0], strings.TrimSpace(action))
	}
	return pipeline, nil
}

func parseTitlePipelineTokens(tokens []string) ([]titleFormatCommand, []string, error) {
	var pipeline []titleFormatCommand
	for {
		var cmd titleFormatCommand
		if len(tokens) == 0 || tokens[0] == "|" || tokens[0] == ")" {
			return nil, nil, fmt.Errorf("missing value in title format action")
		}
		switch tokens[0] {
		case "env", "default":
			cmd.fn = tokens[0]
			tokens = tokens[1:]
		}
		for len(tokens) > 0 && tokens[0] != "|" && tokens[0] != ")" {
			var op titleFormatOperand
			var err error
			op, tokens, err = parseTitleOperand(tokens)
			if err != nil {
				return nil, nil, err
			}
			cmd.args = append(cmd.args, op)
		}
		if cmd.fn == "" && len(cmd.args) != 1 {
			return nil, nil, fmt.Errorf("title format action must be a single value or a call to env or default")
		}
		pipeline = append(pipeline, cmd)
		if len(tokens) == 0 || tokens[0] == ")" {
			break
		}
		tokens = tokens[1:] // consume "|"
	}
	for i, cmd := range pipeline {
		args := len(cmd.args)
		if i > 0 {
			if cmd.fn == "" {
				return nil, nil, fmt.Errorf("title format pipelines may only pipe into env or default")
			}
			args++
		}
		switch {
		case cmd.fn == "env" && args != 1:
			return nil, nil, fmt.Errorf("env takes exactly one argument")
		case cmd.fn == "default" && args != 2:
			return nil, nil, fmt.Errorf("default takes exactly two arguments")
		}
	}
	return pipeline, tokens, nil
}

func parseTitleOperand(tokens []string) (titleFormatOperand, []string, error) {
	tok := tokens[0]
	tokens = tokens[1:]
	switch {
	case tok == "(":
		sub, rest, err := parseTitlePipelineTokens(tokens)
		if err != nil {
			return titleFormatOperand{}, nil, err
		}
		if len(rest) == 0 || rest[0] != ")" {
			return titleFormatOperand{}, nil, fmt.Errorf("unclosed parenthesis in title format")
		}
		return titleFormatOperand{kind: titleOperandPipeline, sub: sub}, rest[1:], nil
	case tok[0] == '"' || tok[0] == '`':
		value, err := strconv.Unquote(tok)
		if err != nil {
			return titleFormatOperand{}, nil, fmt.Errorf("invalid string %s: %w", tok, err)
		}
		return titleFormatOperand{kind: titleOperandString, value: value}, tokens, nil
	case strings.HasPrefix(tok, "env."):
		return titleFormatOperand{kind: titleOperandEnv, value: strings.TrimPrefix(tok, "env.")}, tokens, nil
	case strings.HasPrefix(tok, ".Env."):
		return titleFormatOperand{kind: titleOperandEnv, value: strings.TrimPrefix(tok, ".Env.")}, tokens, nil
	case strings.HasPrefix(tok, "."):
		field := strings.TrimPrefix(tok, ".")
		if !isTerminalTitleDataField(field) {
			return titleFormatOperand{}, nil, fmt.Errorf("can't evaluate field %s in terminal title format", field)
		}
		return titleFormatOperand{kind: titleOperandField, value: field}, tokens, nil
	default:
		field, ok := terminalTitleFormatField(tok)
		if !ok {
			return titleFormatOperand{}, nil, fmt.Errorf("unknown terminal title placeholder %q", tok)
		}
		return titleFormatOperand{kind: titleOperandField, value: field}, tokens, nil
	}
}

func isTerminalTitleDataField(name string) bool {
	_, ok := terminalTitleFormatData{}.field(name)
	return ok
}

func (d terminalTitleFormatData) field(name string) (string, bool) {
	switch name {
	case "Title":
		return d.Title, true
	case "StableTitle":
		return d.StableTitle, true
	case "Agent":
		return d.Agent, true
	case "Task":
		return d.Task, true
	case "Model":
		return d.Model, true
	case "Branch":
		return d.Branch, true
	case "Phase":
		return d.Phase, true
	case "State":
		return d.State, true
	case "Activity":
		return d.Activity, true
	case "Elapsed":
		return d.Elapsed, true
	case "Attention":
		return strconv.FormatBool(d.Attention), true
	case "AttentionMarker":
		return d.AttentionMarker, true
	case "Streaming":
		return strconv.FormatBool(d.Streaming), true
	default:
		return "", false
	}
}

func (p titleFormatProgram) execute(data terminalTitleFormatData, env TerminalTitleEnvironment) string {
	var b strings.Builder
	for _, node := range p {
		if node.pipeline == nil {
			b.WriteString(node.text)
			continue
		}
		b.WriteString(evalTitlePipeline(node.pipeline, data, env))
	}
	return b.String()
}

func evalTitlePipeline(pipeline []titleFormatCommand, data terminalTitleFormatData, env TerminalTitleEnvironment) string {
	var value string
	for i, cmd := range pipeline {
		args := make([]string, 0, len(cmd.args)+1)
		for _, op := range cmd.args {
			args = append(args, evalTitleOperand(op, data, env))
		}
		if i > 0 {
			args = append(args, value)
		}
		switch cmd.fn {
		case "env":
			value = env.Get(args[0])
		case "default":
			value = args[1]
			if strings.TrimSpace(value) == "" {
				value = args[0]
			}
		default:
			value = args[0]
		}
	}
	return value
}

func evalTitleOperand(op titleFormatOperand, data terminalTitleFormatData, env TerminalTitleEnvironment) string {
	switch op.kind {
	case titleOperandString:
		return op.value
	case titleOperandField:
		value, _ := data.field(op.value)
		return value
	case titleOperandEnv:
		return env.Get(op.value)
	case titleOperandPipeline:
		return evalTitlePipeline(op.sub, data, env)
	default:
		return ""
	}
}
