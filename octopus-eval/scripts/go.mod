module github.com/sausheong/octopus-eval/scripts

go 1.22

// Go files in this directory are injected evaluation fixtures and headless
// entrypoints. They are compiled against the parent Octopus snapshot by
// ../run.sh --tier-a, not as a standalone package.
