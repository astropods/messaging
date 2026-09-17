// Package a2awire holds the A2A protocol's wire types, shared by the inbound
// adapter that serves A2A and the outbound client that calls peers. One
// definition for both directions: a client and server in the same repo
// disagreeing about the wire format is a drift trap.
package a2awire

import "encoding/json"

// JSONRPCVersion is the only version the A2A transport accepts.
const JSONRPCVersion = "2.0"

// Request is a JSON-RPC 2.0 request. ID stays raw because the spec allows a
// string, a number or null, and the response must echo it unchanged.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// JSON-RPC transport codes, then the A2A-specific range.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	CodeTaskNotFound         = -32001
	CodeTaskNotCancelable    = -32002
	CodeUnsupportedOperation = -32004
)

func ErrorResponse(id json.RawMessage, code int, message string) Response {
	return Response{JSONRPC: JSONRPCVersion, ID: id, Error: &RPCError{Code: code, Message: message}}
}

func ResultResponse(id json.RawMessage, result any) Response {
	return Response{JSONRPC: JSONRPCVersion, ID: id, Result: result}
}
