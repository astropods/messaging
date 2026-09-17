package a2a

import "encoding/json"

// jsonRPCVersion is the only version the A2A transport accepts.
const jsonRPCVersion = "2.0"

// request is a JSON-RPC 2.0 request. ID stays raw because the spec allows a
// string, a number or null, and the response must echo it unchanged.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// JSON-RPC transport codes, then the A2A-specific range.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603

	codeTaskNotFound         = -32001
	codeTaskNotCancelable    = -32002
	codeUnsupportedOperation = -32004
)

func errorResponse(id json.RawMessage, code int, message string) response {
	return response{JSONRPC: jsonRPCVersion, ID: id, Error: &rpcError{Code: code, Message: message}}
}

func resultResponse(id json.RawMessage, result any) response {
	return response{JSONRPC: jsonRPCVersion, ID: id, Result: result}
}
