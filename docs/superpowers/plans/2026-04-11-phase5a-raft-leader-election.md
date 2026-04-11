# Phase 5a: Raft Leader Election Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 정적 env 기반 Primary/Replica 역할을 대체하는 Raft leader election을 구현하여, Primary 장애 시 자동으로 새 Leader를 선출한다.

**Architecture:** 새 `internal/infrastructure/raft/` 패키지에 Follower→Candidate→Leader 상태 머신을 구현한다. gRPC로 RequestVote + AppendEntries(heartbeat) RPC를 추가하고, 클러스터 모드에서 RaftNode를 background goroutine으로 실행한다. Phase 5a는 기존 WAL 복제 레이어를 건드리지 않는다 — 복제와 Raft log 통합은 Phase 5b에서 한다.

**Tech Stack:** Go stdlib (`sync`, `sync/atomic`, `math/rand`, `time`), gRPC, protobuf

---

## File Map

| 파일 | 역할 |
|------|------|
| `proto/ingest.proto` | RaftService RPC 정의 추가 (RequestVote, AppendEntries) |
| `proto/pb/ingest.pb.go` | protoc 재생성 |
| `proto/pb/ingest_grpc.pb.go` | protoc 재생성 |
| `internal/infrastructure/raft/state.go` | RaftRole enum, 상수 (타임아웃 값) |
| `internal/infrastructure/raft/node.go` | RaftNode: 상태 머신 run loop |
| `internal/infrastructure/raft/client.go` | 피어에게 RequestVote / AppendEntries 전송 |
| `internal/infrastructure/raft/server.go` | gRPC handler: 수신 RPC 처리 |
| `internal/infrastructure/raft/node_test.go` | 상태 전환 단위 테스트 |
| `internal/infrastructure/raft/server_test.go` | RPC handler 단위 테스트 |
| `internal/infrastructure/grpc/server.go` | `RegisterRaftService` 메서드 추가 |
| `internal/infrastructure/metrics/raft.go` | Raft Prometheus 메트릭 (term, role, election count) |
| `cmd/main.go` | RaftNode wiring |
| `docs/adr/0010-raft-leader-election.md` | ADR 문서 |

---

## Task 1: Proto — RaftService RPC 정의 추가

**Files:**
- Modify: `proto/ingest.proto`
- Regenerate: `proto/pb/ingest.pb.go`, `proto/pb/ingest_grpc.pb.go`

- [ ] **Step 1: `proto/ingest.proto` 끝에 RaftService 추가**

`proto/ingest.proto` 파일 끝에 다음을 추가한다:

```protobuf
// RaftService implements Phase 5a leader election RPCs.
// Phase 5b will extend AppendEntries with log entries.
service RaftService {
  // RequestVote is sent by a Candidate to collect votes.
  rpc RequestVote(RequestVoteRequest) returns (RequestVoteResponse);
  // AppendEntries is sent by the Leader as a heartbeat.
  // In Phase 5a it carries no log entries.
  rpc AppendEntries(AppendEntriesRequest) returns (AppendEntriesResponse);
}

// RequestVoteRequest is sent by a Candidate to each peer.
message RequestVoteRequest {
  int64  term         = 1; // Candidate's current term
  string candidate_id = 2; // Candidate's node ID
}

// RequestVoteResponse is the peer's reply to RequestVote.
message RequestVoteResponse {
  int64 term         = 1; // Responder's current term (Candidate uses to update itself)
  bool  vote_granted = 2;
}

// AppendEntriesRequest is sent by the Leader as a periodic heartbeat.
message AppendEntriesRequest {
  int64  term      = 1; // Leader's current term
  string leader_id = 2; // Leader's node ID
}

// AppendEntriesResponse is the Follower's reply to AppendEntries.
message AppendEntriesResponse {
  int64 term    = 1; // Responder's current term
  bool  success = 2;
}
```

- [ ] **Step 2: protoc로 Go 코드 재생성**

```bash
cd /Users/junyoung/workspace/personal/core-x
protoc \
  --go_out=. \
  --go_opt=paths=source_relative \
  --go-grpc_out=. \
  --go-grpc_opt=paths=source_relative \
  proto/ingest.proto
```

Expected: `proto/pb/ingest.pb.go`와 `proto/pb/ingest_grpc.pb.go`에 `RaftServiceServer`, `RaftServiceClient`, `RegisterRaftServiceServer` 등이 추가된다.

- [ ] **Step 3: 컴파일 확인**

```bash
go build ./...
```

Expected: 오류 없이 빌드 성공.

- [ ] **Step 4: 커밋**

```bash
git add proto/ingest.proto proto/pb/ingest.pb.go proto/pb/ingest_grpc.pb.go
git commit -m "feat(raft): add RaftService proto — RequestVote + AppendEntries RPC"
```

---

## Task 2: RaftState — Role enum + 타임아웃 상수

**Files:**
- Create: `internal/infrastructure/raft/state.go`
- Create: `internal/infrastructure/raft/node_test.go` (일부)

- [ ] **Step 1: 실패하는 테스트 작성**

`internal/infrastructure/raft/node_test.go`:

```go
package raft_test

import (
	"testing"

	"github.com/junyoung/core-x/internal/infrastructure/raft"
)

func TestRaftRoleString(t *testing.T) {
	tests := []struct {
		role raft.RaftRole
		want string
	}{
		{raft.RoleFollower, "follower"},
		{raft.RoleCandidate, "candidate"},
		{raft.RoleLeader, "leader"},
	}
	for _, tc := range tests {
		if got := tc.role.String(); got != tc.want {
			t.Errorf("role %d: got %q, want %q", tc.role, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: 실패 확인**

```bash
go test ./internal/infrastructure/raft/...
```

Expected: `cannot find package` 에러.

- [ ] **Step 3: `internal/infrastructure/raft/state.go` 구현**

```go
// Package raft implements Phase 5a Raft leader election.
// Phase 5b will extend this with log replication.
package raft

import "time"

// RaftRole represents the current Raft state of a node.
type RaftRole int32

const (
	// RoleFollower is the initial role. Follower expects heartbeats from a Leader.
	// On election timeout it transitions to Candidate.
	RoleFollower RaftRole = iota
	// RoleCandidate is active during an election. Candidate sends RequestVote to peers.
	RoleCandidate
	// RoleLeader sends periodic heartbeats (AppendEntries) to all peers.
	RoleLeader
)

// String returns a human-readable role name for logging.
func (r RaftRole) String() string {
	switch r {
	case RoleFollower:
		return "follower"
	case RoleCandidate:
		return "candidate"
	case RoleLeader:
		return "leader"
	default:
		return "unknown"
	}
}

// Raft timing constants.
// ElectionTimeoutMin / Max: Raft paper recommends 150–300ms.
// HeartbeatInterval must be << ElectionTimeoutMin to prevent spurious elections.
const (
	ElectionTimeoutMin = 150 * time.Millisecond
	ElectionTimeoutMax = 300 * time.Millisecond
	HeartbeatInterval  = 50 * time.Millisecond
)
```

- [ ] **Step 4: 테스트 통과 확인**

```bash
go test ./internal/infrastructure/raft/...
```

Expected: `PASS`.

- [ ] **Step 5: 커밋**

```bash
git add internal/infrastructure/raft/state.go internal/infrastructure/raft/node_test.go
git commit -m "feat(raft): RaftRole enum + timing constants"
```

---

## Task 3: RaftClient — 피어에게 RequestVote / AppendEntries 전송

**Files:**
- Create: `internal/infrastructure/raft/client.go`

- [ ] **Step 1: `internal/infrastructure/raft/client.go` 작성**

```go
package raft

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/junyoung/core-x/proto/pb"
)

const rpcTimeout = 100 * time.Millisecond

// RaftClient sends Raft RPCs to a single peer.
type RaftClient struct {
	addr string
	conn *grpc.ClientConn
	rpc  pb.RaftServiceClient
}

// NewRaftClient dials the peer at addr and returns a ready-to-use client.
// The caller must call Close when done.
func NewRaftClient(addr string) (*RaftClient, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, err
	}
	return &RaftClient{
		addr: addr,
		conn: conn,
		rpc:  pb.NewRaftServiceClient(conn),
	}, nil
}

// RequestVote sends a RequestVote RPC and returns the response.
// Returns nil, err on timeout or network failure.
func (c *RaftClient) RequestVote(term int64, candidateID string) (*pb.RequestVoteResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	return c.rpc.RequestVote(ctx, &pb.RequestVoteRequest{
		Term:        term,
		CandidateId: candidateID,
	})
}

// AppendEntries sends an AppendEntries (heartbeat) RPC.
// Returns nil, err on timeout or network failure.
func (c *RaftClient) AppendEntries(term int64, leaderID string) (*pb.AppendEntriesResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	return c.rpc.AppendEntries(ctx, &pb.AppendEntriesRequest{
		Term:     term,
		LeaderId: leaderID,
	})
}

// Close releases the gRPC connection.
func (c *RaftClient) Close() error {
	return c.conn.Close()
}

// PeerClients manages RaftClient instances for all peers.
type PeerClients struct {
	clients map[string]*RaftClient // addr → client
}

// NewPeerClients dials all peer addresses and returns a PeerClients.
// Closes all connections if any dial fails.
func NewPeerClients(addrs []string) (*PeerClients, error) {
	clients := make(map[string]*RaftClient, len(addrs))
	for _, addr := range addrs {
		c, err := NewRaftClient(addr)
		if err != nil {
			for _, existing := range clients {
				existing.Close()
			}
			return nil, err
		}
		clients[addr] = c
	}
	return &PeerClients{clients: clients}, nil
}

// All returns all peer clients.
func (p *PeerClients) All() []*RaftClient {
	result := make([]*RaftClient, 0, len(p.clients))
	for _, c := range p.clients {
		result = append(result, c)
	}
	return result
}

// Close closes all peer connections.
func (p *PeerClients) Close() {
	for _, c := range p.clients {
		c.Close()
	}
}
```

- [ ] **Step 2: 컴파일 확인**

```bash
go build ./internal/infrastructure/raft/...
```

Expected: 오류 없이 빌드 성공.

- [ ] **Step 3: 커밋**

```bash
git add internal/infrastructure/raft/client.go
git commit -m "feat(raft): RaftClient — RequestVote + AppendEntries sender"
```

---

## Task 4: RaftServer — 수신 RPC handler

**Files:**
- Create: `internal/infrastructure/raft/server.go`
- Create: `internal/infrastructure/raft/server_test.go`

- [ ] **Step 1: 실패하는 테스트 작성**

`internal/infrastructure/raft/server_test.go`:

```go
package raft_test

import (
	"context"
	"testing"

	"github.com/junyoung/core-x/internal/infrastructure/raft"
	pb "github.com/junyoung/core-x/proto/pb"
)

// fakeNode simulates the subset of RaftNode the server needs.
type fakeNode struct {
	handleAppendEntriesCalled bool
	handleRequestVoteCalled   bool
	grantVote                 bool
	currentTerm               int64
}

func (f *fakeNode) HandleAppendEntries(term int64, leaderID string) (currentTerm int64, success bool) {
	f.handleAppendEntriesCalled = true
	return f.currentTerm, true
}

func (f *fakeNode) HandleRequestVote(term int64, candidateID string) (currentTerm int64, voteGranted bool) {
	f.handleRequestVoteCalled = true
	return f.currentTerm, f.grantVote
}

func TestRaftServer_AppendEntries(t *testing.T) {
	node := &fakeNode{currentTerm: 1}
	srv := raft.NewRaftServer(node)

	resp, err := srv.AppendEntries(context.Background(), &pb.AppendEntriesRequest{
		Term:     1,
		LeaderId: "node-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Success {
		t.Error("expected success=true")
	}
	if !node.handleAppendEntriesCalled {
		t.Error("expected HandleAppendEntries to be called")
	}
}

func TestRaftServer_RequestVote_Granted(t *testing.T) {
	node := &fakeNode{currentTerm: 1, grantVote: true}
	srv := raft.NewRaftServer(node)

	resp, err := srv.RequestVote(context.Background(), &pb.RequestVoteRequest{
		Term:        2,
		CandidateId: "node-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.VoteGranted {
		t.Error("expected vote_granted=true")
	}
}
```

- [ ] **Step 2: 실패 확인**

```bash
go test ./internal/infrastructure/raft/... -run TestRaftServer
```

Expected: `undefined: raft.RaftServer` 에러.

- [ ] **Step 3: `internal/infrastructure/raft/server.go` 구현**

```go
package raft

import (
	"context"

	pb "github.com/junyoung/core-x/proto/pb"
)

// RaftHandler is the interface RaftServer calls into on incoming RPCs.
// Separating this interface from RaftNode allows clean testing with fakes.
type RaftHandler interface {
	HandleAppendEntries(term int64, leaderID string) (currentTerm int64, success bool)
	HandleRequestVote(term int64, candidateID string) (currentTerm int64, voteGranted bool)
}

// RaftServer implements pb.RaftServiceServer.
// It delegates all state decisions to the RaftHandler (normally *RaftNode).
type RaftServer struct {
	pb.UnimplementedRaftServiceServer
	node RaftHandler
}

// NewRaftServer creates a RaftServer backed by the given handler.
func NewRaftServer(node RaftHandler) *RaftServer {
	return &RaftServer{node: node}
}

// AppendEntries handles incoming heartbeat RPCs from the Leader.
func (s *RaftServer) AppendEntries(_ context.Context, req *pb.AppendEntriesRequest) (*pb.AppendEntriesResponse, error) {
	term, success := s.node.HandleAppendEntries(req.Term, req.LeaderId)
	return &pb.AppendEntriesResponse{Term: term, Success: success}, nil
}

// RequestVote handles incoming vote requests from Candidates.
func (s *RaftServer) RequestVote(_ context.Context, req *pb.RequestVoteRequest) (*pb.RequestVoteResponse, error) {
	term, granted := s.node.HandleRequestVote(req.Term, req.CandidateId)
	return &pb.RequestVoteResponse{Term: term, VoteGranted: granted}, nil
}
```

- [ ] **Step 4: 테스트 통과 확인**

```bash
go test ./internal/infrastructure/raft/... -run TestRaftServer -v
```

Expected:
```
--- PASS: TestRaftServer_AppendEntries
--- PASS: TestRaftServer_RequestVote_Granted
PASS
```

- [ ] **Step 5: 커밋**

```bash
git add internal/infrastructure/raft/server.go internal/infrastructure/raft/server_test.go
git commit -m "feat(raft): RaftServer — gRPC handler for AppendEntries + RequestVote"
```

---

## Task 5: RaftNode — Follower 상태 + election timer

**Files:**
- Create: `internal/infrastructure/raft/node.go`
- Modify: `internal/infrastructure/raft/node_test.go`

- [ ] **Step 1: 실패하는 테스트 추가**

`internal/infrastructure/raft/node_test.go`에 추가:

```go
func TestRaftNode_StartsAsFollower(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil)
	if got := node.Role(); got != raft.RoleFollower {
		t.Errorf("expected RoleFollower, got %v", got)
	}
	if got := node.Term(); got != 0 {
		t.Errorf("expected term 0, got %d", got)
	}
}

func TestRaftNode_HandleAppendEntries_HigherTerm_UpdatesTerm(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil)

	term, success := node.HandleAppendEntries(5, "node-2")
	if !success {
		t.Error("expected success=true")
	}
	if term != 5 {
		t.Errorf("expected returned term=5, got %d", term)
	}
	if got := node.Term(); got != 5 {
		t.Errorf("expected node term=5, got %d", got)
	}
	if got := node.Role(); got != raft.RoleFollower {
		t.Errorf("expected RoleFollower after AppendEntries, got %v", got)
	}
}

func TestRaftNode_HandleAppendEntries_LowerTerm_Rejected(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil)
	node.HandleAppendEntries(10, "node-2") // advance term to 10

	term, success := node.HandleAppendEntries(5, "node-3") // stale leader
	if success {
		t.Error("expected success=false for stale term")
	}
	if term != 10 {
		t.Errorf("expected current term 10 in response, got %d", term)
	}
}
```

- [ ] **Step 2: 실패 확인**

```bash
go test ./internal/infrastructure/raft/... -run TestRaftNode
```

Expected: `undefined: raft.NewRaftNode` 에러.

- [ ] **Step 3: `internal/infrastructure/raft/node.go` 구현 (Follower 상태만)**

```go
package raft

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// RaftNode implements the Raft consensus state machine.
// It manages transitions between Follower, Candidate, and Leader roles.
//
// Thread safety: HandleAppendEntries and HandleRequestVote are called from
// gRPC goroutines. The run loop runs in a separate goroutine. Access to
// mutable state is protected by mu.
type RaftNode struct {
	id    string
	peers *PeerClients // nil in tests / single-node mode

	mu          sync.Mutex
	currentTerm int64
	votedFor    string   // nodeID that received our vote this term; "" = none
	role        RaftRole

	// resetCh is sent to from Handle* methods to reset the election timer.
	// Buffer of 1: sender never blocks.
	resetCh chan struct{}
}

// NewRaftNode creates a RaftNode. peers may be nil for single-node or test use.
func NewRaftNode(id string, peers *PeerClients) *RaftNode {
	return &RaftNode{
		id:      id,
		peers:   peers,
		role:    RoleFollower,
		resetCh: make(chan struct{}, 1),
	}
}

// Role returns the current Raft role. Safe to call from any goroutine.
func (n *RaftNode) Role() RaftRole {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.role
}

// Term returns the current term. Safe to call from any goroutine.
func (n *RaftNode) Term() int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.currentTerm
}

// HandleAppendEntries processes an incoming AppendEntries (heartbeat) RPC.
// Implements RaftHandler.
func (n *RaftNode) HandleAppendEntries(term int64, leaderID string) (currentTerm int64, success bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if term < n.currentTerm {
		// Stale leader — reject.
		return n.currentTerm, false
	}

	// Valid heartbeat: update term, revert to Follower.
	if term > n.currentTerm {
		n.currentTerm = term
		n.votedFor = ""
	}
	n.role = RoleFollower

	// Signal the run loop to reset the election timer (non-blocking).
	select {
	case n.resetCh <- struct{}{}:
	default:
	}

	return n.currentTerm, true
}

// HandleRequestVote processes an incoming RequestVote RPC.
// Implements RaftHandler.
func (n *RaftNode) HandleRequestVote(term int64, candidateID string) (currentTerm int64, voteGranted bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if term < n.currentTerm {
		return n.currentTerm, false
	}

	if term > n.currentTerm {
		// Step down: higher term seen.
		n.currentTerm = term
		n.votedFor = ""
		n.role = RoleFollower
	}

	// Grant vote if we haven't voted for anyone else this term.
	if n.votedFor == "" || n.votedFor == candidateID {
		n.votedFor = candidateID
		// Reset election timer: we just heard from a valid Candidate.
		select {
		case n.resetCh <- struct{}{}:
		default:
		}
		return n.currentTerm, true
	}

	return n.currentTerm, false
}

// Run starts the Raft state machine. Blocks until ctx is cancelled.
// Call this in a dedicated goroutine.
func (n *RaftNode) Run(ctx context.Context) {
	for {
		n.mu.Lock()
		role := n.role
		n.mu.Unlock()

		switch role {
		case RoleFollower:
			n.runFollower(ctx)
		case RoleCandidate:
			n.runCandidate(ctx)
		case RoleLeader:
			n.runLeader(ctx)
		}

		if ctx.Err() != nil {
			return
		}
	}
}

// runFollower runs the Follower state until election timeout or ctx cancel.
func (n *RaftNode) runFollower(ctx context.Context) {
	timer := time.NewTimer(randomElectionTimeout())
	defer timer.Stop()

	slog.Info("raft: follower", "id", n.id, "term", n.Term())

	for {
		select {
		case <-ctx.Done():
			return
		case <-n.resetCh:
			// Heartbeat or vote grant received: reset timer.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(randomElectionTimeout())
		case <-timer.C:
			// Election timeout: become Candidate.
			n.mu.Lock()
			n.role = RoleCandidate
			n.mu.Unlock()
			return
		}
	}
}

// randomElectionTimeout returns a random duration in [ElectionTimeoutMin, ElectionTimeoutMax].
func randomElectionTimeout() time.Duration {
	delta := ElectionTimeoutMax - ElectionTimeoutMin
	//nolint:gosec // non-cryptographic random is correct for Raft election timeouts
	return ElectionTimeoutMin + time.Duration(rand.Int63n(int64(delta)))
}

// runCandidate and runLeader are implemented in Task 6 and Task 7.
// Stubs to satisfy the Run loop:

func (n *RaftNode) runCandidate(ctx context.Context) {
	// TODO: implemented in Task 6
	_ = ctx
	// Immediately revert to Follower if peers are nil (test/single-node mode).
	n.mu.Lock()
	n.role = RoleFollower
	n.mu.Unlock()
}

func (n *RaftNode) runLeader(ctx context.Context) {
	// TODO: implemented in Task 7
	_ = ctx
	n.mu.Lock()
	n.role = RoleFollower
	n.mu.Unlock()
}

// termCounter is an atomic helper used only in candidate election.
type termCounter struct{ v atomic.Int64 }
```

- [ ] **Step 4: 테스트 통과 확인**

```bash
go test ./internal/infrastructure/raft/... -run "TestRaftNode|TestRaftRole" -v
```

Expected: 모든 테스트 PASS.

- [ ] **Step 5: 커밋**

```bash
git add internal/infrastructure/raft/node.go internal/infrastructure/raft/node_test.go
git commit -m "feat(raft): RaftNode Follower state + election timer + HandleAppendEntries/RequestVote"
```

---

## Task 6: RaftNode — Candidate 상태 + vote 수집

**Files:**
- Modify: `internal/infrastructure/raft/node.go`
- Modify: `internal/infrastructure/raft/node_test.go`

- [ ] **Step 1: 실패하는 테스트 추가**

`internal/infrastructure/raft/node_test.go`에 추가:

```go
func TestRaftNode_HandleRequestVote_GrantsOnce(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil)

	// First request: should grant.
	_, granted1 := node.HandleRequestVote(1, "node-2")
	if !granted1 {
		t.Error("expected first vote to be granted")
	}

	// Second request from different candidate same term: should deny.
	_, granted2 := node.HandleRequestVote(1, "node-3")
	if granted2 {
		t.Error("expected second vote from different candidate to be denied")
	}
}

func TestRaftNode_HandleRequestVote_HigherTermResetsVote(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil)

	// Vote for node-2 in term 1.
	node.HandleRequestVote(1, "node-2")

	// Higher term: vote should reset, node-3 should get the vote.
	_, granted := node.HandleRequestVote(2, "node-3")
	if !granted {
		t.Error("expected vote grant in new higher term")
	}
	if got := node.Term(); got != 2 {
		t.Errorf("expected term=2, got %d", got)
	}
}
```

- [ ] **Step 2: 실패 확인**

```bash
go test ./internal/infrastructure/raft/... -run "TestRaftNode_HandleRequestVote" -v
```

Expected: PASS (이 테스트들은 Task 5에서 이미 구현한 HandleRequestVote로 통과한다).

참고: Candidate의 `runCandidate`는 피어 없이 테스트하기 어렵다. 여기서는 단위 테스트는 Vote 로직에 집중하고, 전체 election 흐름은 Task 8 통합 테스트에서 검증한다.

- [ ] **Step 3: `node.go`의 `runCandidate` 구현**

`node.go`에서 `runCandidate` stub을 다음으로 교체:

```go
// runCandidate runs the Candidate state:
//  1. Increment term and vote for self.
//  2. Send RequestVote to all peers concurrently.
//  3. If majority votes received → become Leader.
//  4. If AppendEntries from valid Leader received (via resetCh) → revert to Follower.
//  5. If election timer fires without majority → new election (stay Candidate).
func (n *RaftNode) runCandidate(ctx context.Context) {
	n.mu.Lock()
	n.currentTerm++
	term := n.currentTerm
	n.votedFor = n.id
	n.role = RoleCandidate
	n.mu.Unlock()

	slog.Info("raft: starting election", "id", n.id, "term", term)

	// votesNeeded = majority of (self + peers). Self vote already counted.
	var peers []*RaftClient
	if n.peers != nil {
		peers = n.peers.All()
	}
	total := len(peers) + 1    // +1 for self
	majority := total/2 + 1
	votes := 1 // self vote

	// Collect votes concurrently with a timeout equal to one election period.
	type voteResult struct {
		term    int64
		granted bool
	}
	resultCh := make(chan voteResult, len(peers))

	for _, peer := range peers {
		peer := peer
		go func() {
			resp, err := peer.RequestVote(term, n.id)
			if err != nil {
				resultCh <- voteResult{term: 0, granted: false}
				return
			}
			resultCh <- voteResult{term: resp.Term, granted: resp.VoteGranted}
		}()
	}

	timer := time.NewTimer(randomElectionTimeout())
	defer timer.Stop()

	collected := 0
	for collected < len(peers) {
		select {
		case <-ctx.Done():
			return
		case <-n.resetCh:
			// AppendEntries from valid Leader received during election: step down.
			n.mu.Lock()
			n.role = RoleFollower
			n.mu.Unlock()
			slog.Info("raft: stepping down during election (heard from leader)", "id", n.id)
			return
		case <-timer.C:
			// Split vote or timeout: start new election.
			slog.Info("raft: election timed out, restarting", "id", n.id, "term", term)
			return // role stays Candidate, Run() will call runCandidate again
		case res := <-resultCh:
			collected++
			if res.term > term {
				// Higher term seen: step down.
				n.mu.Lock()
				n.currentTerm = res.term
				n.votedFor = ""
				n.role = RoleFollower
				n.mu.Unlock()
				slog.Info("raft: stepping down (higher term in vote response)", "id", n.id, "term", res.term)
				return
			}
			if res.granted {
				votes++
			}
			if votes >= majority {
				n.mu.Lock()
				// Re-check term hasn't changed since we started.
				if n.currentTerm == term {
					n.role = RoleLeader
				}
				n.mu.Unlock()
				slog.Info("raft: elected leader", "id", n.id, "term", term, "votes", votes)
				return
			}
		}
	}
}
```

- [ ] **Step 4: 컴파일 및 기존 테스트 통과 확인**

```bash
go test ./internal/infrastructure/raft/... -v
```

Expected: 모든 테스트 PASS.

- [ ] **Step 5: 커밋**

```bash
git add internal/infrastructure/raft/node.go internal/infrastructure/raft/node_test.go
git commit -m "feat(raft): Candidate state — term increment + concurrent RequestVote + vote counting"
```

---

## Task 7: RaftNode — Leader 상태 + heartbeat

**Files:**
- Modify: `internal/infrastructure/raft/node.go`
- Modify: `internal/infrastructure/raft/node_test.go`

- [ ] **Step 1: 실패하는 테스트 추가**

`node_test.go`에 추가:

```go
func TestRaftNode_HandleAppendEntries_ResetsToFollower_WhenLeader(t *testing.T) {
	node := raft.NewRaftNode("node-1", nil)

	// Manually set to Leader in term 3.
	node.HandleAppendEntries(3, "node-2") // sets term=3, follower
	// Now force Leader state for testing.
	node.ForceRole(raft.RoleLeader, 3)

	if got := node.Role(); got != raft.RoleLeader {
		t.Fatalf("expected RoleLeader, got %v", got)
	}

	// Higher-term AppendEntries from another node should demote.
	_, success := node.HandleAppendEntries(4, "node-3")
	if !success {
		t.Error("expected success=true")
	}
	if got := node.Role(); got != raft.RoleFollower {
		t.Errorf("expected RoleFollower after higher-term heartbeat, got %v", got)
	}
}
```

- [ ] **Step 2: `ForceRole` 테스트 헬퍼 추가 (테스트 전용)**

`node.go`에 추가:

```go
// ForceRole sets the role and term directly. Used only in tests.
func (n *RaftNode) ForceRole(role RaftRole, term int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.role = role
	n.currentTerm = term
}
```

- [ ] **Step 3: 실패 확인**

```bash
go test ./internal/infrastructure/raft/... -run "TestRaftNode_HandleAppendEntries_ResetsToFollower_WhenLeader" -v
```

Expected: PASS (HandleAppendEntries 로직은 이미 올바름).

- [ ] **Step 4: `runLeader` stub을 실제 구현으로 교체**

`node.go`에서 `runLeader` stub을 다음으로 교체:

```go
// runLeader runs the Leader state:
//  1. Send AppendEntries (heartbeat) to all peers every HeartbeatInterval.
//  2. If any peer responds with higher term → step down to Follower.
//  3. Exits when ctx is cancelled or role changes (e.g. step-down from HandleAppendEntries).
func (n *RaftNode) runLeader(ctx context.Context) {
	n.mu.Lock()
	term := n.currentTerm
	n.mu.Unlock()

	slog.Info("raft: became leader", "id", n.id, "term", term)

	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()

	var peers []*RaftClient
	if n.peers != nil {
		peers = n.peers.All()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.mu.Lock()
			// Check if we've been demoted (e.g. by HandleAppendEntries from higher-term leader).
			if n.role != RoleLeader || n.currentTerm != term {
				n.mu.Unlock()
				return
			}
			currentTerm := n.currentTerm
			n.mu.Unlock()

			n.sendHeartbeats(ctx, peers, currentTerm)
		}
	}
}

// sendHeartbeats sends AppendEntries to all peers concurrently.
// If any peer returns a higher term, this node steps down.
func (n *RaftNode) sendHeartbeats(ctx context.Context, peers []*RaftClient, term int64) {
	type result struct {
		respTerm int64
		err      error
	}
	ch := make(chan result, len(peers))

	for _, peer := range peers {
		peer := peer
		go func() {
			resp, err := peer.AppendEntries(term, n.id)
			if err != nil {
				ch <- result{err: err}
				return
			}
			ch <- result{respTerm: resp.Term}
		}()
	}

	for range peers {
		select {
		case <-ctx.Done():
			return
		case res := <-ch:
			if res.err != nil {
				continue
			}
			if res.respTerm > term {
				// Higher term seen: step down.
				n.mu.Lock()
				if res.respTerm > n.currentTerm {
					n.currentTerm = res.respTerm
					n.votedFor = ""
				}
				n.role = RoleFollower
				n.mu.Unlock()
				slog.Info("raft: leader stepping down (higher term in heartbeat response)", "id", n.id, "new_term", res.respTerm)
				return
			}
		}
	}
}
```

- [ ] **Step 5: 모든 테스트 통과 확인**

```bash
go test ./internal/infrastructure/raft/... -v
```

Expected: 모든 테스트 PASS.

- [ ] **Step 6: 커밋**

```bash
git add internal/infrastructure/raft/node.go internal/infrastructure/raft/node_test.go
git commit -m "feat(raft): Leader state — periodic heartbeat + step-down on higher term"
```

---

## Task 8: gRPC Server에 RegisterRaftService 추가

**Files:**
- Modify: `internal/infrastructure/grpc/server.go`

- [ ] **Step 1: `server.go`에 `RegisterRaftService` 메서드 추가**

`internal/infrastructure/grpc/server.go`의 마지막 메서드 뒤에 추가:

```go
// RegisterRaftService registers a RaftService implementation.
// Must be called before Serve().
func (s *Server) RegisterRaftService(srv pb.RaftServiceServer) {
	pb.RegisterRaftServiceServer(s.grpcServer, srv)
}
```

- [ ] **Step 2: 컴파일 확인**

```bash
go build ./internal/infrastructure/grpc/...
```

Expected: 오류 없이 빌드 성공.

- [ ] **Step 3: 커밋**

```bash
git add internal/infrastructure/grpc/server.go
git commit -m "feat(raft): add RegisterRaftService to gRPC Server"
```

---

## Task 9: Raft Prometheus 메트릭

**Files:**
- Create: `internal/infrastructure/metrics/raft.go`
- Modify: `cmd/main.go`

- [ ] **Step 1: `internal/infrastructure/metrics/raft.go` 작성**

```go
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// RaftMetricsSource is implemented by RaftNode to expose observable state.
type RaftMetricsSource interface {
	Term() int64
	Role() interface{ String() string }
}

// RegisterRaftMetrics registers Raft Prometheus gauges.
// node may be nil (no-op in standalone mode).
func RegisterRaftMetrics(reg *prometheus.Registry, node RaftMetricsSource) {
	if node == nil {
		return
	}

	termGauge := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "core_x_raft_term",
		Help: "Current Raft term of this node.",
	}, func() float64 {
		return float64(node.Term())
	})

	roleGauge := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "core_x_raft_is_leader",
		Help: "1 if this node is the Raft leader, 0 otherwise.",
	}, func() float64 {
		if node.Role().String() == "leader" {
			return 1
		}
		return 0
	})

	reg.MustRegister(termGauge, roleGauge)
}
```

- [ ] **Step 2: 컴파일 확인**

```bash
go build ./internal/infrastructure/metrics/...
```

Expected: 오류 없이 빌드 성공.

- [ ] **Step 3: 커밋**

```bash
git add internal/infrastructure/metrics/raft.go
git commit -m "feat(raft): Prometheus metrics — core_x_raft_term + core_x_raft_is_leader"
```

---

## Task 10: cmd/main.go — RaftNode wiring

**Files:**
- Modify: `cmd/main.go`

- [ ] **Step 1: `cmd/main.go`에 Raft wiring 추가**

`cmd/main.go`에서 Phase 3b replication 초기화 블록 이후, `inframetrics.RegisterReplicationMetrics(...)` 호출 직전에 다음을 추가:

```go
// --- Phase 5a: Raft Leader Election ------------------------------------
// Raft is only activated in cluster mode (nodeID + grpcAddr configured).
// Single-node standalone mode skips Raft entirely.
var raftNode *infraraft.RaftNode

if nodeID != "" && grpcAddr != "" && grpcSrv != nil {
    // Parse peer gRPC addresses from the ring.
    // Exclude self — we don't send Raft RPCs to ourselves.
    var peerAddrs []string
    if ring != nil {
        for _, n := range ring.Nodes() {
            if n.ID != nodeID {
                peerAddrs = append(peerAddrs, n.Addr)
            }
        }
    }

    var peerClients *infraraft.PeerClients
    if len(peerAddrs) > 0 {
        var pcErr error
        peerClients, pcErr = infraraft.NewPeerClients(peerAddrs)
        if pcErr != nil {
            slog.Error("raft: failed to dial peers", "err", pcErr)
            os.Exit(1)
        }
        defer peerClients.Close()
    }

    raftNode = infraraft.NewRaftNode(nodeID, peerClients)
    raftServer := infraraft.NewRaftServer(raftNode)
    grpcSrv.RegisterRaftService(raftServer)

    raftCtx, raftCancel := context.WithCancel(context.Background())
    defer raftCancel()

    go raftNode.Run(raftCtx)

    slog.Info("raft: started", "node_id", nodeID, "peers", peerAddrs)
}
```

imports 섹션에 추가:
```go
infraraft "github.com/junyoung/core-x/internal/infrastructure/raft"
```

`inframetrics.RegisterReplicationMetrics(...)` 이후에 다음을 추가:
```go
inframetrics.RegisterRaftMetrics(promReg, raftNode)
```

단, `RegisterRaftMetrics`가 `*infraraft.RaftNode`를 받을 수 있도록, `metrics/raft.go`의 `RaftMetricsSource` 인터페이스를 `RaftNode`가 만족하는지 확인한다. `RaftNode.Role()`의 반환 타입이 `RaftRole`이므로, `metrics/raft.go`의 인터페이스를 다음으로 수정:

```go
// RaftMetricsSource is implemented by RaftNode to expose observable state.
type RaftMetricsSource interface {
	Term() int64
	RoleString() string
}
```

그리고 `RaftNode`에 `RoleString()` 헬퍼를 추가:

`node.go`에 추가:
```go
// RoleString returns the current role as a string. Used by metrics.
func (n *RaftNode) RoleString() string {
	return n.Role().String()
}
```

`metrics/raft.go`의 roleGauge를 수정:
```go
roleGauge := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
    Name: "core_x_raft_is_leader",
    Help: "1 if this node is the Raft leader, 0 otherwise.",
}, func() float64 {
    if node.RoleString() == "leader" {
        return 1
    }
    return 0
})
```

- [ ] **Step 2: 전체 빌드 확인**

```bash
go build ./...
```

Expected: 오류 없이 빌드 성공.

- [ ] **Step 3: 단위 테스트 전체 실행**

```bash
go test ./... -timeout 30s
```

Expected: 모든 테스트 PASS.

- [ ] **Step 4: 커밋**

```bash
git add cmd/main.go internal/infrastructure/raft/node.go internal/infrastructure/metrics/raft.go
git commit -m "feat(raft): wire RaftNode into cmd/main.go — cluster mode only"
```

---

## Task 11: ADR-010 문서

**Files:**
- Create: `docs/adr/0010-raft-leader-election.md`

- [ ] **Step 1: ADR 작성**

```markdown
# ADR-010: Raft Leader Election (Phase 5a)

## Status
Accepted

## Context

Phase 3b (ADR-008)에서 구현된 Primary→Replica WAL 스트리밍은 async 복제를 제공하지만,
Primary 장애 시 Replica가 자동으로 Primary 역할을 이어받는 메커니즘이 없다.
운영자가 수동으로 `CORE_X_ROLE=primary`를 설정해야 한다.

이는 다음 문제를 만든다:
- Primary 장애 → 수동 조치 전까지 쓰기 불가 (단일 장애점)
- Kubernetes 재시작 시 기존 Primary가 어느 노드인지 보장 없음

## Decision

### Phase 5a 범위

Phase 5a는 **Leader Election만** 구현한다. WAL log와 Raft log의 통합(AppendEntries에 실제 log entries 포함)은 Phase 5b에서 한다.

Phase 5a의 AppendEntries는 heartbeat 전용 — log entries 없음.

### 상태 머신

```
Follower ──(election timeout)──► Candidate ──(majority votes)──► Leader
    ▲                                │                               │
    └──(AppendEntries from Leader)───┘                               │
    └──────────────────────────────(higher term heartbeat)───────────┘
```

### 타이밍 (Raft 논문 권고)

| 파라미터 | 값 | 이유 |
|---|---|---|
| Election timeout | 150–300ms (random) | Split vote 방지를 위한 무작위화 |
| Heartbeat interval | 50ms | Election timeout의 1/3 이하 |
| RPC timeout | 100ms | Network RTT 여유분 |

### 새 패키지 구조

```
internal/infrastructure/raft/
  state.go   — RaftRole enum + timing constants
  node.go    — RaftNode state machine
  client.go  — peer RPC sender (RequestVote, AppendEntries)
  server.go  — gRPC handler (implements pb.RaftServiceServer)
```

### Proto 확장

기존 `proto/ingest.proto`에 `RaftService` 추가:
- `RequestVote`: Candidate → Peers
- `AppendEntries`: Leader → Peers (Phase 5a: heartbeat only)

### 기존 Primary/Replica 역할과의 관계

Phase 5a에서 Raft role(Leader/Follower)과 replication role(Primary/Replica)은 **독립적**으로 공존한다.
- Raft Leader = "이 노드가 현재 Leader임"
- Replication Primary = "이 노드에서 WAL이 Replica로 스트리밍됨"

Phase 5b에서 Raft Leader 선출 결과가 replication Primary 역할을 dynamic하게 결정하도록 통합한다.

## Consequences

**Positive:**
- Primary 장애 시 자동 Leader 선출 (수동 조치 불필요)
- Raft term으로 split-brain 방지 가능
- Phase 5b log replication의 기반

**Negative:**
- Phase 5a에서는 Raft 선출과 실제 데이터 복제가 아직 연결되지 않음:
  새 Leader가 선출되어도 WAL 스트리밍은 여전히 static env 설정에 의존
- Phase 5b 전까지는 "Leader임을 알지만 Primary로 자동 승격되지는 않음" 상태

**Known limitation:**
Phase 5a intentionally defers:
- Phase 5b: AppendEntries에 WAL log entries 포함, Raft commit index로 데이터 내구성 보장
- Phase 5c: Follower read (Read Index 기반 linearizable read)
```

- [ ] **Step 2: ADR 파일 저장 및 커밋**

```bash
git add docs/adr/0010-raft-leader-election.md
git commit -m "docs(adr): ADR-010 — Raft leader election design (Phase 5a)"
```

---

## Self-Review

### Spec coverage 체크

| 요구사항 | 구현 Task |
|---|---|
| Follower 상태 + election timer | Task 5 |
| Candidate 상태 + RequestVote | Task 6 |
| Leader 상태 + heartbeat | Task 7 |
| RequestVote RPC handler | Task 4 |
| AppendEntries RPC handler | Task 4 |
| gRPC wiring | Task 8 |
| Prometheus 메트릭 | Task 9 |
| cmd/main.go wiring | Task 10 |
| ADR 문서 | Task 11 |

### Placeholder scan

없음 — 모든 step에 실제 코드 포함.

### Type consistency 체크

- `RaftRole` — `state.go`에서 정의, `node.go`/`server_test.go`에서 사용 ✓
- `RaftHandler` interface — `server.go`에서 정의, `server_test.go`의 `fakeNode`가 구현 ✓
- `PeerClients` — `client.go`에서 정의, `node.go`/`main.go`에서 사용 ✓
- `ForceRole` — `node.go`에서 정의, `node_test.go`에서 사용 ✓
- `RoleString()` — `node.go`에서 정의, `metrics/raft.go`의 `RaftMetricsSource` 인터페이스와 일치 ✓
