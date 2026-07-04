package subagent

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// idLength is the number of hex characters in a subagent node id. Five hex
// characters (20 bits) is a short, git-like sha that is easy for humans and
// models to reference; in a system with at most a few hundred concurrent
// subagents the collision probability is negligible, and callers that mint
// ids through a [Tree] retry on the rare clash anyway.
const idLength = 5

// NewID returns a fresh, git-like short-sha node id (5 hex characters).
func NewID() NodeID {
	var seed [16]byte
	_, _ = rand.Read(seed[:])
	sum := sha256.Sum256(seed[:])
	return NodeID(hex.EncodeToString(sum[:])[:idLength])
}

// SessionRootID is the synthetic tree node that represents a top-level session
// (the swarm root). Its children are the session's own subagents.
func SessionRootID(sessionID string) NodeID {
	return NodeID("root:" + sessionID)
}
