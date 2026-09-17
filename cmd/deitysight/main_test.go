package main

import (
	"flag"
	"os"
	"runtime"
	"testing"
)

func args(t *testing.T, values ...string) {
	t.Helper()
	oldFlags, oldArgs := flag.CommandLine, os.Args
	flag.CommandLine = flag.NewFlagSet("deitysight", flag.ContinueOnError)
	os.Args = append([]string{"deitysight"}, values...)
	t.Cleanup(func() { flag.CommandLine = oldFlags; os.Args = oldArgs })
}
func TestVersion(t *testing.T) {
	args(t, "--version")
	if err := run(); err != nil {
		t.Fatal(err)
	}
}
func TestRejectUnsupportedPlatformOrMissingConfig(t *testing.T) {
	args(t, "--config", t.TempDir()+"/missing.yaml")
	if err := run(); err == nil {
		t.Fatalf("accepted missing config on %s", runtime.GOOS)
	}
}
