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
type MetadataStatusResult struct {
	Generation          uint64                   `json:"generation"`
	Revision            uint64                   `json:"revision"`
	ConnectionState     string                   `json:"connectionState"`
	ConnectionErrorCode string                   `json:"connectionErrorCode,omitempty"`
	MetadataErrorCode   string                   `json:"metadataErrorCode,omitempty"`
	Settled             bool                     `json:"settled"`
	Degraded            bool                     `json:"degraded"`
	Categories          []MetadataCategoryStatus `json:"categories"`
}
