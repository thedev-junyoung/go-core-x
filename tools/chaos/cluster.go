// Package chaos implements a local 3-node cluster manager for chaos testing.
//
// DDIA Reliability Focus:
//   - Validates that the system remains functional despite single-node failure.
//   - Measures recovery time: health probe must detect failure and stop routing
//     within the SLO window (default: 2 seconds).
//   - Validates cascading failure prevention: surviving nodes must not be
//     dragged down by routing requests to a dead peer.
//
// Architecture:
//   - Each node is a separate OS process (real process isolation, not goroutine).
//     This matters: a goroutine "failure" cannot simulate OS-level crash behavior
//     (open file handles, unclean TCP RST, etc.).
//   - kill -9 is used (SIGKILL) — not graceful shutdown. This tests the hard
//     failure path, not the planned shutdown path.
//   - Inter-node communication uses gRPC (consistent with production topology).
package chaos

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// NodeConfig defines a single cluster node's configuration.
type NodeConfig struct {
	ID       string // e.g. "node-1"
	HTTPAddr string // e.g. "127.0.0.1:8081"
	GRPCAddr string // e.g. "127.0.0.1:9081"
	WALPath  string // e.g. "/tmp/chaos-node1.wal"
}

// ClusterNode wraps an OS process for a single Core-X node.
type ClusterNode struct {
	Config  NodeConfig
	cmd     *exec.Cmd
	logFile *os.File
}

// Cluster manages a local 3-node Core-X cluster for chaos testing.
type Cluster struct {
	nodes      []*ClusterNode
	binaryPath string
	logDir     string
}

// NewCluster creates a Cluster backed by the given binary.
// logDir is where per-node log files are written.
func NewCluster(binaryPath, logDir string, nodes []NodeConfig) *Cluster {
	c := &Cluster{
		binaryPath: binaryPath,
		logDir:     logDir,
	}
	for _, nc := range nodes {
		c.nodes = append(c.nodes, &ClusterNode{Config: nc})
	}
	return c
}

// Start launches all nodes in the cluster.
// Each node is passed its peers via CORE_X_PEERS.
// Returns when all nodes report healthy via /healthz (or timeout).
func (c *Cluster) Start(ctx context.Context) error {
	if err := os.MkdirAll(c.logDir, 0750); err != nil {
		return fmt.Errorf("chaos: failed to create log dir: %w", err)
	}

	// Build peer list for each node.
	allPeers := c.buildPeerMap()

	for _, n := range c.nodes {
		if err := c.startNode(n, allPeers[n.Config.ID]); err != nil {
			return fmt.Errorf("chaos: failed to start node %s: %w", n.Config.ID, err)
		}
		slog.Info("chaos: node process started", "id", n.Config.ID, "http", n.Config.HTTPAddr)
	}

	// Wait for all nodes to become healthy.
	for _, n := range c.nodes {
		url := "http://" + n.Config.HTTPAddr + "/healthz"
		if err := waitHealthy(ctx, url, 10*time.Second); err != nil {
			return fmt.Errorf("chaos: node %s did not become healthy: %w", n.Config.ID, err)
		}
		slog.Info("chaos: node healthy", "id", n.Config.ID)
	}

	return nil
}

// KillNode sends SIGKILL to the named node (simulates hard crash / kill -9).
// The process is killed immediately with no cleanup.
func (c *Cluster) KillNode(nodeID string) error {
	n := c.findNode(nodeID)
	if n == nil {
		return fmt.Errorf("chaos: node %s not found", nodeID)
	}
	if n.cmd == nil || n.cmd.Process == nil {
		return fmt.Errorf("chaos: node %s process not started", nodeID)
	}
	slog.Warn("chaos: sending SIGKILL", "node_id", nodeID, "pid", n.cmd.Process.Pid)
	return n.cmd.Process.Signal(syscall.SIGKILL)
}

// StopAll sends SIGTERM to all running nodes and waits for them to exit.
// Used for test cleanup.
func (c *Cluster) StopAll() {
	for _, n := range c.nodes {
		if n.cmd != nil && n.cmd.Process != nil {
			_ = n.cmd.Process.Signal(syscall.SIGTERM)
		}
	}
	for _, n := range c.nodes {
		if n.cmd != nil {
			_ = n.cmd.Wait()
		}
		if n.logFile != nil {
			_ = n.logFile.Close()
		}
	}
}

// IsHealthy returns true if the named node's /healthz returns 200.
// Non-blocking — uses a short timeout (500ms).
func (c *Cluster) IsHealthy(nodeID string) bool {
	n := c.findNode(nodeID)
	if n == nil {
		return false
	}
	url := "http://" + n.Config.HTTPAddr + "/healthz"
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// WaitUntilUnhealthy polls the named node's /healthz until it stops responding
// (or the deadline is reached). Used to confirm kill -9 took effect.
func (c *Cluster) WaitUntilUnhealthy(ctx context.Context, nodeID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if !c.IsHealthy(nodeID) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("chaos: node %s still healthy after %s", nodeID, timeout)
}

// WaitUntilHealthy polls the named node's /healthz until it responds 200.
// Used after restarting a killed node.
func (c *Cluster) WaitUntilHealthy(ctx context.Context, nodeID string, timeout time.Duration) error {
	n := c.findNode(nodeID)
	if n == nil {
		return fmt.Errorf("chaos: node %s not found", nodeID)
	}
	url := "http://" + n.Config.HTTPAddr + "/healthz"
	return waitHealthy(ctx, url, timeout)
}

// SendIngest sends a single POST /ingest to the named node.
// Returns the HTTP status code (or 0 on network error).
func (c *Cluster) SendIngest(nodeID, source, payload string) (int, error) {
	n := c.findNode(nodeID)
	if n == nil {
		return 0, fmt.Errorf("chaos: node %s not found", nodeID)
	}
	url := "http://" + n.Config.HTTPAddr + "/ingest"
	body := fmt.Sprintf(`{"source":%q,"payload":%q}`, source, payload)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// RaftKVSet sends POST /raft/kv to the named node to set a key-value pair.
// Returns the HTTP status code (or 0 on network error).
func (c *Cluster) RaftKVSet(nodeID, key, value string) (int, error) {
	n := c.findNode(nodeID)
	if n == nil {
		return 0, fmt.Errorf("chaos: node %s not found", nodeID)
	}
	url := "http://" + n.Config.HTTPAddr + "/raft/kv"
	body := fmt.Sprintf(`{"key":%q,"value":%q}`, key, value)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// RaftKVGet sends GET /raft/kv/{key} to the named node.
// Returns the value, HTTP status code, and any network error.
func (c *Cluster) RaftKVGet(nodeID, key string) (string, int, error) {
	n := c.findNode(nodeID)
	if n == nil {
		return "", 0, fmt.Errorf("chaos: node %s not found", nodeID)
	}
	url := "http://" + n.Config.HTTPAddr + "/raft/kv/" + key
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode, nil
	}
	var result struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", resp.StatusCode, fmt.Errorf("decode error: %w", err)
	}
	return result.Value, resp.StatusCode, nil
}

// WaitForLeader polls all nodes to find which one is the leader.
// Returns the nodeID of the current leader, or "" if none found within timeout.
func (c *Cluster) WaitForLeader(ctx context.Context, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ""
		default:
		}
		for _, n := range c.nodes {
			// Try a write; only the leader will accept it (204).
			// Followers redirect (307) or return 503.
			url := "http://" + n.Config.HTTPAddr + "/raft/kv"
			body := strings.NewReader(`{"key":"__leader_probe__","value":"probe"}`)
			client := &http.Client{
				Timeout: 500 * time.Millisecond,
				CheckRedirect: func(req *http.Request, via []*http.Request) error {
					return http.ErrUseLastResponse // don't follow redirects
				},
			}
			resp, err := client.Post(url, "application/json", body)
			if err != nil {
				continue
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
				return n.Config.ID
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return ""
}

// NodeLogPath returns the log file path for the named node.
func (c *Cluster) NodeLogPath(nodeID string) string {
	return filepath.Join(c.logDir, nodeID+".log")
}

// LogContent returns the current log content for the named node.
func (c *Cluster) LogContent(nodeID string) string {
	data, err := os.ReadFile(c.NodeLogPath(nodeID))
	if err != nil {
		return ""
	}
	return string(data)
}

// --- internal helpers ---

func (c *Cluster) startNode(n *ClusterNode, peers string) error {
	// Clean up old WAL for a fresh test run.
	_ = os.Remove(n.Config.WALPath)

	logPath := filepath.Join(c.logDir, n.Config.ID+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("failed to create log file: %w", err)
	}
	n.logFile = logFile

	cmd := exec.Command(c.binaryPath)
	cmd.Env = append(os.Environ(),
		"CORE_X_ADDR="+n.Config.HTTPAddr,
		"CORE_X_WAL_PATH="+n.Config.WALPath,
		"CORE_X_WORKERS=4",
		"CORE_X_BUFFER_DEPTH=100",
	)
	// Cluster mode only if both GRPCAddr and NodeID are set.
	// Single-node mode: leave CORE_X_NODE_ID and CORE_X_GRPC_ADDR unset.
	if n.Config.GRPCAddr != "" && n.Config.ID != "" {
		cmd.Env = append(cmd.Env,
			"CORE_X_GRPC_ADDR="+n.Config.GRPCAddr,
			"CORE_X_NODE_ID="+n.Config.ID,
			"CORE_X_FORWARD_TIMEOUT=1s",
		)
	}
	if peers != "" {
		cmd.Env = append(cmd.Env, "CORE_X_PEERS="+peers)
	}
	// Redirect stdout+stderr to log file.
	cmd.Stdout = io.MultiWriter(logFile, os.Stdout)
	cmd.Stderr = io.MultiWriter(logFile, os.Stderr)

	// SysProcAttr: new process group so Ctrl-C doesn't kill nodes directly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return err
	}
	n.cmd = cmd
	return nil
}

// buildPeerMap returns for each nodeID the CORE_X_PEERS value listing its peers.
// Format: "node-2:127.0.0.1:9082,node-3:127.0.0.1:9083"
func (c *Cluster) buildPeerMap() map[string]string {
	result := make(map[string]string)
	for _, n := range c.nodes {
		var peerEntries []string
		for _, peer := range c.nodes {
			if peer.Config.ID == n.Config.ID {
				continue
			}
			peerEntries = append(peerEntries, peer.Config.ID+":"+peer.Config.GRPCAddr)
		}
		result[n.Config.ID] = strings.Join(peerEntries, ",")
	}
	return result
}

func (c *Cluster) findNode(nodeID string) *ClusterNode {
	for _, n := range c.nodes {
		if n.Config.ID == nodeID {
			return n
		}
	}
	return nil
}

// waitHealthy polls url until it returns 200 or timeout is reached.
func waitHealthy(ctx context.Context, url string, timeout time.Duration) error {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		resp, err := client.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s to become healthy", url)
}

// PIDOf returns the PID of the named node's process (for diagnostic logging).
func (c *Cluster) PIDOf(nodeID string) int {
	n := c.findNode(nodeID)
	if n == nil || n.cmd == nil || n.cmd.Process == nil {
		return -1
	}
	return n.cmd.Process.Pid
}

// NodeHTTPAddr returns the HTTP address of the named node.
func (c *Cluster) NodeHTTPAddr(nodeID string) string {
	n := c.findNode(nodeID)
	if n == nil {
		return ""
	}
	return n.Config.HTTPAddr
}

// fmtPeers is a helper for test setup printing.
func fmtPeers(configs []NodeConfig) string {
	parts := make([]string, len(configs))
	for i, nc := range configs {
		parts[i] = nc.ID + "(" + nc.HTTPAddr + "/" + nc.GRPCAddr + ")"
	}
	return strings.Join(parts, ", ")
}

// formatPort extracts the port from addr ("host:port" → port int).
func formatPort(addr string) int {
	parts := strings.Split(addr, ":")
	if len(parts) < 2 {
		return 0
	}
	p, _ := strconv.Atoi(parts[len(parts)-1])
	return p
}

// suppress unused warning
var _ = fmtPeers
var _ = formatPort
