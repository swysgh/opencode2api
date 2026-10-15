package main

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// quartetTools 是上游免费档要求同时出现的四个小写工具名（实测：少一个、
// 只给大写变体、或换成别的名字，都会 403 FreeTierError）。
var quartetTools = []string{"bash", "glob", "grep", "read"}

// decoyToolDescription 让模型不要真的去调这些占位工具。
const decoyToolDescription = "This tool is currently unavailable and must not be used."

type bodyShape int

const (
	shapeChat bodyShape = iota
	shapeClaude
	shapeResponses
)

func (s bodyShape) String() string {
	switch s {
	case shapeClaude:
		return "claude"
	case shapeResponses:
		return "responses"
	default:
		return "openai"
	}
}

// detectShape 判断请求体的工具声明形状。拦截器拿到的是**客户端协议**的请求体
// （上游翻译发生在其后），所以先看 sourceFormat，再用 body 特征兜底。
func detectShape(body []byte, sourceFormat string) bodyShape {
	sf := strings.ToLower(strings.TrimSpace(sourceFormat))
	switch {
	case strings.Contains(sf, "claude"):
		return shapeClaude
	case strings.Contains(sf, "response"):
		return shapeResponses
	case strings.Contains(sf, "openai"):
		return shapeChat
	}
	if gjson.GetBytes(body, "input").Exists() {
		return shapeResponses
	}
	if gjson.GetBytes(body, "system").Exists() {
		return shapeClaude
	}
	return shapeChat
}

// toolNamePath 返回第 i 个工具名的 sjson 路径。
func (s bodyShape) toolNamePath(i int) string {
	if s == shapeChat {
		return fmt.Sprintf("tools.%d.function.name", i)
	}
	return fmt.Sprintf("tools.%d.name", i)
}

// toolName 读取第 i 个工具声明的名字（chat 在 tools[i].function.name，其余在 tools[i].name）。
func (s bodyShape) toolName(body []byte, i int) string {
	if s == shapeChat {
		return strings.TrimSpace(gjson.GetBytes(body, fmt.Sprintf("tools.%d.function.name", i)).String())
	}
	return strings.TrimSpace(gjson.GetBytes(body, fmt.Sprintf("tools.%d.name", i)).String())
}

// decoyTool 按形状构造一个占位工具声明。
func (s bodyShape) decoyTool(name string) map[string]any {
	params := map[string]any{"type": "object", "properties": map[string]any{}}
	switch s {
	case shapeChat:
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": decoyToolDescription,
				"parameters":  params,
			},
		}
	case shapeClaude:
		return map[string]any{
			"name":         name,
			"description":  decoyToolDescription,
			"input_schema": params,
		}
	default:
		return map[string]any{
			"type":        "function",
			"name":        name,
			"description": decoyToolDescription,
			"parameters":  params,
		}
	}
}

func isQuartet(name string) bool {
	for _, q := range quartetTools {
		if name == q {
			return true
		}
	}
	return false
}

type fingerprintOptions struct {
	InjectTools   bool
	ForceStream   bool
	ClientStreams bool
}

type fingerprintResult struct {
	Body []byte
	// Renamed 记录 发送名(小写) -> 客户端原本拼写，供响应侧还原。
	Renamed      map[string]string
	Injected     []string
	ForcedStream bool
	Changed      bool
}

// applyFingerprint 把请求体改造成上游免费档愿意接受的样子：
//  1. 工具四件套归一化为小写、去重、补齐缺失项（改过名的记进 Renamed）；
//  2. 客户端没声明工具时把 tool_choice 设为 none，避免模型去调占位工具；
//  3. 按需强制 stream:true。
func applyFingerprint(body []byte, sourceFormat string, opt fingerprintOptions) (fingerprintResult, error) {
	res := fingerprintResult{Body: body}
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return res, nil
	}
	shape := detectShape(body, sourceFormat)
	out := body

	if opt.InjectTools {
		clientTools := gjson.GetBytes(out, "tools").Array()
		hadClientTools := len(clientTools) > 0

		renamed := map[string]string{}
		seen := map[string]bool{}
		dupes := []int{}
		for i := 0; i < len(clientTools); i++ {
			name := shape.toolName(out, i)
			if name == "" {
				continue
			}
			key := strings.ToLower(name)
			if !isQuartet(key) {
				continue
			}
			if seen[key] {
				// 同一规范名的重复声明（例如客户端同时给了 Bash 和 bash）只留第一个。
				dupes = append(dupes, i)
				continue
			}
			seen[key] = true
			if name != key {
				updated, err := sjson.SetBytes(out, shape.toolNamePath(i), key)
				if err != nil {
					return res, fmt.Errorf("重写工具名 %q 失败: %w", name, err)
				}
				out = updated
				renamed[key] = name
			}
		}
		// 从后往前删，避免下标位移。
		for i := len(dupes) - 1; i >= 0; i-- {
			updated, err := sjson.DeleteBytes(out, fmt.Sprintf("tools.%d", dupes[i]))
			if err != nil {
				return res, fmt.Errorf("删除重复工具声明失败: %w", err)
			}
			out = updated
		}
		// 补齐缺失的四件套。
		for _, name := range quartetTools {
			if seen[name] {
				continue
			}
			idx := len(gjson.GetBytes(out, "tools").Array())
			updated, err := sjson.SetBytes(out, fmt.Sprintf("tools.%d", idx), shape.decoyTool(name))
			if err != nil {
				return res, fmt.Errorf("补齐工具 %q 失败: %w", name, err)
			}
			out = updated
			res.Injected = append(res.Injected, name)
		}
		// tool_choice：把指向被改名工具的显式选择改到小写名上。
		updated, changedChoice := retargetToolChoice(out, shape, renamed)
		if changedChoice {
			out = updated
		}
		// 客户端本来就没工具：占位工具不能被选中。
		if !hadClientTools && len(res.Injected) > 0 && shape == shapeChat {
			if v := gjson.GetBytes(out, "tool_choice"); !v.Exists() || v.String() != "none" {
				if updated, err := sjson.SetBytes(out, "tool_choice", "none"); err == nil {
					out = updated
				}
			}
		}
		res.Renamed = renamed
	}

	if opt.ForceStream && opt.ClientStreams {
		if v := gjson.GetBytes(out, "stream"); !v.Exists() || !v.Bool() {
			updated, err := sjson.SetBytes(out, "stream", true)
			if err != nil {
				return res, fmt.Errorf("设置 stream 失败: %w", err)
			}
			out = updated
			res.ForcedStream = true
		}
	}

	if len(res.Renamed) > 0 || len(res.Injected) > 0 || res.ForcedStream {
		res.Body = out
		res.Changed = true
	}
	return res, nil
}

// retargetToolChoice 把 tool_choice 里指向被改名工具的原始拼写改成小写名。
func retargetToolChoice(body []byte, shape bodyShape, renamed map[string]string) ([]byte, bool) {
	if len(renamed) == 0 {
		return body, false
	}
	choice := gjson.GetBytes(body, "tool_choice")
	if !choice.IsObject() {
		return body, false
	}
	namePath := "tool_choice.name"
	if shape == shapeChat {
		namePath = "tool_choice.function.name"
	}
	current := strings.TrimSpace(gjson.GetBytes(body, namePath).String())
	if current == "" {
		return body, false
	}
	key := strings.ToLower(current)
	if _, ok := renamed[key]; !ok {
		return body, false
	}
	updated, err := sjson.SetBytes(body, namePath, key)
	if err != nil {
		return body, false
	}
	return updated, true
}
