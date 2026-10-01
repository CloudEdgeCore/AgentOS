package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func ScaffoldAgent(name string) error {
	targetDir := name
	if err := os.MkdirAll(filepath.Join(targetDir, "tools"), 0755); err != nil {
		return err
	}

	// 1. 生成 agent.manifest.json
	manifest := map[string]any{
		"apiVersion": "agentos.dev/v1",
		"kind":       "AgentManifest",
		"metadata": map[string]string{
			"name":      name,
			"version":   "1.0.0",
			"namespace": "default",
		},
		"spec": map[string]any{
			"capabilities": map[string]any{
				"tools": []string{
					fmt.Sprintf("%s.action.query@1.0.0", name),
				},
				"models": []string{
					"stealth/space-bunny-alpha",
				},
			},
			"budget": map[string]any{
				"tokens":    20000,
				"costUsd":   1.00,
				"toolCalls": 10,
			},
			"systemPromptRef": "prompt.md",
		},
	}
	mBytes, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(targetDir, "agent.manifest.json"), mBytes, 0644); err != nil {
		return err
	}

	// 2. 生成 prompt.md (系统提示词模板)
	promptContent := fmt.Sprintf(`# %s 智能体系统提示词 (System Prompt)

你是由 AgentOS 治理的企业级专业 AI Agent，专职负责业务问题排查与数据分析。

## 角色与职责 (Role & Responsibilities)
- 严谨、专业、遵循工程客观事实。
- 优先调用注册的工具获取最新实时数据，不主观假设。

## 核心红线约束 (Safety Redlines)
1. 严禁幻觉：没有工具返回证据支持的字段，必须明确标注“缺少直接证据”。
2. 安全防呆：涉及高危操作必须给出明确预警和复核步骤。

## 输出格式规范 (Output Schema)
请严格按以下结构分节输出：
1. 【问题概述】
2. 【数据范围与基线】
3. 【发现的量化异常】
4. 【候选根因与置信度】
5. 【建议排查顺序】
6. 【数据与知识来源】
`, name)
	if err := os.WriteFile(filepath.Join(targetDir, "prompt.md"), []byte(promptContent), 0644); err != nil {
		return err
	}

	// 3. 生成项目级 agent.yaml (分级配置)
	agentYamlContent := fmt.Sprintf(`version: "1.0"
environment: "development"

llm:
  default_provider: "openrouter"
  providers:
    openrouter:
      model: "stealth/space-bunny-alpha"
      base_url: "https://openrouter.ai/api/v1"
      timeout_sec: 180

kernel:
  budget:
    max_cost_usd: 1.00
    max_tokens: 30000
    max_tool_calls: 15

gateway:
  port: 18080
  host: "127.0.0.1"
`)
	if err := os.WriteFile(filepath.Join(targetDir, "agent.yaml"), []byte(agentYamlContent), 0644); err != nil {
		return err
	}

	// 4. 生成 tools/main.go (示例工具代码)
	toolContent := fmt.Sprintf(`package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// 示例自定义工具服务
func main() {
	http.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"output": map[string]any{
				"status": "OK",
				"metric": "sample_telemetry",
				"value":  42.0,
			},
			"receiptId": fmt.Sprintf("rcpt_%%x", time.Now().UnixNano()),
		}
		json.NewEncoder(w).Encode(resp)
	})
	fmt.Println("[%s Tool Server] listening on :18080...")
	_ = http.ListenAndServe(":18080", nil)
}
`, name)
	if err := os.WriteFile(filepath.Join(targetDir, "tools", "main.go"), []byte(toolContent), 0644); err != nil {
		return err
	}

	// 5. 生成 README.md
	readmeContent := fmt.Sprintf(`# %s Agent 工程目录

本工程由 AgentOS CLI (agent init) 自动生成。

## 目录结构
- agent.yaml         : 项目级分级配置文件 (可覆盖全局配置)
- agent.manifest.json: 声明工具权限、模型和预算配额
- prompt.md          : 业务系统提示词与 PRD 输出规范
- tools/             : 自定义工具微服务源码

## 运行方式
$ agent run %s/agent.manifest.json
`, name, name)
	return os.WriteFile(filepath.Join(targetDir, "README.md"), []byte(readmeContent), 0644)
}
