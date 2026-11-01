// Package oracletest locates the oracle toolchain for tests.
package oracletest

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/go-embedded-c/c/internal/oracle"
)

// EnvRequire, when "1", turns "no toolchain" from a skip into a failure (CI).
const EnvRequire = "CORACLE_REQUIRE"

// Require returns the verified toolchain, or skips the test when there is
// none. With $CORACLE_REQUIRE=1 a missing toolchain fails the test instead.
func Require(t testing.TB) *oracle.Toolchain {
	t.Helper()
	tc, err := oracle.Locate()
	if err == nil {
		err = tc.Verify(context.Background())
	}
	if err != nil {
		if errors.Is(err, oracle.ErrNoToolchain) && os.Getenv(EnvRequire) != "1" {
			t.Skipf("no oracle toolchain: %v", err)
		}
		t.Fatalf("oracle toolchain: %v", err)
	}
	return tc
}

// Dir returns an environment-selected corpus directory, or skips (fails with
// $CORACLE_REQUIRE=1) when the variable is unset.
func Dir(t testing.TB, env string) string {
	t.Helper()
	d := os.Getenv(env)
	if d == "" {
		if os.Getenv(EnvRequire) == "1" {
			t.Fatalf("$%s is not set", env)
		}
		t.Skipf("$%s is not set", env)
	}
	return d
}
