package acp

// Client implementation is split by responsibility across client_*.go files:
// runtime starts and owns the child process, auth performs the initialize/auth
// handshake, session contains lifecycle RPCs, prompt handles active turns,
// transport multiplexes JSON-RPC, and permission tracks server-side approvals.
