package terraform

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Limits of an extracted module archive.
const (
	maxArchiveBytes = 512 << 20
	maxArchiveFiles = 20000
)

// extract unpacks a .tgz module archive into dir. Absolute paths, "..", links and special files
// are refused: the archive comes from an artifact, but nothing in it may write outside dir.
func extract(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("terraform plugin: open artifact: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("terraform plugin: the artifact is not a .tgz archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for files := 0; ; files++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("terraform plugin: read artifact: %w", err)
		}
		if files >= maxArchiveFiles {
			return fmt.Errorf("terraform plugin: the artifact has more than %d entries", maxArchiveFiles)
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("terraform plugin: unsafe path %q in the artifact", h.Name)
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			total += h.Size
			if total > maxArchiveBytes {
				return fmt.Errorf("terraform plugin: the artifact is larger than %d MiB once extracted", maxArchiveBytes>>20)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return err
			}
			out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return fmt.Errorf("terraform plugin: extract %s: %w", name, err)
			}
			_, err = io.Copy(out, io.LimitReader(tr, h.Size))
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return fmt.Errorf("terraform plugin: extract %s: %w", name, err)
			}
		case tar.TypeXGlobalHeader:
		default:
			return fmt.Errorf("terraform plugin: %q is a link or a special file: not allowed in the artifact", h.Name)
		}
	}
}

// moduleRoot returns the root module directory: moduleDir if set, else the archive root, or its
// only top-level directory when the root holds no .tf file (archives made with tar -C .. dir).
func moduleRoot(dir, moduleDir string) (string, error) {
	root := filepath.Join(dir, filepath.FromSlash(moduleDir))
	if moduleDir == "." && !hasTF(root) {
		entries, err := os.ReadDir(root)
		if err == nil && len(entries) == 1 && entries[0].IsDir() {
			root = filepath.Join(root, entries[0].Name())
		}
	}
	if !hasTF(root) {
		return "", fmt.Errorf("terraform plugin: no .tf or .tf.json file in %s of the artifact (module_dir)", moduleDir)
	}
	return root, nil
}

func hasTF(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && (strings.HasSuffix(e.Name(), ".tf") || strings.HasSuffix(e.Name(), ".tf.json")) {
			return true
		}
	}
	return false
}
