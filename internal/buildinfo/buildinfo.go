// Package buildinfo exposes release metadata injected by the app build.
package buildinfo

// Version is "dev" for ordinary Go builds and is replaced with the macOS
// bundle's CFBundleShortVersionString by scripts/build-macos-app.sh.
var Version = "dev"
