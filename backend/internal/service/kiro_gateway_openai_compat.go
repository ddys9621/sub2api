package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/gin-gonic/gin"
)

type kiroCompatStats struct {
	requestID          string
	usage              ClaudeUsage
	meteringCredit     float64
	contextUsagePct    float64
	upstreamDriveError error
}

type kiroCompatStream struct {
	resp          *http.Response
	done          <-chan kiroCompatStats
	upstreamModel string
}

type kiroCompatPipeBody struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (b *kiroCompatPipeBody) Close() error {
	if b.cancel != nil {
		b.cancel()
	}
	return b.PipeReader.Close()
}

// ForwardAsResponses serves OpenAI Responses API clients through a Kiro
// account. It converts Responses -> Anthropic, sends the Anthropic turn to
// Kiro/Amazon Q, converts Kiro EventStream back to Anthropic SSE internally,
// then reuses the existing Anthropic SSE -> Responses converter.
func (s *KiroGatewayService) ForwardAsResponses(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	parsed *ParsedRequest,
) (*ForwardResult, error) {
	startTime := time.Now()

	var responsesReq apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &responsesReq); err != nil {
		return nil, fmt.Errorf("parse responses request: %w", err)
	}
	originalModel := responsesReq.Model
	clientStream := responsesReq.Stream
	reasoningEffort := ExtractResponsesReasoningEffortFromBody(body)

	anthropicReq, err := apicompat.ResponsesToAnthropicRequest(&responsesReq)
	if err != nil {
		return nil, fmt.Errorf("convert responses to anthropic: %w", err)
	}
	anthropicReq.Stream = true

	kiroReq, kiroParsed, err := kiroCompatRequestFromAnthropic(anthropicReq, parsed)
	if err != nil {
		return nil, err
	}

	stream, err := s.startKiroCompatAnthropicSSE(ctx, c, account, kiroReq, kiroParsed, startTime)
	if err != nil {
		return nil, err
	}
	defer stream.resp.Body.Close()

	converter := &GatewayService{}
	var result *ForwardResult
	var handleErr error
	if clientStream {
		result, handleErr = converter.handleResponsesStreamingResponse(stream.resp, c, originalModel, stream.upstreamModel, reasoningEffort, startTime)
	} else {
		result, handleErr = converter.handleResponsesBufferedStreamingResponse(stream.resp, c, originalModel, stream.upstreamModel, reasoningEffort, startTime)
	}

	_ = stream.resp.Body.Close()
	stats := waitKiroCompatStats(stream.done)
	mergeKiroCompatStats(result, stats, originalModel, stream.upstreamModel)
	return result, handleErr
}

// ForwardAsChatCompletions serves OpenAI Chat Completions API clients through
// Kiro by chaining Chat Completions -> Responses -> Anthropic -> Kiro, then
// converting the internal Anthropic SSE stream back to Chat Completions.
func (s *KiroGatewayService) ForwardAsChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	parsed *ParsedRequest,
) (*ForwardResult, error) {
	startTime := time.Now()

	var ccReq apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &ccReq); err != nil {
		return nil, fmt.Errorf("parse chat completions request: %w", err)
	}
	originalModel := ccReq.Model
	clientStream := ccReq.Stream
	includeUsage := ccReq.StreamOptions != nil && ccReq.StreamOptions.IncludeUsage
	reasoningEffort := extractCCReasoningEffortFromBody(body)

	responsesReq, err := apicompat.ChatCompletionsToResponses(&ccReq)
	if err != nil {
		return nil, fmt.Errorf("convert chat completions to responses: %w", err)
	}
	anthropicReq, err := apicompat.ResponsesToAnthropicRequest(responsesReq)
	if err != nil {
		return nil, fmt.Errorf("convert responses to anthropic: %w", err)
	}
	anthropicReq.Stream = true

	kiroReq, kiroParsed, err := kiroCompatRequestFromAnthropic(anthropicReq, parsed)
	if err != nil {
		return nil, err
	}

	stream, err := s.startKiroCompatAnthropicSSE(ctx, c, account, kiroReq, kiroParsed, startTime)
	if err != nil {
		return nil, err
	}
	defer stream.resp.Body.Close()

	converter := &GatewayService{}
	var result *ForwardResult
	var handleErr error
	if clientStream {
		result, handleErr = converter.handleCCStreamingFromAnthropic(stream.resp, c, originalModel, stream.upstreamModel, reasoningEffort, startTime, includeUsage)
	} else {
		result, handleErr = converter.handleCCBufferedFromAnthropic(stream.resp, c, originalModel, stream.upstreamModel, reasoningEffort, startTime)
	}

	_ = stream.resp.Body.Close()
	stats := waitKiroCompatStats(stream.done)
	mergeKiroCompatStats(result, stats, originalModel, stream.upstreamModel)
	return result, handleErr
}

func kiroCompatRequestFromAnthropic(req *apicompat.AnthropicRequest, originalParsed *ParsedRequest) (*kiro.AnthropicRequest, *ParsedRequest, error) {
	if req == nil {
		return nil, nil, errors.New("kiro compat: anthropic request is nil")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("kiro compat: marshal anthropic request: %w", err)
	}
	var kiroReq kiro.AnthropicRequest
	if err := json.Unmarshal(body, &kiroReq); err != nil {
		return nil, nil, fmt.Errorf("kiro compat: convert anthropic request: %w", err)
	}
	parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
	if err != nil {
		parsed = &ParsedRequest{
			Body:   body,
			Model:  req.Model,
			Stream: req.Stream,
		}
	}
	if originalParsed != nil {
		parsed.GroupID = originalParsed.GroupID
		parsed.SessionContext = originalParsed.SessionContext
	}
	return &kiroReq, parsed, nil
}

func (s *KiroGatewayService) startKiroCompatAnthropicSSE(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	anthropicReq *kiro.AnthropicRequest,
	parsed *ParsedRequest,
	start time.Time,
) (*kiroCompatStream, error) {
	if account == nil {
		return nil, errors.New("kiro compat: account is nil")
	}
	if anthropicReq == nil {
		return nil, errors.New("kiro compat: request is nil")
	}
	if parsed == nil {
		parsed = &ParsedRequest{
			Model:  anthropicReq.Model,
			Stream: true,
		}
	}

	token, err := s.tokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("kiro token: %w", err)
	}
	profileArn := s.tokenProvider.ProfileArn(account)
	mapping := kiroModelMappingForAccount(account)
	requestConversationID := conversationIDFromContext(c)
	payload, err := kiro.BuildKiroPayload(anthropicReq, kiro.BuildOptions{
		ProfileArn:     profileArn,
		ModelMapping:   mapping,
		ConversationID: requestConversationID,
	})
	if err != nil {
		return nil, fmt.Errorf("kiro compat: build payload: %w", err)
	}

	upstreamModel := kiro.ResolveModel(anthropicReq.Model, mapping)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("kiro compat: marshal payload: %w", err)
	}
	kiroEndpoint := kiroUpstreamEndpointForAccount(account, profileArn)
	forwardCtx, cancel := context.WithTimeout(ctx, kiroForwardTimeout)

	httpReq, err := http.NewRequestWithContext(forwardCtx, http.MethodPost, kiroEndpoint, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("kiro compat: build request: %w", err)
	}
	httpReq.Header = kiroUpstreamHeaders(token, account)

	client := s.clientForAccount(account)
	resp, err := client.Do(httpReq)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("kiro compat: http do: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusBadRequest && kiro.IsInvalidModelError(raw) {
			fallbackModel := kiro.FallbackModelForInvalidModel(parsed.Model, upstreamModel)
			if fallbackModel != "" && fallbackModel != upstreamModel {
				fallbackMapping := cloneKiroModelMapping(mapping)
				fallbackMapping[anthropicReq.Model] = fallbackModel
				fallbackPayload, berr := kiro.BuildKiroPayload(anthropicReq, kiro.BuildOptions{
					ProfileArn:     profileArn,
					ModelMapping:   fallbackMapping,
					ConversationID: requestConversationID,
				})
				if berr != nil {
					cancel()
					return nil, fmt.Errorf("kiro compat: build fallback payload: %w", berr)
				}
				fallbackBody, merr := json.Marshal(fallbackPayload)
				if merr != nil {
					cancel()
					return nil, fmt.Errorf("kiro compat: marshal fallback payload: %w", merr)
				}
				fallbackReq, rerr := http.NewRequestWithContext(forwardCtx, http.MethodPost, kiroEndpoint, bytes.NewReader(fallbackBody))
				if rerr != nil {
					cancel()
					return nil, fmt.Errorf("kiro compat: build fallback request: %w", rerr)
				}
				fallbackReq.Header = kiroUpstreamHeaders(token, account)
				resp, err = client.Do(fallbackReq)
				if err != nil {
					cancel()
					return nil, fmt.Errorf("kiro compat: fallback http do: %w", err)
				}
				mapping = fallbackMapping
				upstreamModel = fallbackModel
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					goto kiroCompatResponseOK
				}
				raw, _ = io.ReadAll(io.LimitReader(resp.Body, 16*1024))
				_ = resp.Body.Close()
			}
		}
		cancel()
		return nil, &UpstreamFailoverError{
			StatusCode:      resp.StatusCode,
			ResponseBody:    raw,
			ResponseHeaders: resp.Header,
		}
	}

kiroCompatResponseOK:
	requestID := resp.Header.Get("X-Amzn-Requestid")
	upstreamConversationID := resp.Header.Get("X-Amzn-Codewhisperer-Conversation-Id")
	effectiveRequestID := kiroFirstNonEmpty(requestID, upstreamConversationID)

	pr, pw := io.Pipe()
	done := make(chan kiroCompatStats, 1)
	go func() {
		defer cancel()
		defer resp.Body.Close()

		encoder := kiro.NewAnthropicSSEEncoder(pw, nil, anthropicReq.Model)
		if estimatedInput := estimateKiroInputTokens(parsed); estimatedInput > 0 {
			encoder.SetInputTokensHint(int64(estimatedInput))
		}
		interceptor := newKiroWebSearchInterceptor(
			forwardCtx,
			client,
			token,
			anthropicReq,
			kiro.BuildOptions{
				ProfileArn:     profileArn,
				ModelMapping:   mapping,
				ConversationID: requestConversationID,
			},
			kiroEndpoint,
			kiroUpstreamHeaders(token, account),
			s.mcpResultCache,
			account.ID,
			resolveAccountProxyURL(account),
		)

		_, driveErr := kiro.DriveEventStreamToAnthropicWithInterceptor(forwardCtx, resp.Body, encoder, interceptor)
		stopReason := "end_turn"
		if driveErr != nil {
			stopReason = "error"
			if !errors.Is(driveErr, context.Canceled) && !errors.Is(driveErr, context.DeadlineExceeded) {
				_ = encoder.Emit(&kiro.StreamEvent{
					Kind:         "error",
					ErrorType:    "upstream_error",
					ErrorMessage: driveErr.Error(),
				})
			}
		}
		finishErr := encoder.Finish(stopReason)
		if finishErr != nil {
			_ = pw.CloseWithError(finishErr)
		} else {
			_ = pw.Close()
		}

		inputTokens := int(encoder.InputTokens())
		if inputTokens == 0 {
			inputTokens = estimateKiroInputTokens(parsed)
		}
		done <- kiroCompatStats{
			requestID: effectiveRequestID,
			usage: ClaudeUsage{
				InputTokens:  inputTokens,
				OutputTokens: int(encoder.OutputTokens()),
			},
			meteringCredit:     encoder.MeteringCredit(),
			contextUsagePct:    encoder.ContextUsagePct(),
			upstreamDriveError: driveErr,
		}
	}()

	syntheticResp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"x-request-id": []string{effectiveRequestID},
		},
		Body: &kiroCompatPipeBody{
			PipeReader: pr,
			cancel:     cancel,
		},
	}
	return &kiroCompatStream{
		resp:          syntheticResp,
		done:          done,
		upstreamModel: upstreamModel,
	}, nil
}

func waitKiroCompatStats(done <-chan kiroCompatStats) kiroCompatStats {
	if done == nil {
		return kiroCompatStats{}
	}
	select {
	case stats := <-done:
		return stats
	case <-time.After(2 * time.Second):
		return kiroCompatStats{}
	}
}

func mergeKiroCompatStats(result *ForwardResult, stats kiroCompatStats, originalModel, upstreamModel string) {
	if result == nil {
		return
	}
	if result.RequestID == "" {
		result.RequestID = stats.requestID
	}
	if result.Model == "" {
		result.Model = originalModel
	}
	if result.UpstreamModel == "" {
		result.UpstreamModel = upstreamModel
	}
	if stats.usage.InputTokens > 0 {
		result.Usage.InputTokens = stats.usage.InputTokens
	}
	if stats.usage.OutputTokens > 0 {
		result.Usage.OutputTokens = stats.usage.OutputTokens
	}
	result.KiroMeteringCredit = stats.meteringCredit
	result.KiroContextUsagePct = stats.contextUsagePct
}
