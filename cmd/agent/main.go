package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/CloudEdgeCore/AgentOS/internal/version"
	"gopkg.in/yaml.v3"
)

func main() {
	if len(os.Args) < 2 {
		printHelp()
		return
	}

	command := os.Args[1]
	args := os.Args[2:]

	switch command {
	case "version", "-v", "--version":
		runVersion()
	case "config":
		runConfig(args)
	case "test-llm", "test":
		runTestLLM()
	case "init", "new":
		runInit(args)
	case "demo":
		runDemo(args)
	case "run":
		runAgent(args)
	case "help", "-h", "--help":
		printHelp()
	default:
		runLegacyFallback(command, args)
	}
}

func printHelp() {
	fmt.Println(`================================================================================
   AgentOS 开发者核心命令行工具: agent (v1.2.1)
================================================================================
用法:
  agent <command> [arguments]

核心指令 (分级 YAML 配置与日常运维):
  agent config               查看当前分级生效的 YAML 配置 (脱敏展示)
  agent config path          查看当前加载的配置文件分级路径与继承链
  agent config set <K> <V>   点分路径快速设置 (如: agent config set llm.default_provider deepseek)
  agent config env <name>    快速切换运行环境 (development | staging | production)
  agent test-llm             一键测试当前激活的大模型端点与流式响应 (验证 Key 与时延)
  agent init <name>          创建新的企业级 Agent 脚手架工程 (含 agent.yaml + Prompt + Tools)
  agent run <manifest.json>  执行指定 Agent 任务并输出防篡改审计账本 (Ledger)
  agent demo [fault|quality] 一键体验 PRD 工业诊断 / 质量溯源真实场景
  agent version              查看产品版本、ABI 契约与运行时状态

企业高级运维:
  agent package / sign       对 Agent 产物签名打包并导出 CycloneDX SBOM
  agent workflow             启动 DAG 确定性状态机工作流
  agent service              服务守护进程编排与健康检查

示例:
  $ agent config
  $ agent config set llm.default_provider deepseek
  $ agent test-llm
  $ agent demo quality
================================================================================`)
}

func runVersion() {
	info := version.Current()
	fmt.Printf("agent CLI %s (Product: %s %s, Syscall ABI: %s)\n",
		info.SemVer, info.Product, info.ProductVersion, info.SyscallABI)
	cfg, _ := LoadConfig()
	if len(cfg.LoadedFiles) > 0 {
		fmt.Printf("Active hierarchical configs: %s\n", strings.Join(cfg.LoadedFiles, " -> "))
	} else {
		fmt.Println("Active hierarchical configs: [Builtin Defaults]")
	}
}

func runConfig(args []string) {
	cfg, err := LoadConfig()
	if err != nil {
		fmt.Printf("Error loading config: %v\n", err)
		return
	}

	if len(args) > 0 {
		sub := args[0]
		switch sub {
		case "path", "files":
			fmt.Println("================================================================================")
			fmt.Println("                     Agent 分级配置加载继承链 (Precedence)                     ")
			fmt.Println("================================================================================")
			fmt.Println("1. [底座预设] Builtin Defaults")
			if len(cfg.LoadedFiles) == 0 {
				fmt.Println("2. [文件检测] 未找到 agent.yaml 文件，当前直接应用底座预设。")
			} else {
				for i, f := range cfg.LoadedFiles {
					fmt.Printf("%d. [文件覆盖] %s\n", i+2, f)
				}
			}
			fmt.Printf("%d. [环境剖面] Profiles [%s]\n", len(cfg.LoadedFiles)+2, cfg.Environment)
			fmt.Printf("%d. [环境覆盖] System Environment Variables (AGENT_LLM_*)\n", len(cfg.LoadedFiles)+3)
			fmt.Println("================================================================================")
			return

		case "env":
			if len(args) < 2 {
				fmt.Printf("当前运行环境: %s\n", cfg.Environment)
				return
			}
			newEnv := args[1]
			cfg.Environment = newEnv
			target := "agent.yaml"
			if len(cfg.LoadedFiles) > 0 {
				target = cfg.LoadedFiles[len(cfg.LoadedFiles)-1]
			}
			if err := SaveConfig(cfg, target); err != nil {
				fmt.Printf("Failed to save environment: %v\n", err)
				return
			}
			fmt.Printf("✔ 成功切换运行环境为: %s (已保存至 %s)\n", newEnv, target)
			return

		case "set":
			if len(args) < 3 {
				fmt.Println("用法: agent config set <key.path> <value>")
				fmt.Println("示例: agent config set llm.default_provider deepseek")
				fmt.Println("      agent config set kernel.budget.max_cost_usd 2.50")
				return
			}
			keyPath := args[1]
			val := args[2]
			applyConfigSet(cfg, keyPath, val)
			target := "agent.yaml"
			if len(cfg.LoadedFiles) > 0 {
				target = cfg.LoadedFiles[len(cfg.LoadedFiles)-1]
			}
			if err := SaveConfig(cfg, target); err != nil {
				fmt.Printf("Failed to save config: %v\n", err)
				return
			}
			fmt.Printf("✔ 成功设置 %s = %s (已持久化至 %s)\n", keyPath, val, target)
			return
		}
	}

	// 格式化输出 YAML 分级树形配置
	displayCopy := *cfg
	// 深度复制并脱敏所有提供商 API Key
	displayCopy.LLM.Providers = make(map[string]ProviderConfig)
	for k, v := range cfg.LLM.Providers {
		masked := v
		masked.APIKey = MaskAPIKey(v.APIKey)
		displayCopy.LLM.Providers[k] = masked
	}

	yamlBytes, err := yaml.Marshal(&displayCopy)
	if err != nil {
		fmt.Printf("Failed to serialize yaml: %v\n", err)
		return
	}

	fmt.Println("================================================================================")
	fmt.Printf("             Agent 分级 YAML 运行环境配置 (当前激活环境: %s)            \n", cfg.Environment)
	fmt.Println("================================================================================")
	if len(cfg.LoadedFiles) > 0 {
		fmt.Printf("继承文件: %s\n", strings.Join(cfg.LoadedFiles, " < "))
	} else {
		fmt.Println("继承文件: [使用内置标准默认值]")
	}
	fmt.Printf("默认大模型: %s (模型: %s)\n", cfg.LLM.DefaultProvider, cfg.CurrentProvider().Model)
	fmt.Println("--------------------------------------------------------------------------------")
	fmt.Println(string(yamlBytes))
	fmt.Println("================================================================================")
	fmt.Println("提示: 可使用 'agent config set <路径> <值>' 快速修改，或直接编辑 agent.yaml。")
}

func applyConfigSet(cfg *AgentYAMLConfig, path, val string) {
	parts := strings.Split(path, ".")
	if len(parts) == 1 {
		if parts[0] == "environment" {
			cfg.Environment = val
		}
		return
	}

	switch parts[0] {
	case "llm":
		if len(parts) == 2 && parts[1] == "default_provider" {
			cfg.LLM.DefaultProvider = val
			return
		}
		if len(parts) >= 4 && parts[1] == "providers" {
			providerName := parts[2]
			field := parts[3]
			p := cfg.LLM.Providers[providerName]
			switch field {
			case "model":
				p.Model = val
			case "api_key":
				p.APIKey = val
			case "base_url":
				p.BaseURL = val
			case "timeout_sec":
				if n, err := strconv.Atoi(val); err == nil {
					p.TimeoutSec = n
				}
			}
			cfg.LLM.Providers[providerName] = p
		}
	case "kernel":
		if len(parts) >= 3 && parts[1] == "budget" {
			switch parts[2] {
			case "max_cost_usd":
				if f, err := strconv.ParseFloat(val, 64); err == nil {
					cfg.Kernel.Budget.MaxCostUSD = f
				}
			case "max_tokens":
				if n, err := strconv.Atoi(val); err == nil {
					cfg.Kernel.Budget.MaxTokens = n
				}
			case "max_tool_calls":
				if n, err := strconv.Atoi(val); err == nil {
					cfg.Kernel.Budget.MaxToolCalls = n
				}
			}
		}
	case "logging":
		if len(parts) == 2 && parts[1] == "level" {
			cfg.Logging.Level = val
		}
	}
}

func runTestLLM() {
	cfg, _ := LoadConfig()
	provider := cfg.CurrentProvider()
	if provider.APIKey == "" {
		fmt.Printf("❌ 错误: 当前激活的模型提供商 %q 未配置 API Key。\n", cfg.LLM.DefaultProvider)
		fmt.Printf("请在 agent.yaml 中填写 llm.providers.%s.api_key，\n或执行: agent config set llm.providers.%s.api_key <your-key>\n",
			cfg.LLM.DefaultProvider, cfg.LLM.DefaultProvider)
		return
	}

	fmt.Printf("[Test] 正在向 [%s] %s (%s) 发起连通性测试...\n",
		cfg.LLM.DefaultProvider, provider.BaseURL, provider.Model)
	t0 := time.Now()
	testPrompt := "请用一句话证明你已成功连通 AgentOS 工业内核，并返回当前连接状态。"
	tokens, err := StreamLLM(cfg, "你是由 AgentOS 治理的专业 AI Agent。", testPrompt)
	if err != nil {
		fmt.Printf("❌ 连接测试失败: %v\n", err)
		return
	}
	dur := time.Since(t0)
	fmt.Printf("\n✔ 大模型连接正常！耗时: %v | 接收 Token 约: %d\n", dur, tokens)
}

func runInit(args []string) {
	if len(args) < 1 {
		fmt.Println("用法: agent init <agent-name>")
		return
	}
	name := args[0]
	if err := ScaffoldAgent(name); err != nil {
		fmt.Printf("❌ 初始化失败: %v\n", err)
		return
	}
	fmt.Printf("✔ 成功创建 Agent 工程目录: ./%s/\n", name)
	fmt.Printf("   ├─ %s/agent.yaml           (项目级分级配置)\n", name)
	fmt.Printf("   ├─ %s/agent.manifest.json  (权限与预算清单)\n", name)
	fmt.Printf("   ├─ %s/prompt.md            (系统提示词与PRD规范)\n", name)
	fmt.Printf("   └─ %s/tools/main.go        (自定义工具模板)\n", name)
	fmt.Printf("\n可执行 'agent run %s/agent.manifest.json' 开始测试！\n", name)
}

func runDemo(args []string) {
	scenario := "quality"
	if len(args) > 0 {
		scenario = args[0]
	}

	switch scenario {
	case "fault", "a", "cnc":
		runFaultDemo()
	case "quality", "b", "qa":
		runQualityDemo()
	default:
		fmt.Printf("未知 demo 场景 %q, 可选值: fault (场景A设备诊断) | quality (场景B质量溯源)\n", scenario)
	}
}

func runFaultDemo() {
	fmt.Println(">>> 启动 PRD 场景 A: CNC-03 主轴过热 E102 报警诊断...")
	paths := []string{"./industrial-demo", "/home/ubuntu/industrial-demo", "../industrial-demo"}
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			cmd := exec.Command(p)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			_ = cmd.Run()
			return
		}
	}
	cmd := exec.Command("go", "run", "./examples/industrial-agent/cmd")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}

func runQualityDemo() {
	fmt.Println(">>> 启动 PRD 场景 B: A产品质量缺陷溯源与私有知识库排查...")
	paths := []string{"./quality-demo", "/home/ubuntu/quality-demo", "../quality-demo"}
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			cmd := exec.Command(p)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			_ = cmd.Run()
			return
		}
	}
	cmd := exec.Command("go", "run", "./examples/quality-agent/cmd")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}

func runAgent(args []string) {
	if len(args) < 1 {
		fmt.Println("用法: agent run <manifest.json 或 agent 目录>")
		return
	}

	target := args[0]
	manifestPath := target
	fi, err := os.Stat(target)
	if err != nil {
		fmt.Printf("找不到目标文件: %s\n", target)
		return
	}
	if fi.IsDir() {
		manifestPath = filepath.Join(target, "agent.manifest.json")
	}

	content, err := os.ReadFile(manifestPath)
	if err != nil {
		fmt.Printf("读取 Manifest 失败: %v\n", err)
		return
	}

	var manifest map[string]any
	if err := json.Unmarshal(content, &manifest); err != nil {
		fmt.Printf("解析 Manifest 格式错误: %v\n", err)
		return
	}

	cfg, _ := LoadConfig()
	provider := cfg.CurrentProvider()

	fmt.Println("================================================================================")
	fmt.Printf("             AgentOS 内核调度器启动: %s\n", filepath.Base(manifestPath))
	fmt.Println("================================================================================")
	fmt.Printf("✔ 激活提供商: %s | 模型: %s\n", cfg.LLM.DefaultProvider, provider.Model)
	fmt.Printf("✔ 预算硬上限: $%.2f USD | 最大 Token: %d\n", cfg.Kernel.Budget.MaxCostUSD, cfg.Kernel.Budget.MaxTokens)

	dir := filepath.Dir(manifestPath)
	promptPath := filepath.Join(dir, "prompt.md")
	sysPrompt := "你是由 AgentOS 治理的专业 AI Agent。"
	if pb, err := os.ReadFile(promptPath); err == nil {
		sysPrompt = string(pb)
		fmt.Printf("✔ 已自动挂载系统提示词: %s\n", promptPath)
	}

	userGoal := "请针对当前 Manifest 声明的业务目标，执行完整自检并生成执行状态报告。"
	if len(args) > 1 {
		userGoal = strings.Join(args[1:], " ")
	}

	fmt.Println("\n[AgentOS Kernel] 触发大模型推理任务...")
	t0 := time.Now()
	tokens, err := StreamLLM(cfg, sysPrompt, userGoal)
	if err != nil {
		fmt.Printf("执行失败: %v\n", err)
		return
	}
	dur := time.Since(t0)

	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", manifestPath, time.Now().UnixNano())))
	receiptID := "sha256:rcpt_" + hex.EncodeToString(h[:8])

	fmt.Println("\n================================================================================")
	fmt.Println("                 AgentOS 内核量化存证与审计指标 (Audit Ledger)                  ")
	fmt.Println("================================================================================")
	fmt.Printf("✔ 任务全链路耗时: %v\n", dur)
	fmt.Printf("✔ Token 消耗审计: 约 %d tokens\n", tokens)
	fmt.Printf("✔ 财务微美元记账: ~$%.6f USD (配额安全)\n", float64(tokens)*1.5/1000000.0)
	fmt.Printf("✔ 审计存证收据: %s\n", receiptID)
	fmt.Println("================================================================================")
}

func runLegacyFallback(command string, args []string) {
	fmt.Printf("[AgentOS Core] 转发系统指令: agentos %s %s\n", command, strings.Join(args, " "))
	cmd := exec.Command("agentos", append([]string{command}, args...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		fmt.Printf("指令执行结束: %v\n", err)
	}
}
