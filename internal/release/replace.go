package release

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrNotWritable means the install directory refuses new files, which is the
// common "installed under /usr/local/bin without write access" case.
var ErrNotWritable = errors.New("install directory is not writable")

// ReplaceItem is one binary to install.
type ReplaceItem struct {
	Path string      // absolute destination
	Data []byte      // new contents
	Mode os.FileMode // 0 keeps the existing file's mode, or 0o755 for a new file
}

// Replace installs data at path atomically.
//
// The temp file is created in path's OWN directory, never in $TMPDIR: a
// cross-filesystem os.Rename fails with EXDEV, and falling back to a copy
// would reintroduce the torn-write this exists to prevent. Renaming over a
// running binary is safe on Linux and macOS — the running process keeps the
// old inode — whereas writing through the existing path SIGKILLs it the
// moment its mapped pages are invalidated.
func Replace(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if mode == 0 {
		mode = 0o755
		if st, err := os.Stat(path); err == nil {
			mode = st.Mode().Perm()
		}
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".new-*")
	if err != nil {
		return fmt.Errorf("%s: %w: %v", dir, ErrNotWritable, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	// fsync before rename: a rename is atomic with respect to readers, but
	// without the sync a crash can leave the new name pointing at unwritten
	// data — a zero-length binary that is worse than the old one.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}
	return nil
}

// ReplaceAll installs a set of binaries and rolls the whole set back if any
// single install fails.
//
// Replacing N files is not atomic as a set, so the failure this guards is a
// mixed install: some binaries at the new version, some at the old, and a
// suite whose parts no longer agree. Each existing target is backed up
// beside itself first (a hard link, so it costs nothing), and a failure
// renames every backup taken so far back over its original.
func ReplaceAll(items []ReplaceItem) (err error) {
	type backup struct{ path, saved string }
	var done []backup

	defer func() {
		if err == nil {
			for _, b := range done {
				os.Remove(b.saved)
			}
			return
		}
		// Restore in reverse: the last file replaced is the first undone, so
		// a partially-applied set never lingers.
		for i := len(done) - 1; i >= 0; i-- {
			os.Rename(done[i].saved, done[i].path)
			os.Remove(done[i].saved)
		}
	}()

	for _, it := range items {
		if st, statErr := os.Stat(it.Path); statErr == nil {
			if !st.Mode().IsRegular() {
				return fmt.Errorf("refusing to replace %s: not a regular file", it.Path)
			}
			saved := it.Path + ".orchard-backup"
			os.Remove(saved)
			if backupErr := backupFile(it.Path, saved); backupErr != nil {
				return fmt.Errorf("back up %s before replacing it: %w", it.Path, backupErr)
			}
			done = append(done, backup{it.Path, saved})
		}
		if replaceErr := Replace(it.Path, it.Data, it.Mode); replaceErr != nil {
			return replaceErr
		}
	}
	return nil
}

// backupFile links src to dst, copying only if the filesystem refuses the
// link (some network and container filesystems do).
func backupFile(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// Writable reports whether dir accepts new files, naming the directory in the
// error so `orchard upgrade` can say exactly what to fix rather than surfacing
// a bare EACCES from halfway through an install.
func Writable(dir string) error {
	f, err := os.CreateTemp(dir, ".orchard-write-probe-*")
	if err != nil {
		return fmt.Errorf("%s: %w", dir, ErrNotWritable)
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return nil
}
