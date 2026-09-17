package agent

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var errStorage = errors.New("storage budget exceeded or storage unavailable")

const metadataReserve int64 = 64 << 10

type store struct {
	mu      sync.Mutex
	root    *os.Root
	lock    *os.File
	cfg     StorageConfig
	used    int64
	reserve int64
	leases  map[string]int
	pending map[string]int64
	closed  bool
}

func openStore(c StorageConfig) (*store, error) {
	if i, e := os.Lstat(c.Path); e == nil && i.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("storage directory cannot be a symlink")
	}
	if e := os.MkdirAll(c.Path, 0700); e != nil {
		return nil, e
	}
	root, e := os.OpenRoot(c.Path)
	if e != nil {
		return nil, e
	}
	fail := func(e error) (*store, error) { root.Close(); return nil, e }
	if e = root.Chmod(".", 0700); e != nil {
		return fail(e)
	}
	if i, e := root.Lstat("agent.lock"); e == nil && !i.Mode().IsRegular() {
		return fail(errors.New("invalid lock file"))
	}
	lock, e := root.OpenFile("agent.lock", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return fail(e)
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		lock.Close()
		return fail(errors.New("storage directory already locked"))
	}
	s := &store{root: root, lock: lock, cfg: c, leases: map[string]int{}, pending: map[string]int64{}}
	if e = root.MkdirAll("tasks", 0700); e == nil {
		e = root.MkdirAll("background", 0700)
	}
	if e == nil {
		e = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlink in storage: %s", path)
			}
			if !d.IsDir() {
				i, e := d.Info()
				if e != nil {
					return e
				}
				if !i.Mode().IsRegular() {
					return errors.New("non-regular storage entry")
				}
				s.used += i.Size()
			}
			return nil
		})
	}
	if e != nil {
		s.Close()
		return nil, e
	}
	return s, nil
}
func (s *store) checkLocked(extra, reserve int64) error {
	if s.closed || extra < 0 || reserve < 0 || s.used > s.cfg.MaxBytes || extra > s.cfg.MaxBytes-s.used || reserve > s.cfg.MaxBytes-s.used-extra {
		return errStorage
	}
	var st syscall.Statfs_t
	if e := syscall.Fstatfs(int(s.lock.Fd()), &st); e != nil {
		return errStorage
	}
	free := uint64(st.Bavail) * uint64(st.Bsize)
	if free < s.cfg.MinFreeBytes || uint64(extra)+uint64(reserve) > free-s.cfg.MinFreeBytes {
		return errStorage
	}
	return nil
}
func (s *store) Available() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkLocked(0, s.reserve+metadataReserve)
}
func (s *store) BeginCapture() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.checkLocked(0, metadataReserve+4096); e != nil {
		return e
	}
	s.reserve = 4096
	return nil
}
func (s *store) EndCapture() { s.mu.Lock(); s.reserve = 0; s.mu.Unlock() }
func (s *store) Atomic(name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.checkLocked(int64(len(data)), s.reserve); e != nil {
		return e
	}
	old := int64(0)
	if i, e := s.root.Lstat(name); e == nil {
		if !i.Mode().IsRegular() {
			return errors.New("invalid metadata entry")
		}
		old = i.Size()
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	tmp := name + "." + uuid() + ".tmp"
	f, e := s.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	n, e := f.Write(data)
	s.used += int64(n)
	if e == nil && n != len(data) {
		e = io.ErrShortWrite
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		if s.root.Remove(tmp) == nil {
			s.used -= int64(n)
		}
		return e
	}
	if e = s.root.Rename(tmp, name); e != nil {
		if s.root.Remove(tmp) == nil {
			s.used -= int64(n)
		}
		return e
	}
	s.used -= old
	return s.syncDirLocked(filepath.Dir(name))
}
func (s *store) syncDirLocked(path string) error {
	f, e := s.root.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}

// capture reserves room for a future archive; consume converts that reservation to archive bytes.
func (s *store) Append(name string, b []byte, capture, consume bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	reserve := s.reserve
	if capture {
		reserve += 2 * int64(len(b))
	}
	if consume {
		reserve -= min(reserve, int64(len(b)))
	}
	if e := s.checkLocked(int64(len(b)), reserve+metadataReserve); e != nil {
		return e
	}
	if i, e := s.root.Lstat(name); e == nil && !i.Mode().IsRegular() {
		return errors.New("invalid data file")
	}
	f, e := s.root.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	n, e := f.Write(b)
	s.used += int64(n)
	s.reserve = reserve
	closeErr := f.Close()
	if e == nil && n != len(b) {
		e = io.ErrShortWrite
	}
	if e == nil {
		e = closeErr
	}
	return e
}
func (s *store) Sync(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, e := s.root.OpenFile(name, os.O_WRONLY, 0)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func (s *store) Read(name string) ([]byte, error) {
	f, e := s.Open(name)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 2<<20))
}
func (s *store) Open(name string) (*os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, e := s.root.Lstat(name)
	if e != nil {
		return nil, e
	}
	if !i.Mode().IsRegular() {
		return nil, errors.New("invalid data file")
	}
	return s.root.Open(name)
}
func (s *store) Mkdir(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.root.Mkdir(name, 0700); e != nil {
		return e
	}
	return s.syncDirLocked(filepath.Dir(name))
}
func (s *store) Entries(name string) ([]os.DirEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, e := s.root.Open(name)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	return f.ReadDir(-1)
}
func (s *store) Publish(tmp, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i, e := s.root.Lstat(name); e == nil {
		if !i.Mode().IsRegular() {
			return errors.New("invalid result")
		}
		if s.leases[name] > 0 {
			return errors.New("result in use")
		}
		if e = s.root.Remove(name); e != nil {
			return e
		}
		s.used -= i.Size()
	}
	if e := s.root.Rename(tmp, name); e != nil {
		return e
	}
	return s.syncDirLocked(filepath.Dir(name))
}
func (s *store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, e := s.root.Lstat(name)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	if !i.Mode().IsRegular() {
		return errors.New("invalid deletion target")
	}
	if s.leases[name] > 0 {
		s.pending[name] = i.Size()
		return nil
	}
	if e = s.root.Remove(name); e != nil {
		return e
	}
	s.used -= i.Size()
	return nil
}
func (s *store) RemoveTask(id string) error {
	if !validID(id) {
		return errors.New("invalid task id")
	}
	base := "tasks/" + id
	entries, e := s.Entries(base)
	if e != nil {
		return e
	}
	for _, d := range entries {
		if d.IsDir() {
			return errors.New("unexpected nested task directory")
		}
		if e = s.Remove(base + "/" + d.Name()); e != nil {
			return e
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.leases {
		if filepath.Dir(name) == base {
			return nil
		}
	}
	return s.root.Remove(base)
}

type leasedFile struct {
	*os.File
	s    *store
	name string
	once sync.Once
}

func (f *leasedFile) Close() error {
	var err error
	f.once.Do(func() {
		err = f.File.Close()
		s := f.s
		s.mu.Lock()
		defer s.mu.Unlock()
		s.leases[f.name]--
		if s.leases[f.name] == 0 {
			delete(s.leases, f.name)
			if n, ok := s.pending[f.name]; ok {
				if s.root.Remove(f.name) == nil {
					s.used -= n
				}
				delete(s.pending, f.name)
			}
		}
	})
	return err
}
func (s *store) Lease(name string) (*leasedFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, e := s.root.Lstat(name)
	if e != nil {
		return nil, e
	}
	if !i.Mode().IsRegular() {
		return nil, errors.New("invalid result file")
	}
	f, e := s.root.Open(name)
	if e != nil {
		return nil, e
	}
	s.leases[name]++
	return &leasedFile{File: f, s: s, name: name}, nil
}
func (s *store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	s.lock.Close()
	s.root.Close()
}
