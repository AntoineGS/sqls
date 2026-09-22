package lsp

import "github.com/sqls-server/sqls/internal/queryparams"

// QueryParameterContext identifies the connection, generation, and query
// text a QueryParameterDiscovery or QueryParameterSubmission was produced
// for. A client compares it against the current state before submitting
// remembered values, so a stale prefill is never sent as a fresh one.
type QueryParameterContext struct {
	Version              int    `json:"version"`
	ConnectionKey        string `json:"connectionKey"`
	ConnectionGeneration int    `json:"connectionGeneration"`
	QueryKey             string `json:"queryKey"`
	DocumentKey          string `json:"documentKey"`
}

// QueryParameterDiscovery is the getQueryParameters command result.
type QueryParameterDiscovery struct {
	QueryParameterContext
	Supported  bool                    `json:"supported"`
	Parameters []queryparams.Parameter `json:"parameters"`
}

// QueryParameterSubmission carries the client's typed answers back to the
// server alongside the identity it was prompted under.
type QueryParameterSubmission struct {
	QueryParameterContext
	Values []queryparams.Value `json:"values"`
}
