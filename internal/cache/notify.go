/*
Monitor file changes in GOROOT and GOPATH
*/
package cache

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
)

type Notifier struct {
	lock    sync.RWMutex
	paths   map[string]struct{}
	watcher *fsnotify.Watcher
	logf    func(string, ...interface{})

	Events chan fsnotify.Event
}

func NewNotifer(logger func(string, ...interface{})) (*Notifier, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	n := &Notifier{
		paths:   make(map[string]struct{}),
		watcher: watcher,
		logf:    logger,
		Events:  make(chan fsnotify.Event, 100),
	}

	go n.runLoop()

	return n, nil
}

// AddPath add path to watcher, do nothing is already added
func (n *Notifier) AddPath(p string) error {
	n.lock.Lock()
	defer n.lock.Unlock()

	p = filepath.Clean(p)
	if _, ok := n.paths[p]; ok {
		return nil
	}

	if err := n.watcher.Add(p); err != nil {
		n.logf("notify: failed to add path: %s: %v", p, err)
		return err
	}
	n.logf("notify: add path: %s", p)
	n.paths[p] = struct{}{}
	return nil
}

// RemovePath remove path from watcher, do nothing if already removed
func (n *Notifier) RemovePath(p string) error {
	n.lock.Lock()
	defer n.lock.Unlock()

	p = filepath.Clean(p)
	if _, ok := n.paths[p]; !ok {
		return nil
	}

	if err := n.watcher.Remove(p); err != nil {
		n.logf("notify: failed to remove path: %s: %v", p, err)
		return err
	}

	n.logf("notify: remove path: %s", p)
	delete(n.paths, p)
	return nil
}

func (n *Notifier) runLoop() {
	defer n.logf("notify: runLoop done")
	for {
		n.logf("notify: runLoop")
		ev, ok := <-n.watcher.Events
		if !ok {
			return
		}
		n.logf("notify: event: %v", ev)

		ext := filepath.Ext(ev.Name)
		if _, ok := goSourceExt[ext]; !ok {
			n.logf("notify: ignore %s, %s", ev.Name, ext)
			continue
		}

		fi, err := os.Stat(ev.Name)
		if err != nil || fi.IsDir() {
			continue
		}

		n.Events <- ev
	}
}

var goSourceExt = map[string]bool{
	".go":      true,
	".c":       true,
	".cc":      true,
	".cxx":     true,
	".h":       true,
	".hh":      true,
	".hpp":     true,
	".hxx":     true,
	".m":       true,
	".f":       true,
	".F":       true,
	".for":     true,
	".f90":     true,
	".s":       true,
	".swig":    true,
	".swigcxx": true,
	".syso":    true,
}
