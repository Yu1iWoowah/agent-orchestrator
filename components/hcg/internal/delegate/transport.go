package delegate

import (
	"context"
	"encoding/json"
	"math"
	"time"
)

type WireRequest struct {
	Provider, Endpoint, Model string
	Payload                   []byte
	ScopeDigest               string
}
type WireResponse struct {
	Status  int
	Headers map[string]string
	Body    []byte
}
type Transport interface {
	Do(context.Context, WireRequest) (WireResponse, error)
}
type Usage struct {
	CacheWriteInput      *int64 `json:"cacheWriteInput,omitempty"`
	Input, Output, Total *int64
	CachedInput          *int64
	ReasoningOutput      *int64
}
type Recorder interface {
	Record(context.Context, Receipt) error
}

type Receipt struct {
	RequestedServiceTier       string `json:"requestedServiceTier,omitempty"`
	ObservedServiceTier        string `json:"observedServiceTier,omitempty"`
	ServiceTierStatus          string `json:"serviceTierStatus,omitempty"`
	Charges                    Charges
	ObservedAt                 time.Time
	Execution                  Execution
	ProfileID, ProfileRevision string
	ProfileDigest              string

	Provider, RequestID, Model string
	ResponseID, UnknownReason  string
	FinishReason               string
	Usage                      Usage
}

func projectResponse(profile Profile, response WireResponse) (Result, error) {
	requestID := response.Headers["x-request-id"]
	if requestID == "" {
		requestID = response.Headers["request-id"]
	}
	if !safeReceiptID(requestID) {
		requestID = ""
	}
	out := Result{Receipt: Receipt{Provider: profile.BillingProvider, Model: profile.Model, RequestID: requestID, UnknownReason: "response_invalid"}}
	if profile.luna() {
		out.Receipt.RequestedServiceTier = "default"
		out.Receipt.ServiceTierStatus = "missing"
	}
	var raw struct {
		ServiceTier json.RawMessage `json:"service_tier"`
		ID, Model   string
		Choices     []struct {
			Message      struct{ Content string }
			FinishReason string `json:"finish_reason"`
		}
		Usage *struct {
			Input   *int64 `json:"prompt_tokens"`
			Output  *int64 `json:"completion_tokens"`
			Total   *int64 `json:"total_tokens"`
			Details *struct {
				Cached *int64          `json:"cached_tokens"`
				Write  json.RawMessage `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
			OutputDetails *struct {
				Reasoning *int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		}
	}
	// fail keeps the provider boundary's first refusal private to the attempt.
	fail := func(stage, code string) (Result, error) {
		out.failure = &replyFailure{Stage: stage, Code: code}
		return out, ErrUnavailable
	}
	if response.Status != 200 {
		out.failure = &replyFailure{Stage: "http_status", Code: "http_status_not_ok", HTTPStatus: response.Status}
		return out, ErrUnavailable
	}
	if json.Unmarshal(response.Body, &raw) != nil {
		return fail("outer_json", "outer_json_invalid")
	}
	if profile.luna() {
		status := "invalid"
		var tier string
		switch {
		case len(raw.ServiceTier) == 0:
			status = "missing"
		case string(raw.ServiceTier) == "null":
			status = "invalid"
		case json.Unmarshal(raw.ServiceTier, &tier) == nil:
			status = "non_default"
			if tier == "default" {
				status = "verified_default"
				out.Receipt.ObservedServiceTier = "default"
			}
		}
		out.Receipt.ServiceTierStatus = status
		if status != "verified_default" {
			out.Receipt.UnknownReason = "service_tier_unverified"
			return fail("service_tier", "service_tier_"+status)
		}
	}
	if raw.ID != "" && !safeReceiptID(raw.ID) {
		return fail("response_id", "response_id_invalid")
	}
	out.Receipt.ResponseID = raw.ID
	switch {
	case raw.Model != profile.Model:
		return fail("model", "model_mismatch")
	case len(raw.Choices) != 1:
		return fail("choices", "choices_count_invalid")
	case out.Receipt.RequestID == "":
		return fail("request_id", "request_id_missing")
	}
	switch raw.Choices[0].FinishReason {
	case "stop", "length", "content_filter", "tool_calls", "function_call":
		out.Receipt.FinishReason = raw.Choices[0].FinishReason
	}
	if raw.Usage == nil {
		out.Receipt.UnknownReason = "usage_missing"
		return fail("usage", "usage_missing")
	}
	u := raw.Usage
	if u.Input == nil || u.Output == nil || u.Total == nil || *u.Input < 0 || *u.Input > profile.ContextTokens || *u.Output < 0 || *u.Output > profile.OutputTokens || *u.Input > math.MaxInt64-*u.Output || *u.Total != *u.Input+*u.Output {
		out.Receipt.UnknownReason = "usage_invalid"
		return fail("usage", "usage_invalid")
	}
	var cached, reasoning, write *int64
	if profile.luna() {
		// Only this reviewed schema defines an omitted write leaf as zero.
		n := int64(0)
		if u.Details == nil || u.Details.Cached == nil || (len(u.Details.Write) > 0 && (string(u.Details.Write) == "null" || json.Unmarshal(u.Details.Write, &n) != nil)) || n < 0 || n > *u.Input || *u.Input > 922000 || *u.Details.Cached < 0 || *u.Details.Cached > *u.Input-n {
			out.Receipt.UnknownReason = "usage_invalid"
			return fail("usage", "usage_invalid")
		}
		// Unknown fee/mode leaves cannot be silently priced as ordinary input.
		var envelope map[string]json.RawMessage
		_ = json.Unmarshal(response.Body, &envelope)
		var usage map[string]json.RawMessage
		_ = json.Unmarshal(envelope["usage"], &usage)
		for key := range usage {
			if key != "prompt_tokens" && key != "completion_tokens" && key != "total_tokens" && key != "prompt_tokens_details" && key != "completion_tokens_details" {
				out.Receipt.UnknownReason = "usage_invalid"
				return fail("usage", "usage_invalid")
			}
		}
		var details map[string]json.RawMessage
		_ = json.Unmarshal(usage["prompt_tokens_details"], &details)
		for key := range details {
			if key == "audio_tokens" && string(details[key]) == "0" {
				continue
			}
			if key != "cached_tokens" && key != "cache_write_tokens" {
				out.Receipt.UnknownReason = "usage_invalid"
				return fail("usage", "usage_invalid")
			}
		}
		// Unknown completion fee categories are outside the approved output model.
		var outputDetails map[string]json.RawMessage
		_ = json.Unmarshal(usage["completion_tokens_details"], &outputDetails)
		for key := range outputDetails {
			if (key == "audio_tokens" || key == "accepted_prediction_tokens" || key == "rejected_prediction_tokens") && string(outputDetails[key]) == "0" {
				continue
			}
			if key != "reasoning_tokens" {
				out.Receipt.UnknownReason = "usage_invalid"
				return fail("usage", "usage_invalid")
			}
		}

		write = &n
	}
	if profile.BillingProvider == "openai" {
		if u.Details != nil {
			cached = u.Details.Cached
		}
		if u.OutputDetails != nil {
			reasoning = u.OutputDetails.Reasoning
		}
	}
	if cached != nil && (*cached < 0 || *cached > *u.Input) || reasoning != nil && (*reasoning < 0 || *reasoning > *u.Output) {
		out.Receipt.UnknownReason = "usage_invalid"
		return fail("usage", "usage_invalid")
	}
	out.Receipt.UnknownReason = ""
	out.Receipt.Usage = Usage{Input: u.Input, Output: u.Output, Total: u.Total, CachedInput: cached, CacheWriteInput: write, ReasoningOutput: reasoning}
	if out.Receipt.FinishReason != "stop" {
		return fail("finish_reason", "finish_reason_not_stop")
	}
	if raw.Choices[0].Message.Content == "" {
		return fail("content", "content_empty")
	}
	out.text = raw.Choices[0].Message.Content
	return out, nil
}

func safeReceiptID(value string) bool { return profileToken.MatchString(value) && safeText(value) }
