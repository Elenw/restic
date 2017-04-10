package local

import (
	"io"
	"os"
	"path/filepath"
	"restic"

	"restic/errors"

	"restic/backend"
	"restic/debug"
	"restic/fs"
)

// Local is a backend in a local directory.
type Local struct {
	Config
	backend.Layout
}

// ensure statically that *Local implements restic.Backend.
var _ restic.Backend = &Local{}

const defaultLayout = "default"

// Open opens the local backend as specified by config.
func Open(cfg Config) (*Local, error) {
	debug.Log("open local backend at %v (layout %q)", cfg.Path, cfg.Layout)
	l, err := backend.ParseLayout(&backend.LocalFilesystem{}, cfg.Layout, defaultLayout, cfg.Path)
	if err != nil {
		return nil, err
	}

	be := &Local{Config: cfg, Layout: l}

	// test if all necessary dirs are there
	for _, d := range be.Paths() {
		if _, err := fs.Stat(d); err != nil {
			return nil, errors.Wrap(err, "Open")
		}
	}

	return be, nil
}

// Create creates all the necessary files and directories for a new local
// backend at dir. Afterwards a new config blob should be created.
func Create(cfg Config) (*Local, error) {
	debug.Log("create local backend at %v (layout %q)", cfg.Path, cfg.Layout)

	l, err := backend.ParseLayout(&backend.LocalFilesystem{}, cfg.Layout, defaultLayout, cfg.Path)
	if err != nil {
		return nil, err
	}

	be := &Local{
		Config: cfg,
		Layout: l,
	}

	// test if config file already exists
	_, err = fs.Lstat(be.Filename(restic.Handle{Type: restic.ConfigFile}))
	if err == nil {
		return nil, errors.New("config file already exists")
	}

	// create paths for data, refs and temp
	for _, d := range be.Paths() {
		err := fs.MkdirAll(d, backend.Modes.Dir)
		if err != nil {
			return nil, errors.Wrap(err, "MkdirAll")
		}
	}

	return be, nil
}

// Location returns this backend's location (the directory name).
func (b *Local) Location() string {
	return b.Path
}

// Save stores data in the backend at the handle.
func (b *Local) Save(h restic.Handle, rd io.Reader) (err error) {
	debug.Log("Save %v", h)
	if err := h.Valid(); err != nil {
		return err
	}

	filename := b.Filename(h)

	// create directories if necessary, ignore errors
	if h.Type == restic.DataFile {
		err = fs.MkdirAll(filepath.Dir(filename), backend.Modes.Dir)
		if err != nil {
			return errors.Wrap(err, "MkdirAll")
		}
	}

	// create new file
	f, err := fs.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, backend.Modes.File)
	if err != nil {
		return errors.Wrap(err, "OpenFile")
	}

	// save data, then sync
	_, err = io.Copy(f, rd)
	if err != nil {
		f.Close()
		return errors.Wrap(err, "Write")
	}

	if err = f.Sync(); err != nil {
		f.Close()
		return errors.Wrap(err, "Sync")
	}

	err = f.Close()
	if err != nil {
		return errors.Wrap(err, "Close")
	}

	// set mode to read-only
	fi, err := fs.Stat(filename)
	if err != nil {
		return errors.Wrap(err, "Stat")
	}

	return setNewFileMode(filename, fi)
}

// Load returns a reader that yields the contents of the file at h at the
// given offset. If length is nonzero, only a portion of the file is
// returned. rd must be closed after use.
func (b *Local) Load(h restic.Handle, length int, offset int64) (io.ReadCloser, error) {
	debug.Log("Load %v, length %v, offset %v", h, length, offset)
	if err := h.Valid(); err != nil {
		return nil, err
	}

	if offset < 0 {
		return nil, errors.New("offset is negative")
	}

	f, err := fs.Open(b.Filename(h))
	if err != nil {
		return nil, err
	}

	if offset > 0 {
		_, err = f.Seek(offset, 0)
		if err != nil {
			f.Close()
			return nil, err
		}
	}

	if length > 0 {
		return backend.LimitReadCloser(f, int64(length)), nil
	}

	return f, nil
}

// Stat returns information about a blob.
func (b *Local) Stat(h restic.Handle) (restic.FileInfo, error) {
	debug.Log("Stat %v", h)
	if err := h.Valid(); err != nil {
		return restic.FileInfo{}, err
	}

	fi, err := fs.Stat(b.Filename(h))
	if err != nil {
		return restic.FileInfo{}, errors.Wrap(err, "Stat")
	}

	return restic.FileInfo{Size: fi.Size()}, nil
}

// Test returns true if a blob of the given type and name exists in the backend.
func (b *Local) Test(h restic.Handle) (bool, error) {
	debug.Log("Test %v", h)
	_, err := fs.Stat(b.Filename(h))
	if err != nil {
		if os.IsNotExist(errors.Cause(err)) {
			return false, nil
		}
		return false, errors.Wrap(err, "Stat")
	}

	return true, nil
}

// Remove removes the blob with the given name and type.
func (b *Local) Remove(h restic.Handle) error {
	debug.Log("Remove %v", h)
	fn := b.Filename(h)

	// reset read-only flag
	err := fs.Chmod(fn, 0666)
	if err != nil {
		return errors.Wrap(err, "Chmod")
	}

	return fs.Remove(fn)
}

func isFile(fi os.FileInfo) bool {
	return fi.Mode()&(os.ModeType|os.ModeCharDevice) == 0
}

func readdir(d string) (fileInfos []os.FileInfo, err error) {
	f, e := fs.Open(d)
	if e != nil {
		return nil, errors.Wrap(e, "Open")
	}

	defer func() {
		e := f.Close()
		if err == nil {
			err = errors.Wrap(e, "Close")
		}
	}()

	return f.Readdir(-1)
}

// listDir returns a list of all files in d.
func listDir(d string) (filenames []string, err error) {
	fileInfos, err := readdir(d)
	if err != nil {
		return nil, err
	}

	for _, fi := range fileInfos {
		if isFile(fi) {
			filenames = append(filenames, fi.Name())
		}
	}

	return filenames, nil
}

// listDirs returns a list of all files in directories within d.
func listDirs(dir string) (filenames []string, err error) {
	fileInfos, err := readdir(dir)
	if err != nil {
		return nil, err
	}

	for _, fi := range fileInfos {
		if !fi.IsDir() {
			continue
		}

		files, err := listDir(filepath.Join(dir, fi.Name()))
		if err != nil {
			continue
		}

		filenames = append(filenames, files...)
	}

	return filenames, nil
}

// List returns a channel that yields all names of blobs of type t. A
// goroutine is started for this. If the channel done is closed, sending
// stops.
func (b *Local) List(t restic.FileType, done <-chan struct{}) <-chan string {
	debug.Log("List %v", t)
	lister := listDir
	if t == restic.DataFile {
		lister = listDirs
	}

	ch := make(chan string)
	items, err := lister(b.Dirname(restic.Handle{Type: t}))
	if err != nil {
		close(ch)
		return ch
	}

	go func() {
		defer close(ch)
		for _, m := range items {
			if m == "" {
				continue
			}

			select {
			case ch <- m:
			case <-done:
				return
			}
		}
	}()

	return ch
}

// Delete removes the repository and all files.
func (b *Local) Delete() error {
	debug.Log("Delete()")
	return fs.RemoveAll(b.Path)
}

// Close closes all open files.
func (b *Local) Close() error {
	debug.Log("Close()")
	// this does not need to do anything, all open files are closed within the
	// same function.
	return nil
}
