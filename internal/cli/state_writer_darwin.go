//go:build darwin

package cli

import "context"

func oldLoadedLibexecStateWriter(ctx context.Context, pid int, image string) bool {
	raw, err := bindingImageOutput(ctx, pid)
	if err != nil {
		return false
	}
	path := stateWriterLsofPath(string(raw), image)
	// A replaced/unlinked vnode without a readable matching path is unknown.
	// The new file at the old name is never evidence about the loaded image.
	return path != "" && oldLibexecStateWriter(path, image)
}
