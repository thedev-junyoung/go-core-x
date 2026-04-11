package cluster_test

import (
	"os"
	"testing"

	"github.com/junyoung/core-x/internal/infrastructure/cluster"
)

func TestRoleFromEnv(t *testing.T) {
	tests := []struct {
		env     string
		want    cluster.Role
		wantErr bool
	}{
		{"primary", cluster.RolePrimary, false},
		{"replica", cluster.RoleReplica, false},
		{"", cluster.RoleStandalone, false},
		{"invalid", cluster.RoleStandalone, true},
	}

	for _, tc := range tests {
		t.Run(tc.env, func(t *testing.T) {
			os.Setenv("CORE_X_ROLE", tc.env)
			defer os.Unsetenv("CORE_X_ROLE")

			got, err := cluster.RoleFromEnv()
			if (err != nil) != tc.wantErr {
				t.Fatalf("RoleFromEnv() err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("RoleFromEnv() = %v, want %v", got, tc.want)
			}
		})
	}
}
