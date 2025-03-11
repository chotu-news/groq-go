package groq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/pkg/errors"
)

const (
	baseURL = "https://api.groq.com/openai"
)

type Client interface {
	CreateChatCompletion(ChatCompletionRequest) (*ChatCompletionResponse, error)
	CreateChatCompletionStream(context.Context, ChatCompletionRequest) (<-chan *ChatCompletionStreamResponse, func(), error)
	ListModels() (*ListModelsResponse, error)
	RetrieveModel(ModelID) (*Model, error)
}

var _ Client = (*client)(nil)

type client struct {
	apiKey string
	// baseURL shouldn't end with a trailing slash
	baseURL                     string
	client                      *http.Client
	max_wait_on_ratelimit_in_ms int
	wait_on_ratelimit           bool
}

// ChatCompletionRequest represents the request body for creating a chat completion.
type ChatCompletionRequest struct {
	Messages         []Message   `json:"messages"`                    // A list of messages comprising the conversation so far.
	Model            ModelID     `json:"model"`                       // ID of the model to use
	MaxTokens        int         `json:"max_tokens,omitempty"`        // The maximum number of tokens that can be generated in the chat completion. The total length of input tokens and generated tokens is limited by the model's context length.
	Temperature      float64     `json:"temperature,omitempty"`       // Sampling temperature
	TopP             float64     `json:"top_p,omitempty"`             // Nucleus sampling probability
	NumChoices       int         `json:"n,omitempty"`                 // Number of completion choices to generate
	PresencePenalty  float64     `json:"presence_penalty,omitempty"`  // Penalty for presence of tokens
	FrequencyPenalty *float64    `json:"frequency_penalty,omitempty"` // Number between -2.0 and 2.0. Positive values penalize new tokens based on their existing frequency in the text so far, decreasing the model's likelihood to repeat the same line verbatim.
	UserID           string      `json:"user,omitempty"`              // Unique identifier for the end-user
	Stream           bool        `json:"stream,omitempty"`            // If set, partial message deltas will be sent as data-only server-sent events
	ToolChoice       interface{} `json:"tool_choice,omitempty"`       // Controls which tool is called by the model
	Tools            interface{} `json:"tools,omitempty"`             // List of tools the model may call
	FunctionCall     interface{} `json:"function_call,omitempty"`     // Controls which function is called by the model
	ResponseFormat   interface{} `json:"response_format,omitempty"`   // Format of the model's response
	Seed             int         `json:"seed,omitempty"`              // Seed for deterministic sampling

	// StopSequences is a predefined or user-specified text string that
	// signals an AI to stop generating content, ensuring its responses
	// remain focused and concise. Examples include punctuation marks and
	// markers like "[end]".
	StopSequences interface{} `json:"stop,omitempty"`
}

// Choice represents a single completion choice returned by the chat completion API.
type Choice struct {
	Index        int     `json:"index"`         // Index of the choice
	Message      Message `json:"message"`       // Message generated by the model
	Delta        Message `json:"delta"`         // Partial generated message when you are streaming
	FinishReason string  `json:"finish_reason"` // Reason why the model stopped generating tokens
}

// ChatCompletionResponse represents the response from the chat completion API.
type ChatCompletionResponse struct {
	ID                string   `json:"id"`                 // Unique identifier for the completion
	Object            string   `json:"object"`             // Type of the object (e.g., "chat.completion")
	Created           int64    `json:"created"`            // Timestamp of creation
	Model             string   `json:"model"`              // ID of the model used
	SystemFingerprint string   `json:"system_fingerprint"` // System fingerprint
	Choices           []Choice `json:"choices"`            // List of completion choices
	Usage             Usage    `json:"usage"`              // Token usage information
}

// Usage represents the token usage information in the chat completion response.
type Usage struct {
	PromptTokens     int     `json:"prompt_tokens"`     // Number of tokens in the prompt
	CompletionTokens int     `json:"completion_tokens"` // Number of tokens in the completion
	TotalTokens      int     `json:"total_tokens"`      // Total number of tokens
	PromptTime       float64 `json:"prompt_time"`       // Time taken for the prompt
	CompletionTime   float64 `json:"completion_time"`   // Time taken for the completion
	TotalTime        float64 `json:"total_time"`        // Total time taken
}
type ErrorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func NewClient(apiKey string, httpClient *http.Client, wait_on_ratelimit bool, max_wait_on_ratelimit_in_ms int) Client {
	return &client{
		apiKey: apiKey,
		client: httpClient,
		// NOTE(@Kcrong): Need to handle if the user wants to use a different base URL
		baseURL:                     baseURL,
		max_wait_on_ratelimit_in_ms: max_wait_on_ratelimit_in_ms,
		wait_on_ratelimit:           wait_on_ratelimit,
	}
}
func (c *client) makeReq(req ChatCompletionRequest) (*http.Response, []byte, error) {
	if req.Stream {
		return nil, nil, fmt.Errorf("use CreateChatCompletionStream for streaming completions")
	}

	url := fmt.Sprintf("%s/v1/chat/completions", c.baseURL)

	jsonData, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal request: %v", err)
	}

	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create request: %v", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.apiKey))
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to send request: %v", err)
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to read response body")
	}
	return resp, body, err
}

// CreateChatCompletion sends a request to create a chat completion.
func (c *client) CreateChatCompletion(req ChatCompletionRequest) (*ChatCompletionResponse, error) {
	resp, body, err := c.makeReq(req)

	if resp.StatusCode != http.StatusOK {
		retry := 0
		maxRetry := 5
		for resp.StatusCode == http.StatusTooManyRequests && retry <= maxRetry {
			retry = retry + 1
			if retry > maxRetry {
				return nil, fmt.Errorf("retry is %d max retry :%d, code: %d, body: %s,response : %v", retry, maxRetry, resp.StatusCode, body, resp)
			}
			var errResp ErrorResponse
			err = json.Unmarshal([]byte(body), &errResp)
			if err != nil {
				return nil, fmt.Errorf("invalid status code: %d, body: %s, Failed to unmarshall the error body, headers: %v", resp.StatusCode, body, resp)
			}
			retrys, err := strconv.Atoi(resp.Header.Get("retry-after"))
			retryMs := retrys * 1000

			if c.wait_on_ratelimit {
				fmt.Println("Retry after (ms):", retryMs)
				if c.max_wait_on_ratelimit_in_ms < retryMs {
					retryMs = c.max_wait_on_ratelimit_in_ms
				}
				time.Sleep(time.Duration(retryMs) * time.Millisecond)
				fmt.Println("Retrying now...")
				resp, body, err = c.makeReq(req)

			} else {
				fmt.Println("skipping waiting as its disabled now...")
			}

			if err != nil {
				return nil, fmt.Errorf("invalid status code: %d, body: %s, Failed to parse retry time", resp.StatusCode, body)
			}

		}

	}

	var chatResp ChatCompletionResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal response")
	}

	return &chatResp, nil
}
func ExtractRetryTime(s string) (int, error) {
	return extractRetryTime(s)
}
func extractRetryTime(message string) (int, error) {
	re := regexp.MustCompile(`Please try again in (\d+)([a-zA-Z]+)\.`)
	matches := re.FindStringSubmatch(message)
	if len(matches) < 3 {
		return 0, fmt.Errorf("retry time not found")
	}

	retryValue, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0, err
	}

	unit := matches[2]
	var retryMs int
	switch unit {
	case "ms":
		retryMs = retryValue
	case "s":
		retryMs = retryValue * 1000
	case "m":
		retryMs = retryValue * 60 * 1000
	case "h":
		retryMs = retryValue * 60 * 60 * 1000
	default:
		return 0, fmt.Errorf("unknown time unit: %s", unit)
	}

	return retryMs, nil
}
