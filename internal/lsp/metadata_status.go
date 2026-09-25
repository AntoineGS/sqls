package lsp

// MetadataCategoryStatus is the public, sanitized state of one metadata job.
type MetadataCategoryStatus struct {
	Kind       string `json:"kind"`
	State      string `json:"state"`
	Count      int    `json:"count"`
	DurationMS int64  `json:"durationMs"`
	ErrorCode  string `json:"errorCode,omitempty"`
}

// MetadataStatusResult is a point-in-time view of attachment and metadata readiness.
//
// Generation is the active metadata store's own generation (the shared
// Lease's Snapshot().Generation): it starts at 1 and advances only on an
// explicit refresh, identically for every consumer sharing that store.
// ConnectionGeneration is this server's own connection-identity counter
// (advances on every reconnect, including one that reuses an existing
// shared store) — use it, not Generation, to tell whether the client's own
// connection was replaced.
type MetadataStatusResult struct {
	Generation           uint64                   `json:"generation"`
	ConnectionGeneration uint64                   `json:"connectionGeneration"`
	Revision             uint64                   `json:"revision"`
	ConnectionState      string                   `json:"connectionState"`
	ConnectionErrorCode  string                   `json:"connectionErrorCode,omitempty"`
	MetadataErrorCode    string                   `json:"metadataErrorCode,omitempty"`
	Settled              bool                     `json:"settled"`
	Degraded             bool                     `json:"degraded"`
	Categories           []MetadataCategoryStatus `json:"categories"`
}
