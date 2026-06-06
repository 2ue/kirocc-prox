package admin

import (
	"net/http"
	"os"
)

// optimizationFix describes one of the 8 fork-only fixes kirocc layers on
// top of the upstream protocol. They are intrinsic to this proxy and not
// runtime-toggleable; the UI shows them so operators understand what
// kirocc actually does for them.
type optimizationFix struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	Detail      string `json:"detail"`
	Status      string `json:"status"` // "always_on" | "env"
	EnvVar      string `json:"env_var,omitempty"`
	EnvValue    string `json:"env_value,omitempty"`
	DefaultHint string `json:"default_hint,omitempty"`
}

// optimizationKnob is a runtime-tweakable parameter currently exposed
// only via environment variable. The UI displays the effective value
// (read-only) so operators can see what's active without ssh'ing in.
type optimizationKnob struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Detail   string `json:"detail"`
	EnvVar   string `json:"env_var"`
	Value    string `json:"value"`
	Default  string `json:"default,omitempty"`
}

type optimizationsResponse struct {
	Fixes []optimizationFix  `json:"fixes"`
	Knobs []optimizationKnob `json:"knobs"`
}

// handleOptimizations returns the 8 kirocc fork fixes + the current
// effective values for the experimental / thinking knobs. The
// "effective" view is: env var if set, else the persisted PostgreSQL settings
// value (which is loaded into env at startup via Optimizations.ApplyToEnv).
func (s *Server) handleOptimizations(w http.ResponseWriter, _ *http.Request) {
	resp := optimizationsResponse{
		Fixes: forkFixes(),
		Knobs: optimizationKnobs(),
	}
	writeJSON(w, http.StatusOK, resp)
}

func forkFixes() []optimizationFix {
	budget := os.Getenv("KIROCC_FORCE_THINKING_BUDGET")
	return []optimizationFix{
		{
			ID:     1,
			Title:  "MCP system-reminder 清理",
			Detail: "剥离历史 MCP system-reminder 大段块。测试样本上 prompt token 约省 14%。",
			Status: "always_on",
		},
		{
			ID:     2,
			Title:  "保留非 MCP 系统提醒",
			Detail: "currentDate、用户偏好等正常 system-reminder 保留，仅清理 MCP 工具说明。",
			Status: "always_on",
		},
		{
			ID:     3,
			Title:  "tool 输入 required 字段校验",
			Detail: "Write {} / Edit {} 这类缺字段调用在代理层就被拦下，不穿透到 Claude Code。",
			Status: "always_on",
		},
		{
			ID:     4,
			Title:  "tool 输入顶层类型校验",
			Detail: "AskUserQuestion.questions=<string> 之类的类型错乱被代理拦截。",
			Status: "always_on",
		},
		{
			ID:     5,
			Title:  "非法 tool_use 自动回灌",
			Detail: "无效 tool_use 块被转换成 tool_result(is_error=true)，让模型本轮有一次自愈机会。",
			Status: "always_on",
		},
		{
			ID:     6,
			Title:  "二次非法降级为可见错误",
			Detail: "有界重试，避免无限循环导致 502 死锁。",
			Status: "always_on",
		},
		{
			ID:     7,
			Title:  "ToolSearch 错误隔离",
			Detail: "单个工具坏掉返回 tool_search_tool_result_error，不再 HTTP 400 杀整轮。",
			Status: "always_on",
		},
		{
			ID:          8,
			Title:       "代理层 thinking budget 兜底",
			Detail:      "KIROCC_FORCE_THINKING_BUDGET 设置后，代理无条件注入该下限到上游请求。客户端的 MAX_THINKING_TOKENS 不一定能传上去，此项作为最终保障。",
			Status:      "env",
			EnvVar:      "KIROCC_FORCE_THINKING_BUDGET",
			EnvValue:    budget,
			DefaultHint: "未设置 = 不强制注入",
		},
	}
}

func optimizationKnobs() []optimizationKnob {
	return []optimizationKnob{
		{
			Key:    "thinking_prefix_mode",
			Label:  "Thinking prompt 注入模式",
			Detail: "控制是否给上游请求注入 thinking 提示前缀。tool = 使用 thinking 工具；minimal = 精简提示；空 = 默认 effort-gated 注入。",
			EnvVar: "KIROCC_EXPERIMENT_THINKING_PROMPT",
			Value:  os.Getenv("KIROCC_EXPERIMENT_THINKING_PROMPT"),
		},
		{
			Key:    "thinking_tool_continue",
			Label:  "Thinking tool 续写模式",
			Detail: "实验性。assistant_only = 让 thinking tool 的 follow-up 仅走 assistant role。",
			EnvVar: "KIROCC_EXPERIMENT_THINKING_TOOL_CONTINUE",
			Value:  os.Getenv("KIROCC_EXPERIMENT_THINKING_TOOL_CONTINUE"),
		},
		{
			Key:    "system_mode",
			Label:  "System 消息模式",
			Detail: "实验性。改写 system 消息的传递方式。",
			EnvVar: "KIROCC_EXPERIMENT_SYSTEM_MODE",
			Value:  os.Getenv("KIROCC_EXPERIMENT_SYSTEM_MODE"),
		},
		{
			Key:    "upstream_origin",
			Label:  "上游 Origin 头",
			Detail: "覆盖发送给 Kiro 的 Origin。空 = 不修改。",
			EnvVar: "KIROCC_UPSTREAM_ORIGIN",
			Value:  os.Getenv("KIROCC_UPSTREAM_ORIGIN"),
		},
		{
			Key:    "model_mappings",
			Label:  "模型映射覆盖",
			Detail: "JSON 形式覆盖 Anthropic 模型名 → Kiro 模型 ID 映射。",
			EnvVar: "KIROCC_MODEL_MAPPINGS",
			Value:  os.Getenv("KIROCC_MODEL_MAPPINGS"),
		},
		{
			Key:    "audit_log",
			Label:  "审计日志文件",
			Detail: "设置后将上游请求/响应完整体写入该路径。仅 debug 用，会大幅增加磁盘 I/O。",
			EnvVar: "KIROCC_AUDIT_LOG",
			Value:  os.Getenv("KIROCC_AUDIT_LOG"),
		},
	}
}
