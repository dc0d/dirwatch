package dirwatch

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/dc0d/retry"
	"github.com/fsnotify/fsnotify"
	"github.com/pkg/errors"
)

//-----------------------------------------------------------------------------

// Event represents a single file system notification.
type Event = fsnotify.Event

//-----------------------------------------------------------------------------

// Watcher watches over a directory and it's sub-directories, recursively.
type Watcher struct {
	notify  func(Event)
	exclude []string

	paths  map[string]bool
	add    chan fspath
	ctx    context.Context
	cancel context.CancelFunc
}

type fspath struct {
	path      string
	recursive *bool
}

// New creates a new *Watcher
func New(notify func(Event), exclude ...string) *Watcher {
	if notify == nil {
		panic("notify can not be nil")
	}

	res := &Watcher{
		add:     make(chan fspath),
		paths:   make(map[string]bool),
		notify:  notify,
		exclude: exclude,
	}
	res.ctx, res.cancel = context.WithCancel(context.Background())

	res.start()
	return res
}

// Stop stops the watcher. Safe to be called mutiple times.
func (dw *Watcher) Stop() {
	dw.cancel()
}

// Add adds a path to be watched.
func (dw *Watcher) Add(path string, recursive bool) {
	started := make(chan struct{})
	go func() {
		close(started)
		v, err := filepath.Abs(path)
		if err != nil {
			lerror(err)
			return
		}
		select {
		case dw.add <- fspath{path: v, recursive: &recursive}:
		case <-dw.stopped():
			return
		}
	}()
	<-started
}

//-----------------------------------------------------------------------------

func (dw *Watcher) stopped() <-chan struct{} { return dw.ctx.Done() }

func (dw *Watcher) start() {
	started := make(chan struct{})
	go func() {
		close(started)
		retry.Retry(
			dw.agent,
			-1,
			func(e error) { lerrorf("watcher agent error: %+v", e) },
			time.Second*5)
	}()
	<-started
	// HACK:
	<-time.After(time.Millisecond * 500)
}

func (dw *Watcher) agent() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return errors.WithStack(err)
	}
	defer watcher.Close()

	for {
		select {
		case <-dw.stopped():
			return nil
		case ev := <-watcher.Events:
			dw.onEvent(ev)
		case err := <-watcher.Errors:
			lerrorf("error: %+v\n", errors.WithStack(err))
		case d := <-dw.add:
			dw.onAdd(watcher, d)
		}
	}
}

func (dw *Watcher) onAdd(
	watcher *fsnotify.Watcher,
	fsp fspath) {
	if fsp.path == "" {
		return
	}
	var err error
	fsp.path, err = filepath.Abs(fsp.path)
	if err != nil {
		lerror(err)
		return
	}
	_, err = os.Stat(fsp.path)
	if err != nil {
		if os.IsNotExist(err) {
			delete(dw.paths, fsp.path)
			return
		}
		lerror(err)
		return
	}
	recursive, ok := dw.paths[fsp.path]
	if ok {
		return
	}
	if dw.excludePath(fsp.path) {
		return
	}
	if err := watcher.Add(fsp.path); err != nil {
		lerrorf("on add error: %+v\n", errors.WithStack(err))
	}
	if fsp.recursive != nil {
		recursive = *fsp.recursive
	}
	dw.paths[fsp.path] = recursive
	isd, _ := isDir(fsp.path)
	if recursive && isd {
		go func() {
			tree := dirTree(fsp.path)
			for v := range tree {
				dw.add <- fspath{path: v}
			}
		}()
	}
}

func (dw *Watcher) onEvent(ev Event) {
	if dw.excludePath(ev.Name) {
		return
	}
	// callback
	go retry.Try(func() error { dw.notify(ev); return nil })

	name := ev.Name
	isdir, err := isDir(name)
	if err != nil {
		if os.IsNotExist(err) {
			delete(dw.paths, name)
		} else {
			lerror(err)
		}
		return
	}

	if !isdir {
		return
	}

	go func() {
		select {
		case <-dw.stopped():
			return
		case dw.add <- fspath{path: name}:
		}
	}()
}

func (dw *Watcher) excludePath(p string) bool {
	for _, ptrn := range dw.exclude {
		matched, err := filepath.Match(ptrn, p)
		if err != nil {
			lerror(err)
			continue
		}
		if matched {
			return true
		}
	}
	return false
}

func dirTree(queryRoot string) <-chan string {
	found := make(chan string)
	go func() {
		defer close(found)
		err := filepath.Walk(queryRoot, func(path string, f os.FileInfo, err error) error {
			if !f.IsDir() {
				return nil
			}
			if filepath.Clean(path) == filepath.Clean(queryRoot) {
				return nil
			}
			found <- path
			return nil
		})
		if err != nil {
			lerrorf("%+v", errors.WithStack(err))
		}
	}()
	return found
}

func isDir(path string) (bool, error) {
	inf, err := os.Stat(path)
	return inf.IsDir(), err
}

//-----------------------------------------------------------------------------
