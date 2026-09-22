package transcodeworker

import (
	"fmt"
	"os"
	"path/filepath"
)

// Resolve existing ancestors too: the job directory may not exist yet, while
// its parent can already redirect local scratch back onto the NAS.
func physicalScratchPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("scratch path must be absolute: %s", path)
	}
	path = filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path, nil
	}
	parent, err = physicalScratchPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

func (w *Worker) requireLocalScratch(path string) error {
	physical, err := physicalScratchPath(path)
	if err != nil {
		return fmt.Errorf("local scratch: %w", err)
	}
	for _, root := range w.cfg.ExternalRoots {
		if root == "" {
			continue
		}
		external, err := physicalScratchPath(root)
		if err != nil {
			return fmt.Errorf("external storage root: %w", err)
		}
		if IsExternalPath(physical, []string{external}) {
			return fmt.Errorf("local scratch must remain on the node, outside external/NAS roots: %s", path)
		}
	}
	if w.mediaStore != nil && w.mediaStore.Maps(path) {
		return fmt.Errorf("local scratch must not be an SMB destination: %s", path)
	}
	return nil
}
