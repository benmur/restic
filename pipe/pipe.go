package pipe

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/restic/restic/debug"
)

type Result interface{}

type Job interface {
	Path() string
	Fullpath() string
	Error() error
	Info() os.FileInfo

	Result() chan<- Result
}

type Entry struct {
	basedir string
	path    string
	info    os.FileInfo
	error   error
	result  chan<- Result

	// points to the old node if available, interface{} is used to prevent
	// circular import
	Node interface{}
}

func (e Entry) Path() string          { return e.path }
func (e Entry) Fullpath() string      { return filepath.Join(e.basedir, e.path) }
func (e Entry) Error() error          { return e.error }
func (e Entry) Info() os.FileInfo     { return e.info }
func (e Entry) Result() chan<- Result { return e.result }

type Dir struct {
	basedir string
	path    string
	error   error
	info    os.FileInfo

	Entries [](<-chan Result)
	result  chan<- Result
}

func (e Dir) Path() string          { return e.path }
func (e Dir) Fullpath() string      { return filepath.Join(e.basedir, e.path) }
func (e Dir) Error() error          { return e.error }
func (e Dir) Info() os.FileInfo     { return e.info }
func (e Dir) Result() chan<- Result { return e.result }

// readDirNames reads the directory named by dirname and returns
// a sorted list of directory entries.
// taken from filepath/path.go
func readDirNames(dirname string) ([]string, error) {
	f, err := os.Open(dirname)
	if err != nil {
		return nil, err
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

func isDir(fi os.FileInfo) bool {
	return fi.IsDir()
}

var errCancelled = errors.New("walk cancelled")

// SelectFunc returns true for all items that should be included (files and
// dirs). If false is returned, files are ignored and dirs are not even walked.
type SelectFunc func(item string, fi os.FileInfo) bool

func walk(basedir, dir string, selectFunc SelectFunc, done <-chan struct{}, jobs chan<- Job, res chan<- Result) {
	debug.Log("pipe.walk", "start on %v", dir)

	relpath, err := filepath.Rel(basedir, dir)
	if err != nil {
		panic(err)
	}

	info, err := os.Lstat(dir)
	if err != nil {
		debug.Log("pipe.walk", "error for %v: %v", dir, err)
		select {
		case jobs <- Dir{basedir: basedir, path: relpath, info: info, error: err, result: res}:
		case <-done:
		}
		return
	}

	if !selectFunc(dir, info) {
		debug.Log("pipe.walk", "file %v excluded by filter", dir)
		return
	}

	if !info.IsDir() {
		select {
		case jobs <- Entry{info: info, basedir: basedir, path: relpath, result: res}:
		case <-done:
		}
		return
	}

	debug.RunHook("pipe.readdirnames", dir)
	names, err := readDirNames(dir)
	if err != nil {
		debug.Log("pipe.walk", "Readdirnames(%v) returned error: %v", dir, err)
		select {
		case <-done:
		case jobs <- Dir{basedir: basedir, path: relpath, info: info, error: err, result: res}:
		}
		return
	}

	// Insert breakpoint to allow testing behaviour with vanishing files
	// between Readdir() and lstat()
	debug.RunHook("pipe.walk1", relpath)

	entries := make([]<-chan Result, 0, len(names))

	for _, name := range names {
		subpath := filepath.Join(dir, name)

		fi, statErr := os.Lstat(subpath)
		if !selectFunc(subpath, fi) {
			debug.Log("pipe.walk", "file %v excluded by filter", subpath)
			continue
		}

		ch := make(chan Result, 1)
		entries = append(entries, ch)

		if statErr != nil {
			select {
			case jobs <- Entry{info: fi, error: statErr, basedir: basedir, path: filepath.Join(relpath, name), result: ch}:
			case <-done:
				return
			}
			continue
		}

		// Insert breakpoint to allow testing behaviour with vanishing files
		// between walk and open
		debug.RunHook("pipe.walk2", filepath.Join(relpath, name))

		walk(basedir, subpath, selectFunc, done, jobs, ch)
	}

	select {
	case jobs <- Dir{basedir: basedir, path: relpath, info: info, Entries: entries, result: res}:
	case <-done:
	}
}

// Walk sends a Job for each file and directory it finds below the paths. When
// the channel done is closed, processing stops.
func Walk(paths []string, selectFunc SelectFunc, done chan struct{}, jobs chan<- Job, res chan<- Result) {
	debug.Log("pipe.Walk", "start on %v", paths)
	defer func() {
		debug.Log("pipe.Walk", "output channel closed")
		close(jobs)
	}()

	entries := make([]<-chan Result, 0, len(paths))
	for _, path := range paths {
		debug.Log("pipe.Walk", "start walker for %v", path)
		ch := make(chan Result, 1)
		walk(filepath.Dir(path), path, selectFunc, done, jobs, ch)
		entries = append(entries, ch)
		debug.Log("pipe.Walk", "walker for %v done", path)
	}

	debug.Log("pipe.Walk", "sending root node")
	select {
	case <-done:
		return
	case jobs <- Dir{Entries: entries, result: res}:
	}

	debug.Log("pipe.Walk", "walker done")
}

// Split feeds all elements read from inChan to dirChan and entChan.
func Split(inChan <-chan Job, dirChan chan<- Dir, entChan chan<- Entry) {
	debug.Log("pipe.Split", "start")
	defer debug.Log("pipe.Split", "done")

	inCh := inChan
	dirCh := dirChan
	entCh := entChan

	var (
		dir Dir
		ent Entry
	)

	// deactivate sending until we received at least one job
	dirCh = nil
	entCh = nil
	for {
		select {
		case job, ok := <-inCh:
			if !ok {
				// channel is closed
				return
			}

			if job == nil {
				panic("nil job received")
			}

			// disable receiving until the current job has been sent
			inCh = nil

			switch j := job.(type) {
			case Dir:
				dir = j
				dirCh = dirChan
			case Entry:
				ent = j
				entCh = entChan
			default:
				panic(fmt.Sprintf("unknown job type %v", j))
			}
		case dirCh <- dir:
			// disable sending, re-enable receiving
			dirCh = nil
			inCh = inChan
		case entCh <- ent:
			// disable sending, re-enable receiving
			entCh = nil
			inCh = inChan
		}
	}
}
