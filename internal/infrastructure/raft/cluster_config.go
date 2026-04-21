package raft

// Invariants (ADR-020 §Safety Analysis):
//   - INV-CC1 (No Config Skip): Every node passes through C_old,new before C_new.
//     A config entry takes effect on append, not on commit, so the leader enforces
//     joint quorum from the moment it writes C_old,new.
//   - INV-CC2 (Election Safety under Joint): A candidate must win majority(C_old)
//     AND majority(C_new). Two concurrent majorities within the same set share at
//     least one member (pigeonhole); a voter grants one vote per term → no two leaders.
//   - INV-CC3 (Monotonic Config): ClusterConfig is only updated via applyConfigEntry,
//     which is called in log-index order; config cannot regress.

// ConfigPhase describes which phase of membership the cluster is in.
type ConfigPhase int

const (
	// ConfigPhaseStable: single active configuration (C_old or C_new).
	// All quorum decisions require only majority(Voters).
	ConfigPhaseStable ConfigPhase = iota

	// ConfigPhaseJoint: transitional configuration C_old,new.
	// All quorum decisions require majority(Voters) AND majority(NewVoters).
	ConfigPhaseJoint
)

// ClusterConfig represents the active cluster membership.
//
// In ConfigPhaseStable, only Voters is populated; NewVoters is nil.
// In ConfigPhaseJoint, both Voters (C_old) and NewVoters (C_new) are populated.
//
// The self node's address is NOT included in either slice; quorumFor always
// adds 1 to both counts for the self node.
//
// Protected by RaftNode.mu whenever accessed from RaftNode.
type ClusterConfig struct {
	Phase     ConfigPhase
	Voters    []string // C_old peer addresses (always present; excludes self)
	NewVoters []string // C_new peer addresses (non-nil during joint phase; excludes self)
}

// IsJoint reports whether the cluster is in the joint-consensus phase.
func (c ClusterConfig) IsJoint() bool {
	return c.Phase == ConfigPhaseJoint
}

// OldQuorumSize returns the majority threshold for C_old (self included).
// The +1 accounts for the self node which is not in Voters.
func (c ClusterConfig) OldQuorumSize() int {
	return (len(c.Voters)+1)/2 + 1
}

// NewQuorumSize returns the majority threshold for C_new (self included).
// The +1 accounts for the self node which is not in NewVoters.
func (c ClusterConfig) NewQuorumSize() int {
	return (len(c.NewVoters)+1)/2 + 1
}

// AllPeers returns a deduplicated union of Voters and NewVoters.
// Used by EnsureConnected to dial any new peers that joined via config change.
func (c ClusterConfig) AllPeers() []string {
	seen := make(map[string]struct{}, len(c.Voters)+len(c.NewVoters))
	var out []string
	for _, addr := range c.Voters {
		if _, ok := seen[addr]; !ok {
			seen[addr] = struct{}{}
			out = append(out, addr)
		}
	}
	for _, addr := range c.NewVoters {
		if _, ok := seen[addr]; !ok {
			seen[addr] = struct{}{}
			out = append(out, addr)
		}
	}
	return out
}

// ConfigChangePayload is the Data field of an EntryTypeConfig log entry.
// Serialised as JSON and stored in LogEntry.Data.
type ConfigChangePayload struct {
	// Phase is the target phase after this entry is applied.
	//   "joint"  → this entry transitions to C_old,new.
	//   "stable" → this entry finalises C_new.
	Phase string `json:"phase"`

	// Voters is the complete C_old (joint) or C_new (stable) peer address list.
	// Does NOT include the self node.
	Voters []string `json:"voters"`

	// NewVoters is non-empty only during the joint phase (C_new peer list).
	// Does NOT include the self node.
	NewVoters []string `json:"new_voters,omitempty"`
}

// quorumFor returns true if the set of nodes that have acknowledged (via ackFn)
// satisfies the active quorum rule for cfg.
//
// ConfigPhaseStable: simple majority of (Voters ∪ {self}).
// ConfigPhaseJoint:  majority of (Voters ∪ {self}) AND majority of (NewVoters ∪ {self}).
//
// ackFn is called with each peer address (never with the self address).
// Self always counts as acknowledged (self is always 1 in both counts).
//
// Must be called with mu held.
func (n *RaftNode) quorumFor(cfg ClusterConfig, ackFn func(addr string) bool) bool {
	switch cfg.Phase {
	case ConfigPhaseStable:
		count := 1 // self
		for _, addr := range cfg.Voters {
			if ackFn(addr) {
				count++
			}
		}
		return count >= cfg.OldQuorumSize()

	case ConfigPhaseJoint:
		// Old quorum (C_old ∪ {self}).
		oldCount := 1 // self
		for _, addr := range cfg.Voters {
			if ackFn(addr) {
				oldCount++
			}
		}
		// New quorum (C_new ∪ {self}).
		newCount := 1 // self
		for _, addr := range cfg.NewVoters {
			if ackFn(addr) {
				newCount++
			}
		}
		return oldCount >= cfg.OldQuorumSize() && newCount >= cfg.NewQuorumSize()
	}
	return false
}
