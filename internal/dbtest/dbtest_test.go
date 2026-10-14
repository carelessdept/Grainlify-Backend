package dbtest

import (
	"context"
	"strings"
	"testing"
)

func TestPackageDBName_DerivesIsolatedNamePerPackage(t *testing.T) {
	cases := []struct {
		callerFile string
		want       string
	}{
		{
			callerFile: "/workspace/Grainlify-Backend/internal/founding/founding_test.go",
			want:       "gl_test_founding",
		},
		{
			callerFile: "/workspace/Grainlify-Backend/internal/payout/payout_test.go",
			want:       "gl_test_payout",
		},
		{
			callerFile: "/workspace/Grainlify-Backend/internal/handlers/payout_claims_test.go",
			want:       "gl_test_handlers",
		},
		{
			callerFile: "/workspace/Grainlify-Backend/internal/hackathon/maintainer_pool_test.go",
			want:       "gl_test_hackathon",
		},
		{
			callerFile: "/workspace/Grainlify-Backend/internal/auth/repo_test.go",
			want:       "gl_test_auth",
		},
		{
			callerFile: "internal/keeperhubrail/release_test.go",
			want:       "gl_test_keeperhubrail",
		},
		{
			callerFile: "/workspace/Grainlify-Backend/cmd/payout/main_test.go",
			want:       "gl_test_cmd_payout",
		},
	}

	seen := map[string]string{}
	for _, tc := range cases {
		got := PackageDBName(tc.callerFile)
		if got != tc.want {
			t.Errorf("PackageDBName(%q) = %q, want %q", tc.callerFile, got, tc.want)
		}
		if prev, exists := seen[got]; exists {
			t.Errorf("collision: %q and %q both mapped to %q", prev, tc.callerFile, got)
		}
		seen[got] = tc.callerFile
	}
}

func TestPackageDBName_RespectsPostgresIdentifierLimit(t *testing.T) {
	longPath := "/workspace/internal/" + strings.Repeat("deeply_nested_package_", 5) + "/some_test.go"
	got := PackageDBName(longPath)
	if len(got) > maxPostgresIdentLen {
		t.Fatalf("len(PackageDBName) = %d, exceeds PostgreSQL limit %d: %q", len(got), maxPostgresIdentLen, got)
	}
	if !strings.HasPrefix(got, "gl_test_") {
		t.Fatalf("expected prefix gl_test_, got %q", got)
	}
	if strings.HasSuffix(got, "_") {
		t.Fatalf("expected trailing underscore to be trimmed, got %q", got)
	}
}

func TestCallerPackageDBName_ResolvesCallingTestPackage(t *testing.T) {
	if got := callerPackageDBName(); got != "gl_test_dbtest" {
		t.Fatalf("callerPackageDBName() = %q, want %q", got, "gl_test_dbtest")
	}
}

func TestDB_ConnectsToPackageScopedDatabase(t *testing.T) {
	d := DB(t)
	var currentDB string
	if err := d.Pool.QueryRow(context.Background(), `SELECT current_database()`).Scan(&currentDB); err != nil {
		t.Fatalf("SELECT current_database(): %v", err)
	}
	if currentDB != "gl_test_dbtest" {
		t.Fatalf("current_database() = %q, want %q", currentDB, "gl_test_dbtest")
	}
}
