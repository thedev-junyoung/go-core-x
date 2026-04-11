package cluster

import (
	"fmt"
	"os"
)

// Role represents the replication role of this node.
type Role int

const (
	// RoleStandalone is the default single-node mode (no replication).
	RoleStandalone Role = iota
	// RolePrimary owns WAL and streams it to replicas.
	RolePrimary
	// RoleReplica receives WAL stream from a primary.
	RoleReplica
)

func (r Role) String() string {
	switch r {
	case RolePrimary:
		return "primary"
	case RoleReplica:
		return "replica"
	default:
		return "standalone"
	}
}

// RoleFromEnv reads CORE_X_ROLE and returns the corresponding Role.
// Returns RoleStandalone if the variable is unset or empty.
// Returns an error if the value is set but unrecognized.
func RoleFromEnv() (Role, error) {
	v := os.Getenv("CORE_X_ROLE")
	switch v {
	case "":
		return RoleStandalone, nil
	case "primary":
		return RolePrimary, nil
	case "replica":
		return RoleReplica, nil
	default:
		return RoleStandalone, fmt.Errorf("cluster: unknown role %q (expected primary|replica)", v)
	}
}
