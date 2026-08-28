package grok

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"regexp"
	"strings"
)

var (
	toolNamePattern      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	toolSyntaxPattern    = regexp.MustCompile(`(?i)<tool_calls|<tool_call|<function_call|<invoke\s|"tool_calls"\s*:`)
	toolCallsRootPattern = regexp.MustCompile(`(?is)<tool_calls\s*>(.*?)</tool_calls\s*>`)
	toolCallPattern      = regexp.MustCompile(`(?is)<tool_call\s*>(.*?)</tool_call\s*>`)
	toolNameTagPattern   = regexp.MustCompile(`(?is)<tool_name\s*>(.*?)</tool_name\s*>`)
	toolParamsTagPattern = regexp.MustCompile(`(?is)<parameters\s*>(.*?)</parameters\s*>`)
)

type functionTool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

type toolConfiguration struct {
	Functions       []functionTool
	HostedWebSearch bool
	available       map[string]struct{}
	Choice          string
	ForcedName      string
}

type parsedToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type toolStreamResult struct {
	SafeText string
	Calls    []parsedToolCall
	Complete bool
	Raw      string
}

type toolStreamSieve struct {
	available map[string]struct{}
	buffer    string
	capturing bool
	done      bool
}

func parseToolConfiguration(rawTools, rawChoice json.RawMessage) (toolConfiguration, error) {
	configuration := toolConfiguration{Choice: "auto"}
	trimmed := bytes.TrimSpace(rawTools)
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		var values []map[string]any
		if err := json.Unmarshal(trimmed, &values); err != nil {
			return toolConfiguration{}, errors.New("tools 必须是数组")
		}
		if len(values) > maxFunctionTools {
			return toolConfiguration{}, fmt.Errorf("tools 不能超过 %d 个", maxFunctionTools)
		}
		for _, value := range values {
			function, supported, err := parseFunctionTool(value)
			if err != nil {
				return toolConfiguration{}, err
			}
			if supported {
				configuration.Functions = append(configuration.Functions, function)
				continue
			}
			typeName, _ := value["type"].(string)
			switch strings.ToLower(strings.TrimSpace(typeName)) {
			case "web_search", "web_search_preview":
				configuration.HostedWebSearch = true
			default:
				return toolConfiguration{}, fmt.Errorf("Grok Web 暂不支持 tools.type=%q", typeName)
			}
		}
	}
	choice, forcedName, err := parseToolChoice(rawChoice)
	if err != nil {
		return toolConfiguration{}, err
	}
	configuration.Choice = choice
	configuration.ForcedName = forcedName
	configuration.available = make(map[string]struct{}, len(configuration.Functions))
	for _, function := range configuration.Functions {
		if _, exists := configuration.available[function.Name]; exists {
			return toolConfiguration{}, fmt.Errorf("function tool 名称 %q 重复", function.Name)
		}
		configuration.available[function.Name] = struct{}{}
	}
	if forcedName != "" {
		if _, ok := configuration.available[forcedName]; !ok {
			return toolConfiguration{}, fmt.Errorf("tool_choice 指定的函数 %q 不存在", forcedName)
		}
	}
	if (choice == "required" || forcedName != "") && len(configuration.Functions) == 0 && !configuration.HostedWebSearch {
		return toolConfiguration{}, errors.New("tool_choice 要求调用函数，但 tools 中没有可用函数")
	}
	return configuration, nil
}

func parseFunctionTool(value map[string]any) (functionTool, bool, error) {
	typeName, _ := value["type"].(string)
	if strings.ToLower(strings.TrimSpace(typeName)) != "function" {
		return functionTool{}, false, nil
	}
	definition := value
	if nested, ok := value["function"].(map[string]any); ok {
		definition = nested
	}
	name, _ := definition["name"].(string)
	name = strings.TrimSpace(name)
	if !toolNamePattern.MatchString(name) {
		return functionTool{}, false, errors.New("function tool 的 name 必须是 1 到 64 位字母、数字、下划线或连字符")
	}
	description, _ := definition["description"].(string)
	if len(description) > maxToolDescriptionSize {
		return functionTool{}, false, fmt.Errorf("函数 %q 的 description 过长", name)
	}
	parameters := json.RawMessage(`{"type":"object","properties":{}}`)
	if raw, ok := definition["parameters"]; ok {
		encoded, err := json.Marshal(raw)
		if err != nil || !json.Valid(encoded) {
			return functionTool{}, false, fmt.Errorf("函数 %q 的 parameters 不是有效 JSON", name)
		}
		parameters = encoded
	}
	return functionTool{Name: name, Description: strings.TrimSpace(description), Parameters: parameters}, true, nil
}

func parseToolChoice(raw json.RawMessage) (string, string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "auto", "", nil
	}
	var text string
	if json.Unmarshal(trimmed, &text) == nil {
		text = strings.ToLower(strings.TrimSpace(text))
		switch text {
		case "auto", "none", "required":
			return text, "", nil
		default:
			return "", "", errors.New("tool_choice 必须是 auto、none、required 或函数对象")
		}
	}
	var value map[string]any
	if json.Unmarshal(trimmed, &value) != nil {
		return "", "", errors.New("tool_choice 格式无效")
	}
	typeName, _ := value["type"].(string)
	typeName = strings.ToLower(strings.TrimSpace(typeName))
	switch typeName {
	case "none", "auto", "required":
		return typeName, "", nil
	case "function":
		name, _ := value["name"].(string)
		if nested, ok := value["function"].(map[string]any); ok {
			name, _ = nested["name"].(string)
		}
		name = strings.TrimSpace(name)
		if !toolNamePattern.MatchString(name) {
			return "", "", errors.New("tool_choice.function.name 无效")
		}
		return "required", name, nil
	default:
		return "", "", fmt.Errorf("Grok Web 暂不支持 tool_choice.type=%q", typeName)
	}
}

func injectToolPrompt(prompt string, configuration toolConfiguration) string {
	if len(configuration.Functions) == 0 || configuration.Choice == "none" {
		return prompt
	}
	var definitions strings.Builder
	for index, function := range configuration.Functions {
		if index > 0 {
			definitions.WriteString("\n\n")
		}
		definitions.WriteString("Tool: ")
		definitions.WriteString(function.Name)
		if function.Description != "" {
			definitions.WriteString("\nDescription: ")
			definitions.WriteString(function.Description)
		}
		definitions.WriteString("\nParameters: ")
		definitions.Write(function.Parameters)
	}
	choiceInstruction := "Call a tool when it is clearly needed. Otherwise respond in plain text."
	if configuration.ForcedName != "" {
		choiceInstruction = fmt.Sprintf("You MUST call the tool named %q and must not write a plain-text reply.", configuration.ForcedName)
	} else if configuration.Choice == "required" && !configuration.HostedWebSearch {
		choiceInstruction = "You MUST call at least one available tool and must not write a plain-text reply."
	}
	system := fmt.Sprintf(`You have access to the following tools.

AVAILABLE TOOLS:
%s

TOOL CALL FORMAT - follow these rules exactly:
- When calling a tool, output only the XML block below, with no text before or after it.
- <parameters> must contain one valid JSON object.
- Put multiple calls inside one <tool_calls> element.
- Do not use Markdown code fences.

<tool_calls>
  <tool_call>
    <tool_name>TOOL_NAME</tool_name>
    <parameters>{"key":"value"}</parameters>
  </tool_call>
</tool_calls>

WHEN TO CALL: %s`, definitions.String(), choiceInstruction)
	return "[system]\n" + system + "\n\n" + prompt
}

func toolCallsToXML(raw json.RawMessage) string {
	var values []struct {
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &values) != nil || len(values) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("<tool_calls>")
	for _, value := range values {
		if !toolNamePattern.MatchString(value.Function.Name) {
			continue
		}
		arguments := normalizeToolArguments(value.Function.Arguments)
		builder.WriteString("\n  <tool_call>\n    <tool_name>")
		builder.WriteString(html.EscapeString(value.Function.Name))
		builder.WriteString("</tool_name>\n    <parameters>")
		builder.WriteString(arguments)
		builder.WriteString("</parameters>\n  </tool_call>")
	}
	builder.WriteString("\n</tool_calls>")
	return builder.String()
}

func normalizeToolArguments(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "{}"
	}
	if json.Valid([]byte(value)) {
		return value
	}
	return "{}"
}

func parseToolCalls(text string, available map[string]struct{}) []parsedToolCall {
	if !toolSyntaxPattern.MatchString(text) {
		return nil
	}
	root := toolCallsRootPattern.FindStringSubmatch(text)
	body := text
	if len(root) > 1 {
		body = root[1]
	}
	matches := toolCallPattern.FindAllStringSubmatch(body, -1)
	calls := make([]parsedToolCall, 0, len(matches))
	for _, match := range matches {
		name := ""
		if names := toolNameTagPattern.FindStringSubmatch(match[1]); len(names) > 1 {
			name = strings.TrimSpace(names[1])
		}
		if !toolNamePattern.MatchString(name) {
			continue
		}
		if available != nil {
			if _, ok := available[name]; !ok {
				continue
			}
		}
		arguments := "{}"
		if params := toolParamsTagPattern.FindStringSubmatch(match[1]); len(params) > 1 {
			arguments = normalizeToolArguments(params[1])
		}
		calls = append(calls, parsedToolCall{ID: newWebID("call"), Name: name, Arguments: arguments})
	}
	return calls
}

func newToolStreamSieve(available map[string]struct{}) *toolStreamSieve {
	return &toolStreamSieve{available: available}
}

func (s *toolStreamSieve) Feed(delta string) toolStreamResult {
	if s.done {
		return toolStreamResult{Complete: true}
	}
	s.buffer += delta
	if !s.capturing && toolSyntaxPattern.MatchString(s.buffer) {
		s.capturing = true
	}
	if !s.capturing {
		out := s.buffer
		s.buffer = ""
		return toolStreamResult{SafeText: out}
	}
	if strings.Contains(s.buffer, "</tool_calls>") || strings.Contains(s.buffer, "</tool_call>") {
		calls := parseToolCalls(s.buffer, s.available)
		s.done = true
		if len(calls) == 0 {
			return toolStreamResult{Complete: true, Raw: s.buffer}
		}
		return toolStreamResult{Complete: true, Calls: calls}
	}
	return toolStreamResult{}
}

func (s *toolStreamSieve) Flush() toolStreamResult {
	if s.done || s.buffer == "" {
		return toolStreamResult{Complete: true}
	}
	calls := parseToolCalls(s.buffer, s.available)
	s.done = true
	if len(calls) == 0 {
		return toolStreamResult{Complete: true, Raw: s.buffer, SafeText: s.buffer}
	}
	return toolStreamResult{Complete: true, Calls: calls}
}
