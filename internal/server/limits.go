package server

// Resource limits protect the public listener from unbounded file descriptors,
// goroutines, and pending data-stream tokens. They are intentionally
// conservative defaults; make them configurable before raising them for large
// deployments.
const (
	maxControlClients        = 256
	maxMappingsPerClient     = 128
	maxConnectionsPerMapping = 1024
	maxPendingDataStreams    = 4096
)
