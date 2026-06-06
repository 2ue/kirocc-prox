package messages

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/niuma/kirocc-pro/internal/anthropic"
	"github.com/niuma/kirocc-pro/internal/auth"
	"github.com/niuma/kirocc-pro/internal/authctx"
	"github.com/niuma/kirocc-pro/internal/dashboard"
	"github.com/niuma/kirocc-pro/internal/httpx"
	"github.com/niuma/kirocc-pro/internal/logging"
	"github.com/niuma/kirocc-pro/internal/models"
	"github.com/niuma/kirocc-pro/internal/pool"
	"github.com/niuma/kirocc-pro/internal/promptcache"
	"github.com/niuma/kirocc-pro/internal/reqconv"
	"github.com/niuma/kirocc-pro/internal/toolsearch"
	"github.com/niuma/kirocc-pro/internal/usage"
)

const headerCCSessionID = "X-Claude-Code-Session-Id"

func (s *Service) HandleMessages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	traceID, short := logging.TraceIDs(ctx)
	start := time.Now()
	requestPath := r.URL.Path
	reportProfile := s.promptCacheReportProfile(requestPath)
	device := summarizeUserAgent(r.Header.Get("User-Agent"))
	deviceID := authctx.DeviceIDFrom(r.Context())
	apiKeyID := authctx.APIKeyIDFrom(r.Context())
	reqType := ""

	publishUsage := func(rec usage.Record) {
		if s.aggregator == nil {
			return
		}
		if rec.Timestamp.IsZero() {
			rec.Timestamp = start
		}
		if rec.Provider == "" {
			rec.Provider = "kiro"
		}
		if rec.RequestPath == "" {
			rec.RequestPath = requestPath
		}
		if reportProfile != nil {
			if rec.PromptCacheProfile == "" {
				rec.PromptCacheProfile = reportProfile.Name
			}
			if rec.PromptCachePrefix == "" {
				rec.PromptCachePrefix = reportProfile.Prefix
			}
		}
		if rec.Status == "" {
			rec.Status = usage.StatusSuccess
		}
		if rec.LatencyMs == 0 {
			rec.LatencyMs = int(time.Since(start).Milliseconds())
		}
		if rec.TraceID == "" {
			rec.TraceID = traceID
		}
		if rec.Type == "" {
			rec.Type = reqType
		}
		if rec.Device == "" {
			rec.Device = device
		}
		if rec.DeviceID == "" {
			rec.DeviceID = deviceID
		}
		if rec.APIKeyID == "" {
			rec.APIKeyID = apiKeyID
		}
		if rec.RawInputTokens == 0 && rec.RawOutputTokens == 0 && rec.RawCacheReadTokens == 0 && rec.RawCacheWriteTokens == 0 {
			rec.RawInputTokens = rec.InputTokens
			rec.RawOutputTokens = rec.OutputTokens
			rec.RawCacheReadTokens = rec.CacheReadTokens
			rec.RawCacheWriteTokens = rec.CacheWriteTokens
		}
		s.aggregator.Publish(rec)
	}

	req, err := parseAndValidateRequest(ctx, w, r)
	if err != nil {
		slog.WarnContext(ctx, "invalid request", "trace_id", short, "err", err)
		httpx.WriteError(w, http.StatusBadRequest, errTypeInvalidRequest, err.Error())
		publishUsage(usage.Record{
			Status:       usage.StatusInvalidRequest,
			ErrorMessage: err.Error(),
		})
		return
	}

	// [fork] Modified from upstream d-kuro/kirocc: synthesize a session id from
	// the trace id when the client omits X-Claude-Code-Session-Id, instead of
	// rejecting with 400. Real Claude Code still sends its own header; this
	// only unblocks generic Anthropic SDK clients and health probes.
	ccSessionID := r.Header.Get(headerCCSessionID)
	if ccSessionID == "" {
		ccSessionID = traceID
	}
	ctx = logging.WithSessionID(ctx, ccSessionID)
	r = r.WithContext(ctx)

	slog.DebugContext(ctx, "client request headers",
		"trace_id", traceID,
		"session_id", ccSessionID,
		"headers", logging.SafeHeaders{H: r.Header},
	)
	if reportProfile != nil {
		slog.DebugContext(ctx, "prompt cache report profile matched",
			"trace_id", short,
			"path", requestPath,
			"profile", reportProfile.Name,
			"prefix", reportProfile.Prefix,
		)
	}

	if before, after, ok := trimWebFetchRequest(req); ok {
		slog.InfoContext(ctx, "web fetch content trimmed", "trace_id", short, "before_chars", before, "after_chars", after)
	}

	kiroModel, thinking, contextWindowSize, anthropicModel := models.Resolve(req.Model, anthropic.HasContext1MBeta(r.Header))
	if req.IsThinkingEnabled() {
		thinking = true
	}

	// [fork] If a provider registry is wired and the routed provider's
	// Execute path is not implemented yet (currently anything other than
	// "kiro"), refuse early with a clear 503 instead of forwarding the
	// request through the Kiro-specific client.
	if s.registry != nil {
		if p := s.registry.RouteFor(req.Model); p != nil && p.ID() != "kiro" {
			msg := "provider " + p.ID() + " execute path is not implemented yet (Milestone III.2). Use claude-* model names to route to Kiro."
			httpx.WriteError(w, http.StatusServiceUnavailable, errTypeAPI,
				msg)
			publishUsage(usage.Record{
				RequestedModel: req.Model,
				ResolvedModel:  kiroModel,
				Status:         usage.StatusUpstreamError,
				ErrorMessage:   msg,
			})
			return
		}
	}

	s.logRequest(ctx, short, ccSessionID, kiroModel, contextWindowSize, req, thinking)

	// [fork] Acquire an upstream credential through the PostgreSQL-backed
	// pool with Redis-coordinated runtime state and session affinity.
	if s.regionHinter != nil {
		if hint := s.regionHinter(r); hint != "" {
			ctx = pool.WithRegionHint(ctx, hint)
		}
	}
	cred, err := s.conductor.Acquire(ctx, kiroModel, ccSessionID)
	if err != nil {
		if errors.Is(err, pool.ErrNoReady) || errors.Is(err, pool.ErrNoCredential) {
			slog.WarnContext(ctx, "credential pool exhausted", "trace_id", short, "err", err)
			httpx.WriteError(w, http.StatusServiceUnavailable, errTypeAPI, "no ready credential capacity")
			publishUsage(usage.Record{
				RequestedModel: req.Model,
				ResolvedModel:  kiroModel,
				Status:         usage.StatusRateLimited,
				ErrorMessage:   err.Error(),
			})
			return
		}
		slog.ErrorContext(ctx, "auth error", "trace_id", short, "err", err)
		httpx.WriteError(w, http.StatusUnauthorized, ErrTypeAuthentication, "authentication failed")
		publishUsage(usage.Record{
			RequestedModel: req.Model,
			ResolvedModel:  kiroModel,
			Status:         usage.StatusAuthError,
			ErrorMessage:   err.Error(),
		})
		return
	}
	defer s.conductor.Release(cred, kiroModel)
	creds := &cred.Credentials
	credID := cred.ID

	// [fork] Capture context for the per-request history record.
	reqType = "non-stream"
	if req.Stream {
		reqType = "stream"
	}

	// Metrics recorder: called at the end of each execution path. Also
	// updates pool scheduler counters and publishes a usage.Record so the
	// admin dashboard reflects per-credential, per-model traffic.
	recordMetric := func(mw *metricsResponseWriter, retryCount int, err error, statusOverride ...string) {
		if mw == nil {
			mw = newMetricsResponseWriter(w, start)
		}
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		status := "ok"
		if err != nil {
			status = "error"
		} else if retryCount > 0 {
			status = "retry"
		}
		usageStatus := usageStatusFor(err)
		if len(statusOverride) > 0 && statusOverride[0] != "" {
			usageStatus = statusOverride[0]
		}
		if usageStatus != usage.StatusInvalidRequest {
			s.markPoolOutcome(credID, kiroModel, err, mw, time.Since(start))
		}
		used, total := credCreditsSnapshot(cred)
		publishUsage(usage.Record{
			CredentialID:         credID,
			RequestedModel:       req.Model,
			ResolvedModel:        kiroModel,
			InputTokens:          mw.inputTokens,
			OutputTokens:         mw.outputTokens,
			CacheReadTokens:      mw.cacheReadTokens,
			CacheWriteTokens:     mw.cacheWriteTokens,
			RawInputTokens:       mw.rawInputTokens,
			RawOutputTokens:      mw.rawOutputTokens,
			RawCacheReadTokens:   mw.rawCacheReadTokens,
			RawCacheWriteTokens:  mw.rawCacheWriteTokens,
			Status:               usageStatus,
			FirstTokenMs:         mw.firstTokenMs,
			ErrorMessage:         errMsg,
			CreditsUsedSnapshot:  used,
			CreditsTotalSnapshot: total,
		})
		// Bump per-key token counter for quota enforcement on the next call.
		if err == nil && apiKeyID != "" && s.apiKeyUsageRecorder != nil {
			tokens := int64(mw.inputTokens + mw.outputTokens)
			if tokens > 0 {
				if err := s.apiKeyUsageRecorder(apiKeyID, tokens); err != nil {
					slog.WarnContext(ctx, "api key usage bump failed", "id", apiKeyID, "err", err)
				}
			}
		}
		if s.collector == nil {
			return
		}
		s.collector.Record(dashboard.RequestRecord{
			ID:           logging.NewTraceID(),
			Time:         start,
			TraceID:      traceID,
			SessionID:    ccSessionID,
			Model:        kiroModel,
			Stream:       req.Stream,
			Thinking:     thinking,
			InputTokens:  mw.inputTokens,
			OutputTokens: mw.outputTokens,
			ContextPct:   mw.contextPct,
			LatencyMs:    time.Since(start).Milliseconds(),
			FirstTokenMs: int64(mw.firstTokenMs),
			RetryCount:   retryCount,
			Status:       status,
			ErrorMsg:     errMsg,
		})
	}

	if hasWebSearchServerTool(req) {
		mw := newMetricsResponseWriter(w, start)
		s.handleLocalWebSearch(ctx, mw, req, anthropicModel, contextWindowSize, ccSessionID, short)
		recordMetric(mw, 0, nil)
		return
	}

	thinkingBudget := resolveThinkingBudget(ctx, req)

	// Tool search short-circuits to the orchestrator, which has its own retry loop.
	if tsCtx := toolsearch.NewContext(req.Tools); tsCtx != nil {
		refs := reqconv.ExtractToolReferences(req.Messages)
		tsCtx.PromoteTools(refs)
		slog.InfoContext(ctx, "tool search enabled",
			"trace_id", short,
			"search_type", tsCtx.SearchType,
			"deferred_tools", len(tsCtx.DeferredTools),
			"active_tools", len(tsCtx.ActiveTools),
		)
		mw := newMetricsResponseWriter(w, start)
		out := s.runToolSearch(ctx, mw, req, creds, credID, tsCtx, kiroModel, anthropicModel, contextWindowSize, thinking, thinkingBudget, ccSessionID, short, reportProfile)
		recordMetric(mw, out.RetryCount, out.TerminalErr)
		return
	}

	payload, nameMap, err := reqconv.BuildPayload(req, reqconv.BuildOptions{
		ProfileARN:     creds.ProfileARN,
		ModelID:        kiroModel,
		ConversationID: ccSessionID,
		Thinking:       thinking,
		ThinkingBudget: thinkingBudget,
		ThinkingEffort: req.Effort(),
	})
	if err != nil {
		slog.WarnContext(ctx, "payload build error", "trace_id", short, "err", err)
		httpx.WriteError(w, http.StatusBadRequest, errTypeInvalidRequest, err.Error())
		recordMetric(newMetricsResponseWriter(w, start), 0, err, usage.StatusInvalidRequest)
		return
	}

	if thinking && os.Getenv("KIROCC_EXPERIMENT_THINKING_PROMPT") == "tool" {
		mw := newMetricsResponseWriter(w, start)
		out := s.runThinkingTool(ctx, mw, req, creds, credID, kiroModel, anthropicModel, contextWindowSize, thinkingBudget, ccSessionID, short, reportProfile)
		recordMetric(mw, out.RetryCount, out.TerminalErr)
		return
	}

	mw := newMetricsResponseWriter(w, start)
	inv := &invocation{
		req:               req,
		payload:           payload,
		creds:             creds,
		credID:            credID,
		conversationID:    ccSessionID,
		model:             kiroModel,
		responseModel:     anthropicModel,
		contextWindowSize: contextWindowSize,
		thinking:          thinking,
		thinkingBudget:    thinkingBudget,
		toolNameMap:       nameMap.ReverseMap(),
		tools:             req.Tools,
		reportProfile:     reportProfile,
	}
	retryCount, terminalErr := s.executeWithRetry(ctx, mw, inv)
	recordMetric(mw, retryCount, terminalErr)
}

func (s *Service) promptCacheReportProfile(path string) *promptcache.MatchedProfile {
	cfg := s.currentPromptCacheReports()
	if cfg.Empty() {
		return nil
	}
	matched, ok := cfg.Match(path)
	if !ok {
		return nil
	}
	return &matched
}

func (s *Service) currentPromptCacheReports() promptcache.ReportConfig {
	if s.promptCacheReportProvider != nil {
		return s.promptCacheReportProvider().Normalized()
	}
	return s.promptCacheReports.Normalized()
}

// logRequest emits the "--> POST /v1/messages" info log summarizing the call.
func (s *Service) logRequest(ctx context.Context, short, ccSessionID, kiroModel string, contextWindowSize int, req *anthropic.Request, thinking bool) {
	var thinkingLog any = false
	if thinking {
		if effort := req.Effort(); effort != "" {
			thinkingLog = effort
		} else {
			thinkingLog = "enabled"
		}
	}
	slog.InfoContext(ctx, "--> POST /v1/messages",
		"trace_id", short,
		"session_id", logging.ShortID(ccSessionID),
		"model", kiroModel,
		"thinking", thinkingLog,
		"stream", req.Stream,
		"context_window", formatContextWindow(contextWindowSize),
	)
}

// runToolSearch wires up the orchestrator and retries once on empty-visible end_turn.
func (s *Service) runToolSearch(ctx context.Context, w http.ResponseWriter, req *anthropic.Request, creds *auth.Credentials, credID string, tsCtx *toolsearch.Context, kiroModel, responseModel string, contextWindowSize int, thinking bool, thinkingBudget int, ccSessionID, short string, reportProfile *promptcache.MatchedProfile) retryOutcome {
	orch := &toolSearchOrchestrator{
		service: s,
		tsCtx:   tsCtx,
		req:     req,
		creds:   creds,
		buildOpts: reqconv.BuildOptions{
			ProfileARN:     creds.ProfileARN,
			ModelID:        kiroModel,
			ConversationID: ccSessionID,
			Thinking:       thinking,
			ThinkingBudget: thinkingBudget,
			ThinkingEffort: req.Effort(),
			ToolSearchCtx:  tsCtx,
		},
		contextWindowSize: contextWindowSize,
		responseModel:     responseModel,
		inv: &invocation{
			req:               req,
			creds:             creds,
			credID:            credID,
			conversationID:    ccSessionID,
			model:             kiroModel,
			responseModel:     responseModel,
			contextWindowSize: contextWindowSize,
			thinking:          thinking,
			thinkingBudget:    thinkingBudget,
			tools:             req.Tools,
			reportProfile:     reportProfile,
		},
	}
	out := orch.run(ctx, w)
	if out.Reason != retryReasonEmptyVisibleEndTurn {
		return out
	}
	slog.WarnContext(ctx, "retrying tool search after empty visible end_turn", "trace_id", short)
	out2 := orch.run(ctx, w)
	out2.RetryCount = 1
	if out2.Reason == retryReasonEmptyVisibleEndTurn {
		slog.ErrorContext(ctx, "tool search retry also returned empty visible end_turn", "trace_id", short)
		httpx.WriteError(w, http.StatusBadGateway, errTypeAPI, "upstream returned empty response")
		out2.Reason = ""
		out2.TerminalErr = errors.New("upstream returned empty response")
	}
	return out2
}
