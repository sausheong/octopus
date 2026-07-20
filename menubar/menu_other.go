//go:build !darwin

package menubar

import "fmt"

func Run(_ string, _ func()) error {
	return fmt.Errorf("the Octopus menu bar is available only on macOS")
}
