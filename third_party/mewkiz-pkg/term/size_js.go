//go:build js

package term

// Size stub for wasm builds - no terminal.
func Size() (width, height int, err error) {
	return 80, 24, nil
}
