package delegate

import (
	"context"
	"encoding/json"
	"math"
)

type WireRequest struct {
	Provider, Endpoint, Model string
	Payload                   []byte
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
	Input, Output, Total *int64
	CachedInput          *int64
	ReasoningOutput      *int64
}
type Recorder interface {
	Record(context.Context, Receipt) error
}

type Receipt struct {
	Execution                  Execution
	ProfileID, ProfileRevision string
	ProfileDigest              string

	Provider, RequestID, Model string
	ResponseID, UnknownReason  string
	FinishReason               string
	Usage                      Usage
}

func projectResponse(profile Profile, response WireResponse) (Result, error) {
	out := Result{Receipt: Receipt{Provider: profile.BillingProvider, Model: profile.Model, RequestID: response.Headers["x-request-id"], UnknownReason: "response_invalid"}}
	var raw struct {
		ID, Model string
		Choices   []struct {
			Message      struct{ Content string }
			FinishReason string `json:"finish_reason"`
		}
		Usage *struct {
			Input   *int64 `json:"prompt_tokens"`
			Output  *int64 `json:"completion_tokens"`
			Total   *int64 `json:"total_tokens"`
			Details *struct {
				Cached *int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			OutputDetails *struct {
				Reasoning *int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		}
	}
	if response.Status != 200 || json.Unmarshal(response.Body, &raw) != nil {
		return out, ErrUnavailable
	}
	out.Receipt.ResponseID = raw.ID
	if out.Receipt.RequestID == "" {
		out.Receipt.RequestID = response.Headers["request-id"]
	}
	if raw.Model != profile.Model || len(raw.Choices) != 1 || out.Receipt.RequestID == "" {
		return out, ErrUnavailable
	}
	switch raw.Choices[0].FinishReason {
	case "stop", "length", "content_filter", "tool_calls", "function_call":
		out.Receipt.FinishReason = raw.Choices[0].FinishReason
	}
	if raw.Usage == nil {
		out.Receipt.UnknownReason = "usage_missing"
		return out, ErrUnavailable
	}
	u := raw.Usage
	if u.Input == nil || u.Output == nil || u.Total == nil || *u.Input < 0 || *u.Input > profile.ContextTokens || *u.Output < 0 || *u.Output > profile.OutputTokens || *u.Input > math.MaxInt64-*u.Output || *u.Total != *u.Input+*u.Output {
		out.Receipt.UnknownReason = "usage_invalid"
		return out, ErrUnavailable
	}
	var cached, reasoning *int64
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
		return out, ErrUnavailable
	}
	out.Receipt.UnknownReason = ""
	out.Receipt.Usage = Usage{Input: u.Input, Output: u.Output, Total: u.Total, CachedInput: cached, ReasoningOutput: reasoning}
	if out.Receipt.FinishReason != "stop" || raw.Choices[0].Message.Content == "" {
		return out, ErrUnavailable
	}
	out.Text = raw.Choices[0].Message.Content
	return out, nil
}
