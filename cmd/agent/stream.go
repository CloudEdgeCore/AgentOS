package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type StreamDelta struct {
	Content   string `json:"content"`
	Reasoning string `json:"reasoning"`
}

type StreamChoice struct {
	Delta StreamDelta `json:"delta"`
}

type StreamChunk struct {
	Choices []StreamChoice `json:"choices"`
}

func StreamLLM(cfg *AgentYAMLConfig, systemPrompt, userPrompt string) (int, error) {
	provider := cfg.CurrentProvider()
	if provider.APIKey == "" {
		return 0, fmt.Errorf("当前模型提供商 %q 未配置 API Key", cfg.LLM.DefaultProvider)
	}

	reqBody := map[string]any{
		"model": provider.Model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"stream": true,
	}

	bodyBytes, _ := json.Marshal(reqBody)
	endpoint := strings.TrimRight(provider.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return 0, err
	}

	req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HTTP-Referer", "https://agentos.dev")
	req.Header.Set("X-Title", "AgentOS-CLI")

	tr := &http.Transport{
		ResponseHeaderTimeout: 45 * time.Second,
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   0, // Long-lived streaming (避免深度思考超时)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("API 响应错误 (状态码 %d): %s", resp.StatusCode, string(b))
	}

	scanner := bufio.NewScanner(resp.Body)
	totalChars := 0
	inReasoning := false

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk StreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if len(chunk.Choices) > 0 {
			c := chunk.Choices[0]
			if c.Delta.Reasoning != "" {
				if !inReasoning {
					fmt.Print("\n\033[36m[深度思考推理链]: \033[0m")
					inReasoning = true
				}
				fmt.Print(c.Delta.Reasoning)
				_ = os.Stdout.Sync()
				totalChars += len(c.Delta.Reasoning)
			}
			if c.Delta.Content != "" {
				if inReasoning {
					fmt.Print("\n\n\033[32m[智能体结构化输出]:\033[0m\n")
					inReasoning = false
				}
				fmt.Print(c.Delta.Content)
				_ = os.Stdout.Sync()
				totalChars += len(c.Delta.Content)
			}
		}
	}

	fmt.Println()
	approxTokens := totalChars / 3
	if approxTokens < 100 {
		approxTokens = 150
	}
	return approxTokens, nil
}
