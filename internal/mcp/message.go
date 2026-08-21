// Package mcp implements the Model Context Protocol edge adapter: the
// JSON-RPC 2.0 message model and the Streamable HTTP transport boundary the
// Tool Gateway is exposed through (architecture §15.1: Agent ↔ Tool = MCP).
// Every input is treated as untrusted: strict JSON, size caps, bounded
// handling and no session state beyond the request (the pinned 2026-07-28
// core has no session).
package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// ProtocolVersion is the pinned MCP protocol version this adapter implements
// (tech baseline §6.3). Negotiation always answers with this version.
const ProtocolVersion = "2026-07-28"

// JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	// codeUnauthorized (implementation-defined range) reports a missing
	// fenced attempt identity: the call is outside an execution window.
	codeUnauthorized = -32001
)

// Error is a JSON-RPC 2.0 error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

// parseError constructs a JSON-RPC parse error.
func parseError(detail string) *Error {
	return &Error{Code: codeParseError, Message: "Parse error", Data: detail}
}

// invalidRequest constructs a JSON-RPC invalid-request error.
func invalidRequest(detail string) *Error {
	return &Error{Code: codeInvalidRequest, Message: "Invalid Request", Data: detail}
}

// invalidParams constructs a JSON-RPC invalid-params error.
func invalidParams(detail string) *Error {
	return &Error{Code: codeInvalidParams, Message: "Invalid params", Data: detail}
}

// Request is the inbound JSON-RPC message. ID is absent for notifications.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification reports whether the request carries no id.
func (r *Request) IsNotification() bool { return len(r.ID) == 0 }

// Response is the outbound JSON-RPC message.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// ParseRequest decodes exactly one strict JSON-RPC message. Batches, trailing
// documents, unknown fields, duplicate keys and non-2.0 versions are rejected;
// notifications carry no id.
func ParseRequest(body []byte) (*Request, *Error) {
	if len(body) == 0 {
		return nil, invalidRequest("empty request body")
	}
	if err := rejectDuplicateJSONKeys(body); err != nil {
		return nil, invalidRequest("request contains duplicate keys")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, parseError("request is not a JSON object")
	}
	if _, ok := raw["method"]; !ok {
		// A JSON array is an unsupported batch.
		if bytes.HasPrefix(bytes.TrimSpace(body), []byte("[")) {
			return nil, invalidRequest("batch requests are not supported")
		}
		return nil, invalidRequest("method is required")
	}
	var request Request
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return nil, invalidRequest("request contains unknown or malformed fields")
	}
	if err := ensureEOF(decoder); err != nil {
		return nil, invalidRequest("request contains more than one JSON value")
	}
	if request.JSONRPC != "2.0" {
		return nil, invalidRequest("jsonrpc must be \"2.0\"")
	}
	if request.Method == "" {
		return nil, invalidRequest("method is required")
	}
	if len(request.ID) > 0 && !json.Valid(request.ID) {
		return nil, invalidRequest("id must be a valid JSON value")
	}
	return &request, nil
}

// rejectDuplicateJSONKeys rejects ambiguous documents with repeated keys.
func rejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := scanValue(decoder); err != nil {
		return fmt.Errorf("invalid or ambiguous JSON")
	}
	return ensureEOF(decoder)
}

func scanValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = struct{}{}
			if err := scanValue(decoder); err != nil {
				return err
			}
		}
		if _, err := decoder.Token(); err != nil {
			return err
		}
	case '[':
		for decoder.More() {
			if err := scanValue(decoder); err != nil {
				return err
			}
		}
		if _, err := decoder.Token(); err != nil {
			return err
		}
	}
	return nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("more than one JSON value")
	}
	return fmt.Errorf("decode trailing data: %w", err)
}
