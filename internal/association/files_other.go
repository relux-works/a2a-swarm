//go:build !unix

package association

import (
	"context"
	"os"
)

func ReadMetadata(string, int64) ([]byte, error)                { return nil, invalid }
func snapshot(Context, context.Context) (map[string]any, error) { return nil, invalid }
func readIntent(f *os.File) ([]byte, string) {
	if f != nil {
		f.Close()
	}
	return nil, "input_untrusted"
}
