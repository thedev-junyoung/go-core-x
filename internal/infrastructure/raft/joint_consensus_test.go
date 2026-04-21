package raft

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	pb "github.com/junyoung/core-x/proto/pb"
)

// helpers for joint consensus tests
func makeClusterConfig(voters ...string) ClusterConfig {
	return ClusterConfig{Phase: ConfigPhaseStable, Voters: voters}
}

func makeJointConfig(oldVoters, newVoters []string) ClusterConfig {
	return ClusterConfig{Phase: ConfigPhaseJoint, Voters: oldVoters, NewVoters: newVoters}
}

// --- Unit tests: ClusterConfig ---

func TestClusterConfig_IsJoint(t *testing.T) {
	stable := makeClusterConfig("a", "b")
	if stable.IsJoint() {
		t.Error("stable config must not be joint")
	}
	joint := makeJointConfig([]string{"a", "b"}, []string{"c", "d"})
	if !joint.IsJoint() {
		t.Error("joint config must be joint")
	}
}

func TestClusterConfig_QuorumSizes(t *testing.T) {
	tests := []struct {
		name      string
		cfg       ClusterConfig
		wantOld   int
		wantNew   int
	}{
		// Old = [peer1, peer2] + self = 3 total → quorum = 2
		{"3-node stable", makeClusterConfig("p1", "p2"), 2, 0},
		// Old = [p1, p2] + self = 3 → quorum = 2
		// New = [p1, p2, p3, p4] + self = 5 → quorum = 3
		{"3→5 joint", makeJointConfig([]string{"p1", "p2"}, []string{"p1", "p2", "p3", "p4"}), 2, 3},
		// Old = [p1..p4] + self = 5 → quorum = 3
		// New = [p1, p2] + self = 3 → quorum = 2
		{"5→3 joint", makeJointConfig(
			[]string{"p1", "p2", "p3", "p4"},
			[]string{"p1", "p2"},
		), 3, 2},
		// Single-node: Voters=[] + self = 1 → quorum = 1
		{"single-node stable", makeClusterConfig(), 1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.OldQuorumSize(); got != tc.wantOld {
				t.Errorf("OldQuorumSize: got %d want %d", got, tc.wantOld)
			}
			if tc.cfg.IsJoint() {
				if got := tc.cfg.NewQuorumSize(); got != tc.wantNew {
					t.Errorf("NewQuorumSize: got %d want %d", got, tc.wantNew)
				}
			}
		})
	}
}

func TestClusterConfig_AllPeers_Deduplicated(t *testing.T) {
	cfg := makeJointConfig(
		[]string{"a", "b", "c"},
		[]string{"b", "c", "d", "e"},
	)
	peers := cfg.AllPeers()
	seen := make(map[string]int)
	for _, p := range peers {
		seen[p]++
	}
	for addr, count := range seen {
		if count > 1 {
			t.Errorf("AllPeers: duplicate %q (count=%d)", addr, count)
		}
	}
	if len(peers) != 5 {
		t.Errorf("AllPeers: want 5 unique peers, got %d: %v", len(peers), peers)
	}
}

// --- Unit tests: quorumFor ---

func TestQuorumFor_Stable(t *testing.T) {
	n := &RaftNode{}

	// 3-node cluster: Voters=[p1, p2] (self implicit)
	cfg := makeClusterConfig("p1", "p2")

	// Self only → 1/3: not a majority (need 2)
	if n.quorumFor(cfg, func(addr string) bool { return false }) {
		t.Error("self-only must not be quorum in 3-node")
	}

	// Self + p1 → 2/3: majority
	if !n.quorumFor(cfg, func(addr string) bool { return addr == "p1" }) {
		t.Error("self+p1 must be quorum in 3-node")
	}

	// All → 3/3: majority
	if !n.quorumFor(cfg, func(addr string) bool { return true }) {
		t.Error("all must be quorum in 3-node")
	}
}

func TestQuorumFor_Joint_BothMustSatisfy(t *testing.T) {
	n := &RaftNode{}

	// Old = [p1, p2] + self = 3 (need 2)
	// New = [p3, p4] + self = 3 (need 2)
	cfg := makeJointConfig([]string{"p1", "p2"}, []string{"p3", "p4"})

	// Only old quorum satisfied (self+p1), new not (self only 1/3) → false
	if n.quorumFor(cfg, func(addr string) bool { return addr == "p1" }) {
		t.Error("only old quorum satisfied must not pass")
	}

	// Only new quorum satisfied (self+p3), old not (self only 1/3) → false
	if n.quorumFor(cfg, func(addr string) bool { return addr == "p3" }) {
		t.Error("only new quorum satisfied must not pass")
	}

	// Both old (self+p1) and new (self+p3) satisfied → true
	if !n.quorumFor(cfg, func(addr string) bool {
		return addr == "p1" || addr == "p3"
	}) {
		t.Error("both quorums satisfied must pass")
	}
}

func TestQuorumFor_SingleNode(t *testing.T) {
	n := &RaftNode{}
	cfg := makeClusterConfig() // no peers; Voters=[]
	// Self alone = 1/1: always a quorum
	if !n.quorumFor(cfg, func(addr string) bool { return false }) {
		t.Error("single node must always be quorum")
	}
}

// --- Unit tests: applyConfigEntry ---

func TestApplyConfigEntry_Joint(t *testing.T) {
	n := &RaftNode{
		clusterConfig: makeClusterConfig("p1", "p2"),
	}

	payload := ConfigChangePayload{
		Phase:     "joint",
		Voters:    []string{"p1", "p2"},
		NewVoters: []string{"p3", "p4"},
	}
	data, _ := json.Marshal(payload)
	entry := LogEntry{Index: 1, Term: 1, Type: EntryTypeConfig, Data: data}

	n.applyConfigEntry(entry)

	if !n.clusterConfig.IsJoint() {
		t.Error("after joint entry, config must be joint")
	}
	if len(n.clusterConfig.Voters) != 2 {
		t.Errorf("Voters want 2, got %d", len(n.clusterConfig.Voters))
	}
	if len(n.clusterConfig.NewVoters) != 2 {
		t.Errorf("NewVoters want 2, got %d", len(n.clusterConfig.NewVoters))
	}
}

func TestApplyConfigEntry_Stable(t *testing.T) {
	n := &RaftNode{
		clusterConfig: makeJointConfig([]string{"p1", "p2"}, []string{"p3", "p4"}),
	}

	payload := ConfigChangePayload{
		Phase:  "stable",
		Voters: []string{"p3", "p4"},
	}
	data, _ := json.Marshal(payload)
	entry := LogEntry{Index: 2, Term: 1, Type: EntryTypeConfig, Data: data}

	n.applyConfigEntry(entry)

	if n.clusterConfig.IsJoint() {
		t.Error("after stable entry, config must not be joint")
	}
	if len(n.clusterConfig.Voters) != 2 {
		t.Errorf("Voters want 2, got %d", len(n.clusterConfig.Voters))
	}
	if n.clusterConfig.NewVoters != nil {
		t.Error("NewVoters must be nil in stable phase")
	}
}

func TestApplyConfigEntry_MalformedData(t *testing.T) {
	n := &RaftNode{
		clusterConfig: makeClusterConfig("p1"),
	}
	original := n.clusterConfig

	entry := LogEntry{Index: 1, Term: 1, Type: EntryTypeConfig, Data: []byte("not-json")}
	n.applyConfigEntry(entry) // must not panic; logs error

	// Config must remain unchanged on decode error.
	if n.clusterConfig.Phase != original.Phase {
		t.Error("malformed entry must not change clusterConfig")
	}
}

// --- Unit tests: ComputeNewVoters ---

func TestComputeNewVoters_Add(t *testing.T) {
	n := &RaftNode{
		clusterConfig: makeClusterConfig("p1", "p2"),
	}

	newVoters, err := n.ComputeNewVoters("add", []string{"p3", "p4"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(newVoters) != 4 {
		t.Errorf("want 4 voters, got %d: %v", len(newVoters), newVoters)
	}
}

func TestComputeNewVoters_Remove(t *testing.T) {
	n := &RaftNode{
		clusterConfig: makeClusterConfig("p1", "p2", "p3", "p4"),
	}

	newVoters, err := n.ComputeNewVoters("remove", []string{"p3", "p4"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(newVoters) != 2 {
		t.Errorf("want 2 voters, got %d: %v", len(newVoters), newVoters)
	}
	for _, v := range newVoters {
		if v == "p3" || v == "p4" {
			t.Errorf("removed peer %q must not be in result", v)
		}
	}
}

func TestComputeNewVoters_InProgress(t *testing.T) {
	n := &RaftNode{
		clusterConfig: makeJointConfig([]string{"p1"}, []string{"p2"}),
	}

	_, err := n.ComputeNewVoters("add", []string{"p3"})
	if err != ErrConfigChangeInProgress {
		t.Errorf("want ErrConfigChangeInProgress, got %v", err)
	}
}

func TestComputeNewVoters_AddIdempotent(t *testing.T) {
	n := &RaftNode{
		clusterConfig: makeClusterConfig("p1", "p2"),
	}
	// Adding an already-present peer must be idempotent.
	newVoters, err := n.ComputeNewVoters("add", []string{"p1", "p3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(newVoters) != 3 {
		t.Errorf("want 3 voters (p1,p2,p3), got %d: %v", len(newVoters), newVoters)
	}
}

// --- Integration test: single-node ProposeConfigChange ---
// Tests that a single-node leader can complete a config change round-trip.

func TestProposeConfigChange_SingleNode_AddVoters(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	node := NewRaftNode("self", nil, nil, nil)
	node.ForceRole(RoleLeader, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go node.Run(ctx)

	// Propose some data first so the node is actively committing.
	_, _, ok := node.Propose([]byte(`{"op":"set","key":"k","value":"v"}`))
	if !ok {
		t.Fatal("Propose returned not leader")
	}

	// Wait briefly for the single-node to commit.
	time.Sleep(200 * time.Millisecond)

	// Now propose a config change adding two hypothetical peers.
	// In a single-node test we can't actually connect to them; we're testing
	// that the protocol phases run correctly, not gRPC connectivity.
	doneCh, err := node.ProposeConfigChange(ctx, []string{"peer1:9001", "peer2:9002"})
	if err != nil {
		t.Fatalf("ProposeConfigChange error: %v", err)
	}

	// Wait for Phase A (joint) to commit. In single-node mode this requires
	// quorumFor on the joint config: self must satisfy both old and new quorum.
	// old = [] + self = 1 ≥ 1 ✓; new = [peer1,peer2] + self = 1/3, need 2 → fails.
	// This is expected: a single-node cannot satisfy majority of new=3 cluster.
	// So doneCh should close (fail) or time out. We just verify no panic/deadlock.
	select {
	case _, ok := <-doneCh:
		if ok {
			t.Log("config change completed (unexpected in single-node with 3-node new config)")
		} else {
			t.Log("config change failed/closed (expected: cannot satisfy new 3-node quorum)")
		}
	case <-ctx.Done():
		t.Log("context expired (expected: cannot satisfy new quorum)")
	}
}

// TestProposeConfigChange_ErrNotLeader verifies non-leaders are rejected.
func TestProposeConfigChange_ErrNotLeader(t *testing.T) {
	node := NewRaftNode("self", nil, nil, nil)
	// Default role is Follower.

	_, err := node.ProposeConfigChange(context.Background(), []string{"peer1:9001"})
	if err != ErrNotLeader {
		t.Errorf("want ErrNotLeader, got %v", err)
	}
}

// TestProposeConfigChange_ErrInProgress verifies concurrent protection.
func TestProposeConfigChange_ErrInProgress(t *testing.T) {
	node := &RaftNode{
		role:          RoleLeader,
		clusterConfig: makeJointConfig([]string{"p1"}, []string{"p2"}),
	}

	_, err := node.ProposeConfigChange(context.Background(), []string{"p3"})
	if err != ErrConfigChangeInProgress {
		t.Errorf("want ErrConfigChangeInProgress, got %v", err)
	}
}

// --- Test: proposeEntry sets clusterConfig immediately on EntryTypeConfig ---

func TestProposeEntry_ConfigEntryAppliedImmediately(t *testing.T) {
	node := NewRaftNode("self", nil, nil, nil)
	node.ForceRole(RoleLeader, 1)

	// Manually propose a config entry to test immediate apply.
	payload := ConfigChangePayload{
		Phase:     "joint",
		Voters:    []string{"p1"},
		NewVoters: []string{"p2"},
	}
	data, _ := json.Marshal(payload)

	node.mu.Lock()
	idx, _, ok := node.proposeEntry(LogEntry{Type: EntryTypeConfig, Data: data})
	node.mu.Unlock()

	if !ok {
		t.Fatal("proposeEntry returned not leader")
	}
	if idx != 1 {
		t.Errorf("expected index 1, got %d", idx)
	}

	// clusterConfig must have been updated immediately (without waiting for commit).
	node.mu.Lock()
	cfg := node.clusterConfig
	node.mu.Unlock()

	if !cfg.IsJoint() {
		t.Error("clusterConfig must be joint immediately after proposeEntry")
	}
}

// --- Test: HandleAppendEntries applies config entry on append ---

func TestHandleAppendEntries_ConfigEntryAppliedOnAppend(t *testing.T) {
	node := NewRaftNode("follower", nil, nil, nil)

	payload := ConfigChangePayload{
		Phase:     "joint",
		Voters:    []string{"leader"},
		NewVoters: []string{"new1"},
	}
	data, _ := json.Marshal(payload)

	args := AppendEntriesArgs{
		Term:         1,
		LeaderID:     "leader",
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Entries: []*pb.LogEntry{
			{Index: 1, Term: 1, EntryType: uint32(EntryTypeConfig), Data: data},
		},
		LeaderCommit: 0,
	}

	result := node.HandleAppendEntries(args)
	if !result.Success {
		t.Fatal("HandleAppendEntries must succeed")
	}

	// Config must be applied immediately on append.
	node.mu.Lock()
	cfg := node.clusterConfig
	node.mu.Unlock()

	if !cfg.IsJoint() {
		t.Error("clusterConfig must be joint after HandleAppendEntries with config entry")
	}
}

// --- Test: WAL encode/decode round-trip with EntryTypeConfig ---

func TestWALLogStore_ConfigEntryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewWALLogStore(dir + "/config_test.wal")
	if err != nil {
		t.Fatalf("NewWALLogStore: %v", err)
	}
	defer store.Close()

	payload := ConfigChangePayload{
		Phase:     "joint",
		Voters:    []string{"p1", "p2"},
		NewVoters: []string{"p3", "p4"},
	}
	data, _ := json.Marshal(payload)

	entry := LogEntry{Index: 1, Term: 1, Type: EntryTypeConfig, Data: data}
	if err := store.Append(entry); err != nil {
		t.Fatalf("Append: %v", err)
	}

	entries, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	got := entries[0]
	if got.Type != EntryTypeConfig {
		t.Errorf("Type: want EntryTypeConfig(%d), got %d", EntryTypeConfig, got.Type)
	}
	if got.Index != 1 || got.Term != 1 {
		t.Errorf("Index/Term: want 1/1, got %d/%d", got.Index, got.Term)
	}

	var gotPayload ConfigChangePayload
	if err := json.Unmarshal(got.Data, &gotPayload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if gotPayload.Phase != "joint" {
		t.Errorf("Phase: want joint, got %s", gotPayload.Phase)
	}
	if len(gotPayload.NewVoters) != 2 {
		t.Errorf("NewVoters: want 2, got %d", len(gotPayload.NewVoters))
	}
}

// TestWALLogStore_MixedEntryTypes tests that command and config entries coexist correctly.
func TestWALLogStore_MixedEntryTypes(t *testing.T) {
	dir := t.TempDir()
	store, err := NewWALLogStore(dir + "/mixed.wal")
	if err != nil {
		t.Fatalf("NewWALLogStore: %v", err)
	}
	defer store.Close()

	cmdEntry := LogEntry{Index: 1, Term: 1, Type: EntryTypeCommand, Data: []byte(`{"op":"set"}`)}
	cfgEntry := LogEntry{Index: 2, Term: 1, Type: EntryTypeConfig, Data: []byte(`{"phase":"joint"}`)}

	if err := store.Append(cmdEntry); err != nil {
		t.Fatalf("Append cmd: %v", err)
	}
	if err := store.Append(cfgEntry); err != nil {
		t.Fatalf("Append cfg: %v", err)
	}

	entries, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	if entries[0].Type != EntryTypeCommand {
		t.Errorf("entries[0] type: want command, got %d", entries[0].Type)
	}
	if entries[1].Type != EntryTypeConfig {
		t.Errorf("entries[1] type: want config, got %d", entries[1].Type)
	}
}

// --- Benchmark: quorumFor under stable and joint configs ---

func BenchmarkQuorumFor_Stable_3Node(b *testing.B) {
	n := &RaftNode{}
	cfg := makeClusterConfig("p1", "p2")
	ackFn := func(addr string) bool { return addr == "p1" }

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = n.quorumFor(cfg, ackFn)
	}
}

func BenchmarkQuorumFor_Joint_5Node(b *testing.B) {
	n := &RaftNode{}
	cfg := makeJointConfig(
		[]string{"p1", "p2"},
		[]string{"p1", "p2", "p3", "p4"},
	)
	ackFn := func(addr string) bool {
		return addr == "p1" || addr == "p3"
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = n.quorumFor(cfg, ackFn)
	}
}
