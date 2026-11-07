package cli

import (
	"context"
	"fmt"
	"os"
)

type fetchResult struct {
	Name, SHA256, Status string
	Size                 int64
}

func markFetch(result *fetchResult, status string) {
	if result != nil {
		result.Status = status
	}
}

func fetchBatch(ctx context.Context, a *App, s *session, pos []string, out string) error {
	parent, name, err := openOutputParent(out)
	if err != nil {
		return err
	}
	defer parent.Close()
	// Mkdir is exclusive, including against a symlink or an empty directory.
	if err := parent.Mkdir(name, 0o700); err != nil {
		return fmt.Errorf("batch fetch requires a fresh directory: %w", err)
	}
	st, err := parent.Lstat(name)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		return fmt.Errorf("batch output directory is not a private directory")
	}
	dir, err := parent.OpenRoot(name)
	if err != nil {
		return err
	}
	defer dir.Close()
	opened, err := dir.Stat(".")
	if err != nil || !os.SameFile(st, opened) {
		return fmt.Errorf("batch output directory changed while opening")
	}
	parentFile, err := parent.Open(".")
	if err != nil {
		return err
	}
	err = parentFile.Sync()
	parentFile.Close()
	if err != nil {
		return fmt.Errorf("sync batch output parent: %w", err)
	}
	failed := 0
	for _, event := range pos[1:] {
		if err := ctx.Err(); err != nil {
			return err
		}
		result := fetchResult{Name: "-", SHA256: "-", Status: "metadata_error"}
		if err := fetchOne(ctx, a, s, []string{pos[0], event}, nil, "", "", dir, &result); err != nil {
			failed++
		}
		// Only fixed status tokens and validated names, hashes and sizes reach
		// stdout. Server error text and response bodies never appear here.
		line := fmt.Sprintf("%s\t%s\t%s\t%d\t%s\n", event, result.Name, result.SHA256, result.Size, result.Status)
		if err := writeCLIOutput(a.Stdout, a.secrets.Scrub(line)); err != nil {
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("batch fetch: %d attachment(s) failed", failed)
	}
	return nil
}
