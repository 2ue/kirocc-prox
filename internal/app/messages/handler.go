package messages

import (
	"context"
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
	"github.com/niuma/kirocc-pro/internal/reqconv"
	"github.com/niuma/kirocc-pro/internal/toolsearch"
	"github.com/niuma/kirocc-pro/internal/usage"
)

const headerCCSessionID = "X-Claude-Code-Session-Id"

func (s *Service) HandleMessages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	traceID, short := logging.TraceIDs(ctx)
	start := time.Now()

	req, err := parseAndValidateRequest(ctx, w, r)
	if err != nil {
		slog.WarnContext(ctx, "invalid request", "trace_id", short, "err", err)
		httpx.WriteError(w, http.StatusBadRequest, errTypeInvalidRequest, err.Error())
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
			httpx.WriteError(w, http.StatusServiceUnavailable, errTypeAPI,
				"provider "+p.ID()+" execute path is not implemented yet (Milestone III.2). Use claude-* model names to route to Kiro.")
			return
		}
	}

	s.logRequest(ctx, short, ccSessionID, kiroModel, contextWindowSize, req, thinking)

	// [fork] Acquire upstream credential through the pool. In single-account
	// mode this just refreshes the SQLite-backed token; in multi-account
	// mode it picks an unloaded credential (with session affinity).
	if s.regionHinter != nil {
		if hint := s.regionHinter(r); hint != "" {
			ctx = pool.WithRegionHint(ctx, hint)
		}
	}
	cred, err := s.conductor.Acquire(ctx, kiroModel, ccSessionID)
	if err != nil {
		slog.ErrorContext(ctx, "auth error", "trace_id", short, "err", err)
		httpx.WriteError(w, http.StatusUnauthorized, ErrTypeAuthentication, "authentication failed")
		if s.aggregator != nil {
			s.aggregator.Publish(usage.Record{
				Timestamp: time.Now(), Provider: "kiro", Status: usage.StatusAuthError,
				RequestedModel: req.Model, ResolvedModel: kiroModel, TraceID: traceID,
				LatencyMs: int(time.Since(start).Milliseconds()),
			})
		}
		return
	}
	defer s.conductor.Release(cred)
	creds := &cred.Credentials
	credID := cred.ID

	// [fork] Capture context for the per-request history record.
	device := summarizeUserAgent(r.Header.Get("User-Agent"))
	deviceID := authctx.DeviceIDFrom(r.Context())
	apiKeyID := authctx.APIKeyIDFrom(r.Context())
	reqType := "non-stream"
	if req.Stream {
		reqType = "stream"
	}

	// Metrics recorder: called at the end of each execution path. Also
	// updates pool scheduler counters and publishes a usage.Record so the
	// admin dashboard reflects per-credential, per-model traffic.
	recordMetric := func(mw *metricsResponseWriter, retryCount int, errMsg string) {
		status := "ok"
		if errMsg != "" {
			status = "error"
		} else if retryCount > 0 {
			status = "retry"
		}
		s.markPoolOutcome(credID, kiroModel, errMsg, mw, time.Since(start))
		if s.aggregator != nil {
			used, total := credCreditsSnapshot(cred)
			s.aggregator.Publish(usage.Record{
				Timestamp:            start,
				CredentialID:         credID,
				Provider:             "kiro",
				RequestedModel:       req.Model,
				ResolvedModel:        kiroModel,
				InputTokens:          mw.inputTokens,
				OutputTokens:         mw.outputTokens,
				Status:               usageStatusFor(errMsg),
				LatencyMs:            int(time.Since(start).Milliseconds()),
				TraceID:              traceID,
				Type:                 reqType,
				Device:               device,
				DeviceID:             deviceID,
				APIKeyID:             apiKeyID,
				CreditsUsedSnapshot:  used,
				CreditsTotalSnapshot: total,
			})
		}
		// Bump per-key token counter for quota enforcement on the next call.
		if errMsg == "" && apiKeyID != "" && s.apiKeyUsageRecorder != nil {
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
			RetryCount:   retryCount,
			Status:       status,
			ErrorMsg:     errMsg,
		})
	}

	if hasWebSearchServerTool(req) {
		mw := newMetricsResponseWriter(w)
		s.handleLocalWebSearch(ctx, mw, req, anthropicModel, contextWindowSize, ccSessionID, short)
		recordMetric(mw, 0, "")
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
		mw := newMetricsResponseWriter(w)
		s.runToolSearch(ctx, mw, req, creds, tsCtx, kiroModel, anthropicModel, contextWindowSize, thinking, thinkingBudget, ccSessionID, short)
		recordMetric(mw, 0, "")
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
		if s.collector != nil {
			recordMetric(newMetricsResponseWriter(w), 0, err.Error())
		}
		return
	}

	if thinking && os.Getenv("KIROCC_EXPERIMENT_THINKING_PROMPT") == "tool" {
		mw := newMetricsResponseWriter(w)
		s.runThinkingTool(ctx, mw, req, creds, kiroModel, anthropicModel, contextWindowSize, thinkingBudget, ccSessionID, short)
		recordMetric(mw, 0, "")
		return
	}

	mw := newMetricsResponseWriter(w)
	inv := &invocation{
		req:               req,
		payload:           payload,
		creds:             creds,
		model:             kiroModel,
		responseModel:     anthropicModel,
		contextWindowSize: contextWindowSize,
		thinking:          thinking,
		thinkingBudget:    thinkingBudget,
		toolNameMap:       nameMap.ReverseMap(),
		tools:             req.Tools,
	}
	retryCount := s.executeWithRetry(ctx, mw, inv)
	recordMetric(mw, retryCount, "")
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
func (s *Service) runToolSearch(ctx context.Context, w http.ResponseWriter, req *anthropic.Request, creds *auth.Credentials, tsCtx *toolsearch.Context, kiroModel, responseModel string, contextWindowSize int, thinking bool, thinkingBudget int, ccSessionID, short string) {
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
	}
	reason := orch.run(ctx, w)
	if reason != retryReasonEmptyVisibleEndTurn {
		return
	}
	slog.WarnContext(ctx, "retrying tool search after empty visible end_turn", "trace_id", short)
	if r2 := orch.run(ctx, w); r2 == retryReasonEmptyVisibleEndTurn {
		slog.ErrorContext(ctx, "tool search retry also returned empty visible end_turn", "trace_id", short)
		httpx.WriteError(w, http.StatusBadGateway, errTypeAPI, "upstream returned empty response")
	}
}
