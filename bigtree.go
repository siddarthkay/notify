package notify

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rjeczalik/fs"
)

// TODO(rjeczalik): Pair every LookPath (the one building paths in a Tree) with
// a TryDel on failures to not have dangling paths?
// TODO(rjeczalik): Refactor all Try*/Look*/Walk* methods.
// TODO(rjeczalik): Create two Tree implementations, one for native recursive
// watchers and second for fake ones.
// TODO(rjeczalik): Locking.
// TODO(rjeczalik): Concurrent tree-dispatch.
// TODO(rjeczalik): Move to separate package notify/tree.

// pathError TODO
type pathError struct {
	Name string
}

func (err pathError) Error() string {
	return `notify: invalid path "` + err.Name + `"`
}

// impl TODO
type impl interface {
	watcher
	recursiveWatcher
}

// Tree TODO
type bigTree struct {
	FS   fs.Filesystem
	Root node

	cnd   chanNodesMap
	stop  chan struct{}
	impl  impl
	isrec bool
}

func (t *bigTree) fs() fs.Filesystem {
	if t.FS != nil {
		return t.FS
	}
	return fs.Default
}

func (t *bigTree) setimpl(w watcher) {
	if os, ok := w.(impl); ok {
		t.impl = os
		t.isrec = true
		return
	}
	t.impl = struct {
		watcher
		recursiveWatcher
	}{w, t}
}

func (t *bigTree) loopdispatch(c <-chan EventInfo) {
	nd, ok := node{}, false
	for {
		select {
		case ei := <-c:
			parent, name := split(ei.Path())
			fn := func(it node, isbase bool) (_ error) {
				// TODO(rjeczalik): rm bool
				if isbase {
					nd = it
				} else {
					it.Watch.Dispatch(ei, recursive)
				}
				return
			}
			// Send to recursive watchpoints.
			if err := t.TryWalkPath(parent, fn); err != nil {
				dbg.Printf("unexpected event: ei=%v, err=%v", ei, err)
			}
			// Send to parent watchpoint.
			nd.Watch.Dispatch(ei, 0)
			// Try send to self watchpoint.
			if nd, ok = nd.Child[name]; ok {
				nd.Watch.Dispatch(ei, 0)
			}
		case <-t.stop:
			return
		}
	}
}

// NewTree TODO
func newTree(w watcher, c <-chan EventInfo) *bigTree {
	t := &bigTree{
		Root: node{Child: make(map[string]node), Watch: make(watchpoint)},
		cnd:  make(chanNodesMap),
		stop: make(chan struct{}),
	}
	t.setimpl(w)
	go t.loopdispatch(c)
	return t
}

func (t *bigTree) root(p string) (node, int) {
	vol := filepath.VolumeName(p)
	return t.Root.child(vol), len(vol) + 1
}

// TryLookPath TODO
func (t *bigTree) TryLookPath(p string) (it node, err error) {
	// TODO(rjeczalik): os.PathSeparator or enforce callers to not pass separator?
	if p == "" || p == "/" {
		return t.Root, nil
	}
	i, ok := 0, false
	it, i = t.root(p)
	for j := indexSep(p[i:]); j != -1; j = indexSep(p[i:]) {
		if it, ok = it.Child[p[i:i+j]]; !ok {
			err = &os.PathError{
				Op:   "TryWalkPath",
				Path: p[:i+j],
				Err:  os.ErrNotExist,
			}
			return
		}
		i += j + 1
	}
	if it, ok = it.Child[p[i:]]; !ok {
		err = &os.PathError{
			Op:   "TryWalkPath",
			Path: p,
			Err:  os.ErrNotExist,
		}
	}
	return
}

// LookPath TODO
//
// TODO(rjeczalik): LookPath(p) should be Look(w.Root, p)
func (t *bigTree) LookPath(p string) node {
	// TODO(rjeczalik): os.PathSeparator or enforce callers to not pass separator?
	if p == "" || p == "/" {
		return t.Root
	}
	it, i := t.root(p)
	for j := indexSep(p[i:]); j != -1; j = indexSep(p[i:]) {
		it = it.child(p[i : i+j])
		i += j + 1
	}
	return it.child(p[i:])
}

// Look TODO
func (t *bigTree) Look(nd node, p string) node {
	if nd.Name == p {
		return nd
	}
	if !strings.HasPrefix(p, nd.Name) || p[len(nd.Name)] != os.PathSeparator {
		return t.LookPath(p)
	}
	i := len(nd.Name) + 1
	for j := indexSep(p[i:]); j != -1; j = indexSep(p[i:]) {
		nd = nd.child(p[i : i+j])
		i += j + 1
	}
	return nd.child(p[i:])
}

// Del TODO
//
// TODO(rjeczalik):
func (t *bigTree) Del(p string) {
	it, i := t.root(p)
	stack := []node{it}
	for j := indexSep(p[i:]); j != -1; j = indexSep(p[i:]) {
		it = it.child(p[i : i+j])
		stack = append(stack, it)
		i += j + 1
	}
	it = it.child(p[i:])
	it.Child = nil
	it.Watch = nil
	name := base(it.Name)
	for i = len(stack); i > 0; i-- {
		it = stack[i-1]
		// TODO(rjeczalik): Watch[nil] != 0
		// NOTE(rjeczalik): Event empty watchpoints can hold special nil key.
		if child := it.Child[name]; len(child.Watch) > 1 || len(child.Child) != 0 {
			break
		} else {
			child.Child = nil
			child.Watch = nil
		}
		delete(it.Child, name)
		name = base(it.Name)
	}
}

// TryWalkPath TODO
func (t *bigTree) TryWalkPath(p string, fn walkPathFunc) error {
	ok := false
	it, i := t.root(p)
	for j := indexSep(p[i:]); j != -1; j = indexSep(p[i:]) {
		if it, ok = it.Child[p[i:i+j]]; !ok {
			return &os.PathError{
				Op:   "TryWalkPath",
				Path: p[:i+j],
				Err:  os.ErrNotExist,
			}
		}
		switch err := fn(it, false); err {
		case nil:
		case skip:
			return nil
		default:
			return err
		}
		i += j + 1
	}
	if it, ok = it.Child[p[i:]]; !ok {
		return &os.PathError{
			Op:   "TryWalkPath",
			Path: p,
			Err:  os.ErrNotExist,
		}
	}
	if err := fn(it, true); err != nil && err != skip {
		return err
	}
	return nil
}

// WalkPath TODO
//
// NOTE(rjeczalik): WalkPath assumes the p is clean.
func (t *bigTree) WalkPath(p string, fn walkPathFunc) error {
	it, i := t.root(p)
	for j := indexSep(p[i:]); j != -1; j = indexSep(p[i:]) {
		it = it.child(p[i : i+j])
		switch err := fn(it, false); err {
		case nil:
		case skip:
			return nil
		default:
			return err
		}
		i += j + 1
	}
	if err := fn(it.child(p[i:]), true); err != nil && err != skip {
		return err
	}
	return nil
}

// WalkDir TODO
//
// Uses BFS.
func (t *bigTree) WalkDir(nd node, fn walkFunc) error {
	switch err := fn(nd); err {
	case nil:
	case skip:
		return nil
	default:
		return err
	}
	stack := []node{nd}
	for n := len(stack); n != 0; n = len(stack) {
		nd, stack = stack[n-1], stack[:n-1]
		f, err := t.fs().Open(nd.Name)
		if err != nil {
			return err
		}
		fis, err := f.Readdir(0)
		f.Close()
		if err != nil {
			return err
		}
		for _, fi := range fis {
			if fi.IsDir() {
				// TODO(rjeczalik): get rid of filepath.Base
				child := nd.child(filepath.Base(fi.Name()))
				switch err := fn(child); err {
				case nil:
					stack = append(stack, child)
				case skip:
				default:
					return err
				}
			}
		}
	}
	return nil
}

// Walk TODO
//
// Uses BFS.
func (t *bigTree) Walk(nd node, fn walkFunc) error {
	switch err := fn(nd); err {
	case nil:
	case skip:
		return nil
	default:
		return err
	}
	stack := []node{nd}
	for n := len(stack); n != 0; n = len(stack) {
		nd, stack = stack[n-1], stack[:n-1]
		// TODO(rjeczalik): The sorting is temporary workaround required by
		// unittests. Remove it after they're dropped and replaced with
		// functional ones.
		names := make([]string, 0, len(nd.Child))
		for name := range nd.Child {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			child := nd.Child[name] // TODO
			switch err := fn(child); err {
			case nil:
				if len(child.Child) != 0 {
					stack = append(stack, child)
				}
			case skip:
			default:
				return err
			}
		}
	}
	return nil
}

// TODO(rjeczalik): Rename.
func (t *bigTree) register(nd node, c chan<- EventInfo, e Event) eventDiff {
	t.cnd.Add(c, nd)
	// TODO(rjeczalik): check if any of the parents are being watched recursively
	// and the event set is sufficient.
	return nd.Watch.Add(c, e)
}

// TODO(rjeczalik): Rename.
func (t *bigTree) unregister(nd node, c chan<- EventInfo, e Event) eventDiff {
	diff := nd.Watch.Del(c, e)
	if diff != none && diff[1] == 0 {
		// TODO(rjeczalik): Use Node for lookup?
		t.Del(nd.Name)
	}
	t.cnd.Del(c, nd)
	return diff
}

// TODO(rjeczalik): Transfer file watchpoint to its directory one?
//
// TODO(rjeczalik): check if any of the parents are being watched recursively
// and the event set is sufficient.
func (t *bigTree) watch(p string, c chan<- EventInfo, e Event) (err error) {
	nd := t.LookPath(p)
	diff := t.register(nd, c, e) // TODO(rjeczalik): inline t.register here?
	if t.isrec {
		// If p is covered by a recursive watchpoint (which can be set above p),
		// we need to rewatch it instead the current one.
		if t.TryWalkPath(p, func(it node, isbase bool) error {
			if it.Watch.IsRecursive() {
				if !isbase {
					diff = it.Watch.AddRecursive(e)
				}
				nd = it
				return found
			}
			return nil
		}) == found {
			switch {
			case diff == none, diff[0] == 0:
			default:
				err = t.impl.RecursiveRewatch(nd.Name, nd.Name, diff[0], diff[1])
				diff = none
			}
		}
	}
	switch {
	case diff == none:
	case diff[0] == 0:
		err = t.impl.Watch(p, diff[1])
	default:
		err = t.impl.Rewatch(p, diff[0], diff[1])
	}
	if err != nil {
		t.unregister(nd, c, diff.Event()) // TODO(rjeczalik): test fine-grained revert
		// TODO(rjeczalik): TryDel?
	}
	return
}

// NOTE(rjeczalik): strategy for fake recursive watcher
func (t *bigTree) watchrec(nd node, c chan<- EventInfo, e Event) (err error) {
	diff := nd.Watch.AddRecursive(e)
	switch {
	case diff == none:
	case diff[0] == 0:
		err = t.impl.RecursiveWatch(nd.Name, diff[1])
	default:
		err = t.impl.RecursiveRewatch(nd.Name, nd.Name, diff[0], diff[1])
	}
	if err != nil {
		nd.Watch.DelRecursive(diff.Event())
		// TODO(rjeczalik): TryDel?
		return
	}
	if diff = t.register(nd, c, e); diff != none {
		panic(fmt.Sprintf("[DEBUG] unexpected non-empty diff: %v", diff))
	}
	return
}

// NOTE(rjeczalik): strategy for native recursive watcher
func (t *bigTree) mergewatchrec(p string, c chan<- EventInfo, e Event) error {
	nd := (*node)(nil)
	// Look up existing, recursive watchpoint already covering the given p.
	err := t.TryWalkPath(p, func(it node, isbase bool) error {
		if it.Watch.IsRecursive() {
			nd = &it
			return skip
		}
		return nil
	})
	if nd != nil {
		// Luckily we have already a recursive watchpoint, now we check whether
		// requested event fits in it and rewatch if not.
		diff := nd.Watch.AddRecursive(e)
		switch {
		case diff == none:
		case diff[0] == 0:
			panic("[DEBUG] dangling watchpoint: " + nd.Name) // TODO
		default:
			if err := t.impl.RecursiveRewatch(nd.Name, nd.Name, diff[0], diff[1]); err != nil {
				nd.Watch.DelRecursive(diff.Event())
				return err
			}
		}
		// TODO(rjeczalik): introduce external watch? watchpoint has e event-set
		// but has no underlying watch, since it's external.
		_ = t.register(t.Look(*nd, p), c, e)
		return nil
	}
	// If previous lookup did not fail (*os.PathError - no such path in the tree),
	// there is a chance there exist one or more recursive watchpoints in the
	// subtree starting at p - we would need to rewatch those as one watch can
	// compensate for several smaller ones.
	var nds nodeSet
	var erec = e
	switch err {
	case nil:
		// TODO(rjeczalik): Use previous nd for Look root?
		nd, err := t.TryLookPath(p)
		if err != nil {
			break
		}
		err = t.Walk(nd, func(it node) error {
			if it.Watch.IsRecursive() {
				nds.Add(it) // node with shortest path is preferred for RecursiveRewatch
				erec |= it.Watch.recursive()
				return skip
			}
			return nil
		})
		if err != nil {
			panic("[DEBUG] unexpected error: " + err.Error()) // TODO
		}
	}
	// TODO(rjeczalik): rm duplication case 1 + default
	switch nd := t.LookPath(p); len(nds) {
	case 0:
		return t.watchrec(nd, c, e) // register regular watchpoint
	case 1:
		// There exists only one recursive, child watchpoint - it's enough to just
		// rewatch it.
		diff := nd.Watch.AddRecursive(erec)
		// If the event set does not need to be expanded, we still need to relocate
		// the watchpoint.
		if diff == none {
			diff[0], diff[1] = erec, erec
		}
		if err = t.impl.RecursiveRewatch(nds[0].Name, nd.Name, diff[0], diff[1]); err != nil {
			nd.Watch.DelRecursive(diff.Event())
			// TODO(rjeczalik): TryDel?
			return err
		}
		if diff = t.register(nd, c, e); diff != none {
			panic(fmt.Sprintf("[DEBUG] unexpected non-empty diff: %v", diff))
		}
		return nil
	default: // TODO ensure RecursiveUnwatch and Stop supports splitting watchpoints
		// There exist multiple recursive, child watchpoints - we need to unwatch
		// all but one, and the last rewatch to new location.
		n := 0
		for i := range nds {
			// TODO(rjeczalik): add (Watchpoint).Total() Event
			if nds[i].Watch[nil]&erec == erec {
				n = i
				break
			}
		}
		for i := range nds {
			if i == n {
				continue
			}
			// NOTE(rjeczalik): DelRecursive is intentionally not issued to allow
			// for later recursive watchpoint splitting in case of RecrusiveUnwatch
			// or Stop.
			if err = t.impl.RecursiveUnwatch(nds[i].Name); err != nil {
				// TODO(rjeczalik): consistency?
				panic("[DEBUG] unexpected error: " + err.Error())
			}
		}
		diff := nd.Watch.AddRecursive(erec)
		// If the event set does not need to be expanded, we still need to relocate
		// the watchpoint.
		if diff == none {
			diff[0], diff[1] = erec, erec
		}
		if err = t.impl.RecursiveRewatch(nds[n].Name, nd.Name, diff[0], diff[1]); err != nil {
			nd.Watch.DelRecursive(diff.Event())
			return err
		}
		if diff = t.register(nd, c, e); diff != none {
			panic(fmt.Sprintf("[DEBUG] unexpected non-empty diff: %v", diff))
		}
		return nil
	}
}

// Watch TODO
//
// Watch does not support symlinks as it does not care. If user cares, p should
// be passed to os.Readlink first.
func (t *bigTree) Watch(p string, c chan<- EventInfo, e ...Event) (err error) {
	if c == nil {
		panic("notify: Watch using nil channel")
	}
	// Expanding with empty event set is a nop.
	if len(e) == 0 {
		return nil
	}
	isrec := false
	if strings.HasSuffix(p, "...") {
		p, isrec = p[:len(p)-3], true
	}
	if p, err = filepath.Abs(p); err != nil {
		return err
	}
	switch e := joinevents(e); {
	case isrec && t.isrec:
		return t.mergewatchrec(p, c, e|recursive)
	case isrec:
		return t.watchrec(t.LookPath(p), c, e|recursive)
	default:
		return t.watch(p, c, e)
	}
}

// TODO(rjeczalik): lookupRecursiveWatchpoint?
var found = errors.New("found")

// Stop TODO
func (t *bigTree) Stop(c chan<- EventInfo) {
	if nds, ok := t.cnd[c]; ok {
		var err error
		for _, nd := range *nds {
			e := nd.Watch[c]
			diff := nd.Watch.Del(c, e)
			if t.isrec {
				if t.TryWalkPath(nd.Name, func(it node, isbase bool) error {
					if it.Watch.IsRecursive() && !isbase {
						nd = it
						return found
					}
					return nil
				}) == found {
					// Recalculate recurisve event set by walking the subtree.
					diff = nd.Watch.DelRecursive(e)
					diff[1] = 0
					t.Walk(nd, func(it node) (_ error) {
						diff[1] |= it.Watch[nil]
						return
					})
					diff[1] &^= recursive
					nd.Watch.AddRecursive(diff[1])
					switch {
					case diff[0] == diff[1]: // None
					case diff[1] == 0:
						err = t.impl.RecursiveUnwatch(nd.Name)
						t.Del(nd.Name)
					default:
						err = t.impl.RecursiveRewatch(nd.Name, nd.Name, diff[0], diff[1])
					}
					diff = none
				}
			}
			switch {
			case diff == none:
			case diff[1] == 0:
				err = t.impl.Unwatch(nd.Name)
				t.Del(nd.Name)
			default:
				err = t.impl.Rewatch(nd.Name, diff[0], diff[1])
			}
			if err != nil {
				panic("[DEBUG] unhandled error: " + err.Error())
			}
		}
		delete(t.cnd, c)
	}
}

// Close TODO
//
// TODO(rjeczalik): Make unexported or remove all watchpoints?
func (t *bigTree) Close() error {
	close(t.stop)
	return nil
}

// RecursiveWatch implements notify.RecursiveWatcher interface.
func (t *bigTree) RecursiveWatch(p string, e Event) error {
	// Before we're able to decide whether we should watch or rewatch p,
	// an watchpoint must be registered for the path.
	// That's why till this point we already have a watchpoint, so we just watch
	// the p.
	if err := t.impl.Watch(p, e); err != nil {
		return err
	}
	fn := func(nd node) error {
		switch diff := nd.Watch.AddRecursive(e); {
		case diff == none:
			return nil
		case diff[0] == 0:
			return t.impl.Watch(nd.Name, diff[1])
		default:
			return t.impl.Rewatch(nd.Name, diff[0], diff[1])
		}
	}
	return t.WalkDir(t.LookPath(p), fn)
}

// RecursiveUnwatch implements notify.RecursiveWatcher interface.
func (t *bigTree) RecursiveUnwatch(p string) error {
	return errors.New("RecurisveUnwatch TODO(rjeczalik)")
}

// Rewatch implements notify.Rewatcher interface.
func (t *bigTree) Rewatch(p string, olde, newe Event) error {
	if err := t.impl.Unwatch(p); err != nil {
		return err
	}
	return t.impl.Watch(p, newe)
}

// RecursiveRewatch implements notify.RecursiveRewatcher interface.
func (t *bigTree) RecursiveRewatch(oldp, newp string, olde, newe Event) error {
	if oldp != newp {
		switch {
		case strings.HasPrefix(newp, oldp):
			// newp is deeper in the tree than oldp, all watchpoints above newp
			// must be deleted.
			fn := func(nd node) (err error) {
				if nd.Name == newp {
					return skip
				}
				switch diff := nd.Watch.DelRecursive(olde); {
				case diff == none:
				case diff[1] == 0:
					err = t.impl.Unwatch(nd.Name)
				default:
					err = t.impl.Rewatch(nd.Name, diff[0], diff[1])
				}
				// TODO(rjeczalik): don't stop if single watch call fails
				return
			}
			nd, err := t.TryLookPath(oldp)
			if err != nil {
				// BUG(rjeczalik): Relocation requested for a path, which does
				// not exist.
				panic(err) // DEBUG
			}
			err = t.Walk(nd, fn)
		case strings.HasPrefix(oldp, newp):
			// This watchpoint relocation is handled below.
		default:
			// May happen, e.g. if oldp="C:\Windows" and newp="D:\Temp", which
			// is a bug.
			panic("[DEBUG] " + oldp + " and " + newp + " do not have common root")
		}
	}
	if olde != newe {
		// Watchpoint for newp was already registered prior to executing this
		// method. Here's the deferred Rewatch:
		if err := t.impl.Rewatch(newp, olde, newe); err != nil {
			return err
		}
	}
	fn := func(nd node) (err error) {
		if olde == newe && nd.Name == oldp {
			// Relocation that does not need to rewatch the old subtree.
			return skip
		}
		diff := nd.Watch.AddRecursive(newe)
		switch {
		case diff == none:
		case diff[0] == 0:
			err = t.impl.Watch(nd.Name, diff[1])
		default:
			err = t.impl.Rewatch(nd.Name, diff[0], diff[1])
		}
		return
	}
	return t.Walk(t.LookPath(newp), fn)
}
